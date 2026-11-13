package config

import (
	"crypto/ed25519"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// DefaultForgeSSHUser is the SSH user of a forge SSH endpoint whose ssh_url
// names none. GitHub and GitLab both serve git over SSH as "git".
const DefaultForgeSSHUser = "git"

// builtinForgeHostKeys are the SSH host keys of GitHub.com and GitLab.com,
// used for a forge whose ssh_known_hosts_file is empty. Any other SSH host
// needs a known_hosts file.
//
// Sources (checked 2026-11; the SHA256 fingerprints were compared with
// ssh-keyscan of each host):
//   - GitHub: https://api.github.com/meta ("ssh_keys") and
//     https://docs.github.com/en/authentication/keeping-your-account-and-data-secure/githubs-ssh-key-fingerprints
//     (SHA256:+DiY3wvvV6TuJJhbpZisF/zLDA0zPMSvHdkr4UvCOqU ED25519,
//     SHA256:p2QAMXNIC1TJYWeIOttrVc98/R1BUFWu3/LiyKgUfQM ECDSA,
//     SHA256:uNiVztksCsDhcc0u9e8BujQXVUpKZIDTMczCvj3tD2s RSA).
//     ssh.github.com (port 443) serves the same keys:
//     https://docs.github.com/en/authentication/troubleshooting-ssh/using-ssh-over-the-https-port
//   - GitLab: https://docs.gitlab.com/user/gitlab_com/#ssh-known_hosts-entries
//     (SHA256:eUXGGm1YGsMAS7vkcx6JOJdOGHPem5gQp4taiCfCLB8 ED25519,
//     SHA256:HbW3g8zUjNSksFbqTiUWPWg2Bq1x8xdGUrliXFzSnUw ECDSA,
//     SHA256:ROQFvPThGrW4RuWLoL9tq9I9zJ42fK4XywyRtbOz/EQ RSA).
//     altssh.gitlab.com (port 443) serves the same keys:
//     https://docs.gitlab.com/user/gitlab_com/#alternative-ssh-port
//
// If GitHub or GitLab rotate a key, update it here; until then a forge can
// set ssh_known_hosts_file.
var builtinForgeHostKeys = map[string][]string{
	"github.com":        githubHostKeys,
	"ssh.github.com":    githubHostKeys,
	"gitlab.com":        gitlabHostKeys,
	"altssh.gitlab.com": gitlabHostKeys,
}

var githubHostKeys = []string{
	"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl",
	"ecdsa-sha2-nistp256 AAAAE2VjZHNhLXNoYTItbmlzdHAyNTYAAAAIbmlzdHAyNTYAAABBBEmKSENjQEezOmxkZMy7opKgwFB9nkt5YRrYMjNuG5N87uRgg6CLrbo5wAdT/y6v0mKV0U2w0WZ2YB/++Tpockg=",
	"ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABgQCj7ndNxQowgcQnjshcLrqPEiiphnt+VTTvDP6mHBL9j1aNUkY4Ue1gvwnGLVlOhGeYrnZaMgRK6+PKCUXaDbC7qtbW8gIkhL7aGCsOr/C56SJMy/BCZfxd1nWzAOxSDPgVsmerOBYfNqltV9/hWCqBywINIR+5dIg6JTJ72pcEpEjcYgXkE2YEFXV1JHnsKgbLWNlhScqb2UmyRkQyytRLtL+38TGxkxCflmO+5Z8CSSNY7GidjMIZ7Q4zMjA2n1nGrlTDkzwDCsw+wqFPGQA179cnfGWOWRVruj16z6XyvxvjJwbz0wQZ75XK5tKSb7FNyeIEs4TT4jk+S4dhPeAUC5y+bDYirYgM4GC7uEnztnZyaVWQ7B381AK4Qdrwt51ZqExKbQpTUNn+EjqoTwvqNj4kqx5QUCI0ThS/YkOxJCXmPUWZbhjpCg56i+2aB6CmK2JGhn57K5mj0MNdBXA4/WnwH6XoPWJzK5Nyu2zB3nAZp+S5hpQs+p1vN1/wsjk=",
}

var gitlabHostKeys = []string{
	"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAfuCHKVTjquxvt6CM6tdG4SLp1Btn/nOeHHE5UOzRdf",
	"ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABAQCsj2bNKTBSpIYDEGk9KxsGh3mySTRgMtXL583qmBpzeQ+jqCMRgBqB98u3z++J1sKlXHWfM9dyhSevkMwSbhoR8XIq/U0tCNyokEi/ueaBMCvbcTHhO7FcwzY92WK4Yt0aGROY5qX2UKSeOvuP4D6TPqKF1onrSzH9bx9XUf2lEdWT/ia1NEKjunUqu1xOB/StKDHMoX4/OKyIzuS0q/T1zOATthvasJFoPrAjkohTyaDUz2LN5JoH839hViyEG82yB+MjcFV5MU3N1l1QL3cVUCh93xSaua1N85qivl+siMkPGbO5xR/En4iEY6K2XPASUEMaieWVNTRCtJ4S8H+9",
	"ecdsa-sha2-nistp256 AAAAE2VjZHNhLXNoYTItbmlzdHAyNTYAAAAIbmlzdHAyNTYAAABBBFSMqzJeV9rUzU4kWitGjeR4PWSa29SPqJ1fVkhtj3Hw9xjLVXVYrU9QlYWrOLXBpQ6KWjbjTDTdDkoohFzgbEY=",
}

// BuiltinForgeKnownHosts returns known_hosts lines with the built-in host
// keys for host (a host name, without port) as served on port, or false
// when Sundae has none for host.
func BuiltinForgeKnownHosts(host, port string) ([]byte, bool) {
	keys, ok := builtinForgeHostKeys[strings.ToLower(host)]
	if !ok {
		return nil, false
	}
	pattern := knownhosts.Normalize(net.JoinHostPort(strings.ToLower(host), port))
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(pattern + " " + k + "\n")
	}
	return []byte(b.String()), true
}

// sshUserRe and sshHostRe restrict what an ssh_url may name, so neither can
// be read as an ssh option.
var (
	sshUserRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]*$`)
	sshHostRe = regexp.MustCompile(`^[A-Za-z0-9_.:-]+$`)
)

// ParseForgeSSHURL parses an ssh_url: ssh://[user@]host[:port], with no
// path. It returns the normalized endpoint: the user defaults to "git" and
// the port to 22; the port is only written when it is not 22.
func ParseForgeSSHURL(raw string) (*url.URL, error) {
	errScheme := errors.New("must be an ssh:// URL, e.g. ssh://git@ssh.github.com:443")
	raw = strings.TrimSpace(raw)
	if !strings.HasPrefix(strings.ToLower(raw), "ssh://") {
		return nil, errScheme
	}
	if len(raw) > len("ssh://") {
		raw = strings.TrimSuffix(raw, "/")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	if !strings.EqualFold(u.Scheme, "ssh") || u.Opaque != "" {
		return nil, errScheme
	}
	host := u.Hostname()
	if host == "" {
		return nil, errors.New("must include a host")
	}
	if strings.HasPrefix(host, "-") || !sshHostRe.MatchString(host) {
		return nil, fmt.Errorf("invalid host %q", host)
	}
	user := DefaultForgeSSHUser
	if u.User != nil {
		if _, ok := u.User.Password(); ok {
			return nil, errors.New("must not include a password; use ssh_key_file")
		}
		if n := u.User.Username(); n != "" {
			user = n
		}
	}
	if !sshUserRe.MatchString(user) {
		return nil, fmt.Errorf("invalid user %q", user)
	}
	port := u.Port()
	if port == "" {
		port = "22"
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return nil, fmt.Errorf("invalid port %q", port)
	}
	if u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("must not include a path, query or fragment: repository paths are added to it")
	}
	out := &url.URL{Scheme: "ssh", User: url.User(user), Host: strings.ToLower(host)}
	if strings.Contains(host, ":") {
		out.Host = "[" + strings.ToLower(host) + "]"
	}
	if port != "22" {
		out.Host = net.JoinHostPort(strings.ToLower(host), port)
	}
	return out, nil
}

// UsesSSH reports whether git operations with the forge go over SSH, which
// is the case when ssh_key_file is set.
func (f ForgeConfig) UsesSSH() bool { return f.SSHKeyFile != "" }

// SSHEndpoint returns the forge's normalized SSH endpoint: ssh_url, or
// ssh://git@<url host> (port 22).
func (f ForgeConfig) SSHEndpoint() (*url.URL, error) {
	raw := f.SSHURL
	if raw == "" {
		u, err := url.Parse(f.URL)
		if err != nil || u.Hostname() == "" {
			return nil, fmt.Errorf("forge %s: no host in url %q to derive ssh_url from", f.Name, f.URL)
		}
		raw = "ssh://" + DefaultForgeSSHUser + "@" + u.Hostname()
		if strings.Contains(u.Hostname(), ":") {
			raw = "ssh://" + DefaultForgeSSHUser + "@[" + u.Hostname() + "]"
		}
	}
	u, err := ParseForgeSSHURL(raw)
	if err != nil {
		return nil, fmt.Errorf("forge %s: ssh_url %q: %w", f.Name, raw, err)
	}
	return u, nil
}

// SSHKey returns the contents of ssh_key_file. It is read on every call, so
// a rotated key is picked up without a restart.
func (f ForgeConfig) SSHKey() ([]byte, error) {
	key, err := readSSHKeyFile(f.SSHKeyFile)
	if err != nil {
		return nil, fmt.Errorf("forge %s: %w", f.Name, err)
	}
	return key, nil
}

// SSHKnownHosts returns the known_hosts content for the forge's SSH
// endpoint: ssh_known_hosts_file, read on every call, or the built-in keys
// of github.com and gitlab.com.
func (f ForgeConfig) SSHKnownHosts() ([]byte, error) {
	if f.SSHKnownHostsFile != "" {
		bts, err := os.ReadFile(f.SSHKnownHostsFile)
		if err != nil {
			return nil, fmt.Errorf("forge %s: read ssh_known_hosts_file: %w", f.Name, err)
		}
		return bts, nil
	}
	ep, err := f.SSHEndpoint()
	if err != nil {
		return nil, err
	}
	kh, ok := BuiltinForgeKnownHosts(ep.Hostname(), sshPort(ep))
	if !ok {
		return nil, fmt.Errorf("forge %s: ssh_known_hosts_file is required for SSH host %s", f.Name, ep.Hostname())
	}
	return kh, nil
}

// sshPort returns the port of an SSH endpoint, 22 when it names none.
func sshPort(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	return "22"
}

// readSSHKeyFile reads a private key file. An empty file is an error.
func readSSHKeyFile(path string) ([]byte, error) {
	bts, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read ssh_key_file: %w", err)
	}
	if strings.TrimSpace(string(bts)) == "" {
		return nil, fmt.Errorf("ssh_key_file %q is empty", path)
	}
	return bts, nil
}

// validateForgeSSH validates the SSH settings of one forge, resolving
// relative paths against dataPath. It reads the key and known_hosts files
// once, so a missing or unusable file fails at startup and in
// `sundae config check`.
func validateForgeSSH(f *ForgeConfig, dataPath string, fail func(string, ...any)) {
	f.SSHKeyFile = strings.TrimSpace(f.SSHKeyFile)
	f.SSHKnownHostsFile = strings.TrimSpace(f.SSHKnownHostsFile)
	f.SSHURL = strings.TrimSuffix(strings.TrimSpace(f.SSHURL), "/")
	if f.SSHKeyFile == "" {
		if f.SSHURL != "" {
			fail("ssh_url is set but ssh_key_file is not; git uses SSH only with ssh_key_file")
		}
		if f.SSHKnownHostsFile != "" {
			fail("ssh_known_hosts_file is set but ssh_key_file is not; git uses SSH only with ssh_key_file")
		}
		return
	}

	for _, p := range []*string{&f.SSHKeyFile, &f.SSHKnownHostsFile} {
		if *p != "" && !filepath.IsAbs(*p) {
			*p = filepath.Join(dataPath, *p)
		}
	}

	if key, err := readSSHKeyFile(f.SSHKeyFile); err != nil {
		fail("%v", err)
	} else if _, err := ssh.ParseRawPrivateKey(key); err != nil {
		var pm *ssh.PassphraseMissingError
		if errors.As(err, &pm) {
			fail("ssh_key_file %q is passphrase-protected; git runs ssh unattended, so use a key without a passphrase", f.SSHKeyFile)
		} else {
			fail("ssh_key_file %q is not a usable private key: %v", f.SSHKeyFile, err)
		}
	}

	var ep *url.URL
	if f.SSHURL != "" {
		u, err := ParseForgeSSHURL(f.SSHURL)
		if err != nil {
			fail("ssh_url %q: %v", f.SSHURL, err)
			return
		}
		ep = u
	} else {
		u, err := f.SSHEndpoint()
		if err != nil {
			// The url problem is already reported.
			return
		}
		ep = u
	}

	hostPort := net.JoinHostPort(ep.Hostname(), sshPort(ep))
	if f.SSHKnownHostsFile == "" {
		if _, ok := BuiltinForgeKnownHosts(ep.Hostname(), sshPort(ep)); !ok {
			fail("ssh_known_hosts_file is required for SSH host %s: built-in host keys cover only github.com and gitlab.com", ep.Hostname())
		}
		return
	}
	if err := checkKnownHostsFile(f.SSHKnownHostsFile, hostPort); err != nil {
		fail("ssh_known_hosts_file: %v", err)
	}
}

// checkKnownHostsFile checks that a known_hosts file parses and has a key
// for hostPort (plain, [host]:port, wildcard or hashed entries).
func checkKnownHostsFile(path, hostPort string) error {
	if _, err := os.ReadFile(path); err != nil {
		return fmt.Errorf("read %w", err)
	}
	cb, err := knownhosts.New(path)
	if err != nil {
		return fmt.Errorf("%q: %w", path, err)
	}
	// Probe with a key no host has: the KeyError lists the keys the file
	// does have for the host.
	probe, err := ssh.NewPublicKey(ed25519.PublicKey(make([]byte, ed25519.PublicKeySize)))
	if err != nil {
		return err
	}
	err = cb(hostPort, &net.TCPAddr{IP: net.IPv4zero, Port: 22}, probe)
	var ke *knownhosts.KeyError
	if errors.As(err, &ke) && len(ke.Want) > 0 {
		return nil
	}
	return fmt.Errorf("%q has no host key for %s", path, knownhosts.Normalize(hostPort))
}
