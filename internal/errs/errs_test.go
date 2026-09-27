package errs

import (
	stdlibErrors "errors"
	"fmt"
	"testing"
	"unicode/utf8"
)

func TestStatusReasonNilIsProgrammedTrue(t *testing.T) {
	m := StatusReason(nil)
	if m.Type != ConditionProgrammed || m.Status != StatusTrue || m.Reason != ReasonProgrammed {
		t.Errorf("nil err -> %+v, want Programmed/True/Programmed", m)
	}
}

func TestStatusReasonTypedMapping(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		wantT  ConditionType
		wantS  Status
		wantR  Reason
		wantIn string // substring required in message
	}{
		{
			name:   "include missing -> Accepted False Invalid",
			err:    IncludeMissing("/etc/nginx/conf.d/k8s-gw", stdlibErrors.New("no effective include")),
			wantT:  ConditionAccepted,
			wantS:  StatusFalse,
			wantR:  ReasonInvalid,
			wantIn: "/etc/nginx/conf.d/k8s-gw",
		},
		{
			name:   "reload failure -> Programmed False Invalid",
			err:    Reload("nginx -t returned non-zero: syntax error in /tmp/x.conf:3", nil),
			wantT:  ConditionProgrammed,
			wantS:  StatusFalse,
			wantR:  ReasonInvalid,
			wantIn: "syntax error",
		},
		{
			name:   "nginx not running -> Programmed False Pending",
			err:    NginxNotRunning("pid file /run/nginx.pid missing", nil),
			wantT:  ConditionProgrammed,
			wantS:  StatusFalse,
			wantR:  ReasonPending,
			wantIn: "/run/nginx.pid",
		},
		{
			name:   "validation -> Accepted False Invalid",
			err:    Validation("spec.listeners[0].port is required", nil),
			wantT:  ConditionAccepted,
			wantS:  StatusFalse,
			wantR:  ReasonInvalid,
			wantIn: "spec.listeners[0].port",
		},
		{
			name:   "unknown error -> Programmed False Invalid (defensive)",
			err:    stdlibErrors.New("something exploded"),
			wantT:  ConditionProgrammed,
			wantS:  StatusFalse,
			wantR:  ReasonInvalid,
			wantIn: "something exploded",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := StatusReason(tc.err)
			if m.Type != tc.wantT || m.Status != tc.wantS || m.Reason != tc.wantR {
				t.Errorf("got %+v, want type=%s status=%s reason=%s",
					m, tc.wantT, tc.wantS, tc.wantR)
			}
			if tc.wantIn != "" && !contains(m.Message, tc.wantIn) {
				t.Errorf("message %q missing %q", m.Message, tc.wantIn)
			}
		})
	}
}

func TestStatusReasonWalksWrapChain(t *testing.T) {
	// Simulate the provider lane wrapping a typed error through %w layers.
	root := Reload("nginx -t failed", fmt.Errorf("exit code 1"))
	wrapped := fmt.Errorf("reconcile gateway default/foo: %w", root)

	m := StatusReason(wrapped)
	if m.Type != ConditionProgrammed || m.Reason != ReasonInvalid {
		t.Errorf("wrapped reload: got %+v", m)
	}
	if !contains(m.Message, "nginx -t failed") {
		t.Errorf("message lost root detail: %q", m.Message)
	}
}

func TestSentinelsMatchThroughWrap(t *testing.T) {
	// errors.Is must match the sentinel even when wrapped.
	err := fmt.Errorf("outer: %w", Reload("detail", nil))
	for _, s := range []error{ErrReload, ErrValidation, ErrNginxNotRunning, ErrIncludeMissing} {
		_ = s // we only need to assert Is(err, ErrReload) below; this loop
		// documents the contract for all four.
	}
	if !stdlibErrors.Is(err, ErrReload) {
		t.Errorf("errors.Is should match ErrReload through %%w wrap")
	}
	if stdlibErrors.Is(err, ErrValidation) {
		t.Errorf("ErrValidation should not match a reload wrap")
	}
}

func TestTruncateCapsLongMessages(t *testing.T) {
	long := make([]byte, 4096)
	for i := range long {
		long[i] = 'x'
	}
	m := StatusReason(Reload(string(long), nil))
	// Use rune count rather than byte length so a multi-byte ellipsis
	// doesn't trip us up.
	if utf8.RuneCountInString(m.Message) > maxMessageLen {
		t.Errorf("truncate failed: got %d runes", utf8.RuneCountInString(m.Message))
	}
	runes := []rune(m.Message)
	if runes[len(runes)-1] != '…' {
		t.Errorf("truncated message should end with ellipsis, got last rune %q", runes[len(runes)-1])
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	// Simple substring search — keeps the test file dep-free.
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
