package filebus

import (
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Wake-up layer (spec section 10). The payload is only a hint: the message id or nothing; receivers always rescan.

const tcpNotifyTimeout = 200 * time.Millisecond

// RunDir is the local per-bus directory where every waiter binds its datagram socket (spec Implementation
// note 6, identical in every implementation): base = $NEXUS_BUS_RUN_DIR, else $XDG_RUNTIME_DIR/nexus-bus, else
// ($TMPDIR or /tmp)/nexus-bus-<uid>; /tmp/nexus-bus-<uid> when base exceeds 60 bytes; then <base>/<key> with key =
// first 8 hex of sha1 over the bus dir's symlink-resolved absolute path.
func (b *Bus) RunDir() string {
	uid := os.Getuid()
	var base string
	switch {
	case os.Getenv("NEXUS_BUS_RUN_DIR") != "":
		base = os.Getenv("NEXUS_BUS_RUN_DIR")
	case os.Getenv("XDG_RUNTIME_DIR") != "":
		base = filepath.Join(os.Getenv("XDG_RUNTIME_DIR"), "nexus-bus")
	default:
		tmp := os.Getenv("TMPDIR")
		if tmp == "" {
			tmp = "/tmp"
		}
		base = filepath.Join(tmp, fmt.Sprintf("nexus-bus-%d", uid))
	}
	if len(base) > 60 {
		base = fmt.Sprintf("/tmp/nexus-bus-%d", uid)
	}
	real := b.Dir
	if r, err := filepath.EvalSymlinks(b.Dir); err == nil {
		real = r
	}
	sum := sha1.Sum([]byte(strings.TrimRight(real, "/")))
	return filepath.Join(base, hex.EncodeToString(sum[:4]))
}

// notify wakes waiters after a message was renamed into place: one datagram (the id, a hint only) to every
// *.sock in the bus run dir whatever the target, then any session notify address not already covered
// (unix on the same kernel, tcp across a boundary). All errors are ignored; a refused socket is unlinked.
func (b *Bus) notify(target, id string) {
	dir := b.RunDir()
	covered := map[string]bool{}
	if entries, err := os.ReadDir(dir); err == nil {
		for _, e := range entries {
			if !strings.HasSuffix(e.Name(), ".sock") {
				continue
			}
			path := filepath.Join(dir, e.Name())
			covered[path] = true
			if path == b.selfSock {
				continue
			}
			b.sendDatagram(path, id, true)
		}
	}
	for _, s := range b.LiveSessions(target) {
		if k := b.localKernel(); s.Notify.Unix != "" && !covered[s.Notify.Unix] && s.Host == b.hostname && k != "" && s.Kernel == k {
			if b.sendDatagram(s.Notify.Unix, id, false) {
				continue
			}
		}
		if s.Notify.TCP != "" {
			if c, err := net.DialTimeout("tcp", s.Notify.TCP, tcpNotifyTimeout); err == nil {
				_ = c.SetWriteDeadline(time.Now().Add(tcpNotifyTimeout))
				_, _ = c.Write([]byte(id + "\n"))
				c.Close()
			}
		}
	}
}

// sendDatagram sends id to a Unix datagram socket; it reports success. With unlinkStale, a refused or missing
// socket (dead waiter) is removed.
func (b *Bus) sendDatagram(path, id string, unlinkStale bool) bool {
	c, err := net.DialTimeout("unixgram", path, tcpNotifyTimeout)
	if err != nil {
		if unlinkStale && (errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, os.ErrNotExist)) {
			os.Remove(path)
		}
		return false
	}
	defer c.Close()
	_ = c.SetWriteDeadline(time.Now().Add(tcpNotifyTimeout))
	_, err = c.Write([]byte(id))
	return err == nil
}

// Notifier receives wake-up hints for one session. Wake delivers at most one pending signal.
type Notifier struct {
	Wake chan struct{}
	Info NotifyInfo

	unix *net.UnixConn
	path string
	tcp  net.Listener
}

func (n *Notifier) signal() {
	select {
	case n.Wake <- struct{}{}:
	default:
	}
}

// StartNotify binds the session's Unix datagram socket (when the platform allows it) and, when tcpAddr is
// set, a loopback TCP listener (D4: loopback only). Failure to bind is not an error: the poll fallback covers it.
func (b *Bus) StartNotify(name, tcpAddr string) (*Notifier, error) {
	n := &Notifier{Wake: make(chan struct{}, 1)}
	n.bindUnix(b, name)
	if tcpAddr != "" {
		host, _, err := net.SplitHostPort(tcpAddr)
		if err != nil {
			return nil, fmt.Errorf("notify tcp address %q: %v", tcpAddr, err)
		}
		if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
			return nil, fmt.Errorf("notify tcp address %q: loopback only", tcpAddr)
		}
		ln, err := net.Listen("tcp", tcpAddr)
		if err != nil {
			return nil, err
		}
		n.tcp = ln
		n.Info.TCP = ln.Addr().String()
		go n.acceptLoop()
	}
	return n, nil
}

func (n *Notifier) bindUnix(b *Bus, name string) {
	dir := b.RunDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	nameSum := sha1.Sum([]byte(name))
	path := filepath.Join(dir, fmt.Sprintf("%s.%d.%s.sock", hex.EncodeToString(nameSum[:4]), os.Getpid(), randHex(2)))
	pc, err := net.ListenPacket("unixgram", path)
	if err != nil {
		return
	}
	uc, ok := pc.(*net.UnixConn)
	if !ok {
		pc.Close()
		return
	}
	_ = os.Chmod(path, 0o600)
	n.unix, n.path, n.Info.Unix = uc, path, path
	b.selfSock = path
	go func() {
		buf := make([]byte, 128)
		for {
			if _, _, err := uc.ReadFrom(buf); err != nil {
				return
			}
			n.signal()
		}
	}()
}

func (n *Notifier) acceptLoop() {
	for {
		c, err := n.tcp.Accept()
		if err != nil {
			return
		}
		go func() {
			defer c.Close()
			_ = c.SetReadDeadline(time.Now().Add(time.Second))
			buf := make([]byte, 80)
			_, _ = c.Read(buf)
			n.signal()
		}()
	}
}

// Close stops the listeners and removes the socket file.
func (n *Notifier) Close() {
	if n == nil {
		return
	}
	if n.unix != nil {
		n.unix.Close()
		os.Remove(n.path)
	}
	if n.tcp != nil {
		n.tcp.Close()
	}
}
