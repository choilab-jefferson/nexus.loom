package filebus

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestMain isolates the suite from host env and doubles as a writer helper for the multi-process test.
func TestMain(m *testing.M) {
	if dir := os.Getenv("FILEBUS_TEST_WRITER"); dir != "" {
		n, _ := strconv.Atoi(os.Getenv("FILEBUS_TEST_COUNT"))
		b, err := Open(dir)
		if err != nil {
			os.Exit(2)
		}
		for i := 0; i < n; i++ {
			if _, err := b.Send(Outgoing{Sender: "proc", Target: "shared", Kind: "dm", Body: "x"}); err != nil {
				os.Exit(3)
			}
		}
		os.Exit(0)
	}
	tmp, err := os.MkdirTemp("", "filebus-hermetic-")
	if err != nil {
		panic(err)
	}
	os.Setenv("XDG_RUNTIME_DIR", tmp)
	os.Setenv("TMPDIR", tmp)
	code := m.Run()
	os.RemoveAll(tmp)
	os.Exit(code)
}

func open(t *testing.T) *Bus {
	t.Helper()
	b, err := Open(filepath.Join(t.TempDir(), "bus"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func send(t *testing.T, b *Bus, target, body string) Message {
	t.Helper()
	m, err := b.Send(Outgoing{Sender: "tester", Target: target, Kind: "dm", Body: body})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestIDGrammarAndNames(t *testing.T) {
	b := open(t)
	m := send(t, b, "loom-a1", "hello")
	if !ValidID(m.ID) || !strings.HasPrefix(m.ID, "m") || len(m.ID) != 1+13+1+8+1+6 {
		t.Fatalf("bad id %q", m.ID)
	}
	if IDMillis(m.ID) < time.Now().UnixMilli()-5000 {
		t.Fatalf("ms %d too old", IDMillis(m.ID))
	}
	for _, id := range []string{"m1-aaaaaaaa-000001", "M1763245081123-3fa9c2d1-000042", "m1763245081123-3FA9C2D1-000042", "m1763245081123-3fa9c2d1-42"} {
		if ValidID(id) {
			t.Errorf("%q accepted", id)
		}
	}
	for _, n := range []string{"", ".", "..", "a/b", `a\b`, ".hidden", "_x", "a\nb", strings.Repeat("x", 129)} {
		if ValidName(n) == nil {
			t.Errorf("name %q accepted", n)
		}
	}
	if ValidName("loom-a1") != nil || ValidName("ALL") != nil {
		t.Error("good name rejected")
	}
	if _, err := b.Send(Outgoing{Sender: "s", Target: "../x", Kind: "dm"}); err == nil {
		t.Error("path traversal target accepted")
	}
}

// The layout and message schema follow the spec example byte for byte (keys, nulls, dir modes).
func TestMessageFileSchemaAndModes(t *testing.T) {
	b := open(t)
	m, err := b.Send(Outgoing{Sender: "planner", Target: "loom-a1", Kind: "loom.task", Body: "first line\nsecond",
		Data: map[string]any{"prompt": "p"}, Meta: map[string]any{"corr": "c1"}})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(b.Dir, "msg", "loom-a1", m.ID+".json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"v", "id", "sender", "target", "kind", "summary", "timestamp", "body", "body_ref", "data", "meta"} {
		if _, ok := generic[k]; !ok {
			t.Errorf("missing key %s: %s", k, raw)
		}
	}
	meta := generic["meta"].(map[string]any)
	for _, k := range []string{"reply_to", "corr", "task_id", "sender_iid", "sender_verified", "redis_id", "origin"} {
		if _, ok := meta[k]; !ok {
			t.Errorf("missing meta key %s", k)
		}
	}
	if generic["v"] != float64(1) || generic["id"] != m.ID || generic["summary"] != "first line" || generic["body_ref"] != nil || meta["corr"] != "c1" {
		t.Fatalf("content: %s", raw)
	}
	for p, want := range map[string]os.FileMode{path: 0o600, filepath.Join(b.Dir, "msg", "loom-a1"): 0o700, filepath.Join(b.Dir, "VERSION"): 0o600, b.Dir: 0o700} {
		if fi, err := os.Stat(p); err != nil || fi.Mode().Perm() != want {
			t.Errorf("%s mode %v err %v, want %v", p, fi.Mode().Perm(), err, want)
		}
	}
	var v map[string]any
	vraw, _ := os.ReadFile(filepath.Join(b.Dir, "VERSION"))
	if json.Unmarshal(vraw, &v) != nil || v["format"] != "nexus.filebus" || v["version"] != float64(1) || v["skew_window_ms"] != float64(300000) {
		t.Fatalf("VERSION %s", vraw)
	}
	// A Python-written message (spec example) parses.
	example := `{"v": 1, "id": "m1763245081123-3fa9c2d1-000042", "sender": "planner", "target": "loom-a1", "kind": "dm",
	 "summary": "s", "timestamp": 1763245081.123, "body": "b", "body_ref": null, "data": null,
	 "meta": {"reply_to": null, "corr": null, "task_id": null, "sender_iid": null, "sender_verified": null, "redis_id": null, "origin": "lite"}}`
	os.WriteFile(filepath.Join(b.Dir, "msg", "loom-a1", "m1763245081123-3fa9c2d1-000042.json"), []byte(example), 0o600)
	msgs, err := b.Unacked("loom-a1", "", 0)
	if err != nil || len(msgs) != 2 || msgs[0].ID != "m1763245081123-3fa9c2d1-000042" || msgs[0].MetaString("origin") != "lite" {
		t.Fatalf("%v %+v", err, msgs)
	}
}

func TestVersionRefusal(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bus")
	os.MkdirAll(dir, 0o700)
	os.WriteFile(filepath.Join(dir, "VERSION"), []byte(`{"format":"nexus.filebus","version":2,"minor":0}`), 0o600)
	if _, err := Open(dir); err == nil || !strings.Contains(err.Error(), "bus layout version 2 unsupported") {
		t.Fatalf("err %v", err)
	}
	os.WriteFile(filepath.Join(dir, "VERSION"), []byte(`{"format":"nexus.filebus","version":1,"minor":7,"skew_window_ms":1000,"extra":true}`), 0o600)
	b, err := Open(dir)
	if err != nil || b.SkewWindowMS != 1000 {
		t.Fatalf("minor bump / unknown keys must open: %v", err)
	}
}

func TestReadValidityPartialThenComplete(t *testing.T) {
	b := open(t)
	dir := filepath.Join(b.Dir, "msg", "t")
	os.MkdirAll(dir, 0o700)
	id := "m1763245081123-3fa9c2d1-000001"
	final := filepath.Join(dir, id+".json")
	good := `{"v":1,"id":"` + id + `","sender":"a","target":"t","kind":"dm","summary":"","timestamp":1,"body":"b","body_ref":null,"data":null,"meta":{}}`
	os.WriteFile(final, []byte(good[:30]), 0o600) // partial (A1 broken)
	os.WriteFile(filepath.Join(dir, ".tmp.deadbeef.1234"), []byte("{}"), 0o600)
	os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("x"), 0o600)
	if msgs, _ := b.Unacked("t", "", 0); len(msgs) != 0 {
		t.Fatalf("partial file delivered: %+v", msgs)
	}
	// Wrong stem / wrong target are invalid too.
	os.WriteFile(final, []byte(strings.Replace(good, `"target":"t"`, `"target":"other"`, 1)), 0o600)
	if msgs, _ := b.Unacked("t", "", 0); len(msgs) != 0 {
		t.Fatalf("wrong-target file delivered")
	}
	os.WriteFile(final, []byte(good), 0o600)
	msgs, _ := b.Unacked("t", "", 0)
	if len(msgs) != 1 || msgs[0].Body != "b" {
		t.Fatalf("complete file not delivered: %+v", msgs)
	}
}

func TestAckIdempotentMonotoneAndOrder(t *testing.T) {
	b := open(t)
	var ids []string
	for i := 0; i < 5; i++ {
		ids = append(ids, send(t, b, "t", strconv.Itoa(i)).ID)
	}
	if !sort.StringsAreSorted(ids) {
		t.Fatalf("ids not in send order: %v", ids)
	}
	msgs, _ := b.Unacked("t", "", 0)
	if len(msgs) != 5 || msgs[0].ID != ids[0] || msgs[4].ID != ids[4] {
		t.Fatalf("unacked %+v", msgs)
	}
	if err := b.Ack("t", "", ids[0]); err != nil {
		t.Fatal(err)
	}
	if err := b.Ack("t", "", ids[0]); err != nil { // idempotent
		t.Fatal(err)
	}
	if err := b.Ack("t", "", ids[2]); err != nil { // out of order: ids[1] stays unacked
		t.Fatal(err)
	}
	msgs, _ = b.Unacked("t", "", 0)
	if got := idsOf(msgs); strings.Join(got, ",") != strings.Join([]string{ids[1], ids[3], ids[4]}, ",") {
		t.Fatalf("after out-of-order ack: %v", got)
	}
	c, _ := b.ReadCursor("t", "")
	if c.Hwm != ids[2] {
		t.Fatalf("hwm %s", c.Hwm)
	}
	b.Ack("t", "", ids[1]) // lower than hwm never moves it back
	c, _ = b.ReadCursor("t", "")
	if c.Hwm != ids[2] || c.Consumer != "_default" || c.Target != "t" {
		t.Fatalf("cursor %+v", c)
	}
	b.Ack("t", "", ids[4], ids[3])
	if msgs, _ = b.Unacked("t", "", 0); len(msgs) != 0 {
		t.Fatalf("still unacked: %v", idsOf(msgs))
	}
	if _, err := os.Stat(filepath.Join(b.Dir, "cur", "t", "_default.json")); err != nil {
		t.Fatal(err)
	}
	// Per-consumer cursors are independent; acking a non-existent id is allowed.
	if msgs, _ = b.Unacked("t", "watcher", 0); len(msgs) != 5 {
		t.Fatalf("watcher cursor must be independent: %d", len(msgs))
	}
	if err := b.Ack("t", "watcher", "m1763245081123-3fa9c2d1-000042"); err != nil {
		t.Fatal(err)
	}
	if b.Ack("t", "", "bogus") == nil {
		t.Fatal("invalid id accepted")
	}
}

func idsOf(ms []Message) []string {
	var out []string
	for _, m := range ms {
		out = append(out, m.ID)
	}
	return out
}

// A message that becomes visible below hwm but inside the skew window is delivered once; below the window never.
func TestSkewWindowLateArrival(t *testing.T) {
	b := open(t)
	write := func(id string) {
		m := Message{V: 1, ID: id, Sender: "x", Target: "t", Kind: "dm", Body: id, Data: json.RawMessage("null"), Meta: map[string]any{}}
		raw, _ := json.Marshal(m)
		if err := b.writeAtomic(filepath.Join(b.Dir, "msg", "t"), id+".json", raw); err != nil {
			t.Fatal(err)
		}
	}
	hi := "m1763245081123-aaaaaaaa-000002"
	late := "m1763245080999-bbbbbbbb-000001"    // 124 ms below hi: inside W
	ancient := "m1763244000000-cccccccc-000001" // far below hwm - W
	write(hi)
	b.Ack("t", "", hi)
	write(late)
	write(ancient)
	msgs, _ := b.Unacked("t", "", 0)
	if got := idsOf(msgs); len(got) != 1 || got[0] != late {
		t.Fatalf("want only the late message, got %v", got)
	}
	b.Ack("t", "", late)
	if msgs, _ = b.Unacked("t", "", 0); len(msgs) != 0 {
		t.Fatalf("late message redelivered: %v", idsOf(msgs))
	}
	c, _ := b.ReadCursor("t", "")
	if c.Hwm != hi || len(c.Acked) != 1 || c.Acked[0] != late {
		t.Fatalf("cursor %+v", c)
	}
}

func TestBroadcastMergeAndWriterClamp(t *testing.T) {
	b := open(t)
	a := send(t, b, "t", "direct")
	all := send(t, b, Broadcast, "everyone")
	msgs, _ := b.Unacked("t", "", 0)
	if got := idsOf(msgs); len(got) != 2 || got[0] != a.ID || got[1] != all.ID || msgs[1].Target != "ALL" {
		t.Fatalf("merge: %v", got)
	}
	// A slow-clocked writer lands above a foreign message already visible (spec 4.2).
	future := time.Now().UnixMilli() + 3_600_000
	fid := "m" + strconv.FormatInt(future, 10) + "-aaaaaaaa-000001"
	raw, _ := json.Marshal(Message{V: 1, ID: fid, Sender: "x", Target: "clamp", Kind: "dm", Data: json.RawMessage("null")})
	b.writeAtomic(filepath.Join(b.Dir, "msg", "clamp"), fid+".json", raw)
	m := send(t, b, "clamp", "after")
	if m.ID <= fid {
		t.Fatalf("clamp failed: %s <= %s", m.ID, fid)
	}
}

func TestBodyRefForLargeBody(t *testing.T) {
	b := open(t)
	big := strings.Repeat("é", MaxInlineBody) // 2 bytes each, over the inline limit
	m, err := b.Send(Outgoing{Sender: "s", Target: "t", Kind: "dm", Body: big})
	if err != nil || m.BodyRef == nil || m.Body != "" {
		t.Fatalf("%v %+v", err, m.BodyRef)
	}
	if !strings.HasPrefix(*m.BodyRef, "blob/t/") {
		t.Fatalf("body_ref %q", *m.BodyRef)
	}
	raw, _ := os.ReadFile(filepath.Join(b.Dir, "msg", "t", m.ID+".json"))
	var generic map[string]any
	json.Unmarshal(raw, &generic)
	if v, present := generic["body"]; !present || v != nil {
		t.Fatalf("body must be null with body_ref: %s", raw[:200])
	}
	msgs, _ := b.Unacked("t", "", 0)
	got, err := b.Body(msgs[0])
	if err != nil || got != big {
		t.Fatalf("body round trip: %v", err)
	}
	if _, err := b.Body(Message{BodyRef: ptr("../../etc/passwd")}); err == nil {
		t.Fatal("body_ref escape accepted")
	}
}

func ptr(s string) *string { return &s }

// Concurrent writers (goroutines and separate processes) lose and duplicate nothing.
func TestConcurrentWriters(t *testing.T) {
	b := open(t)
	const goroutines, perG, procs, perProc = 8, 150, 2, 150
	var wg sync.WaitGroup
	// Several Bus handles (distinct writer ids) plus one shared handle.
	handles := make([]*Bus, goroutines)
	for i := range handles {
		if i%2 == 0 {
			handles[i] = b
		} else {
			h, err := Open(b.Dir)
			if err != nil {
				t.Fatal(err)
			}
			handles[i] = h
		}
	}
	var cmds []*exec.Cmd
	for p := 0; p < procs; p++ {
		cmd := exec.Command(os.Args[0])
		cmd.Env = append(os.Environ(), "FILEBUS_TEST_WRITER="+b.Dir, "FILEBUS_TEST_COUNT="+strconv.Itoa(perProc))
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		cmds = append(cmds, cmd)
	}
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(h *Bus) {
			defer wg.Done()
			for i := 0; i < perG; i++ {
				if _, err := h.Send(Outgoing{Sender: "g", Target: "shared", Kind: "dm", Body: "x"}); err != nil {
					t.Error(err)
					return
				}
			}
		}(handles[g])
	}
	wg.Wait()
	for _, c := range cmds {
		if err := c.Wait(); err != nil {
			t.Fatalf("writer process: %v", err)
		}
	}
	want := goroutines*perG + procs*perProc
	msgs, err := b.Unacked("shared", "", 0)
	if err != nil || len(msgs) != want {
		t.Fatalf("got %d messages, want %d (%v)", len(msgs), want, err)
	}
	seen := map[string]bool{}
	for _, m := range msgs {
		if seen[m.ID] || !ValidID(m.ID) {
			t.Fatalf("duplicate or invalid id %s", m.ID)
		}
		seen[m.ID] = true
	}
	entries, _ := os.ReadDir(filepath.Join(b.Dir, "msg", "shared"))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".tmp.") {
			t.Fatalf("leftover temp file %s", e.Name())
		}
	}
}

func TestSessionLifecycle(t *testing.T) {
	b := open(t)
	s, err := b.Register("loom-a1", NotifyInfo{}, map[string]any{"kind": "loom", "loop_mode": 1}, 0)
	if err != nil {
		t.Fatal(err)
	}
	got, mt, ok := b.ReadSession("loom-a1")
	if !ok || got.Status != "live" || got.PID != os.Getpid() || got.TTLS != 300 || got.Agent["kind"] != "loom" || got.Kernel == "" || got.Host == "" {
		t.Fatalf("%+v", got)
	}
	if !got.Live(mt, time.Now()) {
		t.Fatal("fresh session not live")
	}
	// ttl uses max(updated, mtime): an old content time with a fresh mtime is live, both old is not.
	old := got
	old.Updated -= 1000
	if !old.Live(time.Now(), time.Now()) || old.Live(time.Now().Add(-time.Hour), time.Now()) {
		t.Fatal("liveness rule")
	}
	time.Sleep(5 * time.Millisecond)
	if err := b.Heartbeat(s); err != nil {
		t.Fatal(err)
	}
	if got2, _, _ := b.ReadSession("loom-a1"); got2.Updated <= got.Updated || got2.Started != got.Started {
		t.Fatalf("heartbeat: %+v", got2)
	}
	other, _ := Open(b.Dir)
	other.hostname = b.hostname
	if _, err := other.Register("loom-a1", NotifyInfo{}, nil, 0); err != nil {
		t.Fatalf("same pid may re-register: %v", err) // same process in this test
	}
	if err := b.Unregister(s); err != nil {
		t.Fatal(err)
	}
	if got3, mt3, _ := b.ReadSession("loom-a1"); got3.Status != "unregistered" || got3.Live(mt3, time.Now()) {
		t.Fatalf("%+v", got3)
	}
	if _, err := b.Register("bad/name", NotifyInfo{}, nil, 0); err == nil {
		t.Fatal("bad name accepted")
	}
	// A live session of another host blocks the name.
	foreign := Session{V: 1, Name: "taken", Status: "live", PID: 1, Host: "elsewhere", Updated: toSeconds(time.Now()), TTLS: 300}
	data, _ := marshal(foreign)
	b.writeAtomic(filepath.Join(b.Dir, "sess"), "taken.json", data)
	if _, err := b.Register("taken", NotifyInfo{}, nil, 0); err == nil {
		t.Fatal("live foreign session was taken over")
	}
}

func TestSessionTTLTolerantAndInteger(t *testing.T) {
	b := open(t)
	for _, ttl := range []string{"300", "300.0", "120.0"} {
		raw := `{"v":1,"name":"py","status":"live","pid":1,"host":"h","kernel":"k","started":1.0,"updated":2.5,"ttl_s":` + ttl + `,"notify":{},"agent":{}}`
		os.WriteFile(filepath.Join(b.Dir, "sess", "py.json"), []byte(raw), 0o600)
		s, _, ok := b.ReadSession("py")
		want, _ := strconv.ParseFloat(ttl, 64)
		if !ok || s.TTLS != int(want) {
			t.Fatalf("ttl %s -> %+v ok=%v", ttl, s, ok)
		}
	}
	reg, err := b.Register("loomx", NotifyInfo{}, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(b.Dir, "sess", reg.Name+".json"))
	if !strings.Contains(string(raw), `"ttl_s":300,`) {
		t.Fatalf("ttl_s not written as integer: %s", raw)
	}
}

// Spec note 6: an unregistered waiter binding any *.sock in the per-bus run dir is woken; dead sockets are unlinked.
func TestRunDirWaiterWithoutSession(t *testing.T) {
	base, err := os.MkdirTemp("/tmp", "fb") // short: AF_UNIX path limit (spec: base > 60 bytes falls back)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(base) })
	t.Setenv("NEXUS_BUS_RUN_DIR", base)
	real, _ := filepath.EvalSymlinks(t.TempDir())
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	b, err := Open(link) // symlinked bus path: the key uses the resolved path
	if err != nil {
		t.Fatal(err)
	}
	sum := sha1.Sum([]byte(real))
	want := filepath.Join(base, hex.EncodeToString(sum[:4]))
	if got := b.RunDir(); got != want {
		t.Fatalf("RunDir %s, want %s", got, want)
	}
	t.Setenv("NEXUS_BUS_RUN_DIR", strings.Repeat("x", 70)) // too long: /tmp fallback
	if got := b.RunDir(); !strings.HasPrefix(got, "/tmp/nexus-bus-") {
		t.Fatalf("long base not replaced: %s", got)
	}
	t.Setenv("NEXUS_BUS_RUN_DIR", base)

	os.MkdirAll(want, 0o700)
	pc, err := net.ListenPacket("unixgram", filepath.Join(want, "deadbeef.123.abcd.sock"))
	if err != nil {
		t.Skip("unix datagram unavailable")
	}
	defer pc.Close()
	stale := filepath.Join(want, "stale.sock")
	if l, err := net.ListenPacket("unixgram", stale); err == nil {
		l.Close() // leaves a socket file with no listener
	}
	id := send(t, b, "any-target", "x").ID // target has no session at all
	buf := make([]byte, 128)
	pc.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, _, err := pc.ReadFrom(buf)
	if err != nil || string(buf[:n]) != id {
		t.Fatalf("waiter not woken with the id: %v %q", err, buf[:n])
	}
	if _, err := os.Stat(stale); err == nil {
		t.Fatal("refused socket not unlinked")
	}
}

func TestNotifyWakesReceiver(t *testing.T) {
	b := open(t)
	n, err := b.StartNotify("loom-a1", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()
	if n.Info.Unix == "" {
		t.Skip("unix datagram sockets unavailable")
	}
	if _, err := b.Register("loom-a1", n.Info, nil, 0); err != nil {
		t.Fatal(err)
	}
	send(t, b, "loom-a1", "wake")
	select {
	case <-n.Wake:
	case <-time.After(2 * time.Second):
		t.Fatal("no wake-up")
	}
	// TCP layer: loopback only (D4).
	if _, err := b.StartNotify("x", "0.0.0.0:0"); err == nil {
		t.Fatal("non-loopback tcp notify accepted")
	}
	c := n.Info
	if c.TCP == "" {
		t.Fatal("tcp endpoint not advertised")
	}
	b2 := open(t)
	b2.notify("none", "m1763245081123-3fa9c2d1-000001") // no session: must not panic
}
