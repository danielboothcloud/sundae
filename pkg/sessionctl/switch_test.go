package sessionctl

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/danielboothcloud/sundae/pkg/openshell"
)

func TestSwitchTarget(t *testing.T) {
	next, prev := openshell.SwitchNext, openshell.SwitchPrev
	tests := []struct {
		names   []string
		current string
		dir     openshell.SwitchDir
		want    string
		ok      bool
	}{
		{[]string{"c", "a", "b"}, "b", next, "c", true},
		{[]string{"c", "a", "b"}, "b", prev, "a", true},
		{[]string{"a", "b", "c"}, "c", next, "a", true},
		{[]string{"a", "b", "c"}, "a", prev, "c", true},
		{[]string{"a", "b"}, "a", next, "b", true},
		{[]string{"a", "b"}, "a", prev, "b", true},
		{[]string{"a"}, "a", next, "", false},
		{nil, "a", prev, "", false},
		// The current session is gone: its neighbours by name.
		{[]string{"a", "c"}, "b", next, "c", true},
		{[]string{"a", "c"}, "b", prev, "a", true},
		{[]string{"a", "c"}, "d", next, "a", true},
		{[]string{"x"}, "a", prev, "x", true},
	}
	for _, tt := range tests {
		got, ok := SwitchTarget(tt.names, tt.current, tt.dir)
		if got != tt.want || ok != tt.ok {
			t.Errorf("%v from %q dir %d: %q %v, want %q %v", tt.names, tt.current, tt.dir, got, ok, tt.want, tt.ok)
		}
	}
}

// scriptAttacher returns its scripted results, one per Attach.
type scriptAttacher struct {
	sess    *openshell.Session
	results []error
	log     *[]string
	closed  int
	opts    int
}

func (a *scriptAttacher) Session() *openshell.Session { return a.sess }

func (a *scriptAttacher) Attach(_ context.Context, _ openshell.Terminal, opts ...openshell.BridgeOption) (int, error) {
	*a.log = append(*a.log, "attach "+a.sess.Name)
	a.opts = len(opts)
	if len(a.results) == 0 {
		return 7, nil
	}
	err := a.results[0]
	a.results = a.results[1:]
	return 0, err
}

func (a *scriptAttacher) Close() {
	a.closed++
	*a.log = append(*a.log, "close "+a.sess.Name)
}

// switchFixture is a Switcher over scripted sessions.
type switchFixture struct {
	log      []string
	sessions map[string]*scriptAttacher
	prepErr  map[string]error
}

func newSwitchFixture(results map[string][]error) *switchFixture {
	f := &switchFixture{sessions: map[string]*scriptAttacher{}, prepErr: map[string]error{}}
	for name, r := range results {
		f.sessions[name] = &scriptAttacher{sess: &openshell.Session{Name: name, Repos: []string{"r"}}, results: r, log: &f.log}
	}
	return f
}

func (f *switchFixture) switcher() *Switcher {
	return &Switcher{
		Names: func(context.Context) ([]string, error) {
			names := make([]string, 0, len(f.sessions))
			for n := range f.sessions {
				names = append(names, n)
			}
			return names, nil
		},
		Prepare: func(_ context.Context, name string, progress io.Writer) (Attacher, error) {
			_, _ = io.WriteString(progress, "Resuming sandbox "+name+"...\n")
			f.log = append(f.log, "prepare "+name)
			if err := f.prepErr[name]; err != nil {
				return nil, err
			}
			return f.sessions[name], nil
		},
		Prefix: openshell.DefaultPrefixKey,
	}
}

var (
	next   = &openshell.SwitchRequest{Dir: openshell.SwitchNext}
	prev   = &openshell.SwitchRequest{Dir: openshell.SwitchPrev}
	pick   = &openshell.SwitchRequest{Dir: openshell.SwitchPick}
	detach = openshell.ErrDetached
)

func TestSwitcherNextThenDetach(t *testing.T) {
	f := newSwitchFixture(map[string][]error{"a": {next}, "b": {next}, "c": {detach}})
	term := &detachTerm{in: strings.NewReader("")}
	last, code, err := f.switcher().Run(context.Background(), term, f.sessions["a"])
	if last.Name != "c" || code != 0 || !errors.Is(err, detach) {
		t.Fatalf("last %s code %d err %v", last.Name, code, err)
	}
	want := "attach a,prepare b,close a,attach b,prepare c,close b,attach c,close c"
	if got := strings.Join(f.log, ","); got != want {
		t.Fatalf("log %s", got)
	}
	out := term.out.String()
	if !strings.Contains(out, "Resuming sandbox b...\r\n[sundae] switched to r / b. Detach: Ctrl-] d (or ~. after a newline).\r\n") {
		t.Fatalf("output %q", out)
	}
	if f.sessions["a"].opts != 1 {
		t.Fatal("switching was not enabled")
	}
}

func TestSwitcherExitCodeOfLastSession(t *testing.T) {
	f := newSwitchFixture(map[string][]error{"a": {prev}, "b": nil})
	last, code, err := f.switcher().Run(context.Background(), &detachTerm{in: strings.NewReader("")}, f.sessions["a"])
	if last.Name != "b" || code != 7 || err != nil {
		t.Fatalf("last %s code %d err %v", last.Name, code, err)
	}
	if f.sessions["a"].closed == 0 || f.sessions["b"].closed == 0 {
		t.Fatal("an Attacher was not closed")
	}
}

func TestSwitcherFallsBack(t *testing.T) {
	f := newSwitchFixture(map[string][]error{"a": {next, detach}, "b": nil})
	f.prepErr["b"] = errors.New("boom")
	term := &detachTerm{in: strings.NewReader("")}
	last, _, err := f.switcher().Run(context.Background(), term, f.sessions["a"])
	if last.Name != "a" || !errors.Is(err, detach) {
		t.Fatalf("last %s err %v", last.Name, err)
	}
	if got := strings.Join(f.log, ","); got != "attach a,prepare b,attach a,close a" {
		t.Fatalf("log %s", got)
	}
	if !strings.Contains(term.out.String(), "[sundae] cannot switch to b: boom\r\n") {
		t.Fatalf("output %q", term.out.String())
	}

	// Attaching the previous session again fails too: that is the error.
	lost := errors.New("lost")
	f = newSwitchFixture(map[string][]error{"a": {next, lost}, "b": nil})
	f.prepErr["b"] = errors.New("boom")
	if _, _, err := f.switcher().Run(context.Background(), term, f.sessions["a"]); !errors.Is(err, lost) {
		t.Fatalf("err %v", err)
	}
}

func TestSwitcherNoOtherSession(t *testing.T) {
	f := newSwitchFixture(map[string][]error{"a": {next, detach}})
	term := &detachTerm{in: strings.NewReader("")}
	if _, _, err := f.switcher().Run(context.Background(), term, f.sessions["a"]); !errors.Is(err, detach) {
		t.Fatal(err)
	}
	if !strings.Contains(term.out.String(), "[sundae] no other session") || strings.Contains(strings.Join(f.log, ","), "prepare") {
		t.Fatalf("output %q log %v", term.out.String(), f.log)
	}
}

func TestSwitcherPick(t *testing.T) {
	// Without Pick, s ends Run for the caller's picker.
	f := newSwitchFixture(map[string][]error{"a": {pick}, "b": nil})
	last, _, err := f.switcher().Run(context.Background(), &detachTerm{in: strings.NewReader("")}, f.sessions["a"])
	var sw *openshell.SwitchRequest
	if last.Name != "a" || !errors.As(err, &sw) || sw.Dir != openshell.SwitchPick {
		t.Fatalf("last %s err %v", last.Name, err)
	}

	// With Pick: going back to the current session re-attaches it unprepared.
	f = newSwitchFixture(map[string][]error{"a": {pick, pick, detach}, "b": {detach}})
	sw2 := f.switcher()
	picks := []string{"a", "b"}
	sw2.Pick = func(_ context.Context, _ openshell.Terminal, current string) (string, error) {
		if current != "a" {
			t.Errorf("current %q", current)
		}
		p := picks[0]
		picks = picks[1:]
		return p, nil
	}
	last, _, err = sw2.Run(context.Background(), &detachTerm{in: strings.NewReader("")}, f.sessions["a"])
	if last.Name != "b" || !errors.Is(err, detach) {
		t.Fatalf("last %s err %v", last.Name, err)
	}
	if got := strings.Join(f.log, ","); got != "attach a,attach a,prepare b,close a,attach b,close b" {
		t.Fatalf("log %s", got)
	}
}

func TestZeroSwitcherAttachesOnce(t *testing.T) {
	f := newSwitchFixture(map[string][]error{"a": {next}})
	var sw Switcher
	_, _, err := sw.Run(context.Background(), &detachTerm{in: strings.NewReader("")}, f.sessions["a"])
	if !errors.Is(err, next) || f.sessions["a"].opts != 0 || f.sessions["a"].closed != 1 {
		t.Fatalf("err %v opts %d closed %d", err, f.sessions["a"].opts, f.sessions["a"].closed)
	}
}
