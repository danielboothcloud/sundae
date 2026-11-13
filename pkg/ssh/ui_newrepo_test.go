package ssh

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/ssh"
	"github.com/danielboothcloud/sundae/pkg/backend"
	"github.com/danielboothcloud/sundae/pkg/config"
	"github.com/danielboothcloud/sundae/pkg/proto"
	"github.com/danielboothcloud/sundae/pkg/sessionctl"
	"github.com/danielboothcloud/sundae/pkg/ui/pages/sessions"
	gossh "golang.org/x/crypto/ssh"
)

// startSource is a Sessions tab Source that records the sessions started.
type startSource struct {
	*fakeSource
	mu       sync.Mutex
	requests []sessionctl.Request
}

func (s *startSource) Prepare(_ context.Context, req sessionctl.Request) (sessions.Attacher, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, req)
	return &fakeAttacher{closed: make(chan struct{})}, nil
}

func (s *startSource) started() []sessionctl.Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]sessionctl.Request(nil), s.requests...)
}

// runGitIn runs git in dir.
func runGitIn(t *testing.T, dir, stdin string, env []string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			t.Fatalf("git %v: %v: %s", args, err, ee.Stderr)
		}
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

// seedTemplate gives the repository one commit holding a README.
func seedTemplate(t *testing.T, cfg *config.Config, name string) {
	t.Helper()
	dir := filepath.Join(cfg.DataPath, "repos", name+".git")
	blob := runGitIn(t, dir, "# template\n", nil, "hash-object", "-w", "--stdin")
	tree := runGitIn(t, dir, "100644 blob "+blob+"\tREADME.md\n", nil, "mktree")
	commit := runGitIn(t, dir, "", []string{"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t"},
		"commit-tree", "-m", "template", tree)
	runGitIn(t, dir, "", nil, "update-ref", "refs/heads/main", commit)
	runGitIn(t, dir, "", nil, "symbolic-ref", "HEAD", "refs/heads/main")
}

// newRepoUI is a home page for alice, who may create repositories, with
// sessions, two profiles, a template she can read and one she cannot.
// Without user, the caller is anonymous.
func newRepoUI(t *testing.T, width int, user bool) (*UI, *startSource, *backend.Backend, *config.Config) {
	t.Helper()
	src := &startSource{fakeSource: &fakeSource{rows: []sessionctl.Row{}}}
	var be *backend.Backend
	var cfg *config.Config
	ui := newHomeUI(t, width, 24, func(ctx context.Context, b *backend.Backend) context.Context {
		be, cfg = b, config.FromContext(ctx)
		admin, err := b.User(ctx, "admin")
		if err != nil {
			t.Fatal(err)
		}
		_, priv, err := ed25519.GenerateKey(nil)
		if err != nil {
			t.Fatal(err)
		}
		signer, err := gossh.NewSignerFromKey(priv)
		if err != nil {
			t.Fatal(err)
		}
		pk := signer.PublicKey()
		alice, err := b.CreateUser(ctx, "alice", proto.UserOptions{PublicKeys: []gossh.PublicKey{pk}})
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range []struct {
			name    string
			private bool
		}{{"templates/go", false}, {"templates/secret", true}, {"scratch", false}} {
			if _, err := b.CreateRepository(ctx, r.name, admin, proto.RepositoryOptions{Private: r.private, Description: "about " + r.name}); err != nil {
				t.Fatal(err)
			}
		}
		seedTemplate(t, cfg, "templates/go")
		seedTemplate(t, cfg, "templates/secret")
		cfg.Sessions.Profiles = map[string]config.SessionProfile{
			"claude": {Image: "agent-claude", Command: "claude"},
			"shell":  {Command: "zsh -l"},
		}
		cfg.Projects.Templates = map[string]config.ProjectTemplate{
			"go-service": {Repo: "templates/go", Description: "Go service", Profile: "claude"},
			"secret":     {Repo: "templates/secret"},
		}
		ctx = context.WithValue(ctx, sessions.SourceKey, sessions.Source(src))
		if user {
			// As the SSH server does: the user and the key they came with.
			ctx = proto.WithUserContext(ctx, alice)
			ctx = context.WithValue(ctx, ssh.ContextKeyPublicKey, pk)
		}
		return ctx
	})
	return ui, src, be, cfg
}

// press sends a special key.
func press(ui *UI, code rune, times int) {
	for range times {
		_, cmd := ui.Update(tea.KeyPressMsg{Code: code})
		runUI(ui, cmd, 2)
	}
}

// typeText types text into the focused field.
func typeText(ui *UI, text string) {
	for _, r := range text {
		uiKey(ui, string(r))
	}
}

func TestNewRepoForm(t *testing.T) {
	for _, w := range []int{80, 50} {
		t.Run(fmt.Sprint(w), func(t *testing.T) {
			ui, _, _, _ := newRepoUI(t, w, true)
			uiKey(ui, "n")
			goldenFile(t, fmt.Sprintf("home-new-repo-%d", w), screenText(ui))
		})
	}
}

func TestNewRepoFormValidation(t *testing.T) {
	ui, _, be, _ := newRepoUI(t, 80, true)
	found := false
	for _, b := range ui.ShortHelp() {
		found = found || b.Help().Key == "n" && b.Help().Desc == "new"
	}
	if !found {
		t.Fatal("no n in the Repositories help")
	}
	uiKey(ui, "n")
	s := screenText(ui)
	// Only the template alice can read is offered; without one, none.
	if !strings.Contains(s, "‹ none ›   empty repository") || !strings.Contains(s, "Start session") {
		t.Fatalf("form:\n%s", s)
	}
	press(ui, tea.KeyEnter, 1)
	if s := screenText(ui); !strings.Contains(s, "✗ enter a name") {
		t.Fatalf("empty name:\n%s", s)
	}
	// q, digits and ? are text while the form is open.
	typeText(ui, "q1?")
	if ui.activePage != selectionPage || !strings.Contains(screenText(ui), "Name            q1?") {
		t.Fatalf("typing:\n%s", screenText(ui))
	}
	press(ui, tea.KeyEnter, 1)
	if s := screenText(ui); !strings.Contains(s, "✗ repo can only contain letters") {
		t.Fatalf("bad name:\n%s", s)
	}
	press(ui, tea.KeyBackspace, 3)
	typeText(ui, "scratch")
	press(ui, tea.KeyEnter, 1)
	if s := screenText(ui); !strings.Contains(s, "✗ scratch already exists") {
		t.Fatalf("existing name:\n%s", s)
	}
	// The template list has the readable template only.
	press(ui, tea.KeyDown, 3)
	press(ui, tea.KeyRight, 1)
	s = screenText(ui)
	if !strings.Contains(s, "‹ go-service ›   Go service") {
		t.Fatalf("template:\n%s", s)
	}
	press(ui, tea.KeyRight, 1)
	if !strings.Contains(screenText(ui), "‹ none ›") {
		t.Fatalf("templates wrap to none, the secret one is not offered:\n%s", screenText(ui))
	}
	// esc closes the form; nothing was created.
	press(ui, tea.KeyEscape, 1)
	if s := screenText(ui); strings.Contains(s, "New repository") || !strings.Contains(s, "scratch") {
		t.Fatalf("esc:\n%s", s)
	}
	if _, err := be.Repository(context.Background(), "q1"); !errors.Is(err, proto.ErrRepoNotFound) {
		t.Fatal("a repository was created")
	}
}

// selected returns the ID of the Repositories tab's selected item.
func selected(ui *UI) string {
	return ui.selection().SelectedRepo()
}

func waitFor(t *testing.T, ui *UI, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		drainLate(ui)
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s never happened:\n%s", what, screenText(ui))
}

func TestNewRepoFormCreates(t *testing.T) {
	ui, src, be, _ := newRepoUI(t, 80, true)
	ctx := context.Background()
	uiKey(ui, "n")
	typeText(ui, "apps/web")
	press(ui, tea.KeyDown, 1)
	typeText(ui, "The web app")
	press(ui, tea.KeyDown, 1)
	press(ui, tea.KeyRight, 1) // VCS: git
	press(ui, tea.KeyDown, 1)
	press(ui, tea.KeyRight, 1) // Template: go-service, whose profile is claude
	if s := screenText(ui); !strings.Contains(s, "‹ claude ›") {
		t.Fatalf("the template's profile was not chosen:\n%s", s)
	}
	press(ui, tea.KeyDown, 2)
	uiKey(ui, "y") // Private
	press(ui, tea.KeyEnter, 1)
	waitScreen(t, ui, "Created apps/web.")

	s := screenText(ui)
	if strings.Contains(s, "New repository") || selected(ui) != "apps/web" {
		t.Fatalf("form open or apps/web not selected (%q):\n%s", selected(ui), s)
	}
	r, err := be.Repository(ctx, "apps/web")
	if err != nil {
		t.Fatal(err)
	}
	if r.Vcs() != proto.VCSGit || !r.IsPrivate() || r.Description() != "The web app" {
		t.Fatalf("repository: vcs %s private %v description %q", r.Vcs(), r.IsPrivate(), r.Description())
	}
	if p, _ := be.RepoProfile(ctx, "apps/web"); p != "claude" {
		t.Fatalf("profile %q", p)
	}
	gr, err := r.Open()
	if err != nil {
		t.Fatal(err)
	}
	if got := runGitIn(t, gr.Path, "", nil, "log", "--format=%an|%s", "HEAD"); got != "alice|Initial commit from template go-service" {
		t.Fatalf("log %q", got)
	}
	if len(src.started()) != 0 {
		t.Fatal("a session was started")
	}
	// The notice goes with the next key.
	uiKey(ui, "j")
	if strings.Contains(screenText(ui), "Created apps/web.") {
		t.Fatal("notice kept")
	}
}

func TestNewRepoFormStartsSession(t *testing.T) {
	ui, src, _, _ := newRepoUI(t, 80, true)
	uiKey(ui, "n")
	typeText(ui, "svc")
	press(ui, tea.KeyDown, 4)
	press(ui, tea.KeyRight, 2) // Profile: shell
	press(ui, tea.KeyDown, 2)
	uiKey(ui, "y") // Start session
	press(ui, tea.KeyEnter, 1)
	waitFor(t, ui, "the session start", func() bool { return len(src.started()) == 1 })
	req := src.started()[0]
	if strings.Join(req.Repos, ",") != "svc" || req.Name != "svc" || req.Profile != "shell" || !req.Create {
		t.Fatalf("request %+v", req)
	}
	// The Sessions tab shows it; without a terminal to hand over it is
	// started detached, and says why.
	waitScreen(t, ui, "• Sessions")
	waitScreen(t, ui, "attaching needs the SSH terminal")
}

func TestNewRepoFormErrorKeepsForm(t *testing.T) {
	ui, _, _, cfg := newRepoUI(t, 80, true)
	uiKey(ui, "n")
	typeText(ui, "x")
	press(ui, tea.KeyDown, 3)
	press(ui, tea.KeyRight, 1) // go-service
	// The template's repository disappears before the form is submitted.
	cfg.Projects.Templates["go-service"] = config.ProjectTemplate{Repo: "gone"}
	press(ui, tea.KeyEnter, 1)
	waitScreen(t, ui, `✗ template "go-service": repository "gone": repository not found`)
	if s := screenText(ui); !strings.Contains(s, "New repository") {
		t.Fatalf("form closed:\n%s", s)
	}
}

func TestNewRepoFormOnlyForCreators(t *testing.T) {
	ui, _, _, _ := newRepoUI(t, 80, false)
	for _, b := range ui.ShortHelp() {
		if b.Help().Key == "n" {
			t.Fatal("n in the help of an anonymous user")
		}
	}
	uiKey(ui, "n")
	if s := screenText(ui); strings.Contains(s, "New repository") {
		t.Fatalf("anonymous user opened the form:\n%s", s)
	}
}

// The Sessions tab's new-session form defaults to the repository's own
// profile, before the sessions.repos glob.
func TestSessionsSourceRepoProfile(t *testing.T) {
	cfg, _, _, be := newTestBackend(t)
	ctx := backend.WithContext(config.WithContext(context.Background(), cfg), be)
	cfg.OpenShell.Enabled = true
	cfg.Sessions.Profiles = map[string]config.SessionProfile{"claude": {}, "shell": {}}
	cfg.Sessions.Repos = map[string]config.SessionRepoDefaults{"infra/*": {Profile: "claude"}}
	alice, err := be.CreateUser(ctx, "alice", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := be.CreateRepository(ctx, "infra/dns", alice, proto.RepositoryOptions{}); err != nil {
		t.Fatal(err)
	}
	src := sessions.NewSource(proto.WithUserContext(ctx, alice))
	if got := src.RepoProfile("infra/dns"); got != "claude" {
		t.Fatalf("glob: %q", got)
	}
	if err := be.SetRepoProfile(ctx, "infra/dns", "shell"); err != nil {
		t.Fatal(err)
	}
	if got := src.RepoProfile("infra/dns"); got != "shell" {
		t.Fatalf("repo setting: %q", got)
	}
	// A setting whose profile is gone falls back to the glob.
	delete(cfg.Sessions.Profiles, "shell")
	if got := src.RepoProfile("infra/dns"); got != "claude" {
		t.Fatalf("stale setting: %q", got)
	}
}
