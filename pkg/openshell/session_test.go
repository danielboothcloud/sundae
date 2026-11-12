package openshell

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/ssh"
	v1 "github.com/NVIDIA/OpenShell/sdk/go/openshell/v1"
	"github.com/NVIDIA/OpenShell/sdk/go/openshell/v1/fake"
	"github.com/NVIDIA/OpenShell/sdk/go/openshell/v1/types"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	gossh "golang.org/x/crypto/ssh"
)

// testUser is a minimal proto.User.
type testUser struct {
	id   int64
	name string
}

func (u testUser) ID() int64                     { return u.id }
func (u testUser) Username() string              { return u.name }
func (u testUser) IsAdmin() bool                 { return false }
func (u testUser) PublicKeys() []gossh.PublicKey { return nil }
func (u testUser) Password() string              { return "" }

var _ proto.User = testUser{}

// fakeTokens is an in-memory TokenStore.
type fakeTokens struct {
	mu     sync.Mutex
	nextID int64
	tokens map[int64]proto.AccessToken
	values map[int64]string
}

func newFakeTokens() *fakeTokens {
	return &fakeTokens{tokens: map[int64]proto.AccessToken{}, values: map[int64]string{}}
}

func (f *fakeTokens) CreateAccessToken(_ context.Context, user proto.User, name string, expiresAt time.Time) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	value := "tok-" + strings.Repeat("x", int(f.nextID)) // unique per token
	f.tokens[f.nextID] = proto.AccessToken{ID: f.nextID, Name: name, UserID: user.ID(), ExpiresAt: expiresAt}
	f.values[f.nextID] = value
	return value, nil
}

func (f *fakeTokens) ListAccessTokens(_ context.Context, user proto.User) ([]proto.AccessToken, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []proto.AccessToken
	for _, t := range f.tokens {
		if t.UserID == user.ID() {
			out = append(out, t)
		}
	}
	return out, nil
}

func (f *fakeTokens) DeleteAccessToken(_ context.Context, user proto.User, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.tokens[id]
	if !ok || t.UserID != user.ID() {
		return proto.ErrTokenNotFound
	}
	delete(f.tokens, id)
	delete(f.values, id)
	return nil
}

func (f *fakeTokens) named(name string) []proto.AccessToken {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []proto.AccessToken
	for _, t := range f.tokens {
		if t.Name == name {
			out = append(out, t)
		}
	}
	return out
}

// execCall records one exec invocation.
type execCall struct {
	sandbox string
	command []string
	opts    v1.ExecOptions
}

// fakeExec simulates a sandbox filesystem for the commands Start runs. The
// SDK's fake exec client is Unimplemented, so tests supply their own.
type fakeExec struct {
	mu          sync.Mutex
	dirs        map[string]bool // sandbox + ":" + dir
	runs        []execCall
	interactive []execCall
	cloneExit   int
}

func newFakeExec() *fakeExec { return &fakeExec{dirs: map[string]bool{}} }

func (f *fakeExec) Run(_ context.Context, _, sandbox string, command []string, opts ...v1.ExecOptions) (*types.ExecResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var o v1.ExecOptions
	if len(opts) > 0 {
		o = opts[0]
	}
	f.runs = append(f.runs, execCall{sandbox, command, o})
	switch {
	case len(command) == 3 && command[0] == "test":
		if f.dirs[sandbox+":"+command[2]] {
			return &types.ExecResult{ExitCode: 0}, nil
		}
		return &types.ExecResult{ExitCode: 1}, nil
	case command[0] == "git" || command[0] == "jj":
		if f.cloneExit != 0 {
			return &types.ExecResult{ExitCode: f.cloneExit, Stderr: []byte("fatal: boom")}, nil
		}
		f.dirs[sandbox+":"+command[len(command)-1]] = true
	}
	return &types.ExecResult{}, nil
}

func (f *fakeExec) Stream(context.Context, string, string, []string, ...v1.ExecOptions) (v1.ExecStream, error) {
	return nil, errors.New("unused")
}

func (f *fakeExec) Interactive(_ context.Context, _, sandbox string, command []string, _, _ uint32, opts ...v1.ExecOptions) (v1.InteractiveSession, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.interactive = append(f.interactive, execCall{sandbox, command, opts[0]})
	return newFakeInteractive(), nil
}

func (f *fakeExec) clones() []execCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []execCall
	for _, r := range f.runs {
		if r.command[0] == "git" || r.command[0] == "jj" {
			out = append(out, r)
		}
	}
	return out
}

type harness struct {
	fc     *fake.Client
	exec   *fakeExec
	tokens *fakeTokens
	mgr    *Manager
}

func newHarnessConfig() *config.Config {
	cfg := config.DefaultConfig()
	cfg.HTTP.PublicURL = "https://git.example.com"
	cfg.OpenShell.CloneURL = "http://soft-serve.svc:23232"
	cfg.OpenShell.Image = "default-image"
	return cfg
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	cfg := newHarnessConfig()
	fc := fake.NewClient()
	t.Cleanup(func() { _ = fc.Close() })
	ex := newFakeExec()
	tokens := newFakeTokens()
	mgr := NewManager(newSDKClient("default", fc.Sandboxes(), ex, nil), tokens, cfg)
	return &harness{fc: fc, exec: ex, tokens: tokens, mgr: mgr}
}

var alice = testUser{id: 1, name: "alice"}

func TestStartNewSession(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	var progress bytes.Buffer

	s, err := h.mgr.Start(ctx, StartOptions{User: alice, Repo: "r", VCS: proto.VCSJJ, Term: "xterm", Progress: &progress})
	if err != nil {
		t.Fatal(err)
	}
	if s.Name != "soft-alice-r" || !s.Created || !s.Cloned || s.WorkDir != "/sandbox/r" {
		t.Fatalf("unexpected session: %+v", s)
	}
	if strings.Join(s.Command, " ") != "bash -l" {
		t.Fatalf("command = %q", s.Command)
	}
	if s.Env["TERM"] != "xterm" {
		t.Fatalf("TERM = %q", s.Env["TERM"])
	}

	sb, err := h.fc.Sandboxes().Get(ctx, "default", s.Name)
	if err != nil {
		t.Fatal(err)
	}
	if sb.Status.Phase != types.SandboxReady || sb.Spec.Template.Image != "default-image" {
		t.Fatalf("sandbox = %+v", sb)
	}
	if sb.Labels[LabelUserID] != "1" || sb.Annotations[AnnotationRepo] != "r" {
		t.Fatalf("labels=%v annotations=%v", sb.Labels, sb.Annotations)
	}

	// Token issued under the sandbox's name.
	toks := h.tokens.named("openshell:soft-alice-r")
	if len(toks) != 1 || toks[0].ExpiresAt.IsZero() {
		t.Fatalf("tokens = %+v", toks)
	}
	token := h.tokens.values[toks[0].ID]

	// jj repos clone colocated, with credentials in env only.
	clones := h.exec.clones()
	if len(clones) != 1 {
		t.Fatalf("clones = %d", len(clones))
	}
	want := []string{"jj", "git", "clone", "--colocate", "http://soft-serve.svc:23232/r.git", "/sandbox/r"}
	if strings.Join(clones[0].command, " ") != strings.Join(want, " ") {
		t.Fatalf("clone = %q, want %q", clones[0].command, want)
	}
	for _, arg := range clones[0].command {
		if strings.Contains(arg, token) || strings.Contains(arg, "@") {
			t.Fatalf("credentials leaked into clone argument %q", arg)
		}
	}
	env := clones[0].opts.Env
	wantHdr := "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("alice:"+token))
	if env["GIT_CONFIG_COUNT"] != "1" ||
		env["GIT_CONFIG_KEY_0"] != "http.http://soft-serve.svc:23232/.extraHeader" ||
		env["GIT_CONFIG_VALUE_0"] != wantHdr {
		t.Fatalf("clone env = %v", env)
	}

	// Token never lands in the sandbox spec (which persists on the gateway).
	for k, v := range sb.Spec.Environment {
		if strings.Contains(v, token) {
			t.Fatalf("token in sandbox spec env %s", k)
		}
	}
	if sb.Spec.Template != nil {
		for k, v := range sb.Spec.Template.Environment {
			if strings.Contains(v, token) {
				t.Fatalf("token in sandbox template env %s", k)
			}
		}
	}

	// Attach passes the env and workdir on the exec call.
	is, err := h.mgr.Attach(ctx, s, 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	_ = is.Close()
	call := h.exec.interactive[0]
	if call.opts.WorkDir != "/sandbox/r" || call.opts.Env["GIT_CONFIG_VALUE_0"] != wantHdr {
		t.Fatalf("interactive opts = %+v", call.opts)
	}
}

func TestDetachKeepsSandboxRunning(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)

	s, err := h.mgr.Start(ctx, StartOptions{User: alice, Repo: "r"})
	if err != nil {
		t.Fatal(err)
	}
	is, err := h.mgr.Attach(ctx, s, 80, 24)
	if err != nil {
		t.Fatal(err)
	}

	// The user exits the bridged shell.
	term := newFakeTerm()
	_ = term.inW.Close()
	is.(*fakeInteractive).exit <- 0
	if code, err := Bridge(ctx, term, is); err != nil || code != 0 {
		t.Fatalf("bridge: code=%d err=%v", code, err)
	}

	// The sandbox and its token survive; start re-attaches.
	sb, err := h.fc.Sandboxes().Get(ctx, "default", s.Name)
	if err != nil || sb.Status.Phase != types.SandboxReady {
		t.Fatalf("sandbox after detach: sb=%v err=%v", sb, err)
	}
	if n := len(h.tokens.named(TokenName(s.Name))); n != 1 {
		t.Fatalf("tokens after detach = %d, want 1", n)
	}
	again, err := h.mgr.Start(ctx, StartOptions{User: alice, Repo: "r"})
	if err != nil || again.Created || again.Cloned {
		t.Fatalf("re-attach: s=%+v err=%v", again, err)
	}
}

func TestStartTwiceAttachesWithoutRecloning(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)

	first, err := h.mgr.Start(ctx, StartOptions{User: alice, Repo: "r", VCS: proto.VCSGit})
	if err != nil {
		t.Fatal(err)
	}
	second, err := h.mgr.Start(ctx, StartOptions{User: alice, Repo: "r", VCS: proto.VCSGit})
	if err != nil {
		t.Fatal(err)
	}
	if first.Name != second.Name {
		t.Fatalf("names differ: %q vs %q", first.Name, second.Name)
	}
	if second.Created || second.Cloned {
		t.Fatalf("second start created=%v cloned=%v", second.Created, second.Cloned)
	}
	clones := h.exec.clones()
	if len(clones) != 1 || clones[0].command[0] != "git" {
		t.Fatalf("clones = %+v", clones)
	}
}

func TestStartResumesStoppedSandbox(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.fc.AddSandbox("default", &types.Sandbox{
		Name:   "soft-alice-r",
		Labels: ownerLabels(alice),
		Status: types.SandboxStatus{Phase: types.SandboxStopped},
	})
	h.exec.dirs["soft-alice-r:/sandbox/r"] = true

	var progress bytes.Buffer
	s, err := h.mgr.Start(ctx, StartOptions{User: alice, Repo: "r", Image: "custom", Progress: &progress})
	if err != nil {
		t.Fatal(err)
	}
	if s.Created || s.Cloned {
		t.Fatalf("created=%v cloned=%v", s.Created, s.Cloned)
	}
	sb, _ := h.fc.Sandboxes().Get(ctx, "default", "soft-alice-r")
	if sb.Status.Phase != types.SandboxReady {
		t.Fatalf("phase = %s", sb.Status.Phase)
	}
	if !strings.Contains(progress.String(), "--image ignored") {
		t.Fatalf("missing --image warning: %q", progress.String())
	}
}

func TestStartCustomImage(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	if _, err := h.mgr.Start(ctx, StartOptions{User: alice, Repo: "r", Image: "custom"}); err != nil {
		t.Fatal(err)
	}
	sb, _ := h.fc.Sandboxes().Get(ctx, "default", "soft-alice-r")
	if sb.Spec.Template.Image != "custom" {
		t.Fatalf("image = %q", sb.Spec.Template.Image)
	}
}

func TestStartNestedRepoCreatesParent(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	s, err := h.mgr.Start(ctx, StartOptions{User: alice, Repo: "org/r", VCS: proto.VCSGit})
	if err != nil {
		t.Fatal(err)
	}
	if s.WorkDir != "/sandbox/org/r" {
		t.Fatalf("workdir = %q", s.WorkDir)
	}
	var sawMkdir bool
	for _, r := range h.exec.runs {
		if strings.Join(r.command, " ") == "mkdir -p /sandbox/org" {
			sawMkdir = true
		}
	}
	if !sawMkdir {
		t.Fatal("parent directory not created")
	}
}

func TestStartCloneFailure(t *testing.T) {
	h := newHarness(t)
	h.exec.cloneExit = 128
	_, err := h.mgr.Start(context.Background(), StartOptions{User: alice, Repo: "r"})
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v", err)
	}
}

func TestStartFailedCreateCleansUp(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.mgr.client = newSDKClient("default", failingWait{h.fc.Sandboxes()}, h.exec, nil)
	if _, err := h.mgr.Start(ctx, StartOptions{User: alice, Repo: "r"}); err == nil {
		t.Fatal("expected error")
	}
	if _, err := h.fc.Sandboxes().Get(ctx, "default", "soft-alice-r"); !IsNotFound(err) {
		t.Fatalf("sandbox left behind: %v", err)
	}
	if len(h.exec.clones()) != 0 {
		t.Fatal("cloned into a failed sandbox")
	}
}

func TestStartRejectsForeignSandbox(t *testing.T) {
	h := newHarness(t)
	h.fc.AddSandbox("default", &types.Sandbox{
		Name:   "soft-alice-r",
		Labels: map[string]string{LabelUserID: "99"},
		Status: types.SandboxStatus{Phase: types.SandboxReady},
	})
	if _, err := h.mgr.Start(context.Background(), StartOptions{User: alice, Repo: "r"}); !errors.Is(err, ErrNotOwned) {
		t.Fatalf("err = %v", err)
	}
}

func TestStartAnonymous(t *testing.T) {
	h := newHarness(t)
	if _, err := h.mgr.Start(context.Background(), StartOptions{Repo: "r"}); !errors.Is(err, proto.ErrUnauthorized) {
		t.Fatalf("err = %v", err)
	}
}

func TestStartPrunesExpiredTokens(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	if _, err := h.mgr.Start(ctx, StartOptions{User: alice, Repo: "r"}); err != nil {
		t.Fatal(err)
	}
	// Jump past the TTL: the first token is expired and gets pruned.
	h.mgr.now = func() time.Time { return time.Now().Add(48 * time.Hour) }
	if _, err := h.mgr.Start(ctx, StartOptions{User: alice, Repo: "r"}); err != nil {
		t.Fatal(err)
	}
	if n := len(h.tokens.named("openshell:soft-alice-r")); n != 1 {
		t.Fatalf("tokens = %d, want 1", n)
	}
}

// A second attach within the TTL must not revoke the token an earlier
// attachment (and its agents) is still using.
func TestStartKeepsUnexpiredTokens(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	if _, err := h.mgr.Start(ctx, StartOptions{User: alice, Repo: "r"}); err != nil {
		t.Fatal(err)
	}
	first := h.tokens.named("openshell:soft-alice-r")
	if len(first) != 1 {
		t.Fatalf("tokens after first start = %d", len(first))
	}
	if _, err := h.mgr.Start(ctx, StartOptions{User: alice, Repo: "r"}); err != nil {
		t.Fatal(err)
	}
	toks := h.tokens.named("openshell:soft-alice-r")
	if len(toks) != 2 {
		t.Fatalf("tokens after second start = %d, want 2", len(toks))
	}
	var kept bool
	for _, tok := range toks {
		kept = kept || tok.ID == first[0].ID
	}
	if !kept {
		t.Fatal("first attachment's token was revoked")
	}
}

func TestEndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	// An unrelated token must survive.
	if _, err := h.tokens.CreateAccessToken(ctx, alice, "laptop", time.Time{}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.mgr.Start(ctx, StartOptions{User: alice, Repo: "r"}); err != nil {
		t.Fatal(err)
	}

	existed, err := h.mgr.End(ctx, alice, "r")
	if err != nil || !existed {
		t.Fatalf("end: existed=%v err=%v", existed, err)
	}
	if _, err := h.fc.Sandboxes().Get(ctx, "default", "soft-alice-r"); !IsNotFound(err) {
		t.Fatalf("sandbox not deleted: %v", err)
	}
	if n := len(h.tokens.named("openshell:soft-alice-r")); n != 0 {
		t.Fatalf("token not revoked: %d left", n)
	}
	if n := len(h.tokens.named("laptop")); n != 1 {
		t.Fatal("unrelated token revoked")
	}

	existed, err = h.mgr.End(ctx, alice, "r")
	if err != nil || existed {
		t.Fatalf("second end: existed=%v err=%v", existed, err)
	}
}

func TestEndRejectsForeignSandbox(t *testing.T) {
	h := newHarness(t)
	h.fc.AddSandbox("default", &types.Sandbox{
		Name:   "soft-alice-r",
		Labels: map[string]string{LabelUserID: "99"},
	})
	if _, err := h.mgr.End(context.Background(), alice, "r"); !errors.Is(err, ErrNotOwned) {
		t.Fatalf("err = %v", err)
	}
}

func TestList(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	bob := testUser{id: 2, name: "bob"}
	for _, r := range []string{"zeta", "alpha"} {
		if _, err := h.mgr.Start(ctx, StartOptions{User: alice, Repo: r}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.mgr.Start(ctx, StartOptions{User: bob, Repo: "alpha"}); err != nil {
		t.Fatal(err)
	}
	// A sandbox not created by Soft Serve is ignored.
	h.fc.AddSandbox("default", &types.Sandbox{Name: "other"})

	infos, err := h.mgr.List(ctx, alice)
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 2 || infos[0].Repo != "alpha" || infos[1].Repo != "zeta" {
		t.Fatalf("infos = %+v", infos)
	}
	if infos[0].Name != "soft-alice-alpha" || infos[0].Phase != string(types.SandboxReady) {
		t.Fatalf("info = %+v", infos[0])
	}
}

// fakeInteractive is an InteractiveSession whose output, exit code and
// recorded input/resizes are controlled by the test.
type fakeInteractive struct {
	outR *io.PipeReader
	outW *io.PipeWriter
	exit chan int

	mu       sync.Mutex
	input    bytes.Buffer
	resizes  [][2]uint32
	inClosed bool
}

func newFakeInteractive() *fakeInteractive {
	r, w := io.Pipe()
	return &fakeInteractive{outR: r, outW: w, exit: make(chan int, 1)}
}

func (f *fakeInteractive) Read(p []byte) (int, error) { return f.outR.Read(p) }
func (f *fakeInteractive) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.input.Write(p)
}

func (f *fakeInteractive) Resize(cols, rows uint32) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resizes = append(f.resizes, [2]uint32{cols, rows})
	return nil
}

func (f *fakeInteractive) ExitCode() (int, error) {
	code := <-f.exit
	_ = f.outW.Close()
	return code, nil
}

func (f *fakeInteractive) CloseWrite() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inClosed = true
	return nil
}

func (f *fakeInteractive) Cancel() error {
	f.exit <- -1
	return nil
}
func (f *fakeInteractive) Close() error { return f.outR.Close() }

func (f *fakeInteractive) snapshot() (string, [][2]uint32, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.input.String(), append([][2]uint32(nil), f.resizes...), f.inClosed
}

// fakeTerm is the client side of the bridge.
type fakeTerm struct {
	inR   *io.PipeReader
	inW   *io.PipeWriter
	winch chan ssh.Window

	mu  sync.Mutex
	out bytes.Buffer
}

func newFakeTerm() *fakeTerm {
	r, w := io.Pipe()
	return &fakeTerm{inR: r, inW: w, winch: make(chan ssh.Window, 1)}
}

func (f *fakeTerm) Read(p []byte) (int, error) { return f.inR.Read(p) }
func (f *fakeTerm) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.out.Write(p)
}

func (f *fakeTerm) Pty() (ssh.Pty, <-chan ssh.Window, bool) {
	return ssh.Pty{Term: "xterm", Window: ssh.Window{Width: 80, Height: 24}}, f.winch, true
}

func (f *fakeTerm) output() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.out.String()
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestBridge(t *testing.T) {
	term := newFakeTerm()
	is := newFakeInteractive()

	type result struct {
		code int
		err  error
	}
	done := make(chan result, 1)
	go func() {
		code, err := Bridge(context.Background(), term, is)
		done <- result{code, err}
	}()

	// Output flows sandbox → terminal, CRLF untouched.
	if _, err := is.outW.Write([]byte("prompt$ \r\n")); err != nil {
		t.Fatal(err)
	}
	eventually(t, "output", func() bool { return term.output() == "prompt$ \r\n" })

	// Input flows terminal → sandbox.
	if _, err := term.inW.Write([]byte("ls\r")); err != nil {
		t.Fatal(err)
	}
	eventually(t, "input", func() bool { in, _, _ := is.snapshot(); return in == "ls\r" })

	// Window changes resize the sandbox PTY.
	term.winch <- ssh.Window{Width: 120, Height: 40}
	eventually(t, "resize", func() bool {
		_, rs, _ := is.snapshot()
		return len(rs) == 1 && rs[0] == [2]uint32{120, 40}
	})

	// Client EOF closes the remote input.
	_ = term.inW.Close()
	eventually(t, "input close", func() bool { _, _, closed := is.snapshot(); return closed })

	// The remote exit code is returned.
	is.exit <- 3
	select {
	case r := <-done:
		if r.err != nil || r.code != 3 {
			t.Fatalf("code=%d err=%v", r.code, r.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Bridge did not return")
	}
}

func TestBridgeCancelOnDisconnect(t *testing.T) {
	term := newFakeTerm()
	is := newFakeInteractive()
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan int, 1)
	go func() {
		code, _ := Bridge(ctx, term, is)
		done <- code
	}()
	cancel()
	select {
	case code := <-done:
		if code != -1 {
			t.Fatalf("code = %d", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Bridge did not return after cancel")
	}
}
