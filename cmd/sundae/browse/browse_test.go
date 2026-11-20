package browse

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/danielboothcloud/sundae/git"
	"github.com/danielboothcloud/sundae/pkg/config"
	"github.com/danielboothcloud/sundae/pkg/ui/common"
)

// run runs cmd and feeds what it returns to m, following batches, for up
// to depth rounds; a command still running after 2s (a ticking spinner) is
// left, which gives the repository's reads room on a loaded machine.
func run(m *model, cmd tea.Cmd, depth int) {
	if cmd == nil || depth == 0 {
		return
	}
	done := make(chan tea.Msg, 1)
	go func() { done <- cmd() }()
	var msg tea.Msg
	select {
	case msg = <-done:
	case <-time.After(2 * time.Second):
		return
	}
	switch msg := msg.(type) {
	case nil:
	case tea.BatchMsg:
		for _, c := range msg {
			run(m, c, depth)
		}
	default:
		_, next := m.Update(msg)
		run(m, next, depth-1)
	}
}

func screen(m *model) []string {
	lines := strings.Split(ansi.Strip(m.View().Content), "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " ")
	}
	return lines
}

// TestBrowseFooter runs `sundae browse` on a repository and checks it draws
// the server's footer: an empty message slot, then the ranked help line,
// and ? opens the full help without cutting a key.
func TestBrowseFooter(t *testing.T) {
	dir := t.TempDir()
	gitCmd := func(args ...string) {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
			"GIT_AUTHOR_NAME=T", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=T", "GIT_COMMITTER_EMAIL=t@example.com")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	gitCmd("init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# demo\n\nBrowsed locally.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitCmd("add", "-A")
	gitCmd("commit", "-q", "-m", "first")
	r, err := git.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []int{80, 50} {
		// As main does: the command's context holds the config.
		ctx := config.WithContext(context.Background(), config.DefaultConfig())
		m := newModel(common.NewCommon(ctx, w, 24), r)
		t.Cleanup(m.common.Zone.Close)
		run(m, m.Init(), 4)
		_, cmd := m.Update(tea.WindowSizeMsg{Width: w, Height: 24})
		run(m, cmd, 4)
		lines := screen(m)
		// The bottom margin, the help, the message slot, the footer's margin.
		n := len(lines)
		if n != 24 || lines[n-1] != "" || lines[n-3] != "" || lines[n-4] != "" {
			t.Fatalf("%d: footer:\n%s", w, strings.Join(lines, "\n"))
		}
		if help := lines[n-2]; !strings.HasPrefix(help, "   j/k move") || !strings.HasSuffix(help, "? more") || strings.Contains(help, "…") {
			t.Fatalf("%d: help line %q", w, help)
		}
		if s := strings.Join(lines, "\n"); !strings.Contains(s, "Browsed locally.") {
			t.Fatalf("%d: readme:\n%s", w, s)
		}
		_, cmd = m.Update(tea.KeyPressMsg{Code: '?', Text: "?"})
		run(m, cmd, 2)
		full := strings.Join(screen(m), "\n")
		if !regexp.MustCompile(`\?\s+less`).MatchString(full) || strings.Contains(full, "…") {
			t.Fatalf("%d: full help:\n%s", w, full)
		}
	}
}
