package openshell

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	v1 "github.com/NVIDIA/OpenShell/sdk/go/openshell/v1"
	"github.com/NVIDIA/OpenShell/sdk/go/openshell/v1/fake"
	"github.com/NVIDIA/OpenShell/sdk/go/openshell/v1/types"
	"github.com/charmbracelet/soft-serve/pkg/config"
)

var dnsLabel = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

func TestSandboxName(t *testing.T) {
	cases := []struct {
		user, repo string
		want       string // exact name, or "" to only require a hashed name
	}{
		{"alice", "repo", "soft-alice-repo"},
		{"alice", "my-repo", "soft-alice-my-repo"},
		{"alice", "r2", "soft-alice-r2"},
		{"alice", "org/repo", ""},
		{"alice", "Repo", ""},
		{"alice", "my_repo", ""},
		{"alice", "my.repo", ""},
		{"al-ice", "repo", ""},
		{"alice", strings.Repeat("x", 80), ""},
	}
	for _, tc := range cases {
		got := SandboxName(tc.user, tc.repo)
		if len(got) > maxNameLen || !dnsLabel.MatchString(got) {
			t.Errorf("SandboxName(%q, %q) = %q: not a DNS label of <= 63 chars", tc.user, tc.repo, got)
		}
		if !strings.HasPrefix(got, "soft-") {
			t.Errorf("SandboxName(%q, %q) = %q: missing soft- prefix", tc.user, tc.repo, got)
		}
		if tc.want != "" && got != tc.want {
			t.Errorf("SandboxName(%q, %q) = %q, want %q", tc.user, tc.repo, got, tc.want)
		}
		if tc.want == "" && !regexp.MustCompile(`-[0-9a-f]{8}$`).MatchString(got) {
			t.Errorf("SandboxName(%q, %q) = %q: expected a hash suffix", tc.user, tc.repo, got)
		}
		if again := SandboxName(tc.user, tc.repo); again != got {
			t.Errorf("SandboxName not deterministic: %q vs %q", got, again)
		}
	}
}

func TestSandboxNameCollisions(t *testing.T) {
	pairs := [][2][2]string{
		{{"alice", "a/b"}, {"alice", "a-b"}},
		{{"alice", "Repo"}, {"alice", "repo"}},
		{{"alice", "a_b"}, {"alice", "a.b"}},
		{{"foo-bar", "baz"}, {"foo", "bar-baz"}},
		{{"alice", strings.Repeat("x", 80) + "1"}, {"alice", strings.Repeat("x", 80) + "2"}},
	}
	for _, p := range pairs {
		a := SandboxName(p[0][0], p[0][1])
		b := SandboxName(p[1][0], p[1][1])
		if a == b {
			t.Errorf("SandboxName(%v) == SandboxName(%v) == %q", p[0], p[1], a)
		}
	}
}

// A user must not be able to craft a repository name whose plain sandbox
// name equals another user's hashed name.
func TestSandboxNamePlainCannotForgeHashed(t *testing.T) {
	victim := SandboxName("al-ice", "repo") // hashed: username has a hyphen
	if !strings.Contains(victim, "--") {
		t.Fatalf("hashed name %q lacks the -- separator", victim)
	}
	forged := strings.TrimPrefix(victim, "soft-al-")
	for _, repo := range []string{forged, strings.ReplaceAll(forged, "--", "-")} {
		if got := SandboxName("al", repo); got == victim {
			t.Errorf("SandboxName(al, %q) = %q forges the victim's name", repo, got)
		}
	}
}

func TestSessionSandboxName(t *testing.T) {
	// The default session keeps the pre-named-session sandbox name, so
	// existing sandboxes stay reachable.
	for _, pair := range [][2]string{{"alice", "repo"}, {"al-ice", "org/Repo"}, {"alice", strings.Repeat("x", 80)}} {
		want := SandboxName(pair[0], pair[1])
		for _, name := range []string{"", DefaultSessionName} {
			if got := SessionSandboxName(pair[0], pair[1], name); got != want {
				t.Errorf("SessionSandboxName(%q, %q, %q) = %q, want %q", pair[0], pair[1], name, got, want)
			}
		}
	}

	hashed := regexp.MustCompile(`^soft-[a-z0-9-]*[a-z0-9]--[0-9a-f]{8}$`)
	cases := []struct {
		user, repo, name string
		prefix           string // the readable part before the hash
	}{
		{"alice", "repo", "review", "soft-alice-repo-review"},
		{"alice", "org/repo", "b2", "soft-alice-org-repo-b2"},
		{"al-ice", "repo", "x", "soft-al-ice-repo-x"},
		// Long user and repository parts are cut; the session name stays.
		{"alice", strings.Repeat("r", 80), strings.Repeat("s", 32), ""},
		{strings.Repeat("u", 70), strings.Repeat("r", 70), "review", ""},
	}
	for _, tc := range cases {
		got := SessionSandboxName(tc.user, tc.repo, tc.name)
		if len(got) > maxNameLen || !dnsLabel.MatchString(got) || !hashed.MatchString(got) {
			t.Errorf("SessionSandboxName(%q, %q, %q) = %q: not a hashed DNS label of <= 63 chars", tc.user, tc.repo, tc.name, got)
		}
		if tc.prefix != "" && !strings.HasPrefix(got, tc.prefix+"--") {
			t.Errorf("SessionSandboxName(%q, %q, %q) = %q, want prefix %q", tc.user, tc.repo, tc.name, got, tc.prefix)
		}
		if !strings.Contains(strings.TrimSuffix(got, got[len(got)-10:]), tc.name) {
			t.Errorf("SessionSandboxName(%q, %q, %q) = %q: session name not readable", tc.user, tc.repo, tc.name, got)
		}
		if again := SessionSandboxName(tc.user, tc.repo, tc.name); again != got {
			t.Errorf("not deterministic: %q vs %q", got, again)
		}
	}
	if got := SessionSandboxName("alice", "repo", "review"); got != "soft-alice-repo-review--"+mustHash("alice\x00repo\x00review") {
		t.Errorf("hash = %q", got)
	}
}

func mustHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:8]
}

func TestSessionSandboxNameCollisions(t *testing.T) {
	// A named session never takes another pair's default sandbox name,
	// plain or hashed, nor another named session's.
	names := map[string]string{}
	add := func(what, name string) {
		if prev, ok := names[name]; ok {
			t.Errorf("%s and %s share the sandbox name %q", prev, what, name)
		}
		names[name] = what
	}
	for _, user := range []string{"alice", "al-ice", "al"} {
		for _, repo := range []string{"r", "r-x", "r-review", "ice-r", "r/x", "R"} {
			add(user+"/"+repo+" main", SessionSandboxName(user, repo, ""))
			for _, s := range []string{"x", "review", "r", "main2"} {
				add(user+"/"+repo+" "+s, SessionSandboxName(user, repo, s))
			}
		}
	}
	// "r" + session "x" vs repository "r-x": the plain name of the latter
	// is not the former's readable prefix plus nothing.
	if SessionSandboxName("alice", "r", "x") == SandboxName("alice", "r-x") {
		t.Error("named session collides with a plain default name")
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

	if _, err := c.ResumeSandbox(ctx, "soft-a-r", labels); !IsNotFound(err) {
		t.Fatalf("absent: err = %v", err)
	}
	if _, err := fc.Sandboxes().Get(ctx, "default", "soft-a-r"); !IsNotFound(err) {
		t.Fatal("ResumeSandbox created a sandbox")
	}

	fc.AddSandbox("default", &types.Sandbox{Name: "soft-a-r", Labels: labels, Status: types.SandboxStatus{Phase: types.SandboxStopped}})
	sb, err := c.ResumeSandbox(ctx, "soft-a-r", labels)
	if err != nil || sb.Status.Phase != types.SandboxReady {
		t.Fatalf("stopped: sb=%v err=%v", sb, err)
	}
	if _, err := c.ResumeSandbox(ctx, "soft-a-r", map[string]string{LabelUserID: "2"}); err != ErrNotOwned {
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
		Name:   "soft-a-r",
		Labels: map[string]string{LabelUserID: "1"},
		Status: types.SandboxStatus{Phase: types.SandboxReady},
	})
	c := newSDKClient("default", &raceGet{SandboxInterface: fc.Sandboxes(), missing: 1}, nil, nil, nil)

	sb, created, err := c.EnsureSandbox(ctx, "soft-a-r", req)
	if err != nil || created || sb.Status.Phase != types.SandboxReady {
		t.Fatalf("race: sb=%v created=%v err=%v", sb, created, err)
	}
	// The loser must not delete the winner's sandbox.
	if _, err := fc.Sandboxes().Get(ctx, "default", "soft-a-r"); err != nil {
		t.Fatalf("winner's sandbox deleted: %v", err)
	}
}

func TestEnsureSandboxInconsistentGatewayDoesNotLoop(t *testing.T) {
	ctx := context.Background()
	fc := fake.NewClient()
	fc.AddSandbox("default", &types.Sandbox{Name: "soft-a-r", Status: types.SandboxStatus{Phase: types.SandboxReady}})
	// Get always says NotFound while Create says AlreadyExists.
	c := newSDKClient("default", &raceGet{SandboxInterface: fc.Sandboxes(), missing: 1 << 30}, nil, nil, nil)

	if _, _, err := c.EnsureSandbox(ctx, "soft-a-r", SandboxRequest{}); err == nil {
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

	if _, _, err := c.EnsureSandbox(ctx, "soft-a-r", SandboxRequest{Image: "img"}); err == nil {
		t.Fatal("expected ready-wait failure")
	}
	if _, err := fc.Sandboxes().Get(ctx, "default", "soft-a-r"); !IsNotFound(err) {
		t.Fatalf("half-made sandbox not deleted: err=%v", err)
	}
}

func TestEnsureSandboxStates(t *testing.T) {
	ctx := context.Background()
	fc := fake.NewClient()
	c := newSDKClient("default", fc.Sandboxes(), nil, nil, nil)
	req := SandboxRequest{Image: "img", Labels: map[string]string{LabelUserID: "1"}}

	// New.
	sb, created, err := c.EnsureSandbox(ctx, "soft-a-r", req)
	if err != nil || !created || sb.Status.Phase != types.SandboxReady {
		t.Fatalf("new: sb=%v created=%v err=%v", sb, created, err)
	}
	if sb.Spec.Template == nil || sb.Spec.Template.Image != "img" {
		t.Fatalf("new: image not set: %+v", sb.Spec.Template)
	}

	// Ready → resumed as is.
	if _, created, err := c.EnsureSandbox(ctx, "soft-a-r", req); err != nil || created {
		t.Fatalf("ready: created=%v err=%v", created, err)
	}

	// Stopped → started.
	if _, err := fc.Sandboxes().Stop(ctx, "default", "soft-a-r"); err != nil {
		t.Fatal(err)
	}
	sb, created, err = c.EnsureSandbox(ctx, "soft-a-r", req)
	if err != nil || created || sb.Status.Phase != types.SandboxReady {
		t.Fatalf("stopped: sb=%v created=%v err=%v", sb, created, err)
	}

	// Still provisioning (e.g. created by a concurrent start) → waited on.
	for _, phase := range []types.SandboxPhase{types.SandboxProvisioning, types.SandboxStarting, ""} {
		name := "soft-a-prov" + strings.ToLower(string(phase))
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
	if _, _, err := c.EnsureSandbox(ctx, "soft-a-r", other); err != ErrNotOwned {
		t.Fatalf("foreign: err=%v, want ErrNotOwned", err)
	}

	// Error phase is surfaced, not papered over.
	fc.AddSandbox("default", &types.Sandbox{
		Name:   "soft-a-broken",
		Labels: map[string]string{LabelUserID: "1"},
		Status: types.SandboxStatus{Phase: types.SandboxError},
	})
	if _, _, err := c.EnsureSandbox(ctx, "soft-a-broken", req); err == nil {
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
