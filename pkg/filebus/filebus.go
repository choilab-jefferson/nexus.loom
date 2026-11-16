// Package filebus is a Go stdlib client for the nexus file bus (nexus.lite docs/specs/file-bus.md).
//
// The bus directory is the source of truth: messages, cursors, sessions and the layout version are files.
// Correctness uses only create-temp, write, rename within one directory, read, list and unlink; the Unix
// datagram / TCP notify layer in notify.go is a latency optimisation (spec section 10). Nothing here opens SQLite.
package filebus

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	// Format and MajorVersion identify the layout this client implements (spec section 11).
	Format       = "nexus.filebus"
	MajorVersion = 1
	// DefaultSkewWindowMS is W of spec section 4.3 / D1 (300 s).
	DefaultSkewWindowMS = 300000
	// MaxInlineBody is the largest body kept inline in a message file (D3); bigger bodies use body_ref.
	MaxInlineBody = 256 * 1024
	// DefaultConsumer is the API name of the default consumer; it is stored as "_default".
	DefaultConsumer = ""
	// Broadcast is the reserved broadcast target.
	Broadcast = "ALL"

	defaultFile        = "_default"
	invalidGraceSecond = 60
	sizeListCache      = time.Second
)

// ErrVersion is returned when the bus layout major version is newer than this client knows.
var ErrVersion = errors.New("bus layout version unsupported")

var idRe = regexp.MustCompile(`^m[0-9]{13}-[0-9a-f]{8}-[0-9]{6}$`)

// ValidID reports whether s is a spec section 4.1 message id.
func ValidID(s string) bool { return idRe.MatchString(s) }

// IDMillis returns the Unix-millisecond part of a valid id, or -1.
func IDMillis(id string) int64 {
	if !ValidID(id) {
		return -1
	}
	var n int64
	for _, c := range id[1:14] {
		n = n*10 + int64(c-'0')
	}
	return n
}

// ValidName checks a target, consumer or session name (spec section 3).
func ValidName(s string) error {
	switch {
	case s == "" || s == "." || s == "..":
		return fmt.Errorf("invalid bus name %q", s)
	case strings.ContainsAny(s, "/\\"):
		return fmt.Errorf("invalid bus name %q: contains a path separator", s)
	case s[0] == '.' || s[0] == '_':
		return fmt.Errorf("invalid bus name %q: must not start with . or _", s)
	case len(s) > 128:
		return fmt.Errorf("invalid bus name: longer than 128 bytes")
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("invalid bus name %q: control character", s)
		}
	}
	return nil
}

// Message is one message file (nexus.filebus.msg v1, spec section 5.1).
type Message struct {
	V         int             `json:"v"`
	ID        string          `json:"id"`
	Sender    string          `json:"sender"`
	Target    string          `json:"target"`
	Kind      string          `json:"kind"`
	Summary   string          `json:"summary"`
	Timestamp float64         `json:"timestamp"`
	Body      string          `json:"body"`
	BodyRef   *string         `json:"body_ref"`
	Data      json.RawMessage `json:"data"`
	Meta      map[string]any  `json:"meta"`
}

// MarshalJSON writes `"body": null` when body_ref is set (spec: body is null with a body_ref; readers prefer
// body_ref and accept "").
func (m Message) MarshalJSON() ([]byte, error) {
	var body any = m.Body
	if m.BodyRef != nil && *m.BodyRef != "" {
		body = nil
	}
	return json.Marshal(struct {
		V         int             `json:"v"`
		ID        string          `json:"id"`
		Sender    string          `json:"sender"`
		Target    string          `json:"target"`
		Kind      string          `json:"kind"`
		Summary   string          `json:"summary"`
		Timestamp float64         `json:"timestamp"`
		Body      any             `json:"body"`
		BodyRef   *string         `json:"body_ref"`
		Data      json.RawMessage `json:"data"`
		Meta      map[string]any  `json:"meta"`
	}{m.V, m.ID, m.Sender, m.Target, m.Kind, m.Summary, m.Timestamp, body, m.BodyRef, m.Data, m.Meta})
}

// MetaString returns meta[key] when it is a non-empty string.
func (m Message) MetaString(key string) string {
	s, _ := m.Meta[key].(string)
	return s
}

// Outgoing describes a message to send. Sender, Target and Kind are required.
type Outgoing struct {
	Sender  string
	Target  string
	Kind    string
	Body    string
	Summary string // default: first non-empty line of Body, <= 100 chars
	Data    any
	Meta    map[string]any // merged over the standard meta keys
}

// Bus is a handle on one bus directory. It is safe for concurrent use.
type Bus struct {
	Dir          string
	SkewWindowMS int64
	// Logf receives diagnostics (ids only, never content). Nil discards them.
	Logf func(format string, args ...any)

	now func() time.Time

	mu        sync.Mutex
	writer    string
	ctr       int
	lastMS    int64
	newest    map[string]newestEntry
	invalid   map[string]time.Time
	reported  map[string]bool
	hostname  string
	selfSock  string // our own wake-up socket, skipped when notifying
	kernelMu  sync.Mutex
	kernelVal string
}

type newestEntry struct {
	at time.Time
	ms int64
}

// Open creates the bus layout under dir when absent and checks VERSION (spec section 11).
func Open(dir string) (*Bus, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, errors.New("bus dir is empty")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	b := &Bus{Dir: abs, SkewWindowMS: DefaultSkewWindowMS, now: time.Now,
		newest: map[string]newestEntry{}, invalid: map[string]time.Time{}, reported: map[string]bool{}}
	b.writer = randHex(4)
	b.hostname, _ = os.Hostname()
	for _, sub := range []string{"", "msg", "blob", "cur", "sess", "state"} {
		if err := os.MkdirAll(filepath.Join(abs, sub), 0o700); err != nil {
			return nil, err
		}
	}
	if err := b.checkVersion(); err != nil {
		return nil, err
	}
	return b, nil
}

type versionFile struct {
	Format       string  `json:"format"`
	Version      int     `json:"version"`
	Minor        int     `json:"minor"`
	SkewWindowMS int64   `json:"skew_window_ms"`
	Created      float64 `json:"created"`
}

func (b *Bus) checkVersion() error {
	path := filepath.Join(b.Dir, "VERSION")
	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		raw, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			v := versionFile{Format: Format, Version: MajorVersion, SkewWindowMS: DefaultSkewWindowMS, Created: toSeconds(b.now())}
			data, _ := json.Marshal(v)
			if werr := b.writeAtomic(b.Dir, "VERSION", append(data, '\n')); werr != nil {
				return werr
			}
			continue
		}
		if err != nil {
			return err
		}
		var v versionFile
		if err := json.Unmarshal(raw, &v); err != nil || v.Format != Format {
			lastErr = fmt.Errorf("bus VERSION file is not a %s document", Format)
			time.Sleep(20 * time.Millisecond)
			continue
		}
		if v.Version > MajorVersion {
			return fmt.Errorf("bus layout version %d unsupported: %w", v.Version, ErrVersion)
		}
		if v.SkewWindowMS > 0 {
			b.SkewWindowMS = v.SkewWindowMS
		}
		return nil
	}
	return lastErr
}

func (b *Bus) logf(format string, args ...any) {
	if b.Logf != nil {
		b.Logf(format, args...)
	}
}

func randHex(nBytes int) string {
	buf := make([]byte, nBytes)
	_, _ = rand.Read(buf)
	return hex.EncodeToString(buf)
}

func toSeconds(t time.Time) float64 { return float64(t.UnixNano()/1e6) / 1000 }

// writeAtomic writes data to dir/final through a same-directory temp file and rename (spec section 5.2).
func (b *Bus) writeAtomic(dir, final string, data []byte) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp := filepath.Join(dir, fmt.Sprintf(".tmp.%s.%s", b.writer, randHex(4)))
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, werr := f.Write(data)
	if serr := f.Sync(); serr != nil {
		b.logf("fsync ignored: %v", serr)
	}
	cerr := f.Close()
	if werr != nil || cerr != nil {
		os.Remove(tmp)
		return errors.Join(werr, cerr)
	}
	if err := os.Rename(tmp, filepath.Join(dir, final)); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// marshal encodes v as one compact JSON line without HTML escaping.
func marshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func (b *Bus) msgDir(target string) string { return filepath.Join(b.Dir, "msg", target) }

// listIDs returns the valid message ids in msg/<target>/ in id order.
func (b *Bus) listIDs(target string) []string {
	entries, err := os.ReadDir(b.msgDir(target))
	if err != nil {
		return nil
	}
	ids := make([]string, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".json") {
			continue
		}
		if id := strings.TrimSuffix(name, ".json"); ValidID(id) {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

// newestForeignMS is the largest ms among ids in msg/<target>/ written by other writers (cached for 1 s).
// Our own ids are covered by lastMS, so a burst does not drift ahead of the wall clock.
func (b *Bus) newestForeignMS(target string) int64 {
	if e, ok := b.newest[target]; ok && b.now().Sub(e.at) < sizeListCache {
		return e.ms
	}
	var ms int64 = -1
	for _, id := range b.listIDs(target) {
		if id[15:23] == b.writer {
			continue
		}
		if m := IDMillis(id); m > ms {
			ms = m
		}
	}
	b.newest[target] = newestEntry{at: b.now(), ms: ms}
	return ms
}

// allocID returns a new id for target with the writer clamp of spec section 4.2.
func (b *Bus) allocID(target string) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	for {
		ms := b.now().UnixMilli()
		if b.lastMS > ms {
			ms = b.lastMS
		}
		if n := b.newestForeignMS(target); n >= 0 && n+1 > ms {
			ms = n + 1
		}
		b.lastMS = ms
		b.ctr = (b.ctr + 1) % 1000000
		id := fmt.Sprintf("m%013d-%s-%06d", ms, b.writer, b.ctr)
		if _, err := os.Stat(filepath.Join(b.msgDir(target), id+".json")); err == nil {
			b.writer = randHex(4) // spec 4.1: our own final name exists, draw a new writer and retry
			continue
		}
		return id
	}
}

func deriveSummary(body string) string {
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			if r := []rune(line); len(r) > 100 {
				return string(r[:100])
			}
			return line
		}
	}
	return ""
}

// Send writes one message (spec section 5.2) and wakes the target's live sessions. It returns the
// message as written; the id is known once the rename has happened.
func (b *Bus) Send(o Outgoing) (Message, error) {
	if err := validTargetName(o.Target); err != nil {
		return Message{}, err
	}
	if o.Sender == "" || o.Kind == "" || strings.ContainsAny(o.Sender+o.Kind, "\x00\n") {
		return Message{}, errors.New("sender and kind are required")
	}
	id := b.allocID(o.Target)
	m := Message{V: 1, ID: id, Sender: o.Sender, Target: o.Target, Kind: o.Kind, Body: o.Body,
		Summary: o.Summary, Timestamp: toSeconds(b.now()), Data: json.RawMessage("null")}
	if m.Summary == "" {
		m.Summary = deriveSummary(o.Body)
	}
	m.Meta = map[string]any{"reply_to": nil, "corr": nil, "task_id": nil, "sender_iid": nil,
		"sender_verified": nil, "redis_id": nil, "origin": "loom"}
	for k, v := range o.Meta {
		m.Meta[k] = v
	}
	if o.Data != nil {
		raw, err := json.Marshal(o.Data)
		if err != nil {
			return Message{}, err
		}
		m.Data = raw
	}
	if len(o.Body) > MaxInlineBody {
		ref := filepath.ToSlash(filepath.Join("blob", o.Target, id+".txt"))
		if err := b.writeAtomic(filepath.Join(b.Dir, "blob", o.Target), id+".txt", []byte(o.Body)); err != nil {
			return Message{}, err
		}
		m.Body, m.BodyRef = "", &ref
	}
	data, err := marshal(m)
	if err != nil {
		return Message{}, err
	}
	if err := b.writeAtomic(b.msgDir(o.Target), id+".json", data); err != nil {
		return Message{}, err
	}
	b.notify(o.Target, id)
	return m, nil
}

func validTargetName(t string) error {
	if t == Broadcast {
		return nil
	}
	return ValidName(t)
}

// WriteBlob writes data to blob/<target>/<id>.<ext> (tmp+rename) and returns the bus-relative path.
// A fresh id is allocated for the file name.
func (b *Bus) WriteBlob(target, ext string, data []byte) (path, id string, err error) {
	if err := validTargetName(target); err != nil {
		return "", "", err
	}
	id = b.allocID(target)
	name := id + "." + strings.TrimPrefix(ext, ".")
	if err := b.writeAtomic(filepath.Join(b.Dir, "blob", target), name, data); err != nil {
		return "", "", err
	}
	return filepath.ToSlash(filepath.Join("blob", target, name)), id, nil
}

// Body returns the full body of m, resolving body_ref under <bus>/blob/.
func (b *Bus) Body(m Message) (string, error) {
	if m.BodyRef == nil || *m.BodyRef == "" {
		return m.Body, nil
	}
	clean := filepath.Clean(filepath.FromSlash(*m.BodyRef))
	if !strings.HasPrefix(clean, "blob"+string(filepath.Separator)) || strings.Contains(clean, "..") {
		return "", fmt.Errorf("body_ref %q is outside blob/", *m.BodyRef)
	}
	raw, err := os.ReadFile(filepath.Join(b.Dir, clean))
	return string(raw), err
}

// readMessage reads and validates one message file (spec section 5.3). ok=false means skip this pass.
func (b *Bus) readMessage(dirTarget, id string) (Message, bool) {
	raw, err := os.ReadFile(filepath.Join(b.msgDir(dirTarget), id+".json"))
	if err != nil {
		return Message{}, false // ENOENT (pruned) or transient
	}
	var m Message
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(&m); err != nil || dec.More() || m.V < 1 || m.V > MajorVersion ||
		m.ID != id || (m.Target != dirTarget && m.Target != Broadcast) || m.Sender == "" || m.Kind == "" {
		b.noteInvalid(id)
		return Message{}, false
	}
	b.mu.Lock()
	delete(b.invalid, id)
	b.mu.Unlock()
	if m.Meta == nil {
		m.Meta = map[string]any{}
	}
	return m, true
}

func (b *Bus) noteInvalid(id string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	first, ok := b.invalid[id]
	if !ok {
		b.invalid[id] = b.now()
		return
	}
	if b.now().Sub(first) > invalidGraceSecond*time.Second && !b.reported[id] {
		b.reported[id] = true
		b.logf("message %s still invalid after %ds; left for prune", id, invalidGraceSecond)
	}
}

// Cursor is cur/<target>/<consumer>.json (spec section 6).
type Cursor struct {
	V        int      `json:"v"`
	Target   string   `json:"target"`
	Consumer string   `json:"consumer"`
	Hwm      string   `json:"hwm"`
	Acked    []string `json:"acked"`
	Updated  float64  `json:"updated"`
}

func consumerFile(consumer string) string {
	if consumer == "" {
		return defaultFile
	}
	return consumer
}

// ReadCursor returns the cursor of (target, consumer); a missing file is an empty cursor.
func (b *Bus) ReadCursor(target, consumer string) (Cursor, error) {
	c := Cursor{V: 1, Target: target, Consumer: consumerFile(consumer), Acked: []string{}}
	raw, err := os.ReadFile(filepath.Join(b.Dir, "cur", target, consumerFile(consumer)+".json"))
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return c, fmt.Errorf("cursor %s/%s: %w", target, consumerFile(consumer), err)
	}
	if c.Acked == nil {
		c.Acked = []string{}
	}
	return c, nil
}

// Ack acknowledges ids for (target, consumer): monotone and idempotent (spec section 6).
func (b *Bus) Ack(target, consumer string, ids ...string) error {
	if err := validTargetName(target); err != nil {
		return err
	}
	if consumer != "" {
		if err := ValidName(consumer); err != nil {
			return err
		}
	}
	for _, id := range ids {
		if !ValidID(id) {
			return fmt.Errorf("invalid message id %q", id)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	c, err := b.ReadCursor(target, consumer)
	if err != nil {
		return err
	}
	set := map[string]bool{}
	for _, id := range c.Acked {
		set[id] = true
	}
	oldHwm := c.Hwm
	hwm := oldHwm
	for _, id := range ids {
		if id > hwm {
			hwm = id
		}
	}
	if oldHwm != "" && oldHwm < hwm {
		set[oldHwm] = true // the old high-water mark is now below hwm and must stay acked
	}
	for _, id := range ids {
		if id < hwm {
			set[id] = true
		}
	}
	floor := IDMillis(hwm) - b.SkewWindowMS
	out := make([]string, 0, len(set))
	for id := range set {
		if IDMillis(id) > floor {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	c.Hwm, c.Acked, c.Updated = hwm, out, toSeconds(b.now())
	data, err := marshal(c)
	if err != nil {
		return err
	}
	return b.writeAtomic(filepath.Join(b.Dir, "cur", target), consumerFile(consumer)+".json", data)
}

// Unacked returns the target's unacked messages (its inbox merged with ALL), oldest first, at most limit
// (<=0 = all): ids above hwm plus ids inside the skew window below hwm that are not in acked (spec section 6).
func (b *Bus) Unacked(target, consumer string, limit int) ([]Message, error) {
	c, err := b.ReadCursor(target, consumer)
	if err != nil {
		return nil, err
	}
	acked := make(map[string]bool, len(c.Acked))
	for _, id := range c.Acked {
		acked[id] = true
	}
	floor := IDMillis(c.Hwm) - b.SkewWindowMS
	type cand struct{ id, dir string }
	var cands []cand
	for _, dir := range []string{target, Broadcast} {
		for _, id := range b.listIDs(dir) {
			if c.Hwm == "" || id > c.Hwm || (id < c.Hwm && IDMillis(id) > floor && !acked[id]) {
				cands = append(cands, cand{id, dir})
			}
		}
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].id < cands[j].id })
	var out []Message
	for _, cd := range cands {
		if m, ok := b.readMessage(cd.dir, cd.id); ok {
			out = append(out, m)
			if limit > 0 && len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}

// StateUpdate merges set into state/<name>.json (private per-participant state) and removes the del keys.
func (b *Bus) StateUpdate(name string, set map[string]any, del ...string) error {
	st, err := b.ReadState(name)
	if err != nil {
		return err
	}
	for k, v := range set {
		st[k] = v
	}
	for _, k := range del {
		delete(st, k)
	}
	data, err := marshal(st)
	if err != nil {
		return err
	}
	return b.writeAtomic(filepath.Join(b.Dir, "state"), name+".json", data)
}

// ReadState returns state/<name>.json; a missing file is an empty map.
func (b *Bus) ReadState(name string) (map[string]any, error) {
	st := map[string]any{}
	raw, err := os.ReadFile(filepath.Join(b.Dir, "state", name+".json"))
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, fmt.Errorf("state %s: %w", name, err)
	}
	return st, nil
}
