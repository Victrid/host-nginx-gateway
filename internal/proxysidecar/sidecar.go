// Package proxysidecar implements the cluster-proxy sidecar runtime
// (DESIGN-cluster-proxy.md): a zero-Kubernetes-dependency executor of the
// proxy.json mapping file. For every mapping entry it binds
// <sockets-dir>/<listen> (a unix socket the host nginx connects to) and
// pumps bytes bidirectionally to <dial> (a ClusterIP:port reachable from
// the POD network namespace). The sidecar holds no ServiceAccount token
// and never talks to the API server — the mapping file is the entire
// control-plane contract.
//
// Lifecycle (design §0.8, binding):
//   - fsnotify watches the DIRECTORY (Create|Write|Rename — watching the
//     file inode gets blown up by the controller's tmp+rename atomic
//     write) plus a 1s periodic fallback rescan (mtime/content compare);
//   - one accept goroutine per mapping entry;
//   - delete = close listener (established pumps drain naturally) + unlink;
//   - same listen name, different dial = delete + force-close established
//     pumps + rebind;
//   - at startup every *.sock in the directory is unlinked first (crash
//     self-healing) — sockets are bound only by the sidecar.
package proxysidecar

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
	logr "github.com/go-logr/logr"
)

// Mapping is one proxy.json entry (DESIGN-cluster-proxy.md §3).
type Mapping struct {
	// Listen is the socket BASENAME ("<ns>_<svc>_<port>.sock"); the
	// sidecar binds it inside the sockets directory.
	Listen string `json:"listen"`
	// Dial is the TCP target ("<clusterIP>:<port>") reached from the pod
	// network namespace.
	Dial string `json:"dial"`
}

// Options configures Run.
type Options struct {
	// SocketsDir is the shared hostPath directory holding proxy.json and
	// the *.sock files (chart default /run/hng-proxy).
	SocketsDir string
	// HealthzAddr is the /healthz listener address (chart default
	// 127.0.0.1:9126).
	HealthzAddr string
	// RescanInterval is the periodic proxy.json rescan fallback.
	// Defaults to 1s (design §0.6).
	RescanInterval time.Duration
	// IdleTimeout is the per-read deadline of the byte pumps (design
	// §0.9: 15min, deadlock protection). Test-only overrides are fine.
	IdleTimeout time.Duration
	// DialTimeout bounds one upstream dial. Defaults to 10s.
	DialTimeout time.Duration
	// Log receives lifecycle logging.
	Log logr.Logger
}

// Default values per DESIGN-cluster-proxy.md §0.
const (
	DefaultRescanInterval = time.Second
	DefaultIdleTimeout    = 15 * time.Minute
	DefaultDialTimeout    = 10 * time.Second
)

// pumpBufferSize is the io.CopyBuffer size of the L4 pumps (32KiB,
// design §0.7).
const pumpBufferSize = 32 * 1024

// Sidecar is the mapping executor. All state is guarded by mu; the
// accept loops and per-connection goroutines run on their own.
type Sidecar struct {
	opts Options
	log  logr.Logger

	mu         sync.Mutex
	entries    map[string]*entry // by socket basename; only successfully bound entries
	wanted     []Mapping         // last applied mapping set (sorted by Listen)
	loadedOnce bool              // proxy.json parsed successfully at least once
	lastFile   []byte            // last proxy.json content (rescan no-op check)

	// noFSNotify disables the fsnotify watcher (tests exercising the
	// periodic-rescan fallback path).
	noFSNotify bool

	stopOnce sync.Once
}

// New builds a Sidecar (defaults filled). The zero value is not usable.
func New(opts Options) (*Sidecar, error) {
	if opts.SocketsDir == "" {
		return nil, fmt.Errorf("proxysidecar: empty sockets dir")
	}
	if opts.HealthzAddr == "" {
		return nil, fmt.Errorf("proxysidecar: empty healthz addr")
	}
	if opts.RescanInterval <= 0 {
		opts.RescanInterval = DefaultRescanInterval
	}
	if opts.IdleTimeout <= 0 {
		opts.IdleTimeout = DefaultIdleTimeout
	}
	if opts.DialTimeout <= 0 {
		opts.DialTimeout = DefaultDialTimeout
	}
	if opts.Log.GetSink() == nil {
		opts.Log = logr.Discard()
	}
	return &Sidecar{
		opts:    opts,
		log:     opts.Log,
		entries: map[string]*entry{},
	}, nil
}

// mapPath is the proxy.json path inside the sockets dir.
func (s *Sidecar) mapPath() string { return filepath.Join(s.opts.SocketsDir, ProxyMapFileName) }

// ProxyMapFileName mirrors dataplane.ProxyMapFileName (redeclared to keep
// this package free of dataplane dependencies — the sidecar binary shares
// the build but not the import graph shape).
const ProxyMapFileName = "proxy.json"

// Run blocks until ctx is cancelled: startup cleanup, initial load,
// watch loop (fsnotify + periodic rescan) and the /healthz server.
func (s *Sidecar) Run(ctx context.Context) error {
	if err := os.MkdirAll(s.opts.SocketsDir, 0o755); err != nil {
		return fmt.Errorf("proxysidecar: mkdir %s: %w", s.opts.SocketsDir, err)
	}
	s.logFDLimit()
	if err := s.unlinkStaleSockets(); err != nil {
		return err
	}
	s.reload() // initial load (no-op when proxy.json is absent yet)

	// fsnotify on the DIRECTORY (design §0.6): the controller replaces
	// proxy.json via tmp+rename, so the file's inode changes — watching
	// the directory catches the Rename/Create of the replacement.
	notify := make(chan struct{}, 1)
	kick := func() {
		select {
		case notify <- struct{}{}:
		default: // a pending notification already covers this change
		}
	}
	var watcher *fsnotify.Watcher
	if !s.noFSNotify {
		w, err := fsnotify.NewWatcher()
		if err != nil {
			return fmt.Errorf("proxysidecar: fsnotify watcher: %w", err)
		}
		if err := w.Add(s.opts.SocketsDir); err != nil {
			_ = w.Close()
			return fmt.Errorf("proxysidecar: watch %s: %w", s.opts.SocketsDir, err)
		}
		watcher = w
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case ev, ok := <-w.Events:
					if !ok {
						return
					}
					// Only proxy.json (re)placements matter; the sidecar's
					// own *.sock bind/unlink traffic also lands here and
					// is filtered out by name.
					if filepath.Base(ev.Name) != ProxyMapFileName {
						continue
					}
					if ev.Op&(fsnotify.Create|fsnotify.Write|fsnotify.Rename) != 0 {
						kick()
					}
				case err, ok := <-w.Errors:
					if !ok {
						return
					}
					s.log.Error(err, "proxysidecar: fsnotify error")
				}
			}
		}()
	}

	// /healthz (design §0.9): readiness NEVER requires dial success —
	// backend health is not the sidecar's readiness condition.
	hln, err := net.Listen("tcp", s.opts.HealthzAddr)
	if err != nil {
		if watcher != nil {
			_ = watcher.Close()
		}
		return fmt.Errorf("proxysidecar: healthz listener on %s: %w", s.opts.HealthzAddr, err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		if s.Healthy() {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok\n"))
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("unhealthy: mapping not loaded or listener count != mapping count\n"))
	})
	hsrv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	serveErr := make(chan error, 1)
	go func() { serveErr <- hsrv.Serve(hln) }()
	s.log.Info("cluster-proxy sidecar started",
		"sockets-dir", s.opts.SocketsDir, "healthz-addr", s.opts.HealthzAddr)

	ticker := time.NewTicker(s.opts.RescanInterval)
	defer ticker.Stop()
	defer func() {
		s.shutdown()
		if watcher != nil {
			_ = watcher.Close()
		}
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = hsrv.Shutdown(shutdownCtx)
	}()

	for {
		select {
		case <-ctx.Done():
			s.log.Info("cluster-proxy sidecar stopping")
			return nil
		case err := <-serveErr:
			if !errors.Is(err, http.ErrServerClosed) {
				return fmt.Errorf("proxysidecar: healthz server: %w", err)
			}
			return nil
		case <-notify:
			s.reload()
		case <-ticker.C:
			s.reload()
		}
	}
}

// reload reads proxy.json, applies the diff when the content changed,
// and keeps the previous mapping on parse errors (health then reflects
// the stale-but-working state; the periodic rescan retries).
func (s *Sidecar) reload() {
	data, err := os.ReadFile(s.mapPath())
	if err != nil {
		if os.IsNotExist(err) {
			// Mapping never written (or foreign deletion): the file is
			// the single source of truth — nothing wanted. If it
			// vanished after having been loaded, unbind everything;
			// before the first load this is just the startup race with
			// the controller.
			s.mu.Lock()
			existed := s.lastFile != nil
			s.lastFile = nil
			s.mu.Unlock()
			if existed {
				s.apply(nil)
				s.log.Info("proxy.json disappeared; unbound all sockets")
			}
			return
		}
		s.log.Error(err, "proxy.json read failed (keeping current mapping)")
		return
	}
	s.mu.Lock()
	unchanged := bytesEqual(data, s.lastFile)
	s.mu.Unlock()
	if unchanged {
		return // fsnotify + tick both funnel here; content equality is the no-op gate
	}

	var mappings []Mapping
	if err := json.Unmarshal(data, &mappings); err != nil {
		s.log.Error(err, "proxy.json parse failed (keeping current mapping)")
		return
	}
	s.apply(mappings)
	s.mu.Lock()
	s.lastFile = data
	s.mu.Unlock()
}

// apply diffs the wanted mapping set against the active entries by
// (listen, dial) — design §0.8:
//   - entry no longer wanted → close listener (established pumps drain
//     naturally) + unlink socket;
//   - entry wanted under the same name with a DIFFERENT dial → delete
//     with force-close of established pumps + rebind;
//   - new name → bind + accept loop.
func (s *Sidecar) apply(wanted []Mapping) {
	sort.Slice(wanted, func(i, j int) bool { return wanted[i].Listen < wanted[j].Listen })
	// Deduplicate (deterministic: first wins after sort).
	dedup := wanted[:0]
	for i, m := range wanted {
		if m.Listen == "" || m.Dial == "" {
			s.log.Info("ignoring malformed mapping entry", "listen", m.Listen, "dial", m.Dial)
			continue
		}
		if i > 0 && m.Listen == wanted[i-1].Listen {
			continue
		}
		dedup = append(dedup, m)
	}
	wanted = dedup
	wantByName := make(map[string]string, len(wanted))
	for _, m := range wanted {
		wantByName[m.Listen] = m.Dial
	}

	type bindTask struct{ name, dial string }
	var binds []bindTask

	s.mu.Lock()
	for name, e := range s.entries {
		dial, ok := wantByName[name]
		switch {
		case !ok:
			// Deleted: close listener + unlink; established pumps drain.
			delete(s.entries, name)
			e.stop(false)
		case dial != e.dial:
			// Retarget: delete + force-close pumps, rebind below.
			delete(s.entries, name)
			e.stop(true)
			binds = append(binds, bindTask{name, dial})
		}
	}
	for _, m := range wanted {
		if _, exists := s.entries[m.Listen]; !exists {
			binds = append(binds, bindTask{m.Listen, m.Dial})
		}
	}
	s.wanted = wanted
	s.loadedOnce = true // one proxy.json parsed and applied end-to-end
	s.mu.Unlock()

	for _, b := range binds {
		s.bind(b.name, b.dial)
	}
}

// bind creates one entry: unlink any stale path, bind the unix listener
// and start the accept loop. Bind failures are logged and leave the
// entry out of the active set — Healthy() reports the mismatch.
func (s *Sidecar) bind(name, dial string) {
	path := filepath.Join(s.opts.SocketsDir, name)
	_ = os.Remove(path) // startup unlinked everything; this covers foreign leftovers
	ln, err := net.Listen("unix", path)
	if err != nil {
		s.log.Error(err, "bind failed (entry stays inactive; sidecar reports unhealthy)", "socket", path, "dial", dial)
		return
	}
	// Unix sockets require WRITE permission to connect; net.Listen("unix")
	// applies the process umask (typically 022 → 0755, others r-x), which
	// would lock out the nginx worker user. Make it world-connectable.
	if err := os.Chmod(path, 0o666); err != nil {
		s.log.Error(err, "socket chmod failed (entry stays inactive)", "socket", path)
		ln.Close()
		return
	}
	e := &entry{name: name, dial: dial, path: path, ln: ln.(*net.UnixListener), conns: map[*connPair]struct{}{}}
	s.mu.Lock()
	s.entries[name] = e
	s.mu.Unlock()
	go e.acceptLoop(s.opts, s.log)
	s.log.Info("socket bound", "socket", path, "dial", dial)
}

// Healthy implements design §0.9:
// mappingLoadedOnce && len(wanted) == len(activeListeners). No dial
// requirement — backend reachability is not the sidecar's readiness.
func (s *Sidecar) Healthy() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadedOnce && len(s.wanted) == len(s.entries)
}

// shutdown force-closes every entry (process exit: no draining needed —
// the sockets are unlinked and rebound on the next startup).
func (s *Sidecar) shutdown() {
	s.stopOnce.Do(func() {
		s.mu.Lock()
		entries := make([]*entry, 0, len(s.entries))
		for name, e := range s.entries {
			entries = append(entries, e)
			delete(s.entries, name)
		}
		s.mu.Unlock()
		for _, e := range entries {
			e.stop(true)
		}
	})
}

// unlinkStaleSockets removes every *.sock in the sockets dir before the
// first bind (design §0.8: crash self-healing — only the sidecar binds
// or unlinks sockets; a crashed sidecar leaves stale files behind).
func (s *Sidecar) unlinkStaleSockets() error {
	entries, err := os.ReadDir(s.opts.SocketsDir)
	if err != nil {
		return fmt.Errorf("proxysidecar: readdir %s: %w", s.opts.SocketsDir, err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sock") {
			continue
		}
		p := filepath.Join(s.opts.SocketsDir, e.Name())
		if err := os.Remove(p); err != nil {
			s.log.Error(err, "startup stale-socket unlink failed", "socket", p)
		}
	}
	return nil
}

// logFDLimit records the file-descriptor limit at startup (design §7:
// socket count grows linearly with backends; the cap is logged so the
// operator can correlate exhaustion later).
func (s *Sidecar) logFDLimit() {
	var lim syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &lim); err != nil {
		s.log.Error(err, "could not read RLIMIT_NOFILE")
		return
	}
	s.log.Info("file descriptor limit", "cur", lim.Cur, "max", lim.Max)
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// Per-mapping entry: listener + accept loop + established connections
// ---------------------------------------------------------------------------

// entry is one bound socket (one accept goroutine).
type entry struct {
	name string // socket basename
	dial string // "clusterIP:port"
	path string // absolute socket path
	ln   *net.UnixListener

	mu         sync.Mutex
	conns      map[*connPair]struct{}
	closed     bool // listener shut down
	forceClose bool // closed with force (retarget / process exit)
}

// stop shuts the entry down. force additionally closes every established
// connection (retarget / process exit); without it the pumps drain
// naturally (plain delete, design §0.8). The socket file is unlinked in
// both cases — bind/unlink belongs to the sidecar alone.
func (e *entry) stop(force bool) {
	e.mu.Lock()
	e.closed = true
	e.forceClose = e.forceClose || force
	pairs := make([]*connPair, 0, len(e.conns))
	for p := range e.conns {
		pairs = append(pairs, p)
	}
	e.conns = map[*connPair]struct{}{}
	e.mu.Unlock()

	_ = e.ln.Close() // accept loop exits
	if force {
		for _, p := range pairs {
			p.halfCloseBoth() // abort both directions; pump errors surface as double-close
		}
	}
	_ = os.Remove(e.path)
}

func (e *entry) track(p *connPair) {
	e.mu.Lock()
	if !e.closed {
		e.conns[p] = struct{}{}
		e.mu.Unlock()
		return
	}
	force := e.forceClose
	e.mu.Unlock()
	if force {
		// Accepted while a force shutdown was racing the accept loop
		// (retarget / process exit): the pair belongs to the dying
		// entry — tear it down instead of leaking it into the drain set.
		p.halfCloseBoth()
	}
}

func (e *entry) untrack(p *connPair) {
	e.mu.Lock()
	delete(e.conns, p)
	e.mu.Unlock()
}

// acceptLoop accepts host-nginx connections and dials the mapped
// ClusterIP for each. Dial failures close the accepted side immediately
// — nginx observes an upstream failure and applies its own passive
// retry semantics (design §2: failures pass through unchanged).
func (e *entry) acceptLoop(opts Options, log logr.Logger) {
	for {
		conn, err := e.ln.Accept()
		if err != nil {
			e.mu.Lock()
			closed := e.closed
			e.mu.Unlock()
			if !closed && !errors.Is(err, net.ErrClosed) {
				log.Error(err, "accept failed", "socket", e.name)
			}
			return
		}
		go func(local net.Conn) {
			remote, err := net.DialTimeout("tcp", e.dial, opts.DialTimeout)
			if err != nil {
				_ = local.Close()
				return
			}
			p := &connPair{local: local, remote: remote}
			e.track(p)
			p.pump(opts.IdleTimeout)
			e.untrack(p)
		}(conn)
	}
}

// ---------------------------------------------------------------------------
// L4 byte pump (design §0.7)
// ---------------------------------------------------------------------------

// connPair is one proxied connection: host-nginx (unix) ↔ backend (tcp).
type connPair struct {
	local  net.Conn
	remote net.Conn
}

// pump starts both copy directions and blocks until both are done.
func (p *connPair) pump(idle time.Duration) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); p.copy(p.remote, p.local, idle) }() // request: local → remote
	go func() { defer wg.Done(); p.copy(p.local, p.remote, idle) }() // response: remote → local
	wg.Wait()
	// Both directions finished (each already propagated its FIN or the
	// pair was aborted): full close.
	_ = p.local.Close()
	_ = p.remote.Close()
}

// copy is ONE direction: io.CopyBuffer with a 32KiB buffer, a per-read
// idle deadline (design §0.9: 15min deadlock protection), half-close on
// EOF (CloseWrite propagates the FIN to the other side) and immediate
// double-close on any non-EOF error.
func (p *connPair) copy(dst, src net.Conn, idle time.Duration) {
	buf := make([]byte, pumpBufferSize)
	_, err := io.CopyBuffer(dst, &idleReader{Conn: src, idle: idle}, buf)
	if err == nil {
		// src hit EOF: propagate the half-close so the peer sees the FIN
		// while the reverse direction keeps flowing (design §0.7).
		closeWrite(dst)
		return
	}
	// Non-EOF error (idle timeout, write failure, aborted pair): both
	// sides are torn down immediately.
	p.halfCloseBoth()
}

// idleReader resets the source's read deadline before every Read so the
// deadline measures IDLE time, not total lifetime (a slow-but-flowing
// stream never trips it).
type idleReader struct {
	net.Conn
	idle time.Duration
}

func (r *idleReader) Read(p []byte) (int, error) {
	_ = r.Conn.SetReadDeadline(time.Now().Add(r.idle))
	return r.Conn.Read(p)
}

// closeWrite half-closes a connection when the concrete type supports it
// (unix and TCP both do; the type switch mirrors design §0.7).
func closeWrite(c net.Conn) {
	switch t := c.(type) {
	case *net.UnixConn:
		_ = t.CloseWrite()
	case *net.TCPConn:
		_ = t.CloseWrite()
	}
}

// halfCloseBoth fully closes both ends (the "immediate double close").
func (p *connPair) halfCloseBoth() {
	_ = p.local.Close()
	_ = p.remote.Close()
}
