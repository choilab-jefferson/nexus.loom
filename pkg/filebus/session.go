package filebus

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// LiveTTLSeconds is the default session time-to-live (spec section 7).
const LiveTTLSeconds = 300

// NotifyInfo is the notify endpoint advertised in a session file (spec section 10).
type NotifyInfo struct {
	Unix string `json:"unix,omitempty"`
	TCP  string `json:"tcp,omitempty"`
}

// Session is sess/<name>.json (spec section 7).
type Session struct {
	V       int            `json:"v"`
	Name    string         `json:"name"`
	Status  string         `json:"status"`
	PID     int            `json:"pid"`
	Host    string         `json:"host"`
	Kernel  string         `json:"kernel"`
	Started float64        `json:"started"`
	Updated float64        `json:"updated"`
	TTLS    int            `json:"ttl_s"`
	Notify  NotifyInfo     `json:"notify"`
	Agent   map[string]any `json:"agent"`
}

// UnmarshalJSON accepts ttl_s written as an integer or an integral float (300 or 300.0).
func (s *Session) UnmarshalJSON(data []byte) error {
	type plain Session
	aux := struct {
		*plain
		TTLS float64 `json:"ttl_s"`
	}{plain: (*plain)(s)}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	s.TTLS = int(aux.TTLS)
	return nil
}

// Live reports status == "live" and now - max(updated, mtime) <= ttl_s.
func (s Session) Live(mtime, now time.Time) bool {
	if s.Status != "live" {
		return false
	}
	ttl := s.TTLS
	if ttl <= 0 {
		ttl = LiveTTLSeconds
	}
	last := s.Updated
	if m := toSeconds(mtime); m > last {
		last = m
	}
	return toSeconds(now)-last <= float64(ttl)
}

// ReadSession returns sess/<name>.json and its mtime; ok=false when absent or unreadable.
func (b *Bus) ReadSession(name string) (s Session, mtime time.Time, ok bool) {
	path := filepath.Join(b.Dir, "sess", name+".json")
	raw, err := os.ReadFile(path)
	if err != nil {
		return s, mtime, false
	}
	if json.Unmarshal(raw, &s) != nil || s.Name == "" {
		return Session{}, mtime, false
	}
	if fi, err := os.Stat(path); err == nil {
		mtime = fi.ModTime()
	}
	return s, mtime, true
}

// LiveSessions lists the live sessions of target (every live session for ALL).
func (b *Bus) LiveSessions(target string) []Session {
	var names []string
	if target == Broadcast {
		entries, _ := os.ReadDir(filepath.Join(b.Dir, "sess"))
		for _, e := range entries {
			if n := e.Name(); !strings.HasPrefix(n, ".") && strings.HasSuffix(n, ".json") {
				names = append(names, strings.TrimSuffix(n, ".json"))
			}
		}
	} else {
		names = []string{target}
	}
	var out []Session
	for _, n := range names {
		if s, mt, ok := b.ReadSession(n); ok && s.Live(mt, b.now()) {
			out = append(out, s)
		}
	}
	return out
}

// Host returns the host name written into session files.
func (b *Bus) Host() string { return b.hostname }

// KernelID is the Linux boot id, else a random value persisted in state/<name>.json (spec section 7).
func (b *Bus) KernelID(name string) string {
	b.kernelMu.Lock()
	defer b.kernelMu.Unlock()
	if b.kernelVal != "" {
		return b.kernelVal
	}
	if raw, err := os.ReadFile("/proc/sys/kernel/random/boot_id"); err == nil {
		if v := strings.TrimSpace(string(raw)); v != "" {
			b.kernelVal = v
			return v
		}
	}
	if st, err := b.ReadState(name); err == nil {
		if v, _ := st["kernel"].(string); v != "" {
			b.kernelVal = v
			return v
		}
	}
	b.kernelVal = randHex(16)
	_ = b.StateUpdate(name, map[string]any{"kernel": b.kernelVal})
	return b.kernelVal
}

// localKernel is the cached or Linux boot id; empty when unknown (never matches a peer).
func (b *Bus) localKernel() string {
	b.kernelMu.Lock()
	defer b.kernelMu.Unlock()
	if b.kernelVal != "" {
		return b.kernelVal
	}
	if raw, err := os.ReadFile("/proc/sys/kernel/random/boot_id"); err == nil {
		return strings.TrimSpace(string(raw))
	}
	return ""
}

// ErrNameInUse is returned by Register when another live participant holds the session name.
var ErrNameInUse = errors.New("bus name in use by a live session")

// Register writes a live session file for name. It refuses a name held by another live session
// (same host: the pid must be alive; other host: the file must be within its ttl).
func (b *Bus) Register(name string, notify NotifyInfo, agent map[string]any, ttlS int) (Session, error) {
	if err := ValidName(name); err != nil {
		return Session{}, err
	}
	if ttlS <= 0 {
		ttlS = LiveTTLSeconds
	}
	if old, mt, ok := b.ReadSession(name); ok && old.Live(mt, b.now()) && old.PID != os.Getpid() {
		if old.Host != b.hostname || pidAlive(old.PID) {
			return Session{}, fmt.Errorf("%w: %s (pid %d on %s)", ErrNameInUse, name, old.PID, old.Host)
		}
	}
	now := toSeconds(b.now())
	s := Session{V: 1, Name: name, Status: "live", PID: os.Getpid(), Host: b.hostname,
		Kernel: b.KernelID(name), Started: now, Updated: now, TTLS: ttlS, Notify: notify, Agent: agent}
	return s, b.WriteSession(s)
}

// WriteSession rewrites the session file (tmp+rename) with Updated = now.
func (b *Bus) WriteSession(s Session) error {
	s.Updated = toSeconds(b.now())
	data, err := marshal(s)
	if err != nil {
		return err
	}
	return b.writeAtomic(filepath.Join(b.Dir, "sess"), s.Name+".json", data)
}

// Heartbeat refreshes a live session's Updated time.
func (b *Bus) Heartbeat(s Session) error { return b.WriteSession(s) }

// Unregister rewrites the session with status "unregistered"; the file stays (spec section 7).
func (b *Bus) Unregister(s Session) error {
	s.Status = "unregistered"
	return b.WriteSession(s)
}
