package openshell

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// allScripts are the shell scripts Soft Serve runs in sandboxes.
var allScripts = map[string]string{
	uploadBeginName:    uploadBeginScript,
	uploadChunkName:    uploadChunkScript,
	uploadEndName:      uploadEndScript,
	osewaProbeName:     osewaProbeScript,
	osewaInstallName:   osewaInstallScript,
	osewaServerName:    osewaServerScript,
	osewaEnrollName:    osewaEnrollScript,
	osewaHostsName:     osewaHostsScript,
	osewaListName:      osewaListScript,
	osewaNewName:       osewaNewScript,
	gitCredentialsName: gitCredentialsScript,
}

func TestScriptsParse(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	for name, script := range allScripts {
		if out, err := exec.CommandContext(t.Context(), sh, "-n", "-c", script, name).CombinedOutput(); err != nil {
			t.Errorf("%s: %v: %s", name, err, out)
		}
		// Secrets never reach a script as an argument.
		if regexp.MustCompile(`\$\{?SOFT_SERVE_[A-Z_]+`).MatchString(script) {
			for _, v := range regexp.MustCompile(`\$\{?(SOFT_SERVE_[A-Z_]+)`).FindAllStringSubmatch(script, -1) {
				switch v[1] {
				case "SOFT_SERVE_OSEWA_KEY", "SOFT_SERVE_OSEWA_HOSTS", "SOFT_SERVE_GITCONFIG":
				default:
					t.Errorf("%s reads unexpected variable %s", name, v[1])
				}
			}
		}
	}
}

// The credential script, run for real: a private file, included once from
// ~/.gitconfig, which git then applies to the clone URL only.
func TestGitCredentialsScript(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	home := t.TempDir()
	dir := filepath.Join(home, "tmp", "soft-serve")
	file := filepath.Join(dir, "gitconfig")
	run := func(content string) {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "sh", "-c", gitCredentialsScript, gitCredentialsName, dir, file)
		cmd.Env = []string{"HOME=" + home, "PATH=" + os.Getenv("PATH"), "SOFT_SERVE_GITCONFIG=" + execEnvValue(content)}
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
	}
	run(gitCredentialConfig("http://soft-serve.svc:23232", "alice", "first"))
	run(gitCredentialConfig("http://soft-serve.svc:23232", "alice", "second"))

	st, err := os.Stat(dir)
	if err != nil || st.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode = %v, err = %v", st.Mode(), err)
	}
	if st, err := os.Stat(file); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("file mode = %v, err = %v", st.Mode(), err)
	}
	gitconfig, err := os.ReadFile(filepath.Join(home, ".gitconfig"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(gitconfig), file) != 1 || strings.Contains(string(gitconfig), "Basic") {
		t.Fatalf(".gitconfig = %q", gitconfig)
	}

	git := func(args ...string) string {
		cmd := exec.CommandContext(t.Context(), "git", args...)
		cmd.Env = []string{"HOME=" + home, "PATH=" + os.Getenv("PATH"), "GIT_CONFIG_NOSYSTEM=1"}
		out, _ := cmd.Output()
		return strings.TrimSpace(string(out))
	}
	want := "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("alice:second"))
	if got := git("config", "--get-urlmatch", "http.extraHeader", "http://soft-serve.svc:23232/r.git"); got != want {
		t.Fatalf("header for the clone URL = %q, want %q", got, want)
	}
	if got := git("config", "--get-urlmatch", "http.extraHeader", "https://github.com/r.git"); got != "" {
		t.Fatalf("header leaks to another host: %q", got)
	}
}

func TestNewOsewaKey(t *testing.T) {
	pub, keyPEM, err := newOsewaKey()
	if err != nil {
		t.Fatal(err)
	}
	block, rest := pem.Decode([]byte(keyPEM))
	if block == nil || block.Type != "PRIVATE KEY" || len(rest) != 0 {
		t.Fatalf("key = %q", keyPEM)
	}
	// osewa writes and reads the 48-byte PKCS#8 v1 form: a fixed 16-byte
	// prefix and the 32-byte seed (crates/auth/src/keys.rs PKCS8_PREFIX).
	prefix := []byte{0x30, 0x2e, 0x02, 0x01, 0x00, 0x30, 0x05, 0x06, 0x03, 0x2b, 0x65, 0x70, 0x04, 0x22, 0x04, 0x20}
	if len(block.Bytes) != 48 || string(block.Bytes[:16]) != string(prefix) {
		t.Fatalf("DER = % x", block.Bytes)
	}
	sk, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := base64.StdEncoding.DecodeString(pub)
	if err != nil || string(raw) != string(sk.(ed25519.PrivateKey).Public().(ed25519.PublicKey)) {
		t.Fatalf("public key %q does not match", pub)
	}
}

func TestOsewaArch(t *testing.T) {
	for machine, want := range map[string]string{"x86_64": "amd64", "amd64": "amd64", "aarch64": "arm64", "arm64": "arm64"} {
		if got, err := osewaArch(machine); err != nil || got != want {
			t.Errorf("osewaArch(%q) = %q, %v", machine, got, err)
		}
	}
	if _, err := osewaArch("armv7l"); err == nil {
		t.Error("armv7l accepted")
	}
}

func TestOsewaAttachCommand(t *testing.T) {
	got := strings.Join(OsewaAttachCommand("/usr/local/bin/osewa", "abc"), " ")
	if got != "/usr/local/bin/osewa attach --take --predict never sandbox abc" {
		t.Fatalf("attach = %q", got)
	}
}

// The pinned osewa release is the same in the code and in both
// Containerfiles that install it.
func TestOsewaPinMatchesContainerfiles(t *testing.T) {
	for _, p := range []string{"../../Containerfile", "../../deploy/sandbox/Containerfile"} {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		for arg, want := range map[string]string{
			"OSEWA_VERSION":      OsewaVersion,
			"OSEWA_SHA256_AMD64": OsewaSHA256LinuxAMD64,
			"OSEWA_SHA256_ARM64": OsewaSHA256LinuxARM64,
		} {
			m := regexp.MustCompile(`(?m)^ARG ` + arg + `=(\S+)$`).FindSubmatch(b)
			if m == nil || string(m[1]) != want {
				t.Errorf("%s: ARG %s = %q, want %q", p, arg, m, want)
			}
		}
	}
}
