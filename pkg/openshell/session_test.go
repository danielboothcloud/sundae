package openshell

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/ssh"
	v1 "github.com/NVIDIA/OpenShell/sdk/go/openshell/v1"
	"github.com/NVIDIA/OpenShell/sdk/go/openshell/v1/fake"
	"github.com/NVIDIA/OpenShell/sdk/go/openshell/v1/types"
	"github.com/danielboothcloud/sundae/pkg/config"
	"github.com/danielboothcloud/sundae/pkg/proto"
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

// newest returns the value of the newest token named name.
func (f *fakeTokens) newest(name string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var id int64
	for _, t := range f.tokens {
		if t.Name == name && t.ID > id {
			id = t.ID
		}
	}
	return f.values[id]
}

// execCall records one exec invocation.
type execCall struct {
	sandbox string
	command []string
	opts    v1.ExecOptions
}

// script returns the name of a `sh -c script name args...` call, or "".
func (c execCall) script() string {
	if len(c.command) >= 4 && c.command[0] == "sh" && c.command[1] == "-c" {
		return c.command[3]
	}
	return ""
}

// args returns the arguments after a script's name.
func (c execCall) args() []string { return c.command[4:] }

// fakeSandbox is the state of one simulated sandbox.
type fakeSandbox struct {
	files    map[string]string
	dirs     map[string]bool
	includes []string // ~/.gitconfig include.path entries
	server   bool     // an osewa server is running
	servers  int      // osewa servers started
	sessions map[string]bool
	nextID   int
	// newArgv is the working directory and argv of the last osewa
	// session created, and newEnvFile where its environment was staged
	// ("" for none).
	newArgv    []string
	newEnvFile string
	// identity maps a checkout to the git identity set in it, as
	// "name <email>", and jjIdentity to the TOML values given to jj.
	identity   map[string]string
	jjIdentity map[string]string
}

// fakeExec simulates the sandboxes the commands Start runs act on. The
// SDK's fake exec client is Unimplemented, so tests supply their own.
type fakeExec struct {
	mu          sync.Mutex
	boxes       map[string]*fakeSandbox
	runs        []execCall
	interactive []execCall
	cloneExit   int
	// imageOsewa is the version of the osewa on the image's PATH; "" for
	// none.
	imageOsewa string
	// arch is what uname -m prints.
	arch string
	// probeExit makes the probe fail.
	probeExit int
	// identityExit makes the git identity script fail.
	identityExit int
	// snap configures the snapshot scripts (see snapshot_test.go).
	snap fakeSnapshotKnobs
}

func newFakeExec() *fakeExec {
	return &fakeExec{boxes: map[string]*fakeSandbox{}, imageOsewa: OsewaVersion, arch: "x86_64"}
}

// box returns the state of sandbox. Call with mu held, or before use.
func (f *fakeExec) box(sandbox string) *fakeSandbox {
	b, ok := f.boxes[sandbox]
	if !ok {
		b = &fakeSandbox{
			files: map[string]string{}, dirs: map[string]bool{}, sessions: map[string]bool{},
			identity: map[string]string{}, jjIdentity: map[string]string{},
		}
		f.boxes[sandbox] = b
	}
	return b
}

func ok(stdout string) (*types.ExecResult, error) {
	return &types.ExecResult{ExitCode: 0, Stdout: []byte(stdout)}, nil
}

func fail(code int, stderr string) (*types.ExecResult, error) {
	return &types.ExecResult{ExitCode: code, Stderr: []byte(stderr)}, nil
}

// gatewayLimits checks an exec request against what the OpenShell 0.1.8
// gateway accepts (openshell-server grpc/validation.rs and
// build_remote_exec_command), and what Linux accepts of the one
// `<shell> -c <command>` argument the supervisor then runs.
func gatewayLimits(command []string, o v1.ExecOptions) error {
	invalid := func(format string, args ...any) error {
		return &types.StatusError{Code: types.ErrorInvalidArgument, Message: fmt.Sprintf(format, args...)}
	}
	if len(command) > 1024 {
		return invalid("command array exceeds 1024 argument limit")
	}
	line := 0
	for i, arg := range command {
		if len(arg) > 32<<10 {
			return invalid("command argument %d exceeds 32768 byte limit", i)
		}
		if strings.ContainsRune(arg, 0) {
			return invalid("command[%d] contains null bytes", i)
		}
		line += len(arg) + 3 + strings.Count(arg, "'")*4
	}
	for k, v := range o.Env {
		if !regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`).MatchString(k) || strings.HasPrefix(k, "OPENSHELL_") {
			return invalid("environment key %q", k)
		}
		if len(v) > 32<<10 {
			return invalid("environment value for %q exceeds 32768 byte limit", k)
		}
		if strings.ContainsAny(v, "\x00\r\n") {
			return invalid("environment value for %q contains a control character", k)
		}
		line += len(k) + len(v) + 4
	}
	if strings.ContainsAny(o.WorkDir, "\x00\r\n") {
		return invalid("workdir contains a control character")
	}
	if line+len(o.WorkDir) > 128<<10 {
		return invalid("command line of %d bytes exceeds MAX_ARG_STRLEN", line)
	}
	return nil
}

// env returns the decoded value of a base64 exec environment variable.
func env(o v1.ExecOptions, k string) string {
	b, err := base64.StdEncoding.DecodeString(o.Env[k])
	if err != nil {
		panic(fmt.Sprintf("%s is not base64: %v", k, err))
	}
	return string(b)
}

func (f *fakeExec) Run(_ context.Context, _, sandbox string, command []string, opts ...v1.ExecOptions) (*types.ExecResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var o v1.ExecOptions
	if len(opts) > 0 {
		o = opts[0]
	}
	if err := gatewayLimits(command, o); err != nil {
		return nil, err
	}
	call := execCall{sandbox, command, o}
	f.runs = append(f.runs, call)
	b := f.box(sandbox)

	switch {
	case len(command) == 3 && command[0] == "test":
		if b.dirs[command[2]] {
			return ok("")
		}
		return fail(1, "")
	case command[0] == "mkdir":
		b.dirs[command[len(command)-1]] = true
		return ok("")
	case command[0] == "git" || command[0] == "jj":
		if f.cloneExit != 0 {
			return fail(f.cloneExit, "fatal: boom")
		}
		b.dirs[command[len(command)-1]] = true
		return ok("")
	}

	if call.script() == "" {
		return nil, fmt.Errorf("fakeExec: unexpected command %q", command)
	}
	a := call.args()
	switch call.script() {
	case uploadBeginName:
		b.files[a[0]] = ""
	case uploadChunkName:
		b.files[a[0]] += strings.Join(a[1:], "")
	case uploadEndName:
		raw, err := base64.StdEncoding.DecodeString(b.files[a[0]])
		if err != nil {
			return fail(1, "base64: invalid input")
		}
		delete(b.files, a[0])
		b.files[a[1]] = string(raw)
	case osewaProbeName:
		if f.probeExit != 0 {
			return fail(f.probeExit, "probe failed")
		}
		bin := ""
		if f.imageOsewa == a[0] {
			bin = "/usr/local/bin/osewa"
		} else if b.files[a[1]] == "osewa "+a[0] {
			bin = a[1]
		}
		enrolled := "no"
		if b.files[a[2]+"/client.key"] != "" && b.files[a[2]+"/hosts.json"] != "" {
			enrolled = "yes"
		}
		return ok(fmt.Sprintf("osewa=%s\narch=%s\nenrolled=%s\n", bin, f.arch, enrolled))
	case osewaInstallName:
		gz := b.files[a[0]]
		sum := sha256.Sum256([]byte(gz))
		if hex.EncodeToString(sum[:]) != a[1] {
			return fail(1, a[0]+": FAILED")
		}
		zr, err := gzip.NewReader(strings.NewReader(gz))
		if err != nil {
			return fail(1, "gzip: not in gzip format")
		}
		raw, _ := io.ReadAll(zr)
		b.files[a[2]] = string(raw)
		delete(b.files, a[0])
	case osewaServerName:
		if b.server {
			return ok("osewa server: already running (pid 42)\n")
		}
		b.server = true
		b.servers++
		return ok("osewa server: started (pid 42, " + a[1] + ")\n")
	case osewaEnrollName:
		b.files[a[1]+"/client.key"] = env(o, "SUNDAE_OSEWA_KEY")
		b.files["devices"] += fmt.Sprintf(`{"name":%q,"key":%q}`+"\n", a[3], a[4])
		return ok(`{"server_key":"c2VydmVyLWtleQ==","port":` + a[2] + `,"version":0}` + "\n")
	case osewaHostsName:
		b.files[a[0]+"/hosts.json"] = env(o, "SUNDAE_OSEWA_HOSTS")
	case osewaListName:
		var ls struct {
			Sessions []osewaSessionInfo `json:"sessions"`
		}
		for id, running := range b.sessions {
			ls.Sessions = append(ls.Sessions, osewaSessionInfo{ID: id, Exited: !running})
		}
		out, _ := json.Marshal(ls)
		return ok(strings.TrimSpace(b.files[a[1]]) + "\n" + string(out) + "\n")
	case osewaNewName:
		idf, seen := a[1], a[2]
		if cur := strings.TrimSpace(b.files[idf]); cur != seen {
			return ok("existing " + cur + "\n")
		}
		b.nextID++
		id := fmt.Sprintf("osewa%d", b.nextID)
		b.sessions[id] = true
		b.files[idf] = id + "\n"
		b.newArgv = append([]string{a[3]}, a[5:]...)
		b.newEnvFile = a[4]
		return ok("created " + id + "\n")
	case gitCredentialsName:
		b.files[a[1]] = env(o, "SUNDAE_GITCONFIG")
		found := false
		for _, inc := range b.includes {
			found = found || inc == a[1]
		}
		if !found {
			b.includes = append(b.includes, a[1])
		}
	case gitIdentityCloneName:
		if f.cloneExit != 0 {
			return fail(f.cloneExit, "fatal: boom")
		}
		b.dirs[a[2]] = true
		b.identity[a[2]] = env(o, "SUNDAE_GIT_USER_NAME") + " <" + env(o, "SUNDAE_GIT_USER_EMAIL") + ">"
		if a[0] == "jj" {
			b.jjIdentity[a[2]] = env(o, "SUNDAE_JJ_USER_NAME") + " " + env(o, "SUNDAE_JJ_USER_EMAIL")
		}
	case gitIdentityName:
		if f.identityExit != 0 {
			return fail(f.identityExit, "error: could not lock config file")
		}
		if b.dirs[a[0]] {
			b.identity[a[0]] = env(o, "SUNDAE_GIT_USER_NAME") + " <" + env(o, "SUNDAE_GIT_USER_EMAIL") + ">"
			b.jjIdentity[a[0]] = env(o, "SUNDAE_JJ_USER_NAME") + " " + env(o, "SUNDAE_JJ_USER_EMAIL")
		}
	case readReposName:
		return ok(b.files[a[0]])
	case writeReposName:
		b.files[a[0]] = env(o, "SUNDAE_REPOS")
	case snapshotPackName, snapshotUploadName, snapshotRestoreName, snapshotProbeName:
		return f.snapshotScript(b, call)
	default:
		return nil, fmt.Errorf("fakeExec: unexpected script %q", call.script())
	}
	return ok("")
}

func (f *fakeExec) Stream(context.Context, string, string, []string, ...v1.ExecOptions) (v1.ExecStream, error) {
	return nil, errors.New("unused")
}

func (f *fakeExec) Interactive(_ context.Context, _, sandbox string, command []string, _, _ uint32, opts ...v1.ExecOptions) (v1.InteractiveSession, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := gatewayLimits(command, opts[0]); err != nil {
		return nil, err
	}
	f.interactive = append(f.interactive, execCall{sandbox, command, opts[0]})
	return newFakeInteractive(), nil
}

func (f *fakeExec) clones() []execCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []execCall
	for _, r := range f.runs {
		if r.command[0] == "git" || r.command[0] == "jj" || r.script() == gitIdentityCloneName {
			out = append(out, r)
		}
	}
	return out
}

// scripts returns the names of the scripts run in sandbox, in order.
func (f *fakeExec) scripts(sandbox string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, r := range f.runs {
		if r.sandbox == sandbox && r.script() != "" {
			out = append(out, r.script())
		}
	}
	return out
}

// count returns how often the named script ran in sandbox.
func (f *fakeExec) count(sandbox, name string) int {
	n := 0
	for _, s := range f.scripts(sandbox) {
		if s == name {
			n++
		}
	}
	return n
}

// last returns the last run of the named script in sandbox.
func (f *fakeExec) last(sandbox, name string) execCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.runs) - 1; i >= 0; i-- {
		if r := f.runs[i]; r.sandbox == sandbox && r.script() == name {
			return r
		}
	}
	return execCall{}
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
	cfg.OpenShell.CloneURL = "http://sundae.svc:23232"
	cfg.OpenShell.Image = "default-image"
	return cfg
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	return newHarnessWith(t, newHarnessConfig())
}

func newHarnessWith(t *testing.T, cfg *config.Config) *harness {
	t.Helper()
	fc := fake.NewClient()
	t.Cleanup(func() { _ = fc.Close() })
	ex := newFakeExec()
	tokens := newFakeTokens()
	mgr := NewManager(newSDKClient("default", fc.Sandboxes(), ex, fc.Files(), nil), tokens, cfg)
	return &harness{fc: fc, exec: ex, tokens: tokens, mgr: mgr}
}

// withOsewaBuilds gives the harness's manager a binary dir holding a fake
// osewa build for each architecture, and pins their checksums.
func withOsewaBuilds(t *testing.T, h *harness) {
	t.Helper()
	dir := t.TempDir()
	sums := map[string]string{}
	for _, arch := range []string{"amd64", "arm64"} {
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		_, _ = zw.Write([]byte("osewa " + OsewaVersion))
		_ = zw.Close()
		// Trailing bytes after the gzip stream make it take several
		// upload execs (8 chunks, 3 to an exec); gzip readers ignore them.
		data := append(buf.Bytes(), bytes.Repeat([]byte{0}, 7*uploadChunk+17)...)
		if err := os.WriteFile(filepath.Join(dir, "osewa-linux-"+arch+".gz"), data, 0o600); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(data)
		sums[arch] = hex.EncodeToString(sum[:])
	}
	h.mgr.osewaBinaryDir = dir
	h.mgr.osewaSHA256 = sums
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
	if s.Name != DefaultSessionName || s.Sandbox != SessionSandboxName("alice", "main") || !s.Created || len(s.Cloned) == 0 || s.WorkDir != "/sandbox/r" {
		t.Fatalf("unexpected session: %+v", s)
	}
	if strings.Join(s.Command, " ") != "bash -l" || !s.Started || s.OsewaID != "osewa1" {
		t.Fatalf("program: %+v", s)
	}
	if s.Env["TERM"] != "xterm" || s.Env["OSEWA_CONFIG_DIR"] != osewaClientDir {
		t.Fatalf("env = %v", s.Env)
	}

	sb, err := h.fc.Sandboxes().Get(ctx, "default", s.Sandbox)
	if err != nil {
		t.Fatal(err)
	}
	if sb.Status.Phase != types.SandboxReady || sb.Spec.Template.Image != "default-image" {
		t.Fatalf("sandbox = %+v", sb)
	}
	if sb.Labels[LabelUserID] != "1" || sb.Annotations[AnnotationRepo] != "r" ||
		sb.Annotations[AnnotationUser] != "alice" || sb.Annotations[AnnotationSession] != "main" {
		t.Fatalf("labels=%v annotations=%v", sb.Labels, sb.Annotations)
	}
	if _, ok := sb.Annotations[AnnotationProfile]; ok {
		t.Fatalf("profile annotation without a profile: %v", sb.Annotations)
	}

	// The bootstrap ran in order: credentials before the clone, then osewa.
	want := []string{gitCredentialsName, osewaProbeName, osewaServerName, osewaEnrollName, osewaHostsName, osewaListName, osewaNewName}
	if got := h.exec.scripts(s.Sandbox); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("scripts = %v, want %v", got, want)
	}

	// The program runs in the checkout, through a login shell.
	newCall := h.exec.last(s.Sandbox, osewaNewName)
	if !strings.Contains(newCall.command[2], `cd "$1" || exit; shift 2; exec "$@"' sundae-session "$dir" "$envf" "$@"`) {
		t.Fatalf("new script = %q", newCall.command[2])
	}
	// Without providers, no environment is staged for it.
	if got := strings.Join(h.exec.boxes[s.Sandbox].newArgv, " "); got != "/sandbox/r bash -l" || h.exec.boxes[s.Sandbox].newEnvFile != "" {
		t.Fatalf("osewa new argv = %q, env file %q", got, h.exec.boxes[s.Sandbox].newEnvFile)
	}

	// jj repos clone colocated, without credentials in the URL or the env.
	clones := h.exec.clones()
	if len(clones) != 1 {
		t.Fatalf("clones = %d", len(clones))
	}
	wantClone := []string{"jj", "git", "clone", "--colocate", "http://sundae.svc:23232/r.git", "/sandbox/r"}
	if strings.Join(clones[0].command, " ") != strings.Join(wantClone, " ") {
		t.Fatalf("clone = %q, want %q", clones[0].command, wantClone)
	}
	if len(clones[0].opts.Env) != 1 || clones[0].opts.Env["HOME"] != SandboxRoot {
		t.Fatalf("clone env = %v", clones[0].opts.Env)
	}

	// Attach runs the osewa client against the session, with no
	// credentials.
	is, err := h.mgr.Attach(ctx, s, 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	_ = is.Close()
	call := h.exec.interactive[0]
	wantAttach := "/usr/local/bin/osewa attach --take --predict never sandbox osewa1"
	if strings.Join(call.command, " ") != wantAttach || call.opts.WorkDir != "/sandbox/r" || !call.opts.NoLoginShell {
		t.Fatalf("interactive = %q opts=%+v", call.command, call.opts)
	}
	if call.opts.Env["TERM"] != "xterm" || call.opts.Env["OSEWA_CONFIG_DIR"] != osewaClientDir || call.opts.Env["OSEWA_AUTO_UPDATE"] != "off" {
		t.Fatalf("attach env = %v", call.opts.Env)
	}
	if !strings.Contains(progress.String(), "Starting sandbox "+SessionSandboxName("alice", "main")) {
		t.Fatalf("progress = %q", progress.String())
	}
}

func TestGitCredentials(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)

	s, err := h.mgr.Start(ctx, StartOptions{User: alice, Repo: "r"})
	if err != nil {
		t.Fatal(err)
	}
	toks := h.tokens.named(TokenName(SessionSandboxName("alice", "main")))
	if len(toks) != 1 || toks[0].ExpiresAt.IsZero() {
		t.Fatalf("tokens = %+v", toks)
	}
	token := h.tokens.values[toks[0].ID]

	b := h.exec.boxes[s.Sandbox]
	basic := func(token string) string { return base64.StdEncoding.EncodeToString([]byte("alice:" + token)) }
	want := "# Written by Sundae on every session start and attach.\n" +
		"[http \"http://sundae.svc:23232/\"]\n" +
		"\textraHeader = Authorization: Basic " + basic(token) + "\n"
	if b.files[gitCredentialFile] != want {
		t.Fatalf("credential file = %q, want %q", b.files[gitCredentialFile], want)
	}
	if strings.Join(b.includes, ",") != gitCredentialFile {
		t.Fatalf("includes = %v", b.includes)
	}
	if !strings.HasPrefix(gitCredentialFile, "/tmp/") || strings.HasPrefix(gitCredentialFile, SandboxRoot+"/") {
		t.Fatalf("credential file %s must be outside %s", gitCredentialFile, SandboxRoot)
	}

	// The token is never in an argument, never in an exec env but the
	// credential write's, never in the attach env or the sandbox spec.
	assertTokenConfined := func(token string) {
		t.Helper()
		for _, secret := range []string{token, basic(token)} {
			for _, r := range h.exec.runs {
				for _, arg := range r.command {
					if strings.Contains(arg, secret) {
						t.Fatalf("token in argument of %q", r.command)
					}
				}
				for k, v := range r.opts.Env {
					decoded, _ := base64.StdEncoding.DecodeString(v)
					if (strings.Contains(v, secret) || strings.Contains(string(decoded), secret)) && (r.script() != gitCredentialsName || k != "SUNDAE_GITCONFIG") {
						t.Fatalf("token in env %s of %q", k, r.command)
					}
				}
			}
			for _, r := range h.exec.interactive {
				for k, v := range r.opts.Env {
					if strings.Contains(v, secret) {
						t.Fatalf("token in attach env %s", k)
					}
				}
			}
		}
	}
	if _, err := h.mgr.Attach(ctx, s, 80, 24); err != nil {
		t.Fatal(err)
	}
	assertTokenConfined(token)
	sb, _ := h.fc.Sandboxes().Get(ctx, "default", s.Sandbox)
	for k, v := range sb.Spec.Environment {
		if strings.Contains(v, token) {
			t.Fatalf("token in sandbox spec env %s", k)
		}
	}

	// Every start and attach writes a fresh token; the include is added
	// once.
	if _, err := h.mgr.Resume(ctx, StartOptions{User: alice, Repo: "r"}); err != nil {
		t.Fatal(err)
	}
	if n := len(h.tokens.named(TokenName(SessionSandboxName("alice", "main")))); n != 2 {
		t.Fatalf("tokens after attach = %d", n)
	}
	newest := h.tokens.newest(TokenName(SessionSandboxName("alice", "main")))
	if newest == token || !strings.Contains(b.files[gitCredentialFile], basic(newest)) {
		t.Fatalf("credential file not refreshed: %q", b.files[gitCredentialFile])
	}
	if len(b.includes) != 1 || h.exec.count(s.Sandbox, gitCredentialsName) != 2 {
		t.Fatalf("includes = %v, writes = %d", b.includes, h.exec.count(s.Sandbox, gitCredentialsName))
	}
	assertTokenConfined(newest)
}

func TestOsewaBootstrap(t *testing.T) {
	ctx := context.Background()

	t.Run("image provides osewa", func(t *testing.T) {
		h := newHarness(t)
		s, err := h.mgr.Start(ctx, StartOptions{User: alice, Repo: "r"})
		if err != nil {
			t.Fatal(err)
		}
		if s.Osewa != "/usr/local/bin/osewa" || h.exec.count(s.Sandbox, uploadBeginName) != 0 {
			t.Fatalf("osewa = %q, uploads = %d", s.Osewa, h.exec.count(s.Sandbox, uploadBeginName))
		}
		probe := h.exec.last(s.Sandbox, osewaProbeName)
		if strings.Join(probe.args(), " ") != OsewaVersion+" "+osewaPath()+" "+osewaClientDir {
			t.Fatalf("probe args = %q", probe.args())
		}
	})

	for name, image := range map[string]string{"missing": "", "mismatched": "0.0.1"} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			withOsewaBuilds(t, h)
			h.exec.imageOsewa = image
			h.exec.arch = "aarch64"
			var progress bytes.Buffer
			s, err := h.mgr.Start(ctx, StartOptions{User: alice, Repo: "r", Progress: &progress})
			if err != nil {
				t.Fatal(err)
			}
			if s.Osewa != osewaPath() || s.Osewa != "/sandbox/.local/lib/sundae/osewa-"+OsewaVersion {
				t.Fatalf("osewa = %q", s.Osewa)
			}
			// Uploaded in chunks, then verified and unpacked.
			if n := h.exec.count(s.Sandbox, uploadChunkName); n != 3 {
				t.Fatalf("chunks = %d, want 3", n)
			}
			install := h.exec.last(s.Sandbox, osewaInstallName)
			if strings.Join(install.args(), " ") != osewaPath()+".gz "+h.mgr.osewaSHA256["arm64"]+" "+osewaPath() {
				t.Fatalf("install args = %q", install.args())
			}
			if h.exec.boxes[s.Sandbox].files[osewaPath()] != "osewa "+OsewaVersion {
				t.Fatal("binary not installed")
			}
			if !strings.Contains(progress.String(), "Installing osewa "+OsewaVersion) {
				t.Fatalf("progress = %q", progress.String())
			}
			// The upload is used from then on.
			if _, err := h.mgr.Resume(ctx, StartOptions{User: alice, Repo: "r"}); err != nil {
				t.Fatal(err)
			}
			if n := h.exec.count(s.Sandbox, uploadBeginName); n != 1 {
				t.Fatalf("uploads = %d, want 1", n)
			}
			if got := h.exec.last(s.Sandbox, osewaServerName).args()[0]; got != osewaPath() {
				t.Fatalf("server binary = %q", got)
			}
		})
	}

	t.Run("nothing to upload", func(t *testing.T) {
		h := newHarness(t)
		h.mgr.osewaBinaryDir = t.TempDir()
		h.exec.imageOsewa = ""
		_, err := h.mgr.Start(ctx, StartOptions{User: alice, Repo: "r"})
		if err == nil || !strings.Contains(err.Error(), "osewa-linux-amd64.gz") || !strings.Contains(err.Error(), "sessions.osewa.binary_dir") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("tampered build", func(t *testing.T) {
		h := newHarness(t)
		withOsewaBuilds(t, h)
		h.mgr.osewaSHA256 = map[string]string{"amd64": strings.Repeat("0", 64)}
		h.exec.imageOsewa = ""
		_, err := h.mgr.Start(ctx, StartOptions{User: alice, Repo: "r"})
		if err == nil || !strings.Contains(err.Error(), "SHA-256") {
			t.Fatalf("err = %v", err)
		}
		if h.exec.count(SessionSandboxName("alice", "main"), uploadBeginName) != 0 {
			t.Fatal("uploaded a build that failed verification")
		}
	})

	t.Run("unknown architecture", func(t *testing.T) {
		h := newHarness(t)
		h.exec.imageOsewa = ""
		h.exec.arch = "riscv64"
		if _, err := h.mgr.Start(ctx, StartOptions{User: alice, Repo: "r"}); err == nil || !strings.Contains(err.Error(), "riscv64") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("server start is idempotent, enrolment once", func(t *testing.T) {
		h := newHarness(t)
		h.mgr.osewaPort = 7400
		for range 3 {
			if _, err := h.mgr.Start(ctx, StartOptions{User: alice, Repo: "r"}); err != nil {
				t.Fatal(err)
			}
		}
		b := h.exec.boxes[SessionSandboxName("alice", "main")]
		if h.exec.count(SessionSandboxName("alice", "main"), osewaServerName) != 3 || b.servers != 1 {
			t.Fatalf("server calls = %d, servers started = %d", h.exec.count(SessionSandboxName("alice", "main"), osewaServerName), b.servers)
		}
		server := h.exec.last(SessionSandboxName("alice", "main"), osewaServerName)
		if strings.Join(server.args(), " ") != "/usr/local/bin/osewa 127.0.0.1:7400" {
			t.Fatalf("server args = %q", server.args())
		}
		if !strings.Contains(server.command[2], `server --daemon --listen "$2" --no-quic --no-forward`) {
			t.Fatalf("server script = %q", server.command[2])
		}
		if server.opts.Env["OSEWA_HOME"] != SandboxRoot || server.opts.Env["HOME"] != SandboxRoot || server.opts.Env["OSEWA_AUTO_UPDATE"] != "off" {
			t.Fatalf("server env = %v", server.opts.Env)
		}
		if h.exec.count(SessionSandboxName("alice", "main"), osewaEnrollName) != 1 || h.exec.count(SessionSandboxName("alice", "main"), osewaHostsName) != 1 {
			t.Fatal("enrolled more than once")
		}
		if h.exec.count(SessionSandboxName("alice", "main"), osewaNewName) != 1 {
			t.Fatal("the program was started more than once")
		}
	})

	t.Run("enrolment", func(t *testing.T) {
		h := newHarness(t)
		if _, err := h.mgr.Start(ctx, StartOptions{User: alice, Repo: "r"}); err != nil {
			t.Fatal(err)
		}
		b := h.exec.boxes[SessionSandboxName("alice", "main")]
		enroll := h.exec.last(SessionSandboxName("alice", "main"), osewaEnrollName)
		a := enroll.args()
		if a[1] != osewaClientDir || a[2] != "7322" || a[3] != osewaDevice {
			t.Fatalf("enroll args = %q", a)
		}
		// The private key is a PKCS#8 Ed25519 PEM block whose public key is
		// the one sent to enroll, and it travels only in the env.
		key := b.files[osewaClientDir+"/client.key"]
		block, _ := pem.Decode([]byte(key))
		if block == nil || block.Type != "PRIVATE KEY" {
			t.Fatalf("client.key = %q", key)
		}
		sk, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			t.Fatal(err)
		}
		pub, err := base64.StdEncoding.DecodeString(a[4])
		if err != nil || !bytes.Equal(pub, sk.(ed25519.PrivateKey).Public().(ed25519.PublicKey)) {
			t.Fatalf("public key %q does not match client.key", a[4])
		}
		for _, arg := range enroll.command {
			if strings.Contains(arg, "PRIVATE KEY") {
				t.Fatal("private key in an argument")
			}
		}
		var hosts []osewaHostEntry
		if err := json.Unmarshal([]byte(b.files[osewaClientDir+"/hosts.json"]), &hosts); err != nil {
			t.Fatal(err)
		}
		if len(hosts) != 1 || hosts[0] != (osewaHostEntry{Name: "sandbox", Addr: "127.0.0.1:7322", ServerKey: "c2VydmVyLWtleQ=="}) {
			t.Fatalf("hosts.json = %+v", hosts)
		}
	})

	t.Run("probe failure", func(t *testing.T) {
		h := newHarness(t)
		h.exec.probeExit = 2
		if _, err := h.mgr.Start(ctx, StartOptions{User: alice, Repo: "r"}); err == nil || !strings.Contains(err.Error(), "probe failed") {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestOsewaSessionReuse(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)

	first, err := h.mgr.Start(ctx, StartOptions{User: alice, Repo: "r"})
	if err != nil {
		t.Fatal(err)
	}
	// Running: reused.
	second, err := h.mgr.Start(ctx, StartOptions{User: alice, Repo: "r"})
	if err != nil || second.OsewaID != first.OsewaID || second.Started {
		t.Fatalf("second: %+v err=%v", second, err)
	}

	// The program exited: the next attach starts it again.
	b := h.exec.boxes[SessionSandboxName("alice", "main")]
	b.sessions[first.OsewaID] = false
	var progress bytes.Buffer
	third, err := h.mgr.Resume(ctx, StartOptions{User: alice, Repo: "r", Progress: &progress})
	if err != nil || third.OsewaID == first.OsewaID || !third.Started {
		t.Fatalf("third: %+v err=%v", third, err)
	}
	if !strings.Contains(progress.String(), "Started bash -l again") {
		t.Fatalf("progress = %q", progress.String())
	}

	// The sandbox's runtime was replaced: the server and the program are
	// gone.
	delete(b.sessions, third.OsewaID)
	b.server = false
	fourth, err := h.mgr.Resume(ctx, StartOptions{User: alice, Repo: "r"})
	if err != nil || !fourth.Started || b.servers != 2 {
		t.Fatalf("fourth: %+v err=%v servers=%d", fourth, err, b.servers)
	}

	// Another start recorded a new session between our look and our
	// create: it is used rather than starting a second program.
	b.sessions[fourth.OsewaID] = false
	listed := len(h.exec.runs)
	b.files[osewaSessionFile] = "raced\n"
	b.sessions["raced"] = true
	// Pretend the list showed the old record.
	id, started, err := h.mgr.newOsewaSession(ctx, SessionSandboxName("alice", "main"), "/usr/local/bin/osewa", "/sandbox/r", []string{"bash"}, fourth.OsewaID, false)
	if err != nil || id != "raced" || started {
		t.Fatalf("raced: id=%q started=%v err=%v", id, started, err)
	}
	if len(h.exec.runs) != listed+1 {
		t.Fatal("expected only the create script")
	}
}

func TestDetachKeepsSessionRunning(t *testing.T) {
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

	// The user detaches: the attach client exits 0.
	term := newFakeTerm()
	_ = term.inW.Close()
	is.(*fakeInteractive).exit <- 0
	if code, err := Bridge(ctx, term, is); err != nil || code != 0 {
		t.Fatalf("bridge: code=%d err=%v", code, err)
	}

	// The sandbox, its token and the program survive; start re-attaches
	// to the same osewa session.
	sb, err := h.fc.Sandboxes().Get(ctx, "default", s.Sandbox)
	if err != nil || sb.Status.Phase != types.SandboxReady {
		t.Fatalf("sandbox after detach: sb=%v err=%v", sb, err)
	}
	if n := len(h.tokens.named(TokenName(s.Sandbox))); n != 1 {
		t.Fatalf("tokens after detach = %d, want 1", n)
	}
	again, err := h.mgr.Start(ctx, StartOptions{User: alice, Repo: "r"})
	if err != nil || again.Created || len(again.Cloned) > 0 || again.Started || again.OsewaID != s.OsewaID {
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
	if first.Sandbox != second.Sandbox {
		t.Fatalf("sandboxes differ: %q vs %q", first.Sandbox, second.Sandbox)
	}
	if second.Created || len(second.Cloned) > 0 {
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
		Name:   SessionSandboxName("alice", "main"),
		Labels: ownerLabels(alice),
		Status: types.SandboxStatus{Phase: types.SandboxStopped},
	})
	h.exec.box(SessionSandboxName("alice", "main")).dirs["/sandbox/r"] = true

	var progress bytes.Buffer
	s, err := h.mgr.Start(ctx, StartOptions{User: alice, Repo: "r", Image: "custom", Progress: &progress})
	if err != nil {
		t.Fatal(err)
	}
	if s.Created || len(s.Cloned) > 0 {
		t.Fatalf("created=%v cloned=%v", s.Created, s.Cloned)
	}
	sb, _ := h.fc.Sandboxes().Get(ctx, "default", SessionSandboxName("alice", "main"))
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
	sb, _ := h.fc.Sandboxes().Get(ctx, "default", SessionSandboxName("alice", "main"))
	if sb.Spec.Template.Image != "custom" {
		t.Fatalf("image = %q", sb.Spec.Template.Image)
	}
}

func profileConfig() *config.Config {
	cfg := newHarnessConfig()
	cfg.Sessions.Profiles = map[string]config.SessionProfile{
		"claude": {Image: "agent-claude", Command: `claude --model "opus 4"`},
		"shell":  {Command: "zsh -l"},
	}
	cfg.Sessions.Repos = map[string]config.SessionRepoDefaults{
		"infra/*": {Profile: "claude"},
	}
	return cfg
}

func TestStartProfiles(t *testing.T) {
	ctx := context.Background()

	// The order: --profile, the repository's own profile, the
	// sessions.repos glob, none.
	cases := []struct {
		name, repo, profile, repoProfile, image string
		wantProfile, wantImage                  string
		wantArgv                                []string
		wantWarning                             string
	}{
		{"no profile", "r", "", "", "", "", "default-image", []string{"/sandbox/r", "bash", "-l"}, ""},
		{"repo default", "infra/dns", "", "", "", "claude", "agent-claude", []string{"/sandbox/infra/dns", "claude", "--model", "opus 4"}, ""},
		{"explicit", "r", "shell", "", "", "shell", "default-image", []string{"/sandbox/r", "zsh", "-l"}, ""},
		{"explicit beats repo default", "infra/dns", "shell", "", "", "shell", "default-image", []string{"/sandbox/infra/dns", "zsh", "-l"}, ""},
		{"image flag beats profile", "infra/dns", "", "", "custom", "claude", "custom", []string{"/sandbox/infra/dns", "claude", "--model", "opus 4"}, ""},
		{"repo setting", "r", "", "shell", "", "shell", "default-image", []string{"/sandbox/r", "zsh", "-l"}, ""},
		{"repo setting beats glob", "infra/dns", "", "shell", "", "shell", "default-image", []string{"/sandbox/infra/dns", "zsh", "-l"}, ""},
		{"explicit beats repo setting", "infra/dns", "claude", "shell", "", "claude", "agent-claude", []string{"/sandbox/infra/dns", "claude", "--model", "opus 4"}, ""},
		{"stale repo setting falls back to glob", "infra/dns", "", "gone", "", "claude", "agent-claude", []string{"/sandbox/infra/dns", "claude", "--model", "opus 4"}, `the repository's profile "gone" is no longer configured`},
		{"stale repo setting falls back to none", "r", "", "gone", "", "", "default-image", []string{"/sandbox/r", "bash", "-l"}, `the repository's profile "gone" is no longer configured`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarnessWith(t, profileConfig())
			var progress bytes.Buffer
			s, err := h.mgr.Start(ctx, StartOptions{User: alice, Repo: tc.repo, Profile: tc.profile, RepoProfile: tc.repoProfile, Image: tc.image, Progress: &progress})
			if err != nil {
				t.Fatal(err)
			}
			if tc.wantWarning != "" && !strings.Contains(progress.String(), tc.wantWarning) {
				t.Fatalf("progress = %q", progress.String())
			}
			if s.Profile != tc.wantProfile {
				t.Fatalf("profile = %q, want %q", s.Profile, tc.wantProfile)
			}
			sb, _ := h.fc.Sandboxes().Get(ctx, "default", s.Sandbox)
			if sb.Spec.Template.Image != tc.wantImage || sb.Annotations[AnnotationProfile] != tc.wantProfile {
				t.Fatalf("image = %q, annotations = %v", sb.Spec.Template.Image, sb.Annotations)
			}
			if got := h.exec.boxes[s.Sandbox].newArgv; strings.Join(got, "\x00") != strings.Join(tc.wantArgv, "\x00") {
				t.Fatalf("argv = %q, want %q", got, tc.wantArgv)
			}
		})
	}

	t.Run("unknown profile", func(t *testing.T) {
		h := newHarnessWith(t, profileConfig())
		_, err := h.mgr.Start(ctx, StartOptions{User: alice, Repo: "r", Profile: "nope"})
		if err == nil || !strings.Contains(err.Error(), `unknown profile "nope": use one of claude, shell`) {
			t.Fatalf("err = %v", err)
		}
		if _, err := h.fc.Sandboxes().Get(ctx, "default", SessionSandboxName("alice", "main")); !IsNotFound(err) {
			t.Fatal("sandbox created for an unknown profile")
		}
		h = newHarness(t)
		if _, err := h.mgr.Start(ctx, StartOptions{User: alice, Repo: "r", Profile: "nope"}); err == nil || !strings.Contains(err.Error(), "no profiles are configured") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("existing session keeps its profile", func(t *testing.T) {
		h := newHarnessWith(t, profileConfig())
		if _, err := h.mgr.Start(ctx, StartOptions{User: alice, Repo: "infra/dns"}); err != nil {
			t.Fatal(err)
		}
		var progress bytes.Buffer
		s, err := h.mgr.Start(ctx, StartOptions{User: alice, Repo: "infra/dns", Profile: "shell", Progress: &progress})
		if err != nil {
			t.Fatal(err)
		}
		if s.Profile != "claude" || !strings.Contains(progress.String(), "--profile ignored") {
			t.Fatalf("profile = %q, progress = %q", s.Profile, progress.String())
		}

		// A profile removed from the config falls back to the default
		// command, with a warning, when the program has to start again.
		h.exec.boxes[s.Sandbox].sessions[s.OsewaID] = false
		delete(h.mgr.sessions.Profiles, "claude")
		progress.Reset()
		s, err = h.mgr.Resume(ctx, StartOptions{User: alice, Repo: "infra/dns", Progress: &progress})
		if err != nil {
			t.Fatal(err)
		}
		if strings.Join(s.Command, " ") != "bash -l" || !strings.Contains(progress.String(), `profile "claude" is no longer configured`) {
			t.Fatalf("command = %q, progress = %q", s.Command, progress.String())
		}
	})
}

func TestNamedSessions(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)

	main, err := h.mgr.Start(ctx, StartOptions{User: alice, Repo: "r"})
	if err != nil {
		t.Fatal(err)
	}
	review, err := h.mgr.Start(ctx, StartOptions{User: alice, Repo: "r", Name: "review"})
	if err != nil {
		t.Fatal(err)
	}
	if main.Sandbox != SessionSandboxName("alice", "main") || review.Sandbox != SessionSandboxName("alice", "review") || review.Name != "review" {
		t.Fatalf("main = %q, review = %q (%q)", main.Sandbox, review.Sandbox, review.Name)
	}
	if !review.Created || len(review.Cloned) == 0 || !review.Started {
		t.Fatalf("review: %+v", review)
	}
	sb, _ := h.fc.Sandboxes().Get(ctx, "default", review.Sandbox)
	if sb.Annotations[AnnotationSession] != "review" || sb.Annotations[AnnotationRepo] != "r" {
		t.Fatalf("annotations = %v", sb.Annotations)
	}
	if _, err := h.mgr.Start(ctx, StartOptions{User: alice, Repo: "r", Name: "Bad Name"}); !errors.Is(err, ErrInvalidSessionName) {
		t.Fatalf("invalid name: err = %v", err)
	}

	// Ending one named session leaves the others.
	existed, err := h.mgr.End(ctx, alice, "review")
	if err != nil || !existed {
		t.Fatalf("end review: existed=%v err=%v", existed, err)
	}
	if _, err := h.fc.Sandboxes().Get(ctx, "default", review.Sandbox); !IsNotFound(err) {
		t.Fatalf("review not deleted: %v", err)
	}
	if _, err := h.fc.Sandboxes().Get(ctx, "default", main.Sandbox); err != nil {
		t.Fatalf("main deleted: %v", err)
	}
	if len(h.tokens.named(TokenName(main.Sandbox))) != 1 || len(h.tokens.named(TokenName(review.Sandbox))) != 0 {
		t.Fatal("tokens of the wrong session revoked")
	}
}

func TestResume(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)

	_, err := h.mgr.Resume(ctx, StartOptions{User: alice, Repo: "r", Name: "review"})
	if !errors.Is(err, ErrNoSession) || !strings.Contains(err.Error(), `"session new review --repo <repository>"`) {
		t.Fatalf("absent: err = %v", err)
	}
	if _, err := h.fc.Sandboxes().Get(ctx, "default", SessionSandboxName("alice", "review")); !IsNotFound(err) {
		t.Fatal("Resume created a sandbox")
	}
	_, err = h.mgr.Resume(ctx, StartOptions{User: alice, Repo: "r"})
	if !errors.Is(err, ErrNoSession) || !strings.Contains(err.Error(), `"session new main --repo <repository>"`) {
		t.Fatalf("absent main: err = %v", err)
	}

	// A stopped sandbox from before named sessions is started, and its
	// program started in osewa.
	h.fc.AddSandbox("default", &types.Sandbox{
		Name:   SessionSandboxName("alice", "main"),
		Labels: ownerLabels(alice),
		Status: types.SandboxStatus{Phase: types.SandboxStopped},
	})
	h.exec.box(SessionSandboxName("alice", "main")).dirs["/sandbox/r"] = true
	s, err := h.mgr.Resume(ctx, StartOptions{User: alice, Repo: "r", Image: "ignored", Profile: "ignored"})
	if err != nil || s.Created || len(s.Cloned) > 0 || !s.Started {
		t.Fatalf("resume: %+v err=%v", s, err)
	}
	sb, _ := h.fc.Sandboxes().Get(ctx, "default", SessionSandboxName("alice", "main"))
	if sb.Status.Phase != types.SandboxReady {
		t.Fatalf("phase = %s", sb.Status.Phase)
	}

	// Someone else's sandbox.
	h.fc.AddSandbox("default", &types.Sandbox{
		Name:   SessionSandboxName("alice", "x"),
		Labels: map[string]string{LabelUserID: "99"},
		Status: types.SandboxStatus{Phase: types.SandboxReady},
	})
	if _, err := h.mgr.Resume(ctx, StartOptions{User: alice, Repo: "x", Name: "x"}); !errors.Is(err, ErrNotOwned) {
		t.Fatalf("foreign: err = %v", err)
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
	if h.exec.count(SessionSandboxName("alice", "main"), osewaNewName) != 0 {
		t.Fatal("started the program without a checkout")
	}
}

func TestStartFailedCreateCleansUp(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.mgr.client = newSDKClient("default", failingWait{h.fc.Sandboxes()}, h.exec, h.fc.Files(), nil)
	if _, err := h.mgr.Start(ctx, StartOptions{User: alice, Repo: "r"}); err == nil {
		t.Fatal("expected error")
	}
	if _, err := h.fc.Sandboxes().Get(ctx, "default", SessionSandboxName("alice", "main")); !IsNotFound(err) {
		t.Fatalf("sandbox left behind: %v", err)
	}
	if len(h.exec.clones()) != 0 {
		t.Fatal("cloned into a failed sandbox")
	}
}

func TestStartRejectsForeignSandbox(t *testing.T) {
	h := newHarness(t)
	h.fc.AddSandbox("default", &types.Sandbox{
		Name:   SessionSandboxName("alice", "main"),
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
	if _, err := h.mgr.Resume(context.Background(), StartOptions{Repo: "r"}); !errors.Is(err, proto.ErrUnauthorized) {
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
	if n := len(h.tokens.named(TokenName(SessionSandboxName("alice", "main")))); n != 1 {
		t.Fatalf("tokens = %d, want 1", n)
	}
}

// A second attach within the TTL must not revoke the token a git command
// may have read from the credential file just before it was replaced.
func TestStartKeepsUnexpiredTokens(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	if _, err := h.mgr.Start(ctx, StartOptions{User: alice, Repo: "r"}); err != nil {
		t.Fatal(err)
	}
	first := h.tokens.named(TokenName(SessionSandboxName("alice", "main")))
	if len(first) != 1 {
		t.Fatalf("tokens after first start = %d", len(first))
	}
	if _, err := h.mgr.Start(ctx, StartOptions{User: alice, Repo: "r"}); err != nil {
		t.Fatal(err)
	}
	toks := h.tokens.named(TokenName(SessionSandboxName("alice", "main")))
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

	existed, err := h.mgr.End(ctx, alice, "")
	if err != nil || !existed {
		t.Fatalf("end: existed=%v err=%v", existed, err)
	}
	if _, err := h.fc.Sandboxes().Get(ctx, "default", SessionSandboxName("alice", "main")); !IsNotFound(err) {
		t.Fatalf("sandbox not deleted: %v", err)
	}
	if n := len(h.tokens.named(TokenName(SessionSandboxName("alice", "main")))); n != 0 {
		t.Fatalf("token not revoked: %d left", n)
	}
	if n := len(h.tokens.named("laptop")); n != 1 {
		t.Fatal("unrelated token revoked")
	}

	existed, err = h.mgr.End(ctx, alice, "main")
	if err != nil || existed {
		t.Fatalf("second end: existed=%v err=%v", existed, err)
	}
	if _, err := h.mgr.End(ctx, alice, "-bad"); !errors.Is(err, ErrInvalidSessionName) {
		t.Fatalf("invalid name: err = %v", err)
	}
}

func TestEndRejectsForeignSandbox(t *testing.T) {
	h := newHarness(t)
	h.fc.AddSandbox("default", &types.Sandbox{
		Name:   SessionSandboxName("alice", "main"),
		Labels: map[string]string{LabelUserID: "99"},
	})
	if _, err := h.mgr.End(context.Background(), alice, ""); !errors.Is(err, ErrNotOwned) {
		t.Fatalf("err = %v", err)
	}
}

func TestList(t *testing.T) {
	ctx := context.Background()
	h := newHarnessWith(t, profileConfig())
	bob := testUser{id: 2, name: "bob"}
	for _, o := range []StartOptions{
		{User: alice, Repo: "zeta", Name: "zeta"},
		{User: alice, Repo: "alpha", Name: "review", Profile: "shell"},
		{User: alice, Repos: []RepoSpec{{Name: "alpha"}, {Name: "beta"}}},
		{User: bob, Repo: "alpha"},
	} {
		if _, err := h.mgr.Start(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	// A sandbox from before multi-repository sessions has only the one
	// repository annotation.
	h.fc.AddSandbox("default", &types.Sandbox{
		Name:        "sd-old",
		Labels:      ownerLabels(alice),
		Annotations: map[string]string{AnnotationRepo: "old", AnnotationSession: "old"},
		Status:      types.SandboxStatus{Phase: types.SandboxStopped},
	})
	// A sandbox not created by Sundae is ignored.
	h.fc.AddSandbox("default", &types.Sandbox{Name: "other"})

	infos, err := h.mgr.List(ctx, alice)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, i := range infos {
		got = append(got, fmt.Sprintf("%s/%s/%s/%s/%s", strings.Join(i.Repos, "+"), i.Name, i.Profile, i.Sandbox, i.Phase))
	}
	want := []string{
		"alpha+beta/main//" + SessionSandboxName("alice", "main") + "/Ready",
		"old/old//sd-old/Stopped",
		"alpha/review/shell/" + SessionSandboxName("alice", "review") + "/Ready",
		"zeta/zeta//" + SessionSandboxName("alice", "zeta") + "/Ready",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("infos =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestUploadFallsBackToExec(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	data := bytes.Repeat([]byte("0123456789abcdef"), uploadChunk/16*7+5) // 8 chunks
	local := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(local, data, 0o600); err != nil {
		t.Fatal(err)
	}
	// The SDK's fake file client is Unimplemented, like the real one
	// without a transport.
	if err := h.mgr.client.Upload(ctx, "sb", local, "/sandbox/x/y"); err != nil {
		t.Fatal(err)
	}
	b := h.exec.boxes["sb"]
	if b.files["/sandbox/x/y"] != string(data) {
		t.Fatalf("uploaded %d bytes, want %d", len(b.files["/sandbox/x/y"]), len(data))
	}
	if _, ok := b.files["/sandbox/x/.y.b64"]; ok {
		t.Fatal("staging file left behind")
	}
	if n := h.exec.count("sb", uploadChunkName); n != 3 {
		t.Fatalf("chunks = %d, want 3", n)
	}
	// The fake applies the gateway's limits, so the chunking fits them.
	if err := h.mgr.client.Upload(ctx, "sb", filepath.Join(t.TempDir(), "missing"), "/x"); err == nil {
		t.Fatal("uploaded a missing file")
	}
}

// fakeUploadFiles is a FileInterface that can transfer files.
type fakeUploadFiles struct{ got []string }

func (f *fakeUploadFiles) Upload(_ context.Context, _, sandbox, local, remote string) error {
	f.got = append(f.got, sandbox+":"+local+"->"+remote)
	return nil
}

func (f *fakeUploadFiles) Download(context.Context, string, string, string, string) error {
	return nil
}

func TestUploadUsesSDKWhenAvailable(t *testing.T) {
	h := newHarness(t)
	files := &fakeUploadFiles{}
	c := newSDKClient("default", h.fc.Sandboxes(), h.exec, files, nil)
	if err := c.Upload(context.Background(), "sb", "/l", "/r"); err != nil {
		t.Fatal(err)
	}
	if strings.Join(files.got, ",") != "sb:/l->/r" || len(h.exec.runs) != 0 {
		t.Fatalf("got = %v, runs = %d", files.got, len(h.exec.runs))
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
