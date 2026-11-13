package signals

import (
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/events"
)

func TestSnapshotLifecycleSignals(t *testing.T) {
	sig := func(l events.SessionLifecycle) Signal {
		return Signal{Event: events.SessionLifecycleEvent(1, l), Last: events.StateIdle}
	}
	for _, tc := range []struct {
		l          events.SessionLifecycle
		label, msg string
		needsAttn  bool
	}{
		{events.SessionLifecycle{Step: events.LifecycleSnapshotted, Op: "end", Bytes: 3 << 20}, "○ idle · snapshotted", "/sandbox saved to S3 before the session ended (3.0 MiB)", false},
		{events.SessionLifecycle{Step: events.LifecycleSnapshotted, Op: "park", Bytes: 10}, "○ idle · snapshotted", "/sandbox saved to S3 before the session was parked (10 B)", false},
		{events.SessionLifecycle{Step: events.LifecycleRestored, Op: "start", Bytes: 2048}, "○ idle · restored", "/sandbox restored from its S3 snapshot (2.0 KiB)", false},
		{events.SessionLifecycle{Step: events.LifecycleParked, Op: "auto", Snapshot: true}, "○ idle · parked (snapshot)", "parked while idle to an S3 snapshot: the sandbox was deleted and the program stopped; attach restarts it", false},
		{events.SessionLifecycle{Step: events.LifecycleParked, Op: "auto"}, "○ idle · parked", "parked while idle", false},
		{events.SessionLifecycle{Step: events.LifecycleFailed, Op: "snapshot", Reason: "HTTP 403"}, "✗ failed · snapshot", "could not snapshot the session: HTTP 403", true},
		{events.SessionLifecycle{Step: events.LifecycleFailed, Op: "restore", Reason: "decompress"}, "✗ failed · restore", "could not restore the session: decompress", true},
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
