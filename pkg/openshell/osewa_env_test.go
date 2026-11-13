package openshell

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// What the OpenShell supervisor gives an exec of a sandbox with a
// provider attached, besides the exec request's own environment: the
// provider's credential as a placeholder (openshell-core 0.1.8 secrets.rs,
// placeholder_for_env_key_for_revision), its TLS trust variables and
// SANDBOX. weirdValue has every character sh quoting must survive.
const (
	placeholderValue = "openshell:resolve:env:v3_ANTHROPIC_API_KEY"
	weirdValue       = `it's a $x ` + "`y`" + ` "q" \ done`
)

func supervisorEnv() map[string]string {
	return map[string]string{
		"SANDBOX":           "1",
		"ANTHROPIC_API_KEY": placeholderValue,
		"WEIRD_SETTING":     weirdValue,
		"SSL_CERT_FILE":     "/etc/openshell-tls/ca-bundle.pem",
		"TERM":              "dumb",
		"SUNDAE_PROBE":      "not for the session",
	}
}

// envProgram is a session program that writes what it and a `bash -l` it
// starts see to out, then exits.
const envProgram = `{
printf 'key=%s\n' "${ANTHROPIC_API_KEY-unset}"
printf 'weird=%s\n' "${WEIRD_SETTING-unset}"
printf 'tls=%s\n' "${SSL_CERT_FILE-unset}"
printf 'sandbox=%s\n' "${SANDBOX-unset}"
printf 'osewa=%s\n' "${OSEWA_HOME-unset}"
printf 'sundae=%s\n' "${SUNDAE_PROBE-unset}"
printf 'home=%s\n' "$HOME"
printf 'pwd=%s\n' "$(pwd -P)"
bash -lc 'printf "child=%s\n" "${ANTHROPIC_API_KEY-unset}"'
} > out.part && mv out.part out`

// useLocalPaths points Sundae's sandbox paths into home.
func useLocalPaths(t *testing.T, home string) {
	t.Helper()
	for _, v := range []struct {
		p   *string
		val string
	}{
		{&osewaInstallDir, filepath.Join(home, "not-installed")},
		{&osewaClientDir, filepath.Join(home, ".config/sundae/osewa-client")},
		{&osewaSessionFile, filepath.Join(home, ".config/sundae/osewa-session")},
		{&osewaSessionEnvFile, filepath.Join(home, "tmp-sundae/session-env")},
	} {
		old := *v.p
		*v.p = v.val
		t.Cleanup(func() { *v.p = old })
	}
	if err := os.MkdirAll(filepath.Dir(osewaSessionEnvFile), 0o700); err != nil {
		t.Fatal(err)
	}
}

// localHome returns a short temporary home with bin and work directories:
// osewa's holder sockets live under HOME, and a Unix socket path is limited
// to about 100 bytes.
func localHome(t *testing.T) (home, binDir, work string) {
	t.Helper()
	for _, tool := range []string{"sh", "bash", "sed", "env"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not found", tool)
		}
	}
	home, err := os.MkdirTemp("/tmp", "ssw")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	binDir, work = filepath.Join(home, "bin"), filepath.Join(home, "r")
	for _, d := range []string{binDir, work} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return home, binDir, work
}

// readEnvOut waits for envProgram's output in work and parses it.
func readEnvOut(t *testing.T, work string) map[string]string {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		b, err := os.ReadFile(filepath.Join(work, "out"))
		if err == nil {
			_ = os.Remove(filepath.Join(work, "out"))
			got := map[string]string{}
			for line := range strings.SplitSeq(strings.TrimSuffix(string(b), "\n"), "\n") {
				k, v, _ := strings.Cut(line, "=")
				got[k] = v
			}
			return got
		}
		if time.Now().After(deadline) {
			t.Fatal("the session program wrote nothing")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// checkForwarded checks what a session program of a session with providers
// saw: the supervisor's environment, less TERM and Sundae's own settings,
// in its working directory and passed on to `bash -l`, and that the staged
// file is gone.
func checkForwarded(t *testing.T, got map[string]string, home, work string) {
	t.Helper()
	wd, err := filepath.EvalSymlinks(work)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"key":     placeholderValue,
		"weird":   weirdValue,
		"tls":     "/etc/openshell-tls/ca-bundle.pem",
		"sandbox": "1",
		"osewa":   "unset",
		"sundae":  "unset",
		"home":    home,
		"pwd":     wd,
		"child":   placeholderValue,
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q (all: %q)", k, got[k], v, got)
		}
	}
	staged, _ := filepath.Glob(osewaSessionEnvFile + "*")
	if len(staged) != 0 {
		t.Errorf("staged environment left behind: %v", staged)
	}
}

// fakeOsewaNew stands in for `osewa new HOST -- argv...`: it runs argv to
// completion with a clean environment, as osewa 0.1.4 builds one for a
// session (HOME, a base PATH, nothing of its caller's), and prints an id.
const fakeOsewaNew = `#!/bin/sh
[ "$1" = new ] && [ "$3" = -- ] || { echo "unexpected: $*" >&2; exit 2; }
shift 3
env -i HOME="$HOME" PATH=/usr/bin:/bin "$@" >/dev/null 2>&1
echo "fake-$$"
`

// The program of a session with providers gets the OpenShell exec
// environment through the staged file, though osewa gives sessions a clean
// one; without providers nothing is staged. The script runs with this
// machine's sh, and with dash where there is one (Debian and Ubuntu
// images' sh).
func TestOsewaSessionEnv(t *testing.T) {
	t.Run("sh", func(t *testing.T) { testOsewaSessionEnv(t, "") })
	t.Run("dash", func(t *testing.T) {
		dash, err := exec.LookPath("dash")
		if err != nil {
			t.Skip("dash not found")
		}
		testOsewaSessionEnv(t, dash)
	})
}

func testOsewaSessionEnv(t *testing.T, shell string) {
	ctx := t.Context()
	home, binDir, work := localHome(t)
	useLocalPaths(t, home)
	if shell != "" {
		if err := os.Symlink(shell, filepath.Join(binDir, "sh")); err != nil {
			t.Fatal(err)
		}
	}
	bin := filepath.Join(binDir, "osewa")
	if err := os.WriteFile(bin, []byte(fakeOsewaNew), 0o700); err != nil { //nolint:gosec
		t.Fatal(err)
	}
	lc := &localClient{home: home, bin: binDir, sandbox: SessionSandboxName("alice", "main"), supervisorEnv: supervisorEnv()}
	m := &Manager{client: lc}
	command := []string{"/bin/sh", "-c", envProgram}

	id, started, err := m.newOsewaSession(ctx, lc.sandbox, bin, work, command, "", true)
	if err != nil || !started || !strings.HasPrefix(id, "fake-") {
		t.Fatalf("with providers: id=%q started=%v err=%v", id, started, err)
	}
	checkForwarded(t, readEnvOut(t, work), home, work)

	id, started, err = m.newOsewaSession(ctx, lc.sandbox, bin, work, command, id, false)
	if err != nil || !started {
		t.Fatalf("without providers: id=%q started=%v err=%v", id, started, err)
	}
	if got := readEnvOut(t, work); got["key"] != "unset" || got["tls"] != "unset" || got["child"] != "unset" {
		t.Fatalf("without providers the program saw %q", got)
	}

	// A failed create removes the staged file.
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil { //nolint:gosec
		t.Fatal(err)
	}
	if _, _, err := m.newOsewaSession(ctx, lc.sandbox, bin, work, command, id, true); err == nil {
		t.Fatal("expected the create to fail")
	}
	if staged, _ := filepath.Glob(osewaSessionEnvFile + "*"); len(staged) != 0 {
		t.Fatalf("staged environment left behind: %v", staged)
	}
}

// TestOsewaLiveSessionEnv is TestOsewaSessionEnv against a real osewa
// server and holder (SUNDAE_TEST_OSEWA, the pinned version for this
// machine): the provider placeholder reaches the session's program and
// its `bash -l`, and does not without the staged file, although the
// server itself was started with it in its environment.
func TestOsewaLiveSessionEnv(t *testing.T) {
	osewa := os.Getenv("SUNDAE_TEST_OSEWA")
	if osewa == "" {
		t.Skip("set SUNDAE_TEST_OSEWA to an osewa " + OsewaVersion + " binary for this machine")
	}
	osewa, err := filepath.Abs(osewa)
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	home, binDir, work := localHome(t)
	useLocalPaths(t, home)
	bin := filepath.Join(binDir, "osewa")
	if err := os.Symlink(osewa, bin); err != nil {
		t.Fatal(err)
	}

	var lc0 net.ListenConfig
	l, err := lc0.Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	lc := &localClient{home: home, bin: binDir, sandbox: SessionSandboxName("alice", "main"), supervisorEnv: supervisorEnv()}
	m := &Manager{client: lc, osewaPort: port}
	t.Cleanup(func() {
		_ = exec.CommandContext(context.Background(), "pkill", "-f", "--", "--listen 127.0.0.1:"+strconv.Itoa(port)).Run()
	})
	if err := m.startOsewaServer(ctx, lc.sandbox, bin); err != nil {
		t.Fatal(err)
	}
	if err := m.enrollBridge(ctx, lc.sandbox, bin); err != nil {
		t.Fatal(err)
	}
	command := []string{"/bin/sh", "-c", envProgram}
	kill := func(id string) {
		t.Cleanup(func() {
			_, _ = lc.Run(context.Background(), lc.sandbox, []string{bin, "local", "kill", id}, ExecOptions{Env: osewaEnv()})
		})
	}

	id, started, err := m.ensureOsewaSession(ctx, lc.sandbox, bin, work, command, true)
	if err != nil || !started {
		t.Fatalf("with providers: id=%q started=%v err=%v", id, started, err)
	}
	kill(id)
	checkForwarded(t, readEnvOut(t, work), home, work)

	// The control: osewa's own session environment has none of it.
	id, started, err = m.newOsewaSession(ctx, lc.sandbox, bin, work, command, id, false)
	if err != nil || !started {
		t.Fatalf("without providers: id=%q started=%v err=%v", id, started, err)
	}
	kill(id)
	if got := readEnvOut(t, work); got["key"] != "unset" || got["weird"] != "unset" || got["child"] != "unset" {
		t.Fatalf("osewa passed the server's environment on: %q", got)
	}
}
