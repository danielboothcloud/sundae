package forge

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	clog "charm.land/log/v2"
	cssh "charm.land/ssh"
	"github.com/danielboothcloud/sundae/pkg/config"
	"github.com/danielboothcloud/sundae/pkg/proto"
	gossh "golang.org/x/crypto/ssh"
)

// sshGitServer is a git server over real SSH: it serves git-upload-pack and
// git-receive-pack for bare repositories under root, to one client key.
// Repository "/owner/repo.git" is root/owner_repo.git, where
// serviceFixture.upstream puts it.
type sshGitServer struct {
	addr    string
	port    int
	hostKey gossh.PublicKey

	mu    sync.Mutex
	calls map[string]int
}

func newSSHGitServer(t *testing.T, root string, client gossh.PublicKey) *sshGitServer {
	t.Helper()
	hostSigner, _ := newSSHKey(t)
	gs := &sshGitServer{hostKey: hostSigner.PublicKey(), calls: map[string]int{}}
	srv := &cssh.Server{
		PublicKeyHandler: func(_ cssh.Context, key cssh.PublicKey) bool {
			return cssh.KeysEqual(key, client)
		},
		Handler: func(s cssh.Session) {
			args := s.Command()
			if len(args) != 2 || (args[0] != "git-upload-pack" && args[0] != "git-receive-pack") {
				fmt.Fprintf(s.Stderr(), "unsupported command %q\n", s.RawCommand())
				_ = s.Exit(1)
				return
			}
			p := strings.TrimSuffix(strings.TrimPrefix(args[1], "/"), ".git")
			dir := filepath.Join(root, strings.ReplaceAll(p, "/", "_")+".git")
			if strings.Contains(p, "..") || p == "" {
				fmt.Fprintf(s.Stderr(), "bad path %q\n", args[1])
				_ = s.Exit(1)
				return
			}
			gs.mu.Lock()
			gs.calls[args[0]+" "+args[1]]++
			gs.mu.Unlock()

			cmd := exec.CommandContext(s.Context(), "git", strings.TrimPrefix(args[0], "git-"), dir)
			cmd.Stdout = s
			cmd.Stderr = s.Stderr()
			stdin, err := cmd.StdinPipe()
			if err != nil {
				_ = s.Exit(1)
				return
			}
			if err := cmd.Start(); err != nil {
				fmt.Fprintln(s.Stderr(), err)
				_ = s.Exit(1)
				return
			}
			go func() {
				_, _ = io.Copy(stdin, s)
				_ = stdin.Close()
			}()
			code := 0
			if err := cmd.Wait(); err != nil {
				code = 1
				var ee *exec.ExitError
				if errors.As(err, &ee) {
					code = ee.ExitCode()
				}
			}
			_ = s.Exit(code)
		},
	}
	srv.AddHostKey(hostSigner)
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	gs.addr = ln.Addr().String()
	gs.port = ln.Addr().(*net.TCPAddr).Port
	go srv.Serve(ln)                  //nolint:errcheck
	t.Cleanup(func() { srv.Close() }) //nolint:errcheck
	return gs
}

func (gs *sshGitServer) count(cmd, path string) int {
	gs.mu.Lock()
	defer gs.mu.Unlock()
	return gs.calls[cmd+" /"+path+".git"]
}

// sshEnd is everything a forge needs to reach an sshGitServer: the client
// key and a known_hosts file, both in a directory whose name needs quoting.
type sshEnd struct {
	srv        *sshGitServer
	key        string
	knownHosts string
	sshURL     string
}

func newSSHEnd(t *testing.T, root string) *sshEnd {
	t.Helper()
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Skip("ssh not installed")
	}
	dir := filepath.Join(t.TempDir(), `forge keys 'q' "dq"`)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	client, keyPEM := newSSHKey(t)
	gs := newSSHGitServer(t, root, client.PublicKey())
	return &sshEnd{
		srv: gs,
		// Group-readable and owned by the test user: ssh would refuse this
		// file itself ("UNPROTECTED PRIVATE KEY FILE"), so this also proves
		// the key reaches ssh through the private copy.
		key:        writeFile(t, dir, "id key", keyPEM, 0o640),
		knownHosts: writeFile(t, dir, "known hosts", []byte(knownHostsLine(fmt.Sprintf("[127.0.0.1]:%d", gs.port), gs.hostKey)), 0o644),
		sshURL:     fmt.Sprintf("ssh://git@127.0.0.1:%d", gs.port),
	}
}

func (e *sshEnd) apply(fc *config.ForgeConfig) {
	fc.SSHKeyFile = e.key
	fc.SSHKnownHostsFile = e.knownHosts
	fc.SSHURL = e.sshURL
}

func (ff *fakeForge) setCloneURL(path, cloneURL string) {
	ff.mu.Lock()
	defer ff.mu.Unlock()
	for _, r := range ff.repos {
		if r["full_name"] == path {
			r["clone_url"] = cloneURL
		}
	}
}

// weirdTempDir points TMPDIR, where the per-operation key copy goes, at a
// directory whose path needs both shell and ssh config quoting and has a
// '%' ssh would expand, and returns it.
func weirdTempDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), `tmp dir 'q' "dq" 100% \x`)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", dir)
	return dir
}

// TestSSHEndToEnd runs import, an upstream commit and sync (fast-forward),
// and a local commit and push, all with real git over real SSH through the
// production remote check, then shows that a wrong host key fails.
func TestSSHEndToEnd(t *testing.T) {
	root := t.TempDir()
	var end *sshEnd
	var badKnownHosts string
	fx := newServiceFixtureWith(t, root, func(cfg *config.Config, ff *fakeForge) {
		end = newSSHEnd(t, root)
		end.apply(&cfg.Forges[0])
		cfg.Forges[0].Exclude = nil

		// The same forge, but with a known_hosts holding another key for
		// the server's address.
		other, _ := newSSHKey(t)
		badKnownHosts = writeFile(t, t.TempDir(), "known_hosts", []byte(knownHostsLine(fmt.Sprintf("[127.0.0.1]:%d", end.srv.port), other.PublicKey())), 0o600)
		bad := cfg.Forges[0]
		bad.Name, bad.Prefix, bad.SSHKnownHostsFile = "bad", "bad", badKnownHosts
		cfg.Forges = append(cfg.Forges, bad)
	})
	ctx := fx.ctx
	// No test seam: every URL git sees passes the production check.
	fx.svc.checkRemote = (*Forge).CheckRemote

	proj := fx.upstream(t, "me/proj", true)
	fx.ff.setCloneURL("me/proj", fx.ff.srv.URL+"/me/proj.git")
	tmp := weirdTempDir(t)

	st, err := fx.svc.Status(ctx)
	if err != nil || st[0].Transport != TransportSSH || st[0].SSHURL != end.sshURL {
		t.Fatalf("Status = %+v, %v", st, err)
	}

	// Import over SSH.
	results, err := fx.svc.Import(ctx, fx.user, "gh", ImportOptions{Paths: []string{"me/proj"}})
	if err != nil {
		t.Fatal(err)
	}
	if r := resultFor(t, results, "me/proj"); r.Status != ImportImported {
		t.Fatalf("import: %+v", r)
	}
	if n := end.srv.count("git-upload-pack", "me/proj"); n != 1 {
		t.Errorf("upload-pack over SSH ran %d times on import, want 1", n)
	}
	repo, err := fx.be.Repository(ctx, "gh/me/proj")
	if err != nil {
		t.Fatal(err)
	}
	gr, _ := repo.Open()
	dir := gr.Path
	wantRemote := end.sshURL + "/me/proj.git"
	if got := git(t, dir, "config", "--file", "config", "--get", "remote.gh.url"); got != wantRemote {
		t.Errorf("remote.gh.url = %q, want %q", got, wantRemote)
	}
	if rev(t, dir, "refs/heads/main") != rev(t, proj.bare, "refs/heads/main") {
		t.Error("import did not bring main")
	}

	// Upstream commit, sync over SSH: fast-forward.
	up := proj.commit(t, "main", "upstream work")
	rs, err := fx.svc.SyncRepo(ctx, "gh", "gh/me/proj")
	if err != nil || rs.Error != "" {
		t.Fatalf("sync: %+v %v", rs, err)
	}
	if b, _ := rs.Sync.Branch("main"); b.Action != BranchFastForwarded || rev(t, dir, "refs/heads/main") != up {
		t.Errorf("sync: main %+v, local %s, want fast-forward to %s", b, rev(t, dir, "refs/heads/main"), up)
	}
	if n := end.srv.count("git-upload-pack", "me/proj"); n != 2 {
		t.Errorf("upload-pack over SSH ran %d times, want 2", n)
	}

	// Local commit, push over SSH.
	work := localClone(t, dir)
	local := commitAndPush(t, work, "main", "local work")
	res, err := fx.svc.Push(ctx, "gh/me/proj", "main", false)
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	if res.New != local || rev(t, proj.bare, "refs/heads/main") != local || rev(t, dir, "refs/remotes/gh/heads/main") != local {
		t.Errorf("push: %+v; upstream main %s, want %s", res, rev(t, proj.bare, "refs/heads/main"), local)
	}
	if n := end.srv.count("git-receive-pack", "me/proj"); n != 1 {
		t.Errorf("receive-pack over SSH ran %d times, want 1", n)
	}

	// The per-operation key copies are gone.
	if ents, _ := os.ReadDir(tmp); len(ents) != 0 {
		t.Errorf("temporary files left in TMPDIR: %v", ents)
	}

	// A wrong host key fails, and the failed import leaves nothing.
	results, err = fx.svc.Import(ctx, fx.user, "bad", ImportOptions{Paths: []string{"me/proj"}})
	if err != nil {
		t.Fatal(err)
	}
	r := resultFor(t, results, "me/proj")
	if r.Status != ImportFailed || !strings.Contains(r.Error, "Host key verification failed") {
		t.Errorf("import with a wrong host key: %+v", r)
	}
	if _, err := fx.be.Repository(ctx, "bad/me/proj"); !errors.Is(err, proto.ErrRepoNotFound) {
		t.Errorf("failed import left a repository: %v", err)
	}
	if n := end.srv.count("git-upload-pack", "me/proj"); n != 2 {
		t.Errorf("upload-pack ran after a host key mismatch (%d)", n)
	}
	if ents, _ := os.ReadDir(tmp); len(ents) != 0 {
		t.Errorf("temporary files left in TMPDIR after a failure: %v", ents)
	}
}

// TestSSHTransportSwitch imports over HTTPS (a local upstream through the
// test seam), then syncs after the forge switched to SSH, to another SSH
// endpoint, and back: each time the remote is rewritten before fetching and
// the ref namespace carries on.
func TestSSHTransportSwitch(t *testing.T) {
	root := t.TempDir()
	fx := newServiceFixtureWith(t, root, nil)
	ctx := fx.ctx
	proj := fx.upstream(t, "me/proj", false)
	end := newSSHEnd(t, root)

	results, err := fx.svc.Import(ctx, fx.user, "gh", ImportOptions{Paths: []string{"me/proj"}})
	if err != nil || resultFor(t, results, "me/proj").Status != ImportImported {
		t.Fatalf("import: %+v %v", results, err)
	}
	repo, _ := fx.be.Repository(ctx, "gh/me/proj")
	gr, _ := repo.Open()
	dir := gr.Path
	remote := func() string { return git(t, dir, "config", "--file", "config", "--get", "remote.gh.url") }
	if remote() != proj.bare {
		t.Fatalf("remote after HTTPS import = %q", remote())
	}

	// service returns a service over the same backend with forge gh changed
	// by mutate, and the fixture's seam.
	service := func(mutate func(*config.ForgeConfig)) *Service {
		t.Helper()
		cfg := config.DefaultConfig()
		cfg.DataPath = t.TempDir()
		fc := fx.cfg.Forges[0]
		mutate(&fc)
		cfg.Forges = []config.ForgeConfig{fc}
		if err := cfg.Validate(); err != nil {
			t.Fatal(err)
		}
		svc, err := NewService(cfg, fx.be, clog.New(io.Discard))
		if err != nil {
			t.Fatal(err)
		}
		svc.checkRemote = fx.svc.checkRemote
		return svc
	}
	sync := func(svc *Service, wantFF string) {
		t.Helper()
		rs, err := svc.SyncRepo(ctx, "gh", "gh/me/proj")
		if err != nil || rs.Error != "" {
			t.Fatalf("sync: %+v %v", rs, err)
		}
		if b, _ := rs.Sync.Branch("main"); b.Action != BranchFastForwarded || b.Local != wantFF {
			t.Errorf("sync: main %+v, want fast-forward to %s", b, wantFF)
		}
		if rev(t, dir, "refs/remotes/gh/heads/main") != wantFF {
			t.Error("namespace ref not updated")
		}
	}

	// HTTPS -> SSH.
	toSSH := service(end.apply)
	c1 := proj.commit(t, "main", "one")
	sync(toSSH, c1)
	if want := end.sshURL + "/me/proj.git"; remote() != want {
		t.Errorf("remote after switching to SSH = %q, want %q", remote(), want)
	}
	if end.srv.count("git-upload-pack", "me/proj") != 1 {
		t.Error("sync did not fetch over SSH")
	}
	if fetch := git(t, dir, "config", "--file", "config", "--get-all", "remote.gh.fetch"); fetch != strings.Join(fetchRefspecs("gh"), "\n") {
		t.Errorf("fetch refspecs changed: %q", fetch)
	}

	// Another SSH endpoint for the same server: localhost instead of
	// 127.0.0.1.
	kh := writeFile(t, t.TempDir(), "kh", []byte(knownHostsLine(fmt.Sprintf("[localhost]:%d", end.srv.port), end.srv.hostKey)), 0o600)
	otherEndpoint := service(func(fc *config.ForgeConfig) {
		end.apply(fc)
		fc.SSHURL = fmt.Sprintf("ssh://git@localhost:%d", end.srv.port)
		fc.SSHKnownHostsFile = kh
	})
	c2 := proj.commit(t, "main", "two")
	sync(otherEndpoint, c2)
	if want := fmt.Sprintf("ssh://git@localhost:%d/me/proj.git", end.srv.port); remote() != want {
		t.Errorf("remote after changing ssh_url = %q, want %q", remote(), want)
	}

	// SSH -> HTTPS: the listed clone URL, once a listing is known.
	back := service(func(*config.ForgeConfig) {})
	if _, err := back.Discover(ctx, "gh"); err != nil {
		t.Fatal(err)
	}
	c3 := proj.commit(t, "main", "three")
	sync(back, c3)
	if remote() != proj.bare {
		t.Errorf("remote after switching back to HTTPS = %q, want %q", remote(), proj.bare)
	}
	if n := end.srv.count("git-upload-pack", "me/proj"); n != 2 {
		t.Errorf("upload-pack over SSH ran %d times, want 2", n)
	}
}
