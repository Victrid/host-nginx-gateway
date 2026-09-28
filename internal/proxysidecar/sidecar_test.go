// Integration-style unit tests for the cluster-proxy sidecar
// (DESIGN-cluster-proxy.md §0.6–§0.9): real unix sockets, real fake TCP
// upstreams. Covers mapping diff (add/delete/retarget), the L4 byte pump
// (bidirectional data, half-close FIN propagation, error double-close,
// idle read deadline), startup stale-socket cleanup, orphan handling and
// the /healthz semantics.
package proxysidecar

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// waitFor polls cond every 10ms up to 5s.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", what)
}

// freePort picks an unused TCP port (listen :0, close, reuse).
func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	_, port, _ := net.SplitHostPort(l.Addr().String())
	return port
}

// echoUpstream starts a blind TCP echo server; returns its dial address
// and a stop function.
func echoUpstream(t *testing.T) (string, func()) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				_, _ = io.Copy(c, c)
				c.Close()
			}()
		}
	}()
	return l.Addr().String(), func() { l.Close() }
}

// harness runs one Sidecar over a temp dir.
type harness struct {
	sc     *Sidecar
	dir    string
	cancel context.CancelFunc
	done   chan error
	health string
}

func startSidecar(t *testing.T, modify func(o *Options), preRun ...func(*Sidecar)) *harness {
	t.Helper()
	dir := t.TempDir()
	opts := Options{
		SocketsDir:     dir,
		HealthzAddr:    "127.0.0.1:" + freePort(t),
		RescanInterval: 20 * time.Millisecond, // fast for tests; production default is 1s
		IdleTimeout:    DefaultIdleTimeout,
	}
	if modify != nil {
		modify(&opts)
	}
	sc, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range preRun {
		f(sc)
	}
	ctx, cancel := context.WithCancel(context.Background())
	h := &harness{sc: sc, dir: dir, cancel: cancel, done: make(chan error, 1), health: "http://" + opts.HealthzAddr + "/healthz"}
	go func() { h.done <- sc.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-h.done:
		case <-time.After(5 * time.Second):
			t.Error("sidecar did not stop within 5s")
		}
	})
	return h
}

// writeMap atomically replaces proxy.json the way the controller does
// (tmp + rename).
func (h *harness) writeMap(t *testing.T, mappings ...Mapping) {
	t.Helper()
	data, err := json.Marshal(mappings)
	if err != nil {
		t.Fatal(err)
	}
	tmp := filepath.Join(h.dir, "proxy.json.tmp-test")
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, filepath.Join(h.dir, ProxyMapFileName)); err != nil {
		t.Fatal(err)
	}
}

func (h *harness) sockPath(name string) string { return filepath.Join(h.dir, name) }

func (h *harness) waitHealthy(t *testing.T) {
	t.Helper()
	waitFor(t, "sidecar healthy", func() bool { return h.sc.Healthy() })
}

func dialSock(t *testing.T, path string) *net.UnixConn {
	t.Helper()
	c, err := net.DialTimeout("unix", path, 2*time.Second)
	if err != nil {
		t.Fatalf("dial %s: %v", path, err)
	}
	return c.(*net.UnixConn)
}

// ---------------------------------------------------------------------------
// Mapping lifecycle (§0.8)
// ---------------------------------------------------------------------------

func TestSidecar_AddDeleteRetarget(t *testing.T) {
	h := startSidecar(t, nil)
	echo, stop := echoUpstream(t)
	defer stop()

	// ADD: mapping appears → socket bound + healthy.
	h.writeMap(t, Mapping{Listen: "ns_svc_80.sock", Dial: echo})
	h.waitHealthy(t)
	if _, err := os.Stat(h.sockPath("ns_svc_80.sock")); err != nil {
		t.Fatalf("socket not bound: %v", err)
	}

	// DELETE: mapping emptied → listener closed + socket unlinked.
	h.writeMap(t)
	waitFor(t, "socket unlinked", func() bool {
		_, err := os.Stat(h.sockPath("ns_svc_80.sock"))
		return os.IsNotExist(err)
	})
	h.waitHealthy(t)

	// RETARGET: same name, different dial (a closed port) → delete +
	// force-close + rebind under the same name.
	deadPort := freePort(t) // nothing listens here
	h.writeMap(t, Mapping{Listen: "ns_svc_80.sock", Dial: "127.0.0.1:" + deadPort})
	waitFor(t, "socket rebound after retarget", func() bool {
		_, err := os.Stat(h.sockPath("ns_svc_80.sock"))
		return err == nil
	})
	h.waitHealthy(t)
	// The rebound socket dials the DEAD upstream: connects succeed
	// (socket alive) but are closed immediately (dial failure passed
	// through to the client).
	c := dialSock(t, h.sockPath("ns_svc_80.sock"))
	defer c.Close()
	buf := make([]byte, 8)
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := c.Read(buf); err == nil {
		t.Fatalf("expected the retargeted (dead-upstream) socket to close the connection, got data")
	}
}

// TestSidecar_DeleteDrainsEstablishedPumps: deleting a mapping closes
// the LISTENER and unlinks the socket, but established connections keep
// pumping (natural drain, §0.8).
func TestSidecar_DeleteDrainsEstablishedPumps(t *testing.T) {
	h := startSidecar(t, nil)
	echo, stop := echoUpstream(t)
	defer stop()
	h.writeMap(t, Mapping{Listen: "drain.sock", Dial: echo})
	h.waitHealthy(t)

	c := dialSock(t, h.sockPath("drain.sock"))
	defer c.Close()
	if _, err := c.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 5)
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.ReadFull(c, buf); err != nil {
		t.Fatalf("echo before delete: %v", err)
	}

	// Delete the mapping: new connects must fail, the established
	// connection must keep echoing.
	h.writeMap(t)
	waitFor(t, "socket unlinked", func() bool {
		_, err := os.Stat(h.sockPath("drain.sock"))
		return os.IsNotExist(err)
	})
	if _, err := net.Dial("unix", h.sockPath("drain.sock")); err == nil {
		t.Fatalf("new connections must be refused after delete")
	}
	if _, err := c.Write([]byte("again")); err != nil {
		t.Fatalf("established pump must survive the delete: %v", err)
	}
	buf2 := make([]byte, 5)
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.ReadFull(c, buf2); err != nil {
		t.Fatalf("echo after delete (natural drain): %v", err)
	}
}

// TestSidecar_RetargetForceClosesEstablished: same-name-different-dial
// force-closes the pumps of the OLD entry (§0.8).
func TestSidecar_RetargetForceClosesEstablished(t *testing.T) {
	h := startSidecar(t, nil)
	echo, stop := echoUpstream(t)
	defer stop()
	h.writeMap(t, Mapping{Listen: "rt.sock", Dial: echo})
	h.waitHealthy(t)

	c := dialSock(t, h.sockPath("rt.sock"))
	defer c.Close()
	if _, err := c.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}

	deadPort := freePort(t)
	h.writeMap(t, Mapping{Listen: "rt.sock", Dial: "127.0.0.1:" + deadPort})
	// The established connection is force-closed: its reads end promptly.
	// (The pending echo byte may legitimately arrive first — drain until
	// the connection reports EOF/reset; a 3s idle timeout means the
	// force-close never happened.)
	deadline := time.Now().Add(3 * time.Second)
	buf := make([]byte, 4)
	for {
		_ = c.SetReadDeadline(deadline)
		_, err := c.Read(buf)
		if err == nil {
			continue // pending echo byte(s)
		}
		if os.IsTimeout(err) {
			t.Fatalf("established connection was not force-closed on retarget")
		}
		break // EOF / reset: the old pair is torn down
	}
	waitFor(t, "rebound after retarget", func() bool {
		_, err := os.Stat(h.sockPath("rt.sock"))
		return err == nil
	})
	h.waitHealthy(t)
}

// ---------------------------------------------------------------------------
// Byte pump (§0.7)
// ---------------------------------------------------------------------------

// TestSidecar_BidirectionalBytePump: bidirectional echo traffic through
// the socket, including payloads far larger than the 32KiB pump buffer.
func TestSidecar_BidirectionalBytePump(t *testing.T) {
	h := startSidecar(t, nil)
	echo, stop := echoUpstream(t)
	defer stop()
	h.writeMap(t, Mapping{Listen: "pump.sock", Dial: echo})
	h.waitHealthy(t)

	c := dialSock(t, h.sockPath("pump.sock"))
	defer c.Close()

	payload := make([]byte, 300*1024) // ~10x pump buffer: crosses chunk boundaries
	for i := range payload {
		payload[i] = byte(i % 251)
	}
	go func() { _, _ = c.Write(payload) }()
	got := make([]byte, len(payload))
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.ReadFull(c, got); err != nil {
		t.Fatalf("large echo read: %v", err)
	}
	for i := range payload {
		if got[i] != payload[i] {
			t.Fatalf("echo payload corrupted at %d: %d != %d", i, got[i], payload[i])
		}
	}

	// A second round after the big one (the pump keeps flowing).
	if _, err := c.Write([]byte("round2")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 6)
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.ReadFull(c, buf); err != nil {
		t.Fatalf("second round: %v", err)
	}
	if string(buf) != "round2" {
		t.Fatalf("second round payload: %q", buf)
	}
}

// TestSidecar_HalfCloseFinPropagation: a client CloseWrite propagates a
// FIN through the pump to the upstream (which answers after seeing EOF);
// the upstream's close propagates back as the client's EOF — both
// directions honor half-close (§0.7).
func TestSidecar_HalfCloseFinPropagation(t *testing.T) {
	// request-answer upstream: reads until EOF (client FIN must arrive),
	// then answers and half-closes its own side.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	sawEOF := make(chan struct{}, 8)
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				_, _ = io.Copy(io.Discard, c) // until EOF
				sawEOF <- struct{}{}
				_, _ = c.Write([]byte("answer"))
				c.(*net.TCPConn).CloseWrite()
			}()
		}
	}()

	h := startSidecar(t, nil)
	h.writeMap(t, Mapping{Listen: "fin.sock", Dial: l.Addr().String()})
	h.waitHealthy(t)

	c := dialSock(t, h.sockPath("fin.sock"))
	defer c.Close()
	if _, err := c.Write([]byte("request")); err != nil {
		t.Fatal(err)
	}
	if err := c.CloseWrite(); err != nil {
		t.Fatal(err)
	}

	select {
	case <-sawEOF:
	case <-time.After(3 * time.Second):
		t.Fatalf("upstream never saw the client FIN (half-close not propagated)")
	}

	buf := make([]byte, 6)
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.ReadFull(c, buf); err != nil {
		t.Fatalf("answer after half-close: %v", err)
	}
	if string(buf) != "answer" {
		t.Fatalf("answer payload: %q", buf)
	}
	// The upstream's CloseWrite propagates: the client's next read is EOF
	// while its own write side stays untouched (it may report an error on
	// Write only once the pair fully closes — the EOF is the assertion).
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := c.Read(buf); err != io.EOF {
		t.Fatalf("client must see EOF after the upstream half-closes, got %v", err)
	}
}

// TestSidecar_ErrorDoubleClose: an upstream RST (SO_LINGER 0 close)
// tears BOTH directions immediately — the client observes the failure
// promptly instead of hanging (§0.7).
func TestSidecar_ErrorDoubleClose(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			tc := c.(*net.TCPConn)
			_ = tc.SetLinger(0) // Close sends RST, not FIN
			_ = tc.Close()
		}
	}()

	h := startSidecar(t, nil)
	h.writeMap(t, Mapping{Listen: "rst.sock", Dial: l.Addr().String()})
	h.waitHealthy(t)

	c := dialSock(t, h.sockPath("rst.sock"))
	defer c.Close()
	buf := make([]byte, 16)
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := c.Read(buf); err == nil {
		t.Fatalf("expected an error/EOF from the RST path, got data")
	}
}

// TestSidecar_IdleReadDeadline: with a tiny idle deadline an idle pair
// is torn down; a flowing stream never trips it (the deadline resets
// per read, §0.9).
func TestSidecar_IdleReadDeadline(t *testing.T) {
	h := startSidecar(t, func(o *Options) { o.IdleTimeout = 200 * time.Millisecond })
	echo, stop := echoUpstream(t)
	defer stop()
	h.writeMap(t, Mapping{Listen: "idle.sock", Dial: echo})
	h.waitHealthy(t)

	c := dialSock(t, h.sockPath("idle.sock"))
	defer c.Close()

	// Flowing data across several idle-window lengths must NOT trip the
	// deadline (total time > 3x IdleTimeout, gap between writes << it).
	for i := 0; i < 6; i++ {
		if _, err := c.Write([]byte{byte(i)}); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
		buf := make([]byte, 1)
		_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
		if _, err := io.ReadFull(c, buf); err != nil {
			t.Fatalf("read %d: %v (idle deadline must reset on activity)", i, err)
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Now go idle: the pair is closed within ~IdleTimeout.
	buf := make([]byte, 4)
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	start := time.Now()
	if _, err := c.Read(buf); err == nil {
		t.Fatalf("expected idle teardown, got data")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("idle teardown too slow: %v", d)
	}
}

// ---------------------------------------------------------------------------
// Startup hygiene (§0.8) + orphan cleanup
// ---------------------------------------------------------------------------

// TestSidecar_StartupUnlinksStaleSocks: pre-existing *.sock files from a
// crashed sidecar are removed at startup; only the current mapping is
// bound afterwards.
func TestSidecar_StartupUnlinksStaleSocks(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"crash_a.sock", "crash_b.sock", "keep.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("stale"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	opts := Options{SocketsDir: dir, HealthzAddr: "127.0.0.1:" + freePort(t), RescanInterval: 20 * time.Millisecond}
	sc, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- sc.Run(ctx) }()

	waitFor(t, "stale sockets unlinked", func() bool {
		_, a := os.Stat(filepath.Join(dir, "crash_a.sock"))
		_, b := os.Stat(filepath.Join(dir, "crash_b.sock"))
		return os.IsNotExist(a) && os.IsNotExist(b)
	})
	if _, err := os.Stat(filepath.Join(dir, "keep.txt")); err != nil {
		t.Fatalf("non-.sock files must be left alone: %v", err)
	}
	cancel()
	<-done
}

// TestSidecar_OrphanSocketUnboundWhenMappingShrinks: a socket bound for
// a mapping entry that later disappears is unlinked (the delete path);
// unrelated files (proxy.json included) survive.
func TestSidecar_OrphanSocketUnboundWhenMappingShrinks(t *testing.T) {
	h := startSidecar(t, nil)
	echo, stop := echoUpstream(t)
	defer stop()
	h.writeMap(t,
		Mapping{Listen: "gone.sock", Dial: echo},
		Mapping{Listen: "kept.sock", Dial: echo},
	)
	h.waitHealthy(t)

	// Only "kept" remains; "gone" must be unlinked.
	h.writeMap(t, Mapping{Listen: "kept.sock", Dial: echo})
	waitFor(t, "gone.sock unlinked", func() bool {
		_, err := os.Stat(h.sockPath("gone.sock"))
		return os.IsNotExist(err)
	})
	if _, err := os.Stat(h.sockPath("kept.sock")); err != nil {
		t.Fatalf("kept.sock must stay bound: %v", err)
	}
	if _, err := os.Stat(filepath.Join(h.dir, ProxyMapFileName)); err != nil {
		t.Fatalf("proxy.json must never be removed by the sidecar: %v", err)
	}
	h.waitHealthy(t)
}

// ---------------------------------------------------------------------------
// Watch strategy (§0.6) + health (§0.9)
// ---------------------------------------------------------------------------

// TestSidecar_PeriodicRescanFallback: with fsnotify DISABLED, the 1s
// (test: 20ms) periodic rescan still applies mapping changes — the
// fallback path of the watch strategy.
func TestSidecar_PeriodicRescanFallback(t *testing.T) {
	h := startSidecar(t,
		func(o *Options) { o.RescanInterval = 20 * time.Millisecond },
		func(sc *Sidecar) { sc.noFSNotify = true }) // set before Run wires the watcher
	echo, stop := echoUpstream(t)
	defer stop()

	h.writeMap(t, Mapping{Listen: "tick.sock", Dial: echo})
	waitFor(t, "socket bound via periodic rescan", func() bool {
		_, err := os.Stat(h.sockPath("tick.sock"))
		return err == nil
	})
	h.waitHealthy(t)
}

// TestSidecar_HealthSemantics: healthy == mappingLoadedOnce &&
// len(wanted) == len(activeListeners); readiness never requires a dial.
func TestSidecar_HealthSemantics(t *testing.T) {
	h := startSidecar(t, nil)

	// Before any proxy.json: not healthy (mapping never loaded).
	waitFor(t, "sidecar running", func() bool {
		resp, err := http.Get(h.health)
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		return true // server up
	})
	if h.sc.Healthy() {
		t.Fatalf("must be unhealthy before the first mapping load")
	}
	resp, err := http.Get(h.health)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("healthz before load: %d, want 503", resp.StatusCode)
	}

	// Loaded with an unreachable dial target: STILL healthy — backend
	// reachability is not the sidecar's readiness condition (§0.9).
	deadPort := freePort(t)
	h.writeMap(t, Mapping{Listen: "dead.sock", Dial: "127.0.0.1:" + deadPort})
	h.waitHealthy(t)
	resp, err = http.Get(h.health)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz with loaded mapping: %d, want 200 (no dial requirement)", resp.StatusCode)
	}
}

// TestSidecar_BindFailureKeepsUnhealthy: a mapping entry whose socket
// cannot be bound (path blocked by a non-empty directory) leaves
// wanted != active → unhealthy, while the OTHER entry keeps serving.
func TestSidecar_BindFailureKeepsUnhealthy(t *testing.T) {
	h := startSidecar(t, nil)
	echo, stop := echoUpstream(t)
	defer stop()

	blocked := h.sockPath("blocked.sock")
	if err := os.Mkdir(blocked, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(blocked, "inner"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(blocked) })

	h.writeMap(t,
		Mapping{Listen: "blocked.sock", Dial: echo},
		Mapping{Listen: "fine.sock", Dial: echo},
	)
	// fine.sock is bound and serving…
	waitFor(t, "fine.sock bound", func() bool {
		_, err := os.Stat(h.sockPath("fine.sock"))
		return err == nil
	})
	c := dialSock(t, h.sockPath("fine.sock"))
	if _, err := c.Write([]byte("k")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 1)
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.ReadFull(c, buf); err != nil {
		t.Fatalf("healthy entry must serve: %v", err)
	}
	c.Close()
	// …but the sidecar as a whole is unhealthy (1 of 2 bound).
	time.Sleep(100 * time.Millisecond) // let the bind attempt settle
	if h.sc.Healthy() {
		t.Fatalf("bind failure must keep the sidecar unhealthy")
	}
}

// TestSidecar_ProxyJsonParseErrorKeepsOldMapping: a corrupt proxy.json
// keeps the previous mapping serving (log + retry on the next tick).
func TestSidecar_ProxyJsonParseErrorKeepsOldMapping(t *testing.T) {
	h := startSidecar(t, nil)
	echo, stop := echoUpstream(t)
	defer stop()
	h.writeMap(t, Mapping{Listen: "parse.sock", Dial: echo})
	h.waitHealthy(t)

	if err := os.WriteFile(filepath.Join(h.dir, ProxyMapFileName), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond) // several rescan ticks

	// Old mapping still bound and healthy (loadedOnce stays, wanted unchanged).
	if !h.sc.Healthy() {
		t.Fatalf("corrupt proxy.json must keep the previous healthy mapping")
	}
	c := dialSock(t, h.sockPath("parse.sock"))
	defer c.Close()
	if _, err := c.Write([]byte("z")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 1)
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.ReadFull(c, buf); err != nil {
		t.Fatalf("previous mapping must keep serving: %v", err)
	}
}

// TestSidecar_ConcurrentConnections: several simultaneous client
// connections multiplex over one entry's accept loop.
func TestSidecar_ConcurrentConnections(t *testing.T) {
	h := startSidecar(t, nil)
	echo, stop := echoUpstream(t)
	defer stop()
	h.writeMap(t, Mapping{Listen: "multi.sock", Dial: echo})
	h.waitHealthy(t)

	const n = 8
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		go func(i int) {
			c, err := net.DialTimeout("unix", h.sockPath("multi.sock"), 2*time.Second)
			if err != nil {
				errs <- fmt.Errorf("client %d dial: %w", i, err)
				return
			}
			defer c.Close()
			msg := []byte(fmt.Sprintf("client-%d", i))
			if _, err := c.Write(msg); err != nil {
				errs <- fmt.Errorf("client %d write: %w", i, err)
				return
			}
			buf := make([]byte, len(msg))
			_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
			if _, err := io.ReadFull(c, buf); err != nil {
				errs <- fmt.Errorf("client %d read: %w", i, err)
				return
			}
			if string(buf) != string(msg) {
				errs <- fmt.Errorf("client %d echo mismatch: %q", i, buf)
				return
			}
			errs <- nil
		}(i)
	}
	for i := 0; i < n; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
}
