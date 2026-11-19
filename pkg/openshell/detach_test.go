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
	f := newPrefixFilter(k)
	var out bytes.Buffer
	var acts []action
	for _, r := range reads {
		data := []byte(r)
		for len(data) > 0 {
			fwd, act, rest := f.filter(data)
			out.Write(fwd)
			if act != actionNone {
				acts = append(acts, act)
				if act == actionDetach {
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
