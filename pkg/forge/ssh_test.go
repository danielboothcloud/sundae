package forge

import (
	"bufio"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danielboothcloud/sundae/pkg/config"
	"golang.org/x/crypto/ssh"
)

// newSSHKey returns a new ed25519 signer and its private key in OpenSSH
// PEM form.
func newSSHKey(t *testing.T) (ssh.Signer, []byte) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return signer, pem.EncodeToMemory(block)
}

// writeFile writes data to dir/name with mode and returns the path.
func writeFile(t *testing.T, dir, name string, data []byte, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, data, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	return p
}

// knownHostsLine returns a known_hosts line for host:port and key.
func knownHostsLine(hostPort string, key ssh.PublicKey) string {
	return strings.ToLower(hostPort) + " " + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key))) + "\n"
}

func TestRemoteFor(t *testing.T) {
	_, keyPEM := newSSHKey(t)
	key := writeFile(t, t.TempDir(), "id", keyPEM, 0o600)
	https := newTestForge(t, config.ForgeConfig{Name: "gh", Kind: "github"})
	glSub := newTestForge(t, config.ForgeConfig{Name: "gl", Kind: "gitlab", URL: "https://example.com/gitlab"})
	sshDefault := newTestForge(t, config.ForgeConfig{Name: "gh", Kind: "github", SSHKeyFile: key})
	ssh443 := newTestForge(t, config.ForgeConfig{Name: "gh", Kind: "github", SSHKeyFile: key, SSHURL: "ssh://git@ssh.github.com:443"})

	for _, tc := range []struct {
		f                     *Forge
		path, current, listed string
		want                  string
	}{
		// HTTPS: the API clone URL on import, then the recorded remote.
		{https, "me/x", "", "https://github.com/me/x.git", "https://github.com/me/x.git"},
		{https, "me/x", "https://github.com/me/x.git", "https://github.com/me/y.git", "https://github.com/me/x.git"},
		// Back from SSH: the listed clone URL, else built from the path.
		{https, "me/x", "ssh://git@github.com/me/x.git", "https://github.com/me/x.git", "https://github.com/me/x.git"},
		{https, "me/x", "ssh://git@ssh.github.com:443/me/x.git", "", "https://github.com/me/x.git"},
		{https, "me/x", "git@github.com:me/x.git", "", "https://github.com/me/x.git"},
		{glSub, "g/sub/p", "ssh://git@example.com/g/sub/p.git", "", "https://example.com/gitlab/g/sub/p.git"},
		// SSH: always built from the path and the endpoint, whatever the
		// API says or the repository recorded.
		{sshDefault, "me/x", "", "https://github.com/me/x.git", "ssh://git@github.com/me/x.git"},
		{sshDefault, "me/x", "https://github.com/me/x.git", "", "ssh://git@github.com/me/x.git"},
		{ssh443, "me/x", "ssh://git@github.com/me/x.git", "", "ssh://git@ssh.github.com:443/me/x.git"},
		{ssh443, "Org/Repo.js", "", "", "ssh://git@ssh.github.com:443/Org/Repo.js.git"},
	} {
		if got := tc.f.remoteFor(tc.path, tc.current, tc.listed); got != tc.want {
			t.Errorf("%s remoteFor(%q, %q, %q) = %q, want %q", tc.f.Transport(), tc.path, tc.current, tc.listed, got, tc.want)
		}
		if err := tc.f.CheckRemote(tc.f.remoteFor(tc.path, tc.current, tc.listed)); err != nil {
			t.Errorf("CheckRemote(remoteFor(%q)): %v", tc.path, err)
		}
	}
	if https.Transport() != TransportHTTPS || https.SSHURL() != "" {
		t.Errorf("https forge: transport %q ssh url %q", https.Transport(), https.SSHURL())
	}
	if ssh443.Transport() != TransportSSH || ssh443.SSHURL() != "ssh://git@ssh.github.com:443" {
		t.Errorf("ssh forge: transport %q ssh url %q", ssh443.Transport(), ssh443.SSHURL())
	}
}

func TestCheckRemoteSSH(t *testing.T) {
	_, keyPEM := newSSHKey(t)
	signer, _ := newSSHKey(t)
	dir := t.TempDir()
	key := writeFile(t, dir, "id", keyPEM, 0o600)
	kh := writeFile(t, dir, "kh", []byte(knownHostsLine("[git.example.com]:2222", signer.PublicKey())), 0o600)
	https := newTestForge(t, config.ForgeConfig{Name: "gh", Kind: "github"})
	gh := newTestForge(t, config.ForgeConfig{Name: "gh", Kind: "github", SSHKeyFile: key})
	gl := newTestForge(t, config.ForgeConfig{Name: "gl", Kind: "gitlab", URL: "http://10.0.0.5:8080/gitlab", SSHKeyFile: key, SSHKnownHostsFile: kh, SSHURL: "ssh://git@git.example.com:2222"})

	for _, tc := range []struct {
		f    *Forge
		url  string
		want bool
	}{
		{gh, "ssh://git@github.com/me/tool.git", true},
		{gh, "ssh://git@GitHub.com:22/me/tool.git", true},
		// HTTPS clone URLs (from discovery) are still checked as before.
		{gh, "https://github.com/me/tool.git", true},
		{gh, "https://evil.example/me/tool.git", false},
		{gh, "ssh://git@github.com:443/me/tool.git", false},
		{gh, "ssh://git@ssh.github.com/me/tool.git", false},
		{gh, "ssh://git@evil.example/me/tool.git", false},
		{gh, "ssh://evil@github.com/me/tool.git", false},
		{gh, "ssh://github.com/me/tool.git", false},
		{gh, "ssh://git:pw@github.com/me/tool.git", false},
		{gh, "ssh://git@github.com/me/tool", false},
		{gh, "ssh://git@github.com/tool.git", false},
		{gh, "ssh://git@github.com/me/../x.git", false},
		{gh, "ssh://git@github.com/me/to%20ol.git", false},
		{gh, "ssh://git@github.com/me/tool.git?x=1", false},
		{gh, "ssh://git@github.com/me/tool.git#f", false},
		{gh, "git@github.com:me/tool.git", false},
		{gh, "ssh://git@-oProxyCommand=x/me/tool.git", false},
		{gl, "ssh://git@git.example.com:2222/g/sub/p.git", true},
		{gl, "ssh://git@git.example.com/g/sub/p.git", false},
		{gl, "ssh://git@10.0.0.5:2222/g/sub/p.git", false},
		{gl, "http://10.0.0.5:8080/gitlab/g/sub/p.git", true},
		// A forge on HTTPS accepts no SSH URL.
		{https, "ssh://git@github.com/me/tool.git", false},
	} {
		err := tc.f.CheckRemote(tc.url)
		if (err == nil) != tc.want {
			t.Errorf("%s CheckRemote(%q) = %v, want ok=%v", tc.f.Name(), tc.url, err, tc.want)
		}
		if err != nil && !errors.Is(err, ErrForeignCloneURL) {
			t.Errorf("CheckRemote(%q) error %v is not ErrForeignCloneURL", tc.url, err)
		}
	}
}

// envOf returns the value of key in env.
func envOf(env []string, key string) string {
	for _, e := range env {
		if k, v, ok := strings.Cut(e, "="); ok && k == key {
			return v
		}
	}
	return ""
}

func TestSSHGitEnv(t *testing.T) {
	dir := t.TempDir()
	_, keyPEM := newSSHKey(t)
	// Group-readable, as a Secret mount or a shared directory may be. ssh
	// would ignore this file itself; the copy it gets is 0600.
	key := writeFile(t, dir, "id", keyPEM, 0o640)
	f := newTestForge(t, config.ForgeConfig{Name: "gh", Kind: "github", TokenFile: writeToken(t, "s3cret"), SSHKeyFile: key})

	env, cleanup, err := f.gitEnv()
	if err != nil {
		t.Fatal(err)
	}
	cmd := envOf(env, "GIT_SSH_COMMAND")
	for _, want := range []string{"ssh -F /dev/null ", "'IdentitiesOnly=yes'", "'IdentityAgent=none'", "'StrictHostKeyChecking=yes'", "'BatchMode=yes'", "'GlobalKnownHostsFile=/dev/null'", "'UpdateHostKeys=no'", "'ConnectTimeout=30'"} {
		if !strings.Contains(cmd, want) {
			t.Errorf("GIT_SSH_COMMAND %q lacks %q", cmd, want)
		}
	}
	for _, bad := range []string{"StrictHostKeyChecking=no", "accept-new", "s3cret", "PRIVATE KEY"} {
		if strings.Contains(strings.Join(env, "\n"), bad) {
			t.Errorf("env contains %q: %v", bad, env)
		}
	}
	if envOf(env, "GIT_SSH_VARIANT") != "ssh" || envOf(env, "GIT_TERMINAL_PROMPT") != "0" {
		t.Errorf("env = %v", env)
	}
	for _, e := range env {
		if strings.Contains(e, "extraHeader") || strings.Contains(e, "Authorization") {
			t.Errorf("HTTP auth in SSH mode: %q", e)
		}
	}

	opts := parsedSSHOptions(t, cmd)
	keyCopy, khCopy := opts["identityfile"], opts["userknownhostsfile"]
	if keyCopy == "" || keyCopy == key || filepath.Dir(keyCopy) != filepath.Dir(khCopy) {
		t.Fatalf("identityfile %q userknownhostsfile %q", keyCopy, khCopy)
	}
	st, err := os.Stat(keyCopy)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("key copy mode %o, want 600", st.Mode().Perm())
	}
	if st, err := os.Stat(filepath.Dir(keyCopy)); err != nil || st.Mode().Perm() != 0o700 {
		t.Errorf("temp dir: %v %v", st.Mode(), err)
	}
	if got, _ := os.ReadFile(keyCopy); string(got) != string(keyPEM) {
		t.Error("key copy differs")
	}
	kh, _ := os.ReadFile(khCopy)
	if !strings.HasPrefix(string(kh), "github.com ssh-ed25519 ") {
		t.Errorf("built-in known_hosts = %q", kh)
	}
	cleanup()
	if _, err := os.Stat(filepath.Dir(keyCopy)); !os.IsNotExist(err) {
		t.Errorf("temp dir not removed: %v", err)
	}

	// The key is read again for every operation: a rotated key is used
	// without a new Forge.
	_, rotated := newSSHKey(t)
	writeFile(t, dir, "id", rotated, 0o640)
	env, cleanup, err = f.gitEnv()
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(parsedSSHOptions(t, envOf(env, "GIT_SSH_COMMAND"))["identityfile"]); string(got) != string(rotated) {
		t.Error("rotated key not used")
	}
	cleanup()

	// A key file that went missing fails the operation, not the server.
	if err := os.Remove(key); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.gitEnv(); err == nil || !strings.Contains(err.Error(), "read ssh_key_file") {
		t.Errorf("gitEnv with the key gone: %v", err)
	}
}

// parsedSSHOptions runs GIT_SSH_COMMAND the way git does, through sh, with
// -G appended, so the real ssh prints the options it parsed. The keys are
// lowercase, as ssh -G prints them. It skips the test without ssh.
func parsedSSHOptions(t *testing.T, cmd string) map[string]string {
	t.Helper()
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Skip("ssh not installed")
	}
	// git runs: sh -c '<GIT_SSH_COMMAND> "$@"' '<GIT_SSH_COMMAND>' <args>
	c := exec.CommandContext(t.Context(), "sh", "-c", cmd+` "$@"`, cmd, "-G", "-p", "2222", "git@example.com")
	out, err := c.Output()
	if err != nil {
		t.Fatalf("%s -G: %v", cmd, err)
	}
	opts := map[string]string{}
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	for sc.Scan() {
		k, v, _ := strings.Cut(sc.Text(), " ")
		opts[k] = v
	}
	return opts
}

func TestSSHCommandQuoting(t *testing.T) {
	// Paths with spaces, both quote kinds, '%', '$', '\', ';' and a
	// leading '-' segment reach ssh intact, as one argument each.
	base := filepath.Join(t.TempDir(), `we ird 'q' "dq" 100% $HOME \x; -oX`)
	if err := os.MkdirAll(base, 0o700); err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(base, "id key")
	kh := filepath.Join(base, "known hosts")
	cmd, err := sshCommand(key, kh)
	if err != nil {
		t.Fatal(err)
	}
	opts := parsedSSHOptions(t, cmd)
	// ssh -G prints IdentityFile before its %-expansion (done when the key
	// is loaded, turning %% into %; TestSSHEndToEnd uses such a path for
	// real) and UserKnownHostsFile after it.
	if want := strings.ReplaceAll(key, "%", "%%"); opts["identityfile"] != want {
		t.Errorf("identityfile = %q, want %q", opts["identityfile"], want)
	}
	if opts["userknownhostsfile"] != kh {
		t.Errorf("userknownhostsfile = %q, want %q", opts["userknownhostsfile"], kh)
	}
	for k, want := range map[string]string{
		"stricthostkeychecking": "true",
		"batchmode":             "yes",
		"identitiesonly":        "yes",
		"globalknownhostsfile":  "/dev/null",
		"identityagent":         "none",
		"updatehostkeys":        "false",
		"port":                  "2222",
		"user":                  "git",
	} {
		if opts[k] != want {
			t.Errorf("%s = %q, want %q", k, opts[k], want)
		}
	}

	if _, err := sshCommand("/tmp/${HOME}/id", kh); err == nil {
		t.Error("a path with ${ was accepted")
	}
	if _, err := sshCommand("/tmp/a\nb/id", kh); err == nil {
		t.Error("a path with a newline was accepted")
	}
	if got := shellQuote(`it's`); got != `'it'\''s'` {
		t.Errorf("shellQuote = %s", got)
	}
}
