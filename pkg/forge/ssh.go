package forge

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/danielboothcloud/sundae/pkg/ssrf"
)

// Transports git uses to reach a forge, as ForgeStatus reports them.
const (
	// TransportHTTPS is git over HTTP(S) with the token (the default).
	TransportHTTPS = "https"
	// TransportSSH is git over SSH with ssh_key_file.
	TransportSSH = "ssh"
)

// sshConnectTimeout bounds establishing an SSH connection, in seconds. The
// whole operation is bounded by networkTimeout.
const sshConnectTimeout = 30

// Transport returns how git reaches the forge: TransportSSH when
// ssh_key_file is set, else TransportHTTPS.
func (f *Forge) Transport() string {
	if f.ssh != nil {
		return TransportSSH
	}
	return TransportHTTPS
}

// SSHURL returns the forge's SSH endpoint, e.g. ssh://git@github.com, or ""
// when git uses HTTPS.
func (f *Forge) SSHURL() string {
	if f.ssh == nil {
		return ""
	}
	return f.ssh.String()
}

// remoteFor returns the URL git uses for the forge repository at path.
// current is the remote recorded in the repository ("" for a new import)
// and listed the clone URL of the latest listing ("" when unknown).
//
// Over SSH the URL is always built from the path and the configured
// endpoint, never taken from the API. Over HTTPS the recorded remote is
// kept, as before, unless it is an SSH URL left from when the forge used
// SSH: then it is the listed clone URL, or <url>/<path>.git.
func (f *Forge) remoteFor(path, current, listed string) string {
	if f.ssh != nil {
		return f.ssh.String() + "/" + path + ".git"
	}
	if current != "" && !isSSHRemote(current) {
		return current
	}
	if listed != "" {
		return listed
	}
	return strings.TrimSuffix(f.base.String(), "/") + "/" + path + ".git"
}

// isSSHRemote reports whether a remote is an SSH URL, in URL or scp-like
// form.
func isSSHRemote(remote string) bool {
	lower := strings.ToLower(remote)
	for _, p := range []string{"ssh://", "git+ssh://", "ssh+git://"} {
		if strings.HasPrefix(lower, p) {
			return true
		}
	}
	// scp-like: [user@]host:path, with no slash before the colon.
	colon := strings.Index(remote, ":")
	return !strings.Contains(remote, "://") && colon > 0 && !strings.Contains(remote[:colon], "/")
}

// CheckRemote is the guard for every URL git is pointed at. Over SSH it
// accepts SSH URLs that checkSSHRemote does; anything else, and everything
// over HTTPS, must pass CheckCloneURL.
func (f *Forge) CheckRemote(raw string) error {
	if f.ssh != nil && isSSHRemote(raw) {
		return f.checkSSHRemote(raw)
	}
	return f.CheckCloneURL(raw)
}

// checkSSHRemote checks that an SSH remote is ssh://<user>@<host>[:port]
// of the configured endpoint followed by a plain repository path ending in
// .git, the way CheckCloneURL checks HTTPS clone URLs.
func (f *Forge) checkSSHRemote(raw string) error {
	if f.ssh == nil {
		return fmt.Errorf("%w: %q: the forge does not use SSH", ErrForeignCloneURL, raw)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%w: %q: %v", ErrForeignCloneURL, raw, err)
	}
	if !strings.EqualFold(u.Scheme, "ssh") || u.Opaque != "" || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("%w: %q", ErrForeignCloneURL, raw)
	}
	if _, hasPw := u.User.Password(); u.User == nil || hasPw || u.User.Username() != f.ssh.User.Username() ||
		sshHostPort(u) != sshHostPort(f.ssh) {
		return fmt.Errorf("%w: %q is not on %s", ErrForeignCloneURL, raw, f.ssh)
	}
	p, ok := strings.CutPrefix(u.Path, "/")
	if ok {
		p, ok = strings.CutSuffix(p, ".git")
	}
	if !ok {
		return fmt.Errorf("%w: %q: want %s/<path>.git", ErrForeignCloneURL, raw, f.ssh)
	}
	segs := strings.Split(p, "/")
	if len(segs) < 2 {
		return fmt.Errorf("%w: %q: want %s/<path>.git", ErrForeignCloneURL, raw, f.ssh)
	}
	for _, seg := range segs {
		if seg == "." || seg == ".." || !pathSegmentRe.MatchString(seg) {
			return fmt.Errorf("%w: %q", ErrForeignCloneURL, raw)
		}
	}
	return nil
}

// sshHostPort returns the lowercased host and port of an SSH URL, port 22
// when it names none.
func sshHostPort(u *url.URL) string {
	port := u.Port()
	if port == "" {
		port = "22"
	}
	return net.JoinHostPort(strings.ToLower(u.Hostname()), port)
}

// sshGitEnv returns the environment for a git command that talks to the
// forge over SSH, and a cleanup that removes the files it wrote.
//
// The key and known_hosts are read again for every operation (so rotation
// needs no restart) and copied into a private temporary directory: the key
// as a 0600 file, because ssh ignores a key file owned by the running user
// that group or others can read (OpenSSH sshkey_perm_ok), which a key
// mounted from a Secret or kept in a shared directory may well be.
//
// ssh gets no config file (-F /dev/null), only that key (no agent), only
// that known_hosts, strict host key checking and no prompts. No HTTP auth
// header is set.
func (f *Forge) sshGitEnv() ([]string, func(), error) {
	key, err := f.cfg.SSHKey()
	if err != nil {
		return nil, nil, err
	}
	kh, err := f.cfg.SSHKnownHosts()
	if err != nil {
		return nil, nil, err
	}
	dir, err := os.MkdirTemp("", "sundae-forge-ssh-")
	if err != nil {
		return nil, nil, fmt.Errorf("forge %s: %w", f.Name(), err)
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	fail := func(err error) ([]string, func(), error) {
		cleanup()
		return nil, nil, fmt.Errorf("forge %s: %w", f.Name(), err)
	}
	if dir, err = filepath.Abs(dir); err != nil {
		return fail(err)
	}
	if !bytes.HasSuffix(key, []byte("\n")) {
		// OpenSSH refuses a PEM key without its final newline.
		key = append(key, '\n')
	}
	keyPath := filepath.Join(dir, "id")
	khPath := filepath.Join(dir, "known_hosts")
	if err := writePrivate(keyPath, key); err != nil {
		return fail(err)
	}
	if err := writePrivate(khPath, kh); err != nil {
		return fail(err)
	}
	cmd, err := sshCommand(keyPath, khPath)
	if err != nil {
		return fail(err)
	}
	env := ssrf.GitEnv(ssrf.ValidatedGitRemote{Config: []ssrf.GitConfigEntry{{Key: "credential.helper", Value: ""}}})
	env = append(env,
		"GIT_TERMINAL_PROMPT=0",
		"GIT_SSH_COMMAND="+cmd,
		"GIT_SSH_VARIANT=ssh",
	)
	return env, cleanup, nil
}

// writePrivate writes a new file only the owner can read.
func writePrivate(path string, data []byte) error {
	fh, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := fh.Write(data); err != nil {
		fh.Close() //nolint:errcheck
		return err
	}
	return fh.Close()
}

// sshCommand returns GIT_SSH_COMMAND for a key and a known_hosts file.
//
// git runs it through sh, so every argument is single-quoted for the shell.
// ssh then parses each -o value like a config line: paths are double-quoted
// with '\' and '"' escaped (OpenSSH argv_split), and '%' is doubled because
// ssh expands %-tokens in IdentityFile and UserKnownHostsFile. ssh also
// expands ${VAR} there, which cannot be escaped, so a path containing "${"
// is refused, as are control characters.
func sshCommand(keyPath, knownHostsPath string) (string, error) {
	for _, p := range []string{keyPath, knownHostsPath} {
		if strings.Contains(p, "${") || strings.ContainsFunc(p, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
			return "", errors.New("temporary directory path cannot be passed to ssh: set TMPDIR to a plain path")
		}
	}
	opts := []string{
		"IdentitiesOnly=yes",
		"IdentityAgent=none",
		"IdentityFile=" + sshConfigQuote(keyPath),
		"UserKnownHostsFile=" + sshConfigQuote(knownHostsPath),
		"GlobalKnownHostsFile=/dev/null",
		"StrictHostKeyChecking=yes",
		"UpdateHostKeys=no",
		"BatchMode=yes",
		"ConnectTimeout=" + strconv.Itoa(sshConnectTimeout),
	}
	parts := []string{"ssh", "-F", "/dev/null"}
	for _, o := range opts {
		parts = append(parts, "-o", shellQuote(o))
	}
	return strings.Join(parts, " "), nil
}

// shellQuote quotes s as one POSIX shell word.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// sshConfigQuote quotes a path as one argument of an ssh config option.
func sshConfigQuote(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, "%", "%%")
	return `"` + s + `"`
}
