package openshell

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"charm.land/ssh"
)

// testBar is a status bar on an 80x24 terminal with a fake clock and timer.
type testBar struct {
	*statusBar
	out   *bytes.Buffer
	clock time.Time
	// timer is the pending timer's function; nil when none.
	timer func()
}

func newTestBar(t *testing.T, enabled bool, rows int) *testBar {
	t.Helper()
	tb := &testBar{out: &bytes.Buffer{}, clock: time.Unix(1000, 0)}
	tb.statusBar = newStatusBar(tb.out, ssh.Window{Width: 80, Height: rows}, enabled, "r / main", DefaultHomeKey)
	tb.now = func() time.Time { return tb.clock }
	tb.after = func(_ time.Duration, f func()) func() bool {
		tb.timer = f
		return func() bool { tb.timer = nil; return true }
	}
	return tb
}

// take returns the output since the last call.
func (tb *testBar) take() string {
	s := tb.out.String()
	tb.out.Reset()
	return s
}

func (tb *testBar) write(t *testing.T, s string) {
	t.Helper()
	if _, err := io.WriteString(tb, s); err != nil {
		t.Fatal(err)
	}
}

// fire runs the pending timer.
func (tb *testBar) fire(t *testing.T) {
	t.Helper()
	f := tb.timer
	if f == nil {
		t.Fatal("no timer pending")
	}
	tb.timer = nil
	f()
}

// The bar on an 80x24 terminal: 10 + 53 + 17 cells.
var (
	barLine   = "\x1b[24;1H\x1b[2K\x1b[0;7m r / main " + strings.Repeat(" ", 53) + " Ctrl-] sessions \x1b[0m\x1b8"
	barRegion = "\x1b7\x1b[?6l\x1b(B\x1b[1;23r"
	barDraw   = "\x1b7\x1b[?6l\x1b(B" + barLine
)

func TestStatusBarDrawsOnAttach(t *testing.T) {
	tb := newTestBar(t, true, 24)
	tb.start()
	if got, want := tb.take(), "\n\x1b[A"+barRegion+barLine; got != want {
		t.Fatalf("start wrote %q, want %q", got, want)
	}
}

func TestStatusBarNeverInsideASequence(t *testing.T) {
	tb := newTestBar(t, true, 24)
	tb.start()
	tb.take()
	tb.clock = tb.clock.Add(time.Second)

	// A sequence split across writes: nothing is drawn until it ends.
	tb.write(t, "a\x1b[3")
	if got := tb.take(); got != "a\x1b[3" {
		t.Fatalf("drew inside a sequence: %q", got)
	}
	tb.write(t, "1mb")
	if got := tb.take(); got != "1mb"+barDraw {
		t.Fatalf("got %q", got)
	}
	// So with OSC strings and a UTF-8 character split across writes.
	tb.clock = tb.clock.Add(time.Second)
	for _, part := range []string{"\x1b]0;title", "\x1b", "\\é"[:2]} {
		tb.write(t, part)
		if got := tb.take(); got != part {
			t.Fatalf("drew after %q: %q", part, got)
		}
	}
	tb.write(t, "\\é"[2:])
	if got := tb.take(); !strings.HasSuffix(got, barDraw) {
		t.Fatalf("no redraw once the character ended: %q", got)
	}
}

func TestStatusBarDebounce(t *testing.T) {
	tb := newTestBar(t, true, 24)
	tb.start()
	tb.take()

	// Output within 100 ms of the last draw waits for the timer.
	tb.clock = tb.clock.Add(10 * time.Millisecond)
	tb.write(t, "a")
	tb.write(t, "b")
	if got := tb.take(); got != "ab" {
		t.Fatalf("redrew too soon: %q", got)
	}
	tb.clock = tb.clock.Add(100 * time.Millisecond)
	tb.fire(t)
	if got := tb.take(); got != barDraw {
		t.Fatalf("the timer drew %q", got)
	}
	// Nothing new: the timer is not armed again.
	if tb.timer != nil {
		t.Fatal("a timer is pending without output")
	}
}

func TestStatusBarRedrawsAfterClearAndScreenSwitch(t *testing.T) {
	for _, seq := range []string{"\x1b[2J", "\x1b[3J", "\x1b[?1049h", "\x1b[?1049l", "\x1b[?47h", "\x1b[?1047l", "\x1b[?25;1049h"} {
		tb := newTestBar(t, true, 24)
		tb.start()
		tb.take()
		tb.clock = tb.clock.Add(time.Millisecond)
		tb.write(t, "x"+seq)
		if got := tb.take(); got != "x"+seq+barDraw {
			t.Errorf("%q: got %q", seq, got)
		}
	}
}

func TestStatusBarScrollRegion(t *testing.T) {
	for _, tt := range []struct {
		seq, want string
	}{
		{"\x1b[r", "\x1b[r" + barRegion + barLine},
		{"\x1bc", "\x1bc" + barRegion + barLine},
		{"\x1b[1;24r", "\x1b[1;24r" + barRegion + barLine},
		{"\x1b[3r", "\x1b[3r\x1b7\x1b[?6l\x1b(B\x1b[3;23r" + barLine},
		// The program's own region, inside its rows, is left alone.
		{"\x1b[2;10r", "\x1b[2;10r"},
	} {
		tb := newTestBar(t, true, 24)
		tb.start()
		tb.take()
		tb.clock = tb.clock.Add(time.Millisecond)
		tb.write(t, tt.seq)
		if got := tb.take(); got != tt.want {
			t.Errorf("%q: got %q, want %q", tt.seq, got, tt.want)
		}
	}
}

func TestStatusBarLeave(t *testing.T) {
	tb := newTestBar(t, true, 24)
	tb.start()
	tb.take()
	tb.clock = tb.clock.Add(time.Millisecond)
	tb.write(t, "a")
	tb.leave()
	if got, want := tb.take(), "a"+modesReset+"\x1b7\x1b[r\x1b[24;1H\x1b[2K\x1b8\r\n"; got != want {
		t.Fatalf("leave wrote %q, want %q", got, want)
	}
	if tb.timer != nil {
		t.Fatal("the redraw timer is still pending")
	}
	tb.write(t, "late")
	tb.leave()
	tb.close()
	if got := tb.take(); got != "" {
		t.Fatalf("wrote after leaving: %q", got)
	}
}

func TestStatusBarOffOrTooSmall(t *testing.T) {
	for name, tb := range map[string]*testBar{
		"off":       newTestBar(t, false, 24),
		"too small": newTestBar(t, true, 4),
	} {
		tb.start()
		tb.write(t, "a\x1b[2J")
		tb.close()
		if got := tb.take(); got != "a\x1b[2J" {
			t.Errorf("%s: got %q", name, got)
		}
	}
}

func TestStatusBarResize(t *testing.T) {
	tb := newTestBar(t, true, 24)
	tb.start()
	tb.take()
	cols, rows := tb.resize(ssh.Window{Width: 100, Height: 30})
	if cols != 100 || rows != 29 {
		t.Fatalf("PTY %dx%d, want 100x29", cols, rows)
	}
	if got := tb.take(); !strings.HasPrefix(got, "\x1b7\x1b[?6l\x1b(B\x1b[1;29r\x1b[30;1H") {
		t.Fatalf("resize drew %q", got)
	}
	// Too small: the program gets every row and the region is reset.
	if cols, rows = tb.resize(ssh.Window{Width: 100, Height: 4}); rows != 4 {
		t.Fatalf("PTY %dx%d, want 100x4", cols, rows)
	}
	if got := tb.take(); got != "\x1b7\x1b[r\x1b8" {
		t.Fatalf("shrinking wrote %q", got)
	}
	tb.write(t, "\x1b[2J")
	if got := tb.take(); got != "\x1b[2J" {
		t.Fatalf("drew on a small terminal: %q", got)
	}
}

func TestRemoteSize(t *testing.T) {
	for _, tt := range []struct {
		h    int
		bar  bool
		want uint32
	}{{24, true, 23}, {24, false, 24}, {5, true, 4}, {4, true, 4}} {
		if _, rows := RemoteSize(ssh.Window{Width: 80, Height: tt.h}, tt.bar); rows != tt.want {
			t.Errorf("%d rows, bar %v: %d, want %d", tt.h, tt.bar, rows, tt.want)
		}
	}
}

func TestBarText(t *testing.T) {
	if got := barText(" a long session label ", " Ctrl-] sessions ", 25); got != " a long… Ctrl-] sessions " {
		t.Fatalf("got %q", got)
	}
	if got := barText(" a ", " Ctrl-] sessions ", 10); got != " Ctrl-] se" {
		t.Fatalf("got %q", got)
	}
}

// TestBridgeStatusBar bridges with the bar: it is drawn, the remote PTY is
// resized a row short, and it is taken down when the program exits.
func TestBridgeStatusBar(t *testing.T) {
	term := newFakeTerm()
	is := newFakeInteractive()
	done := make(chan error, 1)
	go func() {
		_, err := Bridge(context.Background(), term, is, WithHomeKey(DefaultHomeKey), WithStatusBar("r / main"))
		done <- err
	}()
	eventually(t, "bar", func() bool { return strings.Contains(term.output(), " r / main ") })
	term.winch <- ssh.Window{Width: 120, Height: 40}
	eventually(t, "resize", func() bool {
		_, rs, _ := is.state()
		return len(rs) == 1 && rs[0] == [2]uint32{120, 39}
	})
	term.winch <- ssh.Window{Width: 120, Height: 3}
	eventually(t, "small resize", func() bool {
		_, rs, _ := is.state()
		return len(rs) == 2 && rs[1] == [2]uint32{120, 3}
	})
	is.exit <- 0
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if out := term.output(); !strings.HasSuffix(out, "\x1b7\x1b[r\x1b[3;1H\x1b[2K\x1b8\r\n") {
		t.Fatalf("the bar was not taken down: %q", out)
	}
}
