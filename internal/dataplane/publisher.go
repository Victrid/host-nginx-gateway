// Publisher: DESIGN.md §5.2 - rollback state machine + §1/S6 applied-hash
// no-op. Sequence per publish:
//  1. render cfg -> bytes
//  2. hash the bytes; if equal to the cached applied hash for this gateway,
//     skip writing/reloading (S6).
//  3. write bytes to <gw>.conf.tmp
//  4. validate via Validator (DESIGN.md §5.1)
//  5. rename <srv>.conf.tmp -> <srv>.conf, preserving the previous
//     <srv>.conf as <srv>.conf.prev
//  6. nginx -s reload
//     - on success: remove .prev, update applied hash, return nil
//     - on failure: restore .prev -> .conf, reload again, return ErrReload
//     wrapping stderr (DESIGN.md §3.4 / N6)
package dataplane

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Victrid/HostNginxGateway/internal/contract"
)

// ErrReload is returned by Publish when the post-rename reload fails and the
// rollback reload also fails (or the rollback path itself trips). Callers
// map it to Programmed=False (Invalid) and put the wrapped stderr into the
// condition message (DESIGN.md §3.4 / N6).
var ErrReload = errors.New("dataplane: nginx reload failed")

// PublisherOptions configures the publisher. The struct is the documented seam
// for tests (validator + nginx client are injected).
type PublisherOptions struct {
	// OutputDir is the directory where <gw>.conf / <gw>.conf.tmp /
	// <gw>.conf.prev files live. Defaults to /etc/nginx/conf.d/k8s-gw.
	OutputDir string
	// Validator performs nginx -t against a temp main config.
	Validator *Validator
	// Nginx issues Probe / Reload.
	Nginx NginxClient
	// FileMode is the mode applied to the rendered config file.
	// Defaults to 0o644.
	FileMode os.FileMode
	// ProbeBeforeReload, when true, calls Nginx.Probe() and short-circuits
	// with ErrNginxNotRunning if it does. Default true.
	ProbeBeforeReload bool
	// ErrorLogPath is the host nginx error log used for reload-effect
	// verification: after a reload that adds NEW listen sockets, the
	// publisher watches the log for bind failures — `nginx -s reload`
	// exits 0 even when the new workers cannot bind and the master keeps
	// serving the previous configuration ("reload signal blindness",
	// DESIGN.md §5.2). Empty disables verification.
	ErrorLogPath string
	// VerifyReloadDelay bounds how long the publisher waits for a bind
	// failure to appear after a reload. Bind failures surface within
	// milliseconds; the default is 2s.
	VerifyReloadDelay time.Duration
}

// Publisher applies Configuration to disk and reloads nginx.
type Publisher struct {
	opts PublisherOptions
	// applied remembers the last hash that successfully survived a reload,
	// keyed by Gateway filename (basename without .conf).
	applied map[string]string
	// appliedListens remembers the listen set of the last successfully
	// applied configuration, keyed like applied — the input for the
	// reload-effect verification (new binds must actually come up).
	appliedListens map[string][]string
}

// NewPublisher returns a Publisher with defaults filled in.
func NewPublisher(opts PublisherOptions) (*Publisher, error) {
	if opts.OutputDir == "" {
		opts.OutputDir = "/etc/nginx/conf.d/k8s-gw"
	}
	if opts.Validator == nil {
		return nil, fmt.Errorf("dataplane: publisher: validator required")
	}
	if opts.Nginx == nil {
		return nil, fmt.Errorf("dataplane: publisher: nginx client required")
	}
	if opts.FileMode == 0 {
		opts.FileMode = 0o644
	}
	// ProbeBeforeReload defaults to true: DESIGN.md §6 requires us to skip
	// reload and report Programmed=False (Pending) when nginx is not running.
	// Tests that don't want this behaviour set it explicitly to false.
	opts.ProbeBeforeReload = true
	if opts.VerifyReloadDelay <= 0 {
		// nginx logs bind failures within a few milliseconds of the reload
		// signal; a short bounded window keeps the single full-sync
		// reconciler responsive (status latency), DESIGN.md §5.2.
		opts.VerifyReloadDelay = 200 * time.Millisecond
	}
	return &Publisher{
		opts:           opts,
		applied:        map[string]string{},
		appliedListens: map[string][]string{},
	}, nil
}

// Published is returned by Publish on success. Reloaded=false means the
// no-op check skipped the write+reload (S6); the on-disk file is byte-equal
// to the desired rendered output.
type Published struct {
	File     string
	Hash     string
	Reloaded bool
}

// Publish renders cfg and applies it as <name>.conf via the rollback
// state machine. The hash no-op check (DESIGN.md §1 / S6) is keyed by name.
//
// Lifecycle (per publish):
//   - hash(rendered) equals applied[name]?  -> return Reloaded:false
//   - otherwise: write tmp -> validate -> backup prev -> rename -> reload
//   - reload OK: drop prev, remember hash, return Reloaded:true
//   - reload fail: restore prev, reload again, return ErrReload
func (p *Publisher) Publish(ctx context.Context, name string, cfg *contract.Configuration) (Published, error) {
	if name == "" {
		return Published{}, fmt.Errorf("dataplane: publish: empty gateway name")
	}
	rendered, err := Render(cfg)
	if err != nil {
		return Published{}, fmt.Errorf("dataplane: publish: render: %w", err)
	}
	return p.applyRendered(ctx, name, rendered, listenKeysOf(cfg))
}

// PublishRendered is the no-render variant of Publish; useful when callers
// have already produced bytes (tests, hash-only paths). No listen set is
// known, so the reload-effect verification is skipped.
func (p *Publisher) PublishRendered(ctx context.Context, name string, rendered []byte) (Published, error) {
	if name == "" {
		return Published{}, fmt.Errorf("dataplane: publish: empty gateway name")
	}
	if rendered == nil {
		return Published{}, fmt.Errorf("dataplane: publish: nil rendered bytes")
	}
	return p.applyRendered(ctx, name, rendered, nil)
}

func (p *Publisher) applyRendered(ctx context.Context, name string, rendered []byte, listens []string) (Published, error) {
	hash := Hash(rendered)

	// §1/S6 no-op: same hash => same bytes on disk; skip everything.
	if prev, ok := p.applied[name]; ok && prev == hash {
		return Published{File: name + ".conf", Hash: hash, Reloaded: false}, nil
	}

	if p.opts.ProbeBeforeReload {
		if err := p.opts.Nginx.Probe(); err != nil {
			return Published{}, fmt.Errorf("dataplane: publish: probe: %w", err)
		}
	}

	fileName := name + ".conf"
	dir := p.opts.OutputDir
	finalPath := filepath.Join(dir, fileName)
	tmpPath := finalPath + ".tmp"
	prevPath := finalPath + ".prev"

	// 1. Write the .tmp file.
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Published{}, fmt.Errorf("dataplane: publish: mkdir: %w", err)
	}
	if err := atomicWrite(tmpPath, rendered, p.opts.FileMode); err != nil {
		return Published{}, fmt.Errorf("dataplane: publish: write tmp: %w", err)
	}

	// 2. Validate via the temporary-main-config method (DESIGN.md §5.1).
	//    We pass the rendered tmp path so a reviewer can cross-check that
	//    nginx -t actually sees the new server block; the validator does not
	//    need to inspect it.
	if _, err := p.opts.Validator.Validate(ctx, tmpPath); err != nil {
		_ = os.Remove(tmpPath)
		return Published{}, fmt.Errorf("dataplane: publish: validate: %w", err)
	}

	// Snapshot the error log size BEFORE the reload so verification only
	// inspects lines appended by THIS reload.
	logOffset := p.logSize()

	// 3. Backup current -> .prev (best-effort; absence is fine), then
	//    rename .tmp -> .conf. rename(2) is atomic on POSIX.
	hadPrev := false
	if _, err := os.Stat(finalPath); err == nil {
		hadPrev = true
		if err := copyFile(finalPath, prevPath); err != nil {
			_ = os.Remove(tmpPath)
			return Published{}, fmt.Errorf("dataplane: publish: backup prev: %w", err)
		}
	}
	if err := os.Rename(tmpPath, finalPath); err != nil {
		_ = os.Remove(tmpPath)
		return Published{}, fmt.Errorf("dataplane: publish: rename: %w", err)
	}

	// 4. Reload.
	if err := p.opts.Nginx.Reload(ctx); err != nil {
		// 5. Rollback (DESIGN.md §5.2).
		return Published{}, p.rollback(ctx, name, prevPath, finalPath, hadPrev, err, logOffset, hash)
	}

	// 5b. Reload-effect verification (DESIGN.md §5.2): a reload that adds
	// listen sockets can be REJECTED at bind time while still exiting 0;
	// the master then keeps serving the previous configuration. Check the
	// error log for bind failures; on detection run the same rollback.
	if err := p.verifyReloadEffect(name, listens, logOffset); err != nil {
		return Published{}, p.rollback(ctx, name, prevPath, finalPath, hadPrev, err, logOffset, hash)
	}

	// 6. Success: drop .prev and remember the applied hash + listen set.
	if hadPrev {
		_ = os.Remove(prevPath)
	}
	p.applied[name] = hash
	p.appliedListens[name] = listens
	return Published{File: fileName, Hash: hash, Reloaded: true}, nil
}

// rollback restores the previous configuration (or removes the new one when
// no previous file exists), reloads it and wraps the original cause so the
// provider reports Programmed=False (Invalid).
func (p *Publisher) rollback(ctx context.Context, name, prevPath, finalPath string, hadPrev bool,
	cause error, logOffset int64, failedHash string) error {
	if hadPrev {
		if rerr := os.Rename(prevPath, finalPath); rerr != nil {
			return fmt.Errorf("%w: reload err: %v; rollback rename err: %v", ErrReload, cause, rerr)
		}
		if rerr := p.opts.Nginx.Reload(ctx); rerr != nil {
			return fmt.Errorf("%w: reload err: %v; rollback reload err: %v", ErrReload, cause, rerr)
		}
	} else {
		// No prior .conf existed; best we can do is delete the broken file.
		_ = os.Remove(finalPath)
	}
	delete(p.appliedListens, name)
	// The applied hash (if any) refers to the PREVIOUS config — keep it so
	// the next sync of the old desired state can no-op, and so a retry of
	// the failed config re-applies (its hash differs).
	if p.applied[name] == failedHash {
		delete(p.applied, name)
	}
	return fmt.Errorf("%w: %v", ErrReload, cause)
}

// logSize returns the current size of the error log (0 when unavailable).
func (p *Publisher) logSize() int64 {
	if p.opts.ErrorLogPath == "" {
		return 0
	}
	info, err := os.Stat(p.opts.ErrorLogPath)
	if err != nil {
		return 0
	}
	return info.Size()
}

// verifyReloadEffect watches the error log for bind failures appended after
// logOffset, for a bounded window (DESIGN.md §5.2). Only a reload that ADDS
// listen sockets is verified — socket additions are the transitions nginx
// can reject at bind time. Best-effort: an unreadable/absent log skips the
// check.
func (p *Publisher) verifyReloadEffect(name string, listens []string, logOffset int64) error {
	if p.opts.ErrorLogPath == "" || len(listens) == 0 {
		return nil
	}
	if prev, ok := p.appliedListens[name]; ok && sameStrings(prev, listens) {
		return nil // no new sockets
	}
	deadline := time.Now().Add(p.opts.VerifyReloadDelay)
	var tail []byte
	for {
		tail = readLogTail(p.opts.ErrorLogPath, logOffset)
		if line, ok := findBindFailure(tail); ok {
			return fmt.Errorf("reload did not take effect: %s", line)
		}
		if time.Now().After(deadline) {
			return nil // no bind failure observed within the window
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// readLogTail reads the log bytes appended after offset (best-effort).
func readLogTail(path string, offset int64) []byte {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || info.Size() <= offset {
		return nil
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil
	}
	data := make([]byte, info.Size()-offset)
	n, err := io.ReadFull(f, data)
	if err != nil && n == 0 {
		return nil
	}
	return data[:n]
}

// findBindFailure scans appended log lines for nginx bind rejections
// (`[emerg] … bind() to … failed …`).
func findBindFailure(data []byte) (string, bool) {
	if len(data) == 0 {
		return "", false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.Contains(line, "bind() to") && strings.Contains(line, "failed") {
			return strings.TrimSpace(line), true
		}
	}
	return "", false
}

// listenKeysOf flattens a configuration's listen set into "addr:port" keys.
func listenKeysOf(cfg *contract.Configuration) []string {
	if cfg == nil {
		return nil
	}
	seen := map[string]struct{}{}
	var out []string
	for _, s := range cfg.Servers {
		for _, l := range s.Listens {
			k := l.Address + ":" + strconv.Itoa(l.Port)
			if _, dup := seen[k]; !dup {
				seen[k] = struct{}{}
				out = append(out, k)
			}
		}
	}
	sort.Strings(out)
	return out
}

func sameStrings(a, b []string) bool {
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

// Forget clears the no-op memory for one gateway (used by tests; the
// reconciler can also use it when a Gateway is deleted).
func (p *Publisher) Forget(name string) {
	delete(p.applied, name)
}

// Applied returns the last-applied hash for name, if any. Useful for tests
// asserting that Publish recorded its result.
func (p *Publisher) Applied(name string) (string, bool) {
	h, ok := p.applied[name]
	return h, ok
}

// atomicWrite writes data to a sibling tmp file in dir, sets its mode,
// and renames over the destination. The double-step is necessary because
// os.WriteFile/Create don't honour mode on every platform (and on Linux,
// the file is created with mode from umask). We need explicit 0644 (or
// whatever the publisher was configured with).
func atomicWrite(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpPath) }
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Chmod(tmpPath, mode); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		cleanup()
		return err
	}
	return nil
}

// copyFile copies src to dst via a sibling tmp+rename so a partial write
// never overwrites a valid .prev. Source mode is preserved.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	info, err := in.Stat()
	if err != nil {
		return err
	}

	dir := filepath.Dir(dst)
	tmp, err := os.CreateTemp(dir, filepath.Base(dst)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	if _, err := io.Copy(tmp, in); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if err := os.Chmod(tmpPath, info.Mode()); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return os.Rename(tmpPath, dst)
}
