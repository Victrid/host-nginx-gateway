package nodeaddrs

import (
	"context"
	"errors"
	"testing"
	"time"

	logr "github.com/go-logr/logr"
)

// scriptedProbe returns a probe function serving queued results; each
// call consumes one entry (the last repeats).
func scriptedProbe(results ...[]string) (func(context.Context) ([]string, error), *int) {
	calls := 0
	return func(context.Context) ([]string, error) {
		i := calls
		calls++
		if i >= len(results) {
			i = len(results) - 1
		}
		if results[i] == nil {
			return nil, errors.New("scripted probe failure")
		}
		return results[i], nil
	}, &calls
}

func TestProber_FirstProbeAdoptsImmediately(t *testing.T) {
	probe, calls := scriptedProbe([]string{"192.0.2.10"})
	p := NewProber(probe, time.Minute, 2, logr.Discard())
	if p.Current() != nil {
		t.Fatal("no fingerprint before the first probe")
	}
	var fired int
	p.OnChange(func() { fired++ })
	if err := p.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if *calls != 1 {
		t.Fatalf("calls = %d", *calls)
	}
	cur := p.Current()
	if cur == nil || !cur.Contains("192.0.2.10") {
		t.Fatalf("first successful probe must adopt immediately: %v", cur)
	}
	if fired != 1 {
		t.Fatalf("onChange fired %d times, want 1", fired)
	}
	// Steady state: same result again → no change event, no churn.
	if err := p.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fired != 1 {
		t.Fatalf("unchanged probe must not fire onChange (fired %d)", fired)
	}
}

func TestProber_DebounceRequiresConsistentProbes(t *testing.T) {
	// Startup set, then a change that must be seen twice (N=2) before
	// adoption — interface flapping must not thrash the config
	// (DESIGN-multinode-addresses.md §7).
	probe, _ := scriptedProbe(
		[]string{"192.0.2.10"},               // startup: adopt
		[]string{"198.51.100.7"},             // candidate 1/2
		[]string{"192.0.2.10"},               // flap back: candidate discarded
		[]string{"198.51.100.7"},             // candidate 1/2 again
		[]string{"198.51.100.7"},             // candidate 2/2 → adopt
		[]string{"198.51.100.7", "10.0.0.1"}, // next change 1/2
	)
	p := NewProber(probe, time.Minute, 2, logr.Discard())
	var fired int
	p.OnChange(func() { fired++ })
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if err := p.Refresh(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if fired != 2 {
		t.Fatalf("onChange fired %d times, want 2 (startup + debounced change)", fired)
	}
	if !p.Current().Equal(NewSet("198.51.100.7")) {
		t.Fatalf("current = %v, want the debounced adoption", p.Current().Addresses())
	}
	if err := p.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if fired != 2 {
		t.Fatalf("first probe of the next change must not adopt yet (fired %d)", fired)
	}
	if p.Current().Contains("10.0.0.1") {
		t.Fatal("adopted a change seen only once")
	}
}

func TestProber_FailureKeepsCurrent(t *testing.T) {
	probe, _ := scriptedProbe([]string{"192.0.2.10"}, nil, []string{"198.51.100.7"})
	p := NewProber(probe, time.Minute, 3, logr.Discard())
	ctx := context.Background()
	if err := p.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if err := p.Refresh(ctx); err == nil {
		t.Fatal("probe failure must surface")
	}
	if !p.Current().Equal(NewSet("192.0.2.10")) {
		t.Fatalf("failed probe must keep the current fingerprint: %v", p.Current().Addresses())
	}
	// After the failure, a genuinely consistent new set still adopts.
	p.Refresh(ctx)
	p.Refresh(ctx) // debounce 3: 2/3
	if p.Current().Contains("198.51.100.7") {
		t.Fatal("adopted before the debounce threshold")
	}
	p.Refresh(ctx) // 3/3
	if !p.Current().Contains("198.51.100.7") {
		t.Fatal("must adopt after 3 consistent probes")
	}
}

func TestProber_EmptyResultIsError(t *testing.T) {
	p := NewProber(func(context.Context) ([]string, error) { return nil, nil }, time.Minute, 2, logr.Discard())
	if err := p.Refresh(context.Background()); err == nil {
		t.Fatal("an empty address list must be treated as a probe failure")
	}
	if p.Current() != nil {
		t.Fatal("nothing adopted from an empty probe")
	}
}

func TestProber_StartStopsWithContext(t *testing.T) {
	probe, calls := scriptedProbe([]string{"192.0.2.10"})
	p := NewProber(probe, 5*time.Millisecond, 2, logr.Discard())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.Start(ctx); close(done) }()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("Start did not tick")
		default:
		}
		if *calls >= 3 {
			cancel()
			break
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not stop after ctx cancel")
	}
}
