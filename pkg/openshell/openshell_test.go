package openshell

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	v1 "github.com/NVIDIA/OpenShell/sdk/go/openshell/v1"
	"github.com/NVIDIA/OpenShell/sdk/go/openshell/v1/fake"
	"github.com/NVIDIA/OpenShell/sdk/go/openshell/v1/types"
	"github.com/danielboothcloud/sundae/pkg/config"
)

var dnsLabel = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

func TestSessionSandboxName(t *testing.T) {
	label := regexp.MustCompile(`^sd-[0-9a-f]{16}$`)
	for _, tc := range []struct{ user, session string }{
		{"alice", "main"},
		{"alice", "a-very-long-session-name-of-32ch"},
		{"Bob.Smith", "tool"},
		{"x", "y"},
	} {
		got := SessionSandboxName(tc.user, tc.session)
		// OpenShell 0.1.8 refuses sandbox names over 19 characters.
		if !label.MatchString(got) || len(got) > 19 {
			t.Errorf("SessionSandboxName(%q, %q) = %q: not sd- and 16 hex digits", tc.user, tc.session, got)
		}
		if again := SessionSandboxName(tc.user, tc.session); again != got {
			t.Errorf("not deterministic: %q vs %q", got, again)
		}
	}
	if SessionSandboxName("alice", "") != SessionSandboxName("alice", DefaultSessionName) {
		t.Error("an empty session name is not the default one")
	}
	// The NUL separator keeps the pairs apart.
	pairs := [][2][2]string{
		{{"al", "ice-main"}, {"al-ice", "main"}},
		{{"alice", "main"}, {"bob", "main"}},
		{{"alice", "a"}, {"alice", "b"}},
	}
	for _, p := range pairs {
		if a, b := SessionSandboxName(p[0][0], p[0][1]), SessionSandboxName(p[1][0], p[1][1]); a == b {
			t.Errorf("%v and %v both get %q", p[0], p[1], a)
		}
	}
}

func TestDefaultSessionNameFor(t *testing.T) {
	for repo, want := range map[string]string{
		"tool":                         "tool",
		"github/me/My_Tool.git":        "my-tool",
		"gitlab/a/b/c-d":               "c-d",
		"x/" + strings.Repeat("a", 40): strings.Repeat("a", 32),
		"x/---":                        DefaultSessionName,
		"":                             DefaultSessionName,
	} {
		if got := DefaultSessionNameFor(repo); got != want {
			t.Errorf("DefaultSessionNameFor(%q) = %q, want %q", repo, got, want)
		}
	}
}

func TestMergeRepos(t *testing.T) {
	got := mergeRepos([]string{"a", "b"}, []string{"c", "a", "d"}, []string{"b", "d"})
	if strings.Join(got, ",") != "a,c" {
		t.Fatalf("merge = %v", got)
	}
	if got := sessionWorkDir([]string{"x/y"}); got != "/sandbox/x/y" {
		t.Fatalf("one repository: %s", got)
	}
	if got := sessionWorkDir([]string{"x", "y"}); got != SandboxRoot {
		t.Fatalf("two repositories: %s", got)
	}
}

func TestValidSessionName(t *testing.T) {
	for _, name := range []string{"main", "a", "review-2", "0", strings.Repeat("a", 32)} {
		if !ValidSessionName(name) {
			t.Errorf("ValidSessionName(%q) = false", name)
		}
	}
	for _, name := range []string{"", "-a", "a-", "A", "a_b", "a b", "a.b", strings.Repeat("a", 33), "a/b"} {
		if ValidSessionName(name) {
			t.Errorf("ValidSessionName(%q) = true", name)
		}
	}
}

func TestResumeSandbox(t *testing.T) {
	ctx := context.Background()
	fc := fake.NewClient()
	c := newSDKClient("default", fc.Sandboxes(), nil, nil, nil)
	labels := map[string]string{LabelUserID: "1"}

	if _, err := c.ResumeSandbox(ctx, "sundae-a-r", labels); !IsNotFound(err) {
		t.Fatalf("absent: err = %v", err)
	}
	if _, err := fc.Sandboxes().Get(ctx, "default", "sundae-a-r"); !IsNotFound(err) {
		t.Fatal("ResumeSandbox created a sandbox")
	}

	fc.AddSandbox("default", &types.Sandbox{Name: "sundae-a-r", Labels: labels, Status: types.SandboxStatus{Phase: types.SandboxStopped}})
	sb, err := c.ResumeSandbox(ctx, "sundae-a-r", labels)
	if err != nil || sb.Status.Phase != types.SandboxReady {
		t.Fatalf("stopped: sb=%v err=%v", sb, err)
	}
	if _, err := c.ResumeSandbox(ctx, "sundae-a-r", map[string]string{LabelUserID: "2"}); err != ErrNotOwned {
		t.Fatalf("foreign: err = %v", err)
	}
}

// raceGet makes the first Get report NotFound, as if another start created
// the sandbox between this start's Get and Create.
type raceGet struct {
	v1.SandboxInterface
	missing int // number of Gets that report NotFound
}

func (r *raceGet) Get(ctx context.Context, ws, name string) (*types.Sandbox, error) {
	if r.missing > 0 {
		r.missing--
		return nil, &types.StatusError{Code: types.ErrorNotFound, Message: "not found"}
	}
	return r.SandboxInterface.Get(ctx, ws, name)
}

func TestEnsureSandboxLostCreateRace(t *testing.T) {
	ctx := context.Background()
	fc := fake.NewClient()
	req := SandboxRequest{Image: "img", Labels: map[string]string{LabelUserID: "1"}}
	// The winner's sandbox already exists; this start's Get misses it.
	fc.AddSandbox("default", &types.Sandbox{
		Name:   "sundae-a-r",
		Labels: map[string]string{LabelUserID: "1"},
		Status: types.SandboxStatus{Phase: types.SandboxReady},
	})
	c := newSDKClient("default", &raceGet{SandboxInterface: fc.Sandboxes(), missing: 1}, nil, nil, nil)

	sb, created, err := c.EnsureSandbox(ctx, "sundae-a-r", req)
	if err != nil || created || sb.Status.Phase != types.SandboxReady {
		t.Fatalf("race: sb=%v created=%v err=%v", sb, created, err)
	}
	// The loser must not delete the winner's sandbox.
	if _, err := fc.Sandboxes().Get(ctx, "default", "sundae-a-r"); err != nil {
		t.Fatalf("winner's sandbox deleted: %v", err)
	}
}

func TestEnsureSandboxInconsistentGatewayDoesNotLoop(t *testing.T) {
	ctx := context.Background()
	fc := fake.NewClient()
	fc.AddSandbox("default", &types.Sandbox{Name: "sundae-a-r", Status: types.SandboxStatus{Phase: types.SandboxReady}})
	// Get always says NotFound while Create says AlreadyExists.
	c := newSDKClient("default", &raceGet{SandboxInterface: fc.Sandboxes(), missing: 1 << 30}, nil, nil, nil)

	if _, _, err := c.EnsureSandbox(ctx, "sundae-a-r", SandboxRequest{}); err == nil {
		t.Fatal("expected an error, not a resumed sandbox")
	}
}

// failingWait wraps a sandbox client so WaitReady fails.
type failingWait struct {
	v1.SandboxInterface
}

func (failingWait) WaitReady(context.Context, string, string, ...v1.WaitOptions) (*types.Sandbox, error) {
	return nil, &types.StatusError{Code: types.ErrorDeadlineExceeded, Message: "not ready"}
}

func TestEnsureSandboxDeletesHalfMadeSandbox(t *testing.T) {
	ctx := context.Background()
	fc := fake.NewClient()
	c := newSDKClient("default", failingWait{fc.Sandboxes()}, nil, nil, nil)

	if _, _, err := c.EnsureSandbox(ctx, "sundae-a-r", SandboxRequest{Image: "img"}); err == nil {
		t.Fatal("expected ready-wait failure")
	}
	if _, err := fc.Sandboxes().Get(ctx, "default", "sundae-a-r"); !IsNotFound(err) {
		t.Fatalf("half-made sandbox not deleted: err=%v", err)
	}
}

func TestEnsureSandboxStates(t *testing.T) {
	ctx := context.Background()
	fc := fake.NewClient()
	c := newSDKClient("default", fc.Sandboxes(), nil, nil, nil)
	req := SandboxRequest{Image: "img", Labels: map[string]string{LabelUserID: "1"}}

	// New.
	sb, created, err := c.EnsureSandbox(ctx, "sundae-a-r", req)
	if err != nil || !created || sb.Status.Phase != types.SandboxReady {
		t.Fatalf("new: sb=%v created=%v err=%v", sb, created, err)
	}
	if sb.Spec.Template == nil || sb.Spec.Template.Image != "img" {
		t.Fatalf("new: image not set: %+v", sb.Spec.Template)
	}

	// Ready → resumed as is.
	if _, created, err := c.EnsureSandbox(ctx, "sundae-a-r", req); err != nil || created {
		t.Fatalf("ready: created=%v err=%v", created, err)
	}

	// Stopped → started.
	if _, err := fc.Sandboxes().Stop(ctx, "default", "sundae-a-r"); err != nil {
		t.Fatal(err)
	}
	sb, created, err = c.EnsureSandbox(ctx, "sundae-a-r", req)
	if err != nil || created || sb.Status.Phase != types.SandboxReady {
		t.Fatalf("stopped: sb=%v created=%v err=%v", sb, created, err)
	}

	// Still provisioning (e.g. created by a concurrent start) → waited on.
	for _, phase := range []types.SandboxPhase{types.SandboxProvisioning, types.SandboxStarting, ""} {
		name := "sundae-a-prov" + strings.ToLower(string(phase))
		fc.AddSandbox("default", &types.Sandbox{
			Name:   name,
			Labels: map[string]string{LabelUserID: "1"},
			Status: types.SandboxStatus{Phase: phase},
		})
		sb, created, err := c.EnsureSandbox(ctx, name, req)
		if err != nil || created || sb.Status.Phase != types.SandboxReady {
			t.Fatalf("%q: sb=%v created=%v err=%v", phase, sb, created, err)
		}
	}

	// Owned by someone else.
	other := SandboxRequest{Labels: map[string]string{LabelUserID: "2"}}
	if _, _, err := c.EnsureSandbox(ctx, "sundae-a-r", other); err != ErrNotOwned {
		t.Fatalf("foreign: err=%v, want ErrNotOwned", err)
	}

	// Error phase is surfaced, not papered over.
	fc.AddSandbox("default", &types.Sandbox{
		Name:   "sundae-a-broken",
		Labels: map[string]string{LabelUserID: "1"},
		Status: types.SandboxStatus{Phase: types.SandboxError},
	})
	if _, _, err := c.EnsureSandbox(ctx, "sundae-a-broken", req); err == nil {
		t.Fatal("error phase: expected error")
	}
}

func TestNewReadsTokenFile(t *testing.T) {
	cfg := config.OpenShellConfig{
		GatewayAddress: "https://openshell.invalid:8080",
		Workspace:      "default",
		TokenFile:      filepath.Join(t.TempDir(), "missing"),
	}
	if _, err := New(cfg); err == nil || !strings.Contains(err.Error(), "token_file") {
		t.Fatalf("missing token file: want a token_file error, got %v", err)
	}

	if err := os.WriteFile(cfg.TokenFile, []byte("gw-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := New(cfg)
	if err != nil {
		t.Fatalf("token file present: %v", err)
	}
	_ = c.Close()
}
