package signals

import (
	"testing"

	"github.com/danielboothcloud/sundae/pkg/events"
)

func TestSuspendDeleteLifecycleSignals(t *testing.T) {
	sig := func(l events.SessionLifecycle) Signal {
		return Signal{Event: events.SessionLifecycleEvent(1, l), Last: events.StateIdle}
	}
	for _, tc := range []struct {
		l          events.SessionLifecycle
		label, msg string
		needsAttn  bool
	}{
		{events.SessionLifecycle{Step: events.LifecycleSuspended, Op: "auto"}, "○ idle · suspended", "suspended while idle; storage kept", false},
		{events.SessionLifecycle{Step: events.LifecycleSuspended, Op: "suspend"}, "○ idle · suspended", "suspended by session suspend; storage kept", false},
		{events.SessionLifecycle{Step: events.LifecycleDeleted}, "- deleted", "sandbox and storage deleted", false},
		{events.SessionLifecycle{Step: events.LifecycleFailed, Op: "delete", Reason: "forbidden"}, "✗ failed · delete", "could not delete the session: forbidden", true},
	} {
		s := sig(tc.l)
		if got := s.Label(); got != tc.label {
			t.Errorf("%+v: label %q, want %q", tc.l, got, tc.label)
		}
		if got := s.Message(0); got != tc.msg {
			t.Errorf("%+v: message %q, want %q", tc.l, got, tc.msg)
		}
		if s.NeedsAttention() != tc.needsAttn {
			t.Errorf("%+v: needs attention = %v", tc.l, s.NeedsAttention())
		}
	}
}
