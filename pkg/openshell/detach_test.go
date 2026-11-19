package openshell

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

// runFilter feeds the reads to a filter as pumpInput does, stopping at a
// detach, and returns what it forwarded and the actions in order.
func runFilter(k PrefixKey, reads ...string) (string, []action) {
	return runSwitchFilter(k, false, reads...)
}

// runSwitchFilter is runFilter with the switch keys bound or not; a switch
// stops it as a detach does.
func runSwitchFilter(k PrefixKey, switching bool, reads ...string) (string, []action) {
	f := newPrefixFilter(k)
	f.switching = switching
	var out bytes.Buffer
	var acts []action
	for _, r := range reads {
		data := []byte(r)
		for len(data) > 0 {
			fwd, act, rest := f.filter(data)
			out.Write(fwd)
			if act != actionNone {
				acts = append(acts, act)
				if act != actionHelp {
					return out.String(), acts
				}
			}
			data = rest
		}
	}
	return out.String(), acts
}

func TestPrefixFilter(t *testing.T) {
	const (
		ctrlBracket = "\x1d"
		csiU        = "\x1b[93;5u"
		otherKeys   = "\x1b[27;5;93~"
	)
	tests := []struct {
		name  string
		key   PrefixKey
		reads []string
		out   string
		acts  []action
	}{
		{"plain input passes", DefaultPrefixKey, []string{"ls -l\r"}, "ls -l\r", nil},
		{"prefix d detaches", DefaultPrefixKey, []string{"ls" + ctrlBracket + "d"}, "ls", []action{actionDetach}},
		{"split across reads", DefaultPrefixKey, []string{"a" + ctrlBracket, "d"}, "a", []action{actionDetach}},
		{"input after detach dropped", DefaultPrefixKey, []string{ctrlBracket + "drm -rf\r"}, "", []action{actionDetach}},
		{"prefix twice sends it once", DefaultPrefixKey, []string{ctrlBracket + ctrlBracket + "x"}, ctrlBracket + "x", nil},
		{"unbound key passes with prefix", DefaultPrefixKey, []string{ctrlBracket + "x", "y"}, ctrlBracket + "xy", nil},
		{"help continues", DefaultPrefixKey, []string{"a" + ctrlBracket + "?b"}, "ab", []action{actionHelp}},
		{"lone prefix stays pending", DefaultPrefixKey, []string{"a" + ctrlBracket}, "a", nil},
		{"tilde dot passes", DefaultPrefixKey, []string{"\r~."}, "\r~.", nil},
		{"lone escape passes", DefaultPrefixKey, []string{"\x1b"}, "\x1b", nil},
		{"arrow keys pass", DefaultPrefixKey, []string{"\x1b[A\x1b[1;5C"}, "\x1b[A\x1b[1;5C", nil},
		{"csi-u prefix", DefaultPrefixKey, []string{csiU + "d"}, "", []action{actionDetach}},
		{"csi-u prefix split", DefaultPrefixKey, []string{"x\x1b[93", ";5u", "d"}, "x", []action{actionDetach}},
		{"csi-u d", DefaultPrefixKey, []string{csiU + "\x1b[100u"}, "", []action{actionDetach}},
		{"csi-u releases ignored", DefaultPrefixKey, []string{"\x1b[93;5:1u\x1b[93;5:3u\x1b[100;1:1u"}, "", []action{actionDetach}},
		{"csi-u prefix twice", DefaultPrefixKey, []string{csiU + csiU}, csiU, nil},
		{"csi-u with caps lock", DefaultPrefixKey, []string{"\x1b[93;69u" + "d"}, "", []action{actionDetach}},
		{"csi-u shifted help", DefaultPrefixKey, []string{csiU + "\x1b[47:63;2u"}, "", []action{actionHelp}},
		{"csi-u other ctrl key passes", DefaultPrefixKey, []string{"\x1b[99;5u"}, "\x1b[99;5u", nil},
		{"modifyOtherKeys prefix", DefaultPrefixKey, []string{otherKeys + "d"}, "", []action{actionDetach}},
		{"modifyOtherKeys unbound", DefaultPrefixKey, []string{otherKeys + "\x1b[27;5;99~"}, otherKeys + "\x1b[27;5;99~", nil},
		{"ctrl-a", PrefixKey('a'), []string{"\x1d\x01d"}, "\x1d", []action{actionDetach}},
		{"ctrl-a csi-u", PrefixKey('a'), []string{"\x1b[97;5ud"}, "", []action{actionDetach}},
		{"ctrl-@ is NUL", PrefixKey('@'), []string{"\x00d"}, "", []action{actionDetach}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, acts := runFilter(tt.key, tt.reads...)
			if out != tt.out || !equalActions(acts, tt.acts) {
				t.Fatalf("out %q acts %v, want %q %v", out, acts, tt.out, tt.acts)
			}
		})
	}
}

func TestPrefixFilterSwitchKeys(t *testing.T) {
	const ctrlBracket, csiU = "\x1d", "\x1b[93;5u"
	tests := []struct {
		name      string
		switching bool
		reads     []string
		out       string
		acts      []action
	}{
		{"n", true, []string{"a" + ctrlBracket + "nrest"}, "a", []action{actionNext}},
		{"p", true, []string{ctrlBracket + "p"}, "", []action{actionPrev}},
		{"s", true, []string{ctrlBracket, "s"}, "", []action{actionPick}},
		{"csi-u n", true, []string{csiU + "\x1b[110u"}, "", []action{actionNext}},
		{"csi-u p", true, []string{csiU + "\x1b[112;1:1u"}, "", []action{actionPrev}},
		{"csi-u s", true, []string{csiU + "\x1b[115u"}, "", []action{actionPick}},
		{"modifyOtherKeys s", true, []string{"\x1b[27;5;93~s"}, "", []action{actionPick}},
		{"unbound without switching", false, []string{ctrlBracket + "n" + ctrlBracket + "s"}, ctrlBracket + "n" + ctrlBracket + "s", nil},
		{"shifted N unbound", true, []string{ctrlBracket + "N"}, ctrlBracket + "N", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, acts := runSwitchFilter(DefaultPrefixKey, tt.switching, tt.reads...)
			if out != tt.out || !equalActions(acts, tt.acts) {
				t.Fatalf("out %q acts %v, want %q %v", out, acts, tt.out, tt.acts)
			}
		})
	}
}

func TestBridgeSwitch(t *testing.T) {
	for _, tt := range []struct {
		key string
		dir SwitchDir
	}{{"n", SwitchNext}, {"p", SwitchPrev}, {"s", SwitchPick}} {
		t.Run(tt.key, func(t *testing.T) {
			term := newFakeTerm()
			is := newFakeInteractive()
			others := make(chan bool, 2)
			others <- false
			others <- true
			done := make(chan error, 1)
			go func() {
				code, err := Bridge(context.Background(), term, is, WithPrefixKey(DefaultPrefixKey),
					WithSwitching(func() bool { return <-others }))
				if code != 0 {
					err = errors.Join(err, errors.New("code not 0"))
				}
				done <- err
			}()
			if tt.dir != SwitchPick {
				// No other session: say so and stay attached.
				if _, err := term.inW.Write([]byte("a\x1d" + tt.key)); err != nil {
					t.Fatal(err)
				}
				eventually(t, "notice", func() bool { return strings.Contains(term.output(), "[sundae] no other session") })
			}
			if _, err := term.inW.Write([]byte("b\x1d" + tt.key + "late")); err != nil {
				t.Fatal(err)
			}
			var err error
			select {
			case err = <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("Bridge did not return after the switch")
			}
			var sw *SwitchRequest
			if !errors.As(err, &sw) || sw.Dir != tt.dir || errors.Is(err, ErrDetached) {
				t.Fatalf("err %v, want a switch %d", err, tt.dir)
			}
			in, _, closed := is.state()
			want := "b"
			if tt.dir != SwitchPick {
				want = "ab"
			}
			if in != want || !closed || !is.isCancelled() {
				t.Fatalf("input %q closed %v cancelled %v", in, closed, is.isCancelled())
			}
			if !strings.HasSuffix(term.output(), terminalReset) {
				t.Fatalf("no terminal reset: %q", term.output())
			}
		})
	}
}

func TestPrefixHelp(t *testing.T) {
	if h := prefixHelp(DefaultPrefixKey, true); !strings.Contains(h, "Ctrl-] n/p next/previous session · Ctrl-] s choose a session") {
		t.Fatalf("help %q", h)
	}
	if h := prefixHelp(DefaultPrefixKey, false); strings.Contains(h, "n/p") {
		t.Fatalf("help %q", h)
	}
}

func equalActions(a, b []action) bool {
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

func TestPrefixFilterPendingAtEOF(t *testing.T) {
	f := newPrefixFilter(DefaultPrefixKey)
	if out, _, _ := f.filter([]byte("a\x1d")); string(out) != "a" {
		t.Fatalf("out %q", out)
	}
	if p := f.pending(); string(p) != "\x1d" {
		t.Fatalf("pending %q", p)
	}
	if out, _, _ := f.filter([]byte("\x1b[93;")); len(out) != 0 {
		t.Fatalf("out %q", out)
	}
	if p := f.pending(); string(p) != "\x1b[93;" {
		t.Fatalf("pending %q", p)
	}
}

func TestPrefixKeyString(t *testing.T) {
	for k, want := range map[PrefixKey]string{DefaultPrefixKey: "Ctrl-]", 'a': "Ctrl-A", '\\': "Ctrl-\\", 0: ""} {
		if got := k.String(); got != want {
			t.Errorf("%q: %q, want %q", byte(k), got, want)
		}
	}
}

func TestPumpInputWithoutPrefixKey(t *testing.T) {
	// none: the plain copy, so Ctrl-] reaches the program.
	term := newFakeTerm()
	is := newFakeInteractive()
	done := make(chan error, 1)
	go func() {
		_, err := Bridge(context.Background(), term, is, WithPrefixKey(0))
		done <- err
	}()
	if _, err := term.inW.Write([]byte("\x1dd")); err != nil {
		t.Fatal(err)
	}
	eventually(t, "input", func() bool { in, _, _ := is.state(); return in == "\x1dd" })
	is.exit <- 0
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestBridgeDetach(t *testing.T) {
	term := newFakeTerm()
	is := newFakeInteractive()
	type result struct {
		code int
		err  error
	}
	done := make(chan result, 1)
	go func() {
		code, err := Bridge(context.Background(), term, is, WithPrefixKey(DefaultPrefixKey))
		done <- result{code, err}
	}()

	if _, err := is.outW.Write([]byte("prompt$ ")); err != nil {
		t.Fatal(err)
	}
	eventually(t, "output", func() bool { return term.output() == "prompt$ " })

	// Help is shown and the session continues.
	if _, err := term.inW.Write([]byte("ls\x1d?")); err != nil {
		t.Fatal(err)
	}
	eventually(t, "help", func() bool { return strings.Contains(term.output(), "[sundae] Ctrl-] d detach") })

	// Detach: input after it is dropped, the client is cancelled.
	if _, err := term.inW.Write([]byte("\x1ddrm\r")); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-done:
		if r.code != 0 || !errors.Is(r.err, ErrDetached) {
			t.Fatalf("code %d err %v", r.code, r.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Bridge did not return after detach")
	}
	in, _, closed := is.state()
	if in != "ls" || !closed {
		t.Fatalf("input %q closed %v", in, closed)
	}
	if !strings.HasSuffix(term.output(), terminalReset) {
		t.Fatalf("no terminal reset: %q", term.output())
	}
}

func TestBridgeOutputMute(t *testing.T) {
	var b bytes.Buffer
	o := &bridgeOutput{w: &b}
	_, _ = io.WriteString(o, "a")
	o.mute("reset")
	_, _ = io.WriteString(o, "late")
	o.mute("again")
	if b.String() != "areset" {
		t.Fatalf("output %q", b.String())
	}
}
