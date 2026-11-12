package ssh

import (
	"context"
	"io"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/ssh"
)

func newTestSwitch() (*terminalSwitch, *io.PipeWriter, chan tea.Msg) {
	r, w := io.Pipe()
	msgs := make(chan tea.Msg, 8)
	t := &terminalSwitch{
		src:     r,
		out:     io.Discard,
		pty:     ssh.Pty{Term: "xterm", Window: ssh.Window{Width: 80, Height: 24}},
		win:     ssh.Window{Width: 80, Height: 24},
		changed: make(chan struct{}),
		tuiWin:  make(chan ssh.Window, 1),
	}
	t.setSend(func(m tea.Msg) { msgs <- m })
	return t, w, msgs
}

func readString(t *testing.T, r io.Reader) string {
	t.Helper()
	buf := make([]byte, 64)
	n, err := r.Read(buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return string(buf[:n])
}

func TestTerminalSwitchHandsInputOver(t *testing.T) {
	sw, w, msgs := newTestSwitch()
	tui := tuiReader{sw}
	go func() { _, _ = w.Write([]byte("a")) }()
	if got := readString(t, tui); got != "a" {
		t.Fatalf("TUI read %q", got)
	}

	term, release := sw.Take()
	// The TUI's read returns empty while the attach holds the input, so
	// Bubble Tea's cancelled reader can notice and stop.
	start := time.Now()
	if n, err := tui.Read(make([]byte, 8)); n != 0 || err != nil {
		t.Fatalf("paused TUI read = %d, %v", n, err)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("paused TUI read took %s", d)
	}
	go func() { _, _ = w.Write([]byte("b")) }()
	if got := readString(t, term); got != "b" {
		t.Fatalf("attach read %q", got)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	winch := make(chan ssh.Window)
	go sw.routeWindows(ctx, winch)
	go sw.sendWindows(ctx)
	winch <- ssh.Window{Width: 100, Height: 30}
	_, attachWin, _ := term.Pty()
	if got := <-attachWin; got.Width != 100 {
		t.Fatalf("attach window %v", got)
	}
	if pty, _, _ := term.Pty(); pty.Window.Width != 100 || pty.Term != "xterm" {
		t.Fatalf("attach pty %+v", pty)
	}

	// A read in progress when the attach ends returns EOF, and the
	// input goes back to the TUI, with the window size.
	done := make(chan error, 1)
	go func() {
		_, err := term.Read(make([]byte, 8))
		done <- err
	}()
	time.Sleep(20 * time.Millisecond)
	release()
	release()
	if err := <-done; err != io.EOF {
		t.Fatalf("attach read after release: %v", err)
	}
	select {
	case m := <-msgs:
		if ws, ok := m.(tea.WindowSizeMsg); !ok || ws.Width != 100 || ws.Height != 30 {
			t.Fatalf("TUI got %#v", m)
		}
	case <-time.After(time.Second):
		t.Fatal("the TUI was not told the window size")
	}
	go func() { _, _ = w.Write([]byte("c")) }()
	if got := readString(t, tui); got != "c" {
		t.Fatalf("TUI read %q after release", got)
	}

	// Window changes go to the TUI again.
	winch <- ssh.Window{Width: 90, Height: 20}
	select {
	case m := <-msgs:
		if ws, ok := m.(tea.WindowSizeMsg); !ok || ws.Width != 90 {
			t.Fatalf("TUI got %#v", m)
		}
	case <-time.After(time.Second):
		t.Fatal("window change did not reach the TUI")
	}

	// Closing ends reads, and the pump with the channel.
	sw.Close()
	if _, err := tui.Read(make([]byte, 8)); err != io.EOF {
		t.Fatalf("read after close: %v", err)
	}
	_ = w.Close()
}

func TestTerminalSwitchSourceError(t *testing.T) {
	sw, w, _ := newTestSwitch()
	go func() {
		_, _ = w.Write([]byte("x"))
		_ = w.Close()
	}()
	tui := tuiReader{sw}
	if got := readString(t, tui); got != "x" {
		t.Fatalf("read %q", got)
	}
	if _, err := tui.Read(make([]byte, 8)); err != io.EOF {
		t.Fatalf("read after the channel closed: %v", err)
	}
}
