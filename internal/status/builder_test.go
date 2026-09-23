package status

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestBuilderAddReturnsExpectedShape(t *testing.T) {
	clock := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	b := NewBuilder(func() time.Time { return clock })

	got := b.Add("Programmed", metav1.ConditionTrue, "Programmed", "ok", 7)
	if got.Type != "Programmed" ||
		got.Status != metav1.ConditionTrue ||
		got.Reason != "Programmed" ||
		got.Message != "ok" ||
		got.ObservedGeneration != 7 {
		t.Errorf("unexpected condition: %+v", got)
	}
	if !got.LastTransitionTime.Time.Equal(clock) {
		t.Errorf("LastTransitionTime = %v, want %v", got.LastTransitionTime.Time, clock)
	}
}

func TestBuilderNoOpPreservesLastTransitionTime(t *testing.T) {
	first := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	b := NewBuilder(func() time.Time { return first })
	b.Add("Programmed", metav1.ConditionTrue, "Programmed", "ok", 1)

	// Second call with identical (Status, Reason, Message) but later
	// clock + new generation should preserve the original timestamp.
	b.FreezeNow(first.Add(time.Hour))
	got := b.Add("Programmed", metav1.ConditionTrue, "Programmed", "ok", 2)

	if got.ObservedGeneration != 2 {
		t.Errorf("ObservedGeneration should refresh, got %d", got.ObservedGeneration)
	}
	if !got.LastTransitionTime.Time.Equal(first) {
		t.Errorf("LastTransitionTime should be preserved on no-op, got %v", got.LastTransitionTime.Time)
	}
}

func TestBuilderTransitionUpdatesTimestamp(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	b := NewBuilder(func() time.Time { return t0 })
	b.Add("Programmed", metav1.ConditionFalse, "Invalid", "bad", 1)

	t1 := t0.Add(time.Minute)
	b.FreezeNow(t1)
	got := b.Add("Programmed", metav1.ConditionTrue, "Programmed", "ok", 1)
	if !got.LastTransitionTime.Time.Equal(t1) {
		t.Errorf("transition should set LastTransitionTime=t1, got %v", got.LastTransitionTime.Time)
	}
}

func TestBuilderConditionsIsStable(t *testing.T) {
	b := NewBuilder(nil)
	b.Add("Z", metav1.ConditionTrue, "r", "m", 1)
	b.Add("A", metav1.ConditionFalse, "r", "m", 1)
	b.Add("M", metav1.ConditionTrue, "r", "m", 1)

	got := b.Conditions()
	want := []string{"A", "M", "Z"}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i].Type != w {
			t.Errorf("conditions[%d].Type = %q, want %q", i, got[i].Type, w)
		}
	}
}

func TestMergeByTypeReadModifyWrite(t *testing.T) {
	existing := []metav1.Condition{{
		Type:               "Programmed",
		Status:             metav1.ConditionTrue,
		Reason:             "Programmed",
		Message:            "ok",
		ObservedGeneration: 1,
		LastTransitionTime: metav1.NewTime(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)),
	}, {
		Type:               "Accepted",
		Status:             metav1.ConditionFalse,
		Reason:             "Invalid",
		Message:            "include missing",
		ObservedGeneration: 1,
		LastTransitionTime: metav1.NewTime(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)),
	}}

	// incoming has refreshed Programmed (same content, new gen) and
	// dropped Accepted (no longer relevant because the include is now in
	// place). Plus a brand-new Condition.
	incoming := []metav1.Condition{{
		Type:               "Programmed",
		Status:             metav1.ConditionTrue,
		Reason:             "Programmed",
		Message:            "ok",
		ObservedGeneration: 2,
		LastTransitionTime: metav1.NewTime(time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)),
	}, {
		Type:               "Ready",
		Status:             metav1.ConditionTrue,
		Reason:             "Ready",
		Message:            "ready",
		ObservedGeneration: 2,
		LastTransitionTime: metav1.NewTime(time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)),
	}}

	merged := MergeByType(existing, incoming)

	// Existing Accepted must be gone (caller is authoritative).
	for _, c := range merged {
		if c.Type == "Accepted" {
			t.Errorf("stale Accepted survived merge: %+v", c)
		}
	}

	// Programmed should keep its old LastTransitionTime (no-op), but
	// pick up the new ObservedGeneration.
	var prog *metav1.Condition
	for i := range merged {
		if merged[i].Type == "Programmed" {
			prog = &merged[i]
		}
	}
	if prog == nil {
		t.Fatalf("Programmed missing in merged result: %+v", merged)
	}
	if prog.ObservedGeneration != 2 {
		t.Errorf("Programmed.ObservedGeneration = %d, want 2", prog.ObservedGeneration)
	}
	if !prog.LastTransitionTime.Time.Equal(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("Programmed.LastTransitionTime changed on no-op: %v", prog.LastTransitionTime.Time)
	}

	// Ready should be present.
	var ready *metav1.Condition
	for i := range merged {
		if merged[i].Type == "Ready" {
			ready = &merged[i]
		}
	}
	if ready == nil {
		t.Fatalf("Ready missing in merged result: %+v", merged)
	}

	// Result is sorted by Type.
	for i := 1; i < len(merged); i++ {
		if merged[i-1].Type >= merged[i].Type {
			t.Errorf("merge not sorted: %v", merged)
		}
	}
}

func TestEqualConditionsIgnoresLastTransitionTime(t *testing.T) {
	a := metav1.Condition{
		Type:               "Programmed",
		Status:             metav1.ConditionTrue,
		Reason:             "Programmed",
		Message:            "ok",
		ObservedGeneration: 1,
		LastTransitionTime: metav1.NewTime(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)),
	}
	b := a
	b.LastTransitionTime = metav1.NewTime(time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC))
	if !EqualConditions([]metav1.Condition{a}, []metav1.Condition{b}) {
		t.Errorf("LastTransitionTime must not affect equality")
	}

	c := a
	c.Reason = "Other"
	if EqualConditions([]metav1.Condition{a}, []metav1.Condition{c}) {
		t.Errorf("Reason difference must affect equality")
	}
}
