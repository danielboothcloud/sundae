package openshell

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// runFilter feeds the reads to a filter as pumpInput does, stopping at the
// home key, and returns what it forwarded and whether the key was pressed.
func runFilter(k HomeKey, reads ...string) (string, bool) {
	f := &homeFilter{home: k}
	var out bytes.Buffer
	for _, r := range reads {
		fwd, home := f.filter([]byte(r))
		out.Write(fwd)
		if home {
			return out.String(), true
		}
	}
	return out.String(), false
}

func TestHomeFilter(t *testing.T) {
	const (
		ctrlBracket = "\x1d"
		csiU        = "\x1b[93;5u"
		otherKeys   = "\x1b[27;5;93~"
	)
	tests := []struct {
		name  string
		key   HomeKey
		reads []string
		out   string
		home  bool
	}{
		{"plain input passes", DefaultHomeKey, []string{"ls -l\r"}, "ls -l\r", false},
		{"home key", DefaultHomeKey, []string{"ls" + ctrlBracket}, "ls", true},
		{"acts alone: the old letters are dropped after it", DefaultHomeKey, []string{"a" + ctrlBracket + "d"}, "a", true},
		{"old letters are plain input", DefaultHomeKey, []string{"dnps?"}, "dnps?", false},
		{"input after it dropped", DefaultHomeKey, []string{ctrlBracket + "rm -rf\r"}, "", true},
		{"tilde dot passes", DefaultHomeKey, []string{"\r~."}, "\r~.", false},
		{"lone escape passes", DefaultHomeKey, []string{"\x1b"}, "\x1b", false},
		{"arrow keys pass", DefaultHomeKey, []string{"\x1b[A\x1b[1;5C"}, "\x1b[A\x1b[1;5C", false},
		{"csi-u", DefaultHomeKey, []string{"x" + csiU}, "x", true},
		{"csi-u split", DefaultHomeKey, []string{"x\x1b[93", ";5u"}, "x", true},
		{"csi-u press with event type", DefaultHomeKey, []string{"\x1b[93;5:1u"}, "", true},
		{"csi-u release passes", DefaultHomeKey, []string{"\x1b[93;5:3u"}, "\x1b[93;5:3u", false},
		{"csi-u with caps lock", DefaultHomeKey, []string{"\x1b[93;69u"}, "", true},
		{"csi-u with num lock", DefaultHomeKey, []string{"\x1b[93;133u"}, "", true},
		{"csi-u other ctrl key passes", DefaultHomeKey, []string{"\x1b[99;5u"}, "\x1b[99;5u", false},
		{"csi-u unmodified ] passes", DefaultHomeKey, []string{"\x1b[93u"}, "\x1b[93u", false},
		{"modifyOtherKeys", DefaultHomeKey, []string{otherKeys + "rest"}, "", true},
		{"modifyOtherKeys other key passes", DefaultHomeKey, []string{"\x1b[27;5;99~"}, "\x1b[27;5;99~", false},
		{"ctrl-a", HomeKey('a'), []string{"\x1d\x01"}, "\x1d", true},
		{"ctrl-a csi-u", HomeKey('a'), []string{"\x1b[97;5u"}, "", true},
		{"ctrl-@ is NUL", HomeKey('@'), []string{"\x00"}, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, home := runFilter(tt.key, tt.reads...)
			if out != tt.out || home != tt.home {
				t.Fatalf("out %q home %v, want %q %v", out, home, tt.out, tt.home)
			}
		})
	}
}

func TestHomeFilterPendingAtEOF(t *testing.T) {
	f := &homeFilter{home: DefaultHomeKey}
	if out, _ := f.filter([]byte("a\x1b[93;")); string(out) != "a" {
		t.Fatalf("out %q", out)
	}
	if p := f.pending(); string(p) != "\x1b[93;" {
		t.Fatalf("pending %q", p)
	}
}

func TestHomeKeyString(t *testing.T) {
	for k, want := range map[HomeKey]string{DefaultHomeKey: "Ctrl-]", 'a': "Ctrl-A", '\\': "Ctrl-\\", 0: ""} {
		if got := k.String(); got != want {
			t.Errorf("%q: %q, want %q", byte(k), got, want)
		}
	}
}

func TestBridgeWithoutHomeKey(t *testing.T) {
	// none: the plain copy, so Ctrl-] reaches the program.
	term := newFakeTerm()
	is := newFakeInteractive()
	done := make(chan error, 1)
	go func() {
		_, err := Bridge(context.Background(), term, is, WithHomeKey(0))
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

// TestBridgeHomeKey presses the home key in each encoding: Bridge returns 0
// and ErrHome, the program's input is closed and its attach client cancelled.
func TestBridgeHomeKey(t *testing.T) {
	for name, press := range map[string]string{
		"plain":           "\x1d",
		"csi-u":           "\x1b[93;5u",
		"csi-u caps lock": "\x1b[93;69:1u",
		"modifyOtherKeys": "\x1b[27;5;93~",
	} {
		t.Run(name, func(t *testing.T) {
			term := newFakeTerm()
			is := newFakeInteractive()
			type result struct {
				code int
				err  error
			}
			done := make(chan result, 1)
			go func() {
				code, err := Bridge(context.Background(), term, is, WithHomeKey(DefaultHomeKey))
				done <- result{code, err}
			}()
			if _, err := is.outW.Write([]byte("prompt$ ")); err != nil {
				t.Fatal(err)
			}
			eventually(t, "output", func() bool { return term.output() == "prompt$ " })

			if _, err := term.inW.Write([]byte("ls" + press + "drm\r")); err != nil {
				t.Fatal(err)
			}
			select {
			case r := <-done:
				if r.code != 0 || !errors.Is(r.err, ErrHome) {
					t.Fatalf("code %d err %v", r.code, r.err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("Bridge did not return after the home key")
			}
			in, _, closed := is.state()
			if in != "ls" || !closed || !is.isCancelled() {
				t.Fatalf("input %q closed %v cancelled %v", in, closed, is.isCancelled())
			}
			if out := term.output(); !strings.Contains(out, "\x1b[?1049l") || !strings.HasSuffix(out, "\x1b7\x1b[r\x1b8\r\n") {
				t.Fatalf("no terminal reset: %q", out)
			}
		})
	}
}
