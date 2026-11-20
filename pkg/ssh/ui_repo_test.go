package ssh

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/danielboothcloud/sundae/pkg/ui/components/footer"
)

// readmeLines makes the readme longer than the screen, so it scrolls.
var readmeLines = func() string {
	var b strings.Builder
	b.WriteString("# sundae\n\nA git home and an agent cockpit.\n\n")
	for i := 1; i <= 30; i++ {
		fmt.Fprintf(&b, "- note %d\n", i)
	}
	return b.String()
}()

// commitContent commits a readme, then a Go file in pkg/ui, to the bare
// repository name on main, with fixed authors and dates; written in place,
// so no server hook runs.
func commitContent(t *testing.T, ui *UI, name string) {
	t.Helper()
	ctx := ui.common.Context()
	r, err := ui.common.Backend().Repository(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	gr, err := r.Open()
	if err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "git", append([]string{"--git-dir", gr.Path, "--work-tree", work}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
			"GIT_AUTHOR_NAME=Daniel", "GIT_AUTHOR_EMAIL=daniel@example.com",
			"GIT_COMMITTER_NAME=Daniel", "GIT_COMMITTER_EMAIL=daniel@example.com",
			"GIT_AUTHOR_DATE=2001-02-03T04:05:06Z", "GIT_COMMITTER_DATE=2001-02-03T04:05:06Z")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	write := func(path, content string) {
		t.Helper()
		p := filepath.Join(work, path)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil { //nolint:gosec
			t.Fatal(err)
		}
	}
	git("symbolic-ref", "HEAD", "refs/heads/main")
	write("README.md", readmeLines)
	git("add", "-A")
	git("commit", "-q", "-m", "Add the readme")
	var src strings.Builder
	src.WriteString("package ui\n\n// Page is a page of the TUI.\ntype Page int\n")
	for i := 1; i <= 40; i++ {
		fmt.Fprintf(&src, "\n// Line%d is line %d.\nconst Line%d = %d\n", i, i, i, i)
	}
	write("pkg/ui/repo.go", src.String())
	git("add", "-A")
	git("commit", "-q", "-m", "Add the repository page")
}

// contentUI is the rich home page with sundae holding commits, opened on
// its Readme tab.
func contentUI(t *testing.T, w int) *UI {
	t.Helper()
	ui, _, _ := richHomeUI(t, w, 24)
	commitContent(t, ui, "sundae")
	uiKeyDepth(ui, 6, "enter") // sundae
	settle(t, ui)
	return ui
}

// waitRow waits for a screen line ending in suffix, such as a file's row.
func waitRow(t *testing.T, ui *UI, suffix string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		for _, l := range strings.Split(screenText(ui), "\n") {
			if strings.HasSuffix(l, suffix) {
				return
			}
		}
		drainLate(ui)
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no line ends in %q:\n%s", suffix, screenText(ui))
}

// activeTab is the raw rendering of the active Sessions tab: its marker
// and name, with their colours.
func activeTab(raw []string) string {
	for _, l := range raw {
		plain := ansi.Strip(l)
		if !strings.Contains(plain, "• Sessions") && !strings.Contains(plain, "‹ Sessions ›") {
			continue
		}
		m := strings.Index(l, "•")
		if m < 0 {
			m = strings.Index(l, "‹")
		}
		start := strings.LastIndex(l[:m], "\x1b")
		end := m + strings.Index(l[m:], "Sessions") + len("Sessions")
		if j := strings.Index(l[end:], "\x1b[m"); j >= 0 {
			end += j + len("\x1b[m")
		}
		return l[start:end]
	}
	return ""
}

// TestRepoPageScreens renders the repository page's tabs at 80 and 50
// columns: the header's context, the home tabs and the home footer.
func TestRepoPageScreens(t *testing.T) {
	for _, w := range []int{80, 50} {
		t.Run(fmt.Sprint(w), func(t *testing.T) {
			ui := contentUI(t, w)
			s := screenText(ui)
			if !strings.Contains(s, "main · 0%") {
				t.Fatalf("readme context:\n%s", s)
			}
			goldenFile(t, fmt.Sprintf("repo-readme-%d", w), s)

			uiKey(ui, "?")
			s = screenText(ui)
			if !strings.Contains(s, "esc back") || !regexp.MustCompile(`\?\s+less`).MatchString(s) {
				t.Fatalf("full help:\n%s", s)
			}
			goldenFile(t, fmt.Sprintf("repo-help-%d", w), s)
			uiKey(ui, "?")

			uiKey(ui, "tab")
			waitRow(t, ui, " README.md")
			uiKey(ui, "enter") // pkg
			waitRow(t, ui, " ui")
			uiKey(ui, "enter") // ui
			waitRow(t, ui, " repo.go")
			s = screenText(ui)
			// At 50 columns the path gives way to the summary's words.
			if want := map[int]string{80: "main · pkg/ui · 1 of 1", 50: "1 of 1   ▲ 1 blocked"}[w]; !strings.Contains(s, want) {
				t.Fatalf("directory context:\n%s", s)
			}
			goldenFile(t, fmt.Sprintf("repo-files-dir-%d", w), s)

			uiKey(ui, "enter") // repo.go
			waitScreen(t, ui, "Page is a page")
			s = screenText(ui)
			if want := map[int]string{80: "pkg/ui/repo.go · 0%", 50: "0%   ▲ 1 blocked"}[w]; !strings.Contains(s, want) {
				t.Fatalf("file context:\n%s", s)
			}
			goldenFile(t, fmt.Sprintf("repo-files-file-%d", w), s)
			uiKey(ui, "esc", "esc", "esc")

			uiKey(ui, "tab")
			waitScreen(t, ui, "Add the repository page")
			s = screenText(ui)
			if !strings.Contains(s, "1 of 2") {
				t.Fatalf("commits context:\n%s", s)
			}
			goldenFile(t, fmt.Sprintf("repo-commits-%d", w), s)
		})
	}
}

// TestRepoPageFooterIsHome checks the repository page draws the footer
// home draws, at the same lines from the bottom, and home's tabs.
func TestRepoPageFooterIsHome(t *testing.T) {
	for _, w := range []int{80, 50} {
		t.Run(fmt.Sprint(w), func(t *testing.T) {
			ui := contentUI(t, w)
			uiKey(ui, "esc")
			uiKey(ui, "2") // home Sessions, the tab both pages have
			home := screenLines(ui)
			homeRaw := strings.Split(ui.View().Content, "\n")
			uiKey(ui, "1")
			uiKeyDepth(ui, 6, "enter")
			settle(t, ui)
			for range 5 {
				uiKey(ui, "tab")
			}
			repo := screenLines(ui)
			repoRaw := strings.Split(ui.View().Content, "\n")
			if len(repo) != len(home) {
				t.Fatalf("%d lines on the repository page, %d at home", len(repo), len(home))
			}
			// The bottom margin, the help, the message slot, the footer's margin.
			for i := len(repo) - 4; i < len(repo); i++ {
				if i == len(repo)-2 {
					want := strings.TrimRight("  "+ansi.Strip(ui.footer.ViewNoMargin()), " ")
					if repo[i] != want || !strings.Contains(repo[i], "esc back") || !strings.Contains(repo[i], "? more") {
						t.Fatalf("help line %q, want %q", repo[i], want)
					}
					continue
				}
				if repo[i] != "" || home[i] != "" {
					t.Fatalf("line %d: %q on the repository page, %q at home", i, repo[i], home[i])
				}
			}
			// The active tab is drawn exactly as home draws it.
			if a, b := activeTab(homeRaw), activeTab(repoRaw); a == "" || a != b {
				t.Fatalf("active tab %q on the repository page, %q at home", b, a)
			}
			if strings.Contains(strings.Join(repo, "\n"), "│") {
				t.Fatalf("Soft Serve tab separators:\n%s", strings.Join(repo, "\n"))
			}
		})
	}
}

// TestRepoContextFits checks the header's context gives way in order: the
// summary keeps its words where the tab's context can shrink instead.
func TestRepoContextFits(t *testing.T) {
	ui := contentUI(t, 50)
	header := strings.Split(screenText(ui), "\n")[2]
	if !strings.Contains(header, "▲ 1 blocked") || !strings.Contains(header, "0%") {
		t.Fatalf("50 columns: %q", header)
	}
	ui = contentUI(t, 40)
	header = strings.Split(screenText(ui), "\n")[2]
	if !strings.Contains(header, "▲1 ✓1 ●2") || !strings.Contains(header, "0%") || ansi.StringWidth(header) > 40 {
		t.Fatalf("40 columns: %q", header)
	}
}

// TestRepoTabsHelp checks every repository tab ranks its keys on the home
// help line and opens the home full help with ?.
func TestRepoTabsHelp(t *testing.T) {
	ui := contentUI(t, 80)
	want := []string{"j/k move", "j/k move • enter open", "j/k move • enter open", "j/k move • enter browse", "j/k move • enter browse", "j/k move • enter attach"}
	for i, w := range want {
		if i > 0 {
			uiKey(ui, "tab")
		}
		settle(t, ui)
		s := screenText(ui)
		help := s[strings.LastIndex(s, "\n")+1:]
		if !strings.HasPrefix(help, "   "+w) || !strings.Contains(help, "esc back") || !strings.HasSuffix(help, "? more") {
			t.Fatalf("tab %d help line %q, want %q first", i+1, help, w)
		}
		for _, b := range ui.ShortHelp() {
			if footer.RankOf(b) == footer.RankOther && b.Help().Desc == "" {
				t.Fatalf("tab %d: key %q has no help", i+1, b.Help().Key)
			}
		}
		uiKey(ui, "?")
		full := screenText(ui)
		if !regexp.MustCompile(`\?\s+less`).MatchString(full) || !strings.Contains(full, "esc back") || len(ui.FullHelp()) < 2 {
			t.Fatalf("tab %d full help:\n%s", i+1, full)
		}
		uiKey(ui, "?")
	}
}
