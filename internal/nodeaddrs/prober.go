// Prober: the fingerprint refresh loop (DESIGN-multinode-addresses.md §2
// + §7 "节点 IP 抖动导致配置震荡：防抖"). The controller probes the
// node's addresses at startup and every Interval; a candidate change is
// adopted only after Debounce consecutive probes agree on it, so a
// flapping interface address does not thrash the rendered configuration.
package nodeaddrs

import (
	"context"
	"fmt"
	"time"

	logr "github.com/go-logr/logr"
)

// DefaultProbeInterval is the periodic re-probe cadence (design doc §2:
// "周期探测（60s）").
const DefaultProbeInterval = 60 * time.Second

// DefaultDebounce is the number of consecutive agreeing probes before a
// fingerprint change is adopted (design doc §7: "连续 N 次探测一致才更新
// 指纹"; N=2).
const DefaultDebounce = 2

// Prober maintains the current Set from a probe function. The zero value
// is not usable — build one with NewProber.
type Prober struct {
	probe    func(ctx context.Context) ([]string, error)
	interval time.Duration
	debounce int
	log      logr.Logger

	onChange func()

	// candidate/candCount carry the debounce state: the pending Set and
	// how many consecutive probes produced exactly it.
	candidate *Set
	candCount int

	current *Set
}

// NewProber builds a Prober. interval <= 0 defaults to
// DefaultProbeInterval; debounce <= 0 defaults to DefaultDebounce. A nil
// probe defaults to Enumerate (current network namespace).
func NewProber(probe func(ctx context.Context) ([]string, error), interval time.Duration, debounce int, log logr.Logger) *Prober {
	if probe == nil {
		probe = func(context.Context) ([]string, error) { return Enumerate() }
	}
	if interval <= 0 {
		interval = DefaultProbeInterval
	}
	if debounce <= 0 {
		debounce = DefaultDebounce
	}
	if log.GetSink() == nil {
		log = logr.Discard()
	}
	return &Prober{probe: probe, interval: interval, debounce: debounce, log: log}
}

// Current returns the adopted fingerprint (nil until the first
// successful probe — callers treat nil as "ownership unknown").
func (p *Prober) Current() *Set { return p.current }

// OnChange registers a callback fired (outside the prober lock) every
// time the adopted fingerprint changes. The controller wires this to a
// full-sync trigger.
func (p *Prober) OnChange(fn func()) { p.onChange = fn }

// Refresh runs one probe through the debounce logic and returns the
// probe error (the previous fingerprint is kept on failure). The FIRST
// successful probe adopts immediately — without it a restart would need
// Debounce×Interval before owning anything.
func (p *Prober) Refresh(ctx context.Context) error {
	addrs, err := p.probe(ctx)
	if err != nil {
		return err
	}
	if len(addrs) == 0 {
		return fmt.Errorf("nodeaddrs: probe returned no addresses")
	}
	next := NewSet(addrs...)

	adopted := false
	func() {
		if p.current == nil {
			p.current = next
			adopted = true
			return
		}
		if next.Equal(p.current) {
			p.candidate, p.candCount = nil, 0 // steady state again
			return
		}
		if p.candidate != nil && next.Equal(p.candidate) {
			p.candCount++
		} else {
			p.candidate, p.candCount = next, 1
		}
		if p.candCount >= p.debounce {
			p.current = next
			p.candidate, p.candCount = nil, 0
			adopted = true
		}
	}()
	if adopted {
		p.log.Info("node address fingerprint adopted",
			"fingerprint", p.current.Fingerprint(), "addresses", p.current.Addresses())
		if p.onChange != nil {
			p.onChange()
		}
	}
	return nil
}

// Start loops Refresh on the probe interval until ctx is done. Probe
// failures are logged and retried on the next tick (the last adopted
// fingerprint keeps serving).
func (p *Prober) Start(ctx context.Context) error {
	t := time.NewTicker(p.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			if err := p.Refresh(ctx); err != nil {
				p.log.Error(err, "node address probe failed (keeping current fingerprint)")
			}
		}
	}
}
