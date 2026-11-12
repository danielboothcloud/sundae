package openshell

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"maps"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
)

// osewa (https://github.com/rjyo/osewa) holds each session's program in the
// sandbox: `osewa server` runs detached, one holder process per osewa session
// owns the program's PTY, and the SSH bridge runs `osewa attach` against the
// loopback server. Closing the SSH connection ends only the attach client;
// the holder and the program keep running.
//
// The osewa protocol is a draft that may change until v1, so Soft Serve pins
// one release. Change OsewaVersion, the checksums below, the
// OSEWA_* build arguments in Containerfile and deploy/sandbox/Containerfile
// together; TestOsewaPinMatchesContainerfiles checks they agree.
const (
	// OsewaVersion is the pinned osewa release.
	OsewaVersion = "0.1.4"
	// OsewaSHA256LinuxAMD64 is the SHA-256 of osewa-linux-amd64.gz in that
	// release's SHA256SUMS.
	OsewaSHA256LinuxAMD64 = "160667bfd60e999b4059b30dfad74fd38c8ab0da413668ec176678a4a0d4bb58"
	// OsewaSHA256LinuxARM64 is the SHA-256 of osewa-linux-arm64.gz in that
	// release's SHA256SUMS.
	OsewaSHA256LinuxARM64 = "5a08537f6ebd2b2b6870a54c2c6a78cb26a16d1426e261ab5b8b896d24967325"
)

// Where Soft Serve keeps its state in a sandbox. Everything is under the
// sandbox user's home, SandboxRoot, except the git credentials (see
// gitCredentialDir).
var (
	// osewaInstallDir holds the osewa binary Soft Serve uploads when the
	// image lacks the pinned release. It is versioned, so a pin bump
	// installs next to the old build instead of replacing a running one.
	osewaInstallDir = path.Join(SandboxRoot, ".local/lib/soft-serve")
	// osewaClientDir is the bridge's osewa client state (client.key,
	// hosts.json), apart from ~/.config/osewa, which holds the server's
	// (server.key, devices), so nothing of a user's own osewa use is
	// touched.
	osewaClientDir = path.Join(SandboxRoot, ".config/soft-serve/osewa-client")
	// osewaSessionFile records the id of the osewa session holding this
	// sandbox's program. The osewa CLI cannot tag a session, so the id is
	// how Soft Serve finds it again.
	osewaSessionFile = path.Join(SandboxRoot, ".config/soft-serve/osewa-session")
)

const (
	// osewaHost is the bridge's name for the sandbox's own server in
	// hosts.json.
	osewaHost = "sandbox"
	// osewaDevice is the device name the bridge client enrols as.
	osewaDevice = "soft-serve-bridge"
)

// osewaPath is where an uploaded osewa binary of the pinned version lives.
func osewaPath() string {
	return path.Join(osewaInstallDir, "osewa-"+OsewaVersion)
}

// osewaSHA256 maps a release build's architecture to its pinned checksum.
var osewaSHA256 = map[string]string{
	"amd64": OsewaSHA256LinuxAMD64,
	"arm64": OsewaSHA256LinuxARM64,
}

// osewaArch returns the release build architecture for a sandbox's machine
// name, as `uname -m` prints it.
func osewaArch(machine string) (string, error) {
	switch machine {
	case "x86_64", "amd64":
		return "amd64", nil
	case "aarch64", "arm64":
		return "arm64", nil
	default:
		return "", fmt.Errorf("openshell: no osewa build for sandbox architecture %q", machine)
	}
}

// osewaEnv is the environment every osewa command in the sandbox runs with:
// a fixed home, so the server, holders, `osewa local` and the bridge client
// agree on their directories whatever the image's passwd says; sessions'
// HOME set to the same (OSEWA_HOME); no self-updating.
func osewaEnv() map[string]string {
	return map[string]string{
		"HOME":              SandboxRoot,
		"OSEWA_HOME":        SandboxRoot,
		"OSEWA_AUTO_UPDATE": "off",
	}
}

// Bootstrap scripts. Each runs as `sh -c <script> <name> <args...>`: the
// name shows in the sandbox's process list and lets tests tell the steps
// apart. Arguments are paths and versions, never secrets; secrets travel in
// the exec environment, base64-encoded, because the gateway refuses
// environment values with newlines (see execEnvValue).
const (
	osewaProbeName = "soft-serve-osewa-probe"
	// $1: pinned version, $2: the uploaded binary's path, $3: the client
	// directory. Prints key=value lines: osewa (the binary to use, empty
	// when none is the pinned version), arch (uname -m) and enrolled.
	osewaProbeScript = `pinned() { "$1" version --json 2>/dev/null | grep -q "\"osewa\":\"$2\""; }
bin=
if p=$(command -v osewa 2>/dev/null) && pinned "$p" "$1"; then bin=$p
elif [ -x "$2" ] && pinned "$2" "$1"; then bin=$2
fi
echo "osewa=$bin"
echo "arch=$(uname -m)"
if [ -s "$3/client.key" ] && [ -s "$3/hosts.json" ]; then echo enrolled=yes; else echo enrolled=no; fi`

	osewaInstallName = "soft-serve-osewa-install"
	// $1: the uploaded .gz, $2: its pinned SHA-256, $3: the binary's path.
	// Verifies, unpacks and renames the binary into place.
	osewaInstallScript = `set -e
echo "$2  $1" | sha256sum -c - >/dev/null
gzip -dc "$1" > "$3.part"
chmod 0755 "$3.part"
mv -f "$3.part" "$3"
rm -f "$1"`

	osewaServerName = "soft-serve-osewa-server"
	// $1: the binary, $2: the listen address. --daemon returns once the
	// server listens, or at once when one is already running. The server
	// runs in its own session (setsid), so it outlives this exec.
	osewaServerScript = `exec "$1" server --daemon --listen "$2" --no-quic --no-forward`

	osewaEnrollName = "soft-serve-osewa-enroll"
	// $1: the binary, $2: the client directory, $3: the port, $4: the device
	// name, $5: the client's public key. SOFT_SERVE_OSEWA_KEY: the client's
	// private key, base64. Writes the key and enrols it with the server;
	// prints the enrol response.
	osewaEnrollScript = `set -e
umask 077
mkdir -p "$2"
chmod 0700 "$2"
printf '%s' "$SOFT_SERVE_OSEWA_KEY" | base64 -d > "$2/client.key.part"
mv -f "$2/client.key.part" "$2/client.key"
printf '{"name":"%s","key":"%s"}\n' "$4" "$5" | "$1" enroll --port "$3"`

	osewaHostsName = "soft-serve-osewa-hosts"
	// $1: the client directory. SOFT_SERVE_OSEWA_HOSTS: hosts.json, base64.
	osewaHostsScript = `set -e
umask 077
printf '%s' "$SOFT_SERVE_OSEWA_HOSTS" | base64 -d > "$1/hosts.json.part"
mv -f "$1/hosts.json.part" "$1/hosts.json"`

	osewaListName = "soft-serve-osewa-ls"
	// $1: the binary, $2: the session id file. Prints the recorded id on
	// the first line, then `osewa local ls` JSON.
	osewaListScript = `printf '%s\n' "$(cat "$2" 2>/dev/null)"
exec "$1" local ls`

	osewaStatusName = "soft-serve-osewa-status"
	// $1: the uploaded binary's path, $2: the session id file. The status
	// poll: prints the recorded id on the first line, then, when there is
	// one, `osewa local ls` JSON. It finds osewa itself (the uploaded build,
	// else the image's), so a poll is one exec. Exits 3 when the sandbox has
	// no osewa although a session was recorded.
	osewaStatusScript = `id=$(cat "$2" 2>/dev/null)
printf '%s\n' "$id"
[ -n "$id" ] || exit 0
bin=$1
if [ ! -x "$bin" ]; then bin=$(command -v osewa 2>/dev/null) || { echo "no osewa in the sandbox" >&2; exit 3; }; fi
exec "$bin" local ls`

	osewaNewName = "soft-serve-osewa-new"
	// $1: the binary, $2: the session id file, $3: the id the caller saw
	// there, $4: the working directory, then the program's argv. Creates
	// the osewa session and records its id, unless someone else recorded a
	// new one meanwhile (serialized with flock where the image has it).
	// Prints "created ID" or "existing ID".
	osewaNewScript = `set -e
bin=$1 idf=$2 seen=$3 dir=$4
shift 4
mkdir -p "$(dirname "$idf")"
if command -v flock >/dev/null 2>&1; then exec 9>"$idf.lock"; flock 9; fi
cur=$(cat "$idf" 2>/dev/null || true)
if [ "$cur" != "$seen" ]; then echo "existing $cur"; exit 0; fi
id=$("$bin" new ` + osewaHost + ` -- /bin/sh -lc 'cd "$1" || exit; shift; exec "$@"' soft-serve-session "$dir" "$@")
printf '%s\n' "$id" > "$idf.part"
mv -f "$idf.part" "$idf"
echo "created $id"`
)

// osewaBinary returns the path of the pinned osewa in the sandbox,
// uploading it when the image does not provide it, and reports whether the
// bridge client is already enrolled.
func (m *Manager) osewaBinary(ctx context.Context, sandbox string, progress io.Writer) (bin string, enrolled bool, err error) {
	out, err := m.script(ctx, sandbox, osewaProbeName, osewaProbeScript, nil, OsewaVersion, osewaPath(), osewaClientDir)
	if err != nil {
		return "", false, err
	}
	probe := parseKeyValues(out)
	enrolled = probe["enrolled"] == "yes"
	if bin = probe["osewa"]; bin != "" {
		return bin, enrolled, nil
	}

	arch, err := osewaArch(probe["arch"])
	if err != nil {
		return "", false, err
	}
	sum := m.osewaSHA256[arch]
	local := filepath.Join(m.osewaBinaryDir, "osewa-linux-"+arch+".gz")
	if err := verifySHA256(local, sum); err != nil {
		return "", false, fmt.Errorf("openshell: the sandbox image has no osewa %s and there is none to upload: %w (install it in the image, or put the release's osewa-linux-%s.gz in sessions.osewa.binary_dir)", OsewaVersion, err, arch)
	}

	fmt.Fprintf(progress, "Installing osewa %s in the sandbox...\n", OsewaVersion) //nolint:errcheck
	gz := osewaPath() + ".gz"
	if err := m.client.Upload(ctx, sandbox, local, gz); err != nil {
		return "", false, err
	}
	if _, err := m.script(ctx, sandbox, osewaInstallName, osewaInstallScript, nil, gz, sum, osewaPath()); err != nil {
		return "", false, err
	}
	return osewaPath(), enrolled, nil
}

// verifySHA256 checks that the file at p has the given SHA-256.
func verifySHA256(p, want string) error {
	f, err := os.Open(p)
	if err != nil {
		return err //nolint:wrapcheck
	}
	defer f.Close() //nolint:errcheck
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return fmt.Errorf("read %s: %w", p, err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return fmt.Errorf("%s has SHA-256 %s, want %s", p, got, want)
	}
	return nil
}

// osewaListen is the loopback address of the sandbox's osewa server.
func (m *Manager) osewaListen() string {
	return "127.0.0.1:" + strconv.Itoa(m.osewaPort)
}

// startOsewaServer starts the sandbox's osewa server unless one is running.
func (m *Manager) startOsewaServer(ctx context.Context, sandbox, bin string) error {
	_, err := m.script(ctx, sandbox, osewaServerName, osewaServerScript, nil, bin, m.osewaListen())
	return err
}

// enrollResponse is what `osewa enroll` prints.
type enrollResponse struct {
	ServerKey string `json:"server_key"`
	Port      int    `json:"port"`
	Version   int    `json:"version"`
}

// osewaHostEntry is one entry of an osewa client's hosts.json.
type osewaHostEntry struct {
	Name      string `json:"name"`
	Addr      string `json:"addr"`
	ServerKey string `json:"server_key"`
}

// enrollBridge gives the sandbox a client identity for the bridge and enrols
// it with the sandbox's own server: a fresh Ed25519 key written as
// client.key, `osewa enroll` with its public key, and hosts.json pinning the
// server key under the name osewaHost. It runs once per sandbox; the probe
// reports a sandbox with both files as enrolled.
func (m *Manager) enrollBridge(ctx context.Context, sandbox, bin string) error {
	pub, keyPEM, err := newOsewaKey()
	if err != nil {
		return err
	}

	out, err := m.script(ctx, sandbox, osewaEnrollName, osewaEnrollScript,
		map[string]string{"SOFT_SERVE_OSEWA_KEY": execEnvValue(keyPEM)},
		bin, osewaClientDir, strconv.Itoa(m.osewaPort), osewaDevice, pub)
	if err != nil {
		return err
	}
	var resp enrollResponse
	line, _, _ := strings.Cut(strings.TrimSpace(out), "\n")
	if err := json.Unmarshal([]byte(line), &resp); err != nil || resp.ServerKey == "" {
		return fmt.Errorf("openshell: osewa enroll: unexpected response %q", trimOutput([]byte(out)))
	}

	hosts, err := json.MarshalIndent([]osewaHostEntry{{
		Name:      osewaHost,
		Addr:      m.osewaListen(),
		ServerKey: resp.ServerKey,
	}}, "", "  ")
	if err != nil {
		return fmt.Errorf("openshell: osewa hosts.json: %w", err)
	}
	_, err = m.script(ctx, sandbox, osewaHostsName, osewaHostsScript,
		map[string]string{"SOFT_SERVE_OSEWA_HOSTS": execEnvValue(string(hosts) + "\n")},
		osewaClientDir)
	return err
}

// newOsewaKey returns a fresh Ed25519 key as osewa stores it: the public key
// as base64 of its 32 raw bytes, and the private key as a PKCS#8 PEM block,
// which for Ed25519 is byte for byte the form osewa writes itself.
func newOsewaKey() (pub, keyPEM string, err error) {
	pk, sk, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", "", fmt.Errorf("openshell: generate osewa key: %w", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(sk)
	if err != nil {
		return "", "", fmt.Errorf("openshell: encode osewa key: %w", err)
	}
	return base64.StdEncoding.EncodeToString(pk), string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})), nil
}

// osewaSessionInfo is the part of an `osewa local ls` session Soft Serve
// reads. In osewa 0.1.4, `osewa local ls` (crates/server/src/local.rs, main)
// prints one line, a wire::Response with only `id` (0) and `sessions` set;
// each session is a wire::SessionInfo (crates/wire/src/messages.rs), whose
// `status` is the holder's latest root OSC 7501 record, a
// wire::ProgramStatus, absent when the program never reported one or
// cleared it. Title and msg in it are already base64-decoded by the holder.
type osewaSessionInfo struct {
	ID           string `json:"id"`
	Created      int64  `json:"created"`
	Exited       bool   `json:"exited"`
	ExitCode     int64  `json:"exit_code"`
	Attached     int64  `json:"attached"`
	IdleSeconds  int64  `json:"idle_seconds"`
	Incompatible string `json:"incompatible,omitempty"`
	// Status is nil when the program has no root record.
	Status *osewaProgramStatus `json:"status,omitempty"`
}

// osewaProgramStatus is an osewa wire::ProgramStatus.
type osewaProgramStatus struct {
	State    string `json:"state"`
	Kind     string `json:"kind,omitempty"`
	App      string `json:"app,omitempty"`
	Title    string `json:"title,omitempty"`
	Msg      string `json:"msg,omitempty"`
	Progress *int64 `json:"progress,omitempty"`
}

// osewaList is the output of `osewa local ls`.
type osewaList struct {
	Sessions []osewaSessionInfo `json:"sessions"`
}

// parseOsewaList parses the output of `osewa local ls`.
func parseOsewaList(out string) (osewaList, error) {
	var ls osewaList
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &ls); err != nil {
		return osewaList{}, fmt.Errorf("openshell: osewa local ls: unexpected output %q", trimOutput([]byte(out)))
	}
	return ls, nil
}

// find returns the session with the given id.
func (ls osewaList) find(id string) (osewaSessionInfo, bool) {
	if id == "" {
		return osewaSessionInfo{}, false
	}
	for _, s := range ls.Sessions {
		if s.ID == id {
			return s, true
		}
	}
	return osewaSessionInfo{}, false
}

// ensureOsewaSession returns the id of the osewa session running the
// session's program, starting the program in a new osewa session when the
// recorded one is gone, has exited or is held by an incompatible osewa
// build. started reports whether it did.
func (m *Manager) ensureOsewaSession(ctx context.Context, sandbox, bin, workDir string, command []string) (id string, started bool, err error) {
	out, err := m.script(ctx, sandbox, osewaListName, osewaListScript, m.osewaClientEnv(), bin, osewaSessionFile)
	if err != nil {
		return "", false, err
	}
	seen, list, _ := strings.Cut(out, "\n")
	seen = strings.TrimSpace(seen)
	ls, err := parseOsewaList(list)
	if err != nil {
		return "", false, err
	}
	if s, ok := ls.find(seen); ok && !s.Exited && s.Incompatible == "" {
		return seen, false, nil
	}

	return m.newOsewaSession(ctx, sandbox, bin, workDir, command, seen)
}

// newOsewaSession starts command in workDir in a new osewa session and
// records its id, unless the recorded id is no longer seen, which means a
// concurrent start recorded a new session first: that one is returned.
func (m *Manager) newOsewaSession(ctx context.Context, sandbox, bin, workDir string, command []string, seen string) (id string, started bool, err error) {
	args := append([]string{bin, osewaSessionFile, seen, workDir}, command...)
	out, err := m.script(ctx, sandbox, osewaNewName, osewaNewScript, m.osewaClientEnv(), args...)
	if err != nil {
		return "", false, err
	}
	verb, id, _ := strings.Cut(strings.TrimSpace(lastLine(out)), " ")
	if id == "" || (verb != "created" && verb != "existing") {
		return "", false, fmt.Errorf("openshell: osewa new: unexpected output %q", trimOutput([]byte(out)))
	}
	return id, verb == "created", nil
}

// osewaClientEnv is osewaEnv for the bridge's osewa client.
func (m *Manager) osewaClientEnv() map[string]string {
	env := osewaEnv()
	env["OSEWA_CONFIG_DIR"] = osewaClientDir
	return env
}

// OsewaAttachCommand returns the command that attaches the bridged terminal
// to the osewa session id: as owner (--take, since the previous owner may be
// a bridge that was cut off), and without local echo prediction, which
// guesses at keystrokes the loopback round trip answers at once.
func OsewaAttachCommand(bin, id string) []string {
	return []string{bin, "attach", "--take", "--predict", "never", osewaHost, id}
}

// script runs `sh -c script name args...` in the sandbox with osewaEnv plus
// env and returns its standard output. A non-zero exit is an error carrying
// the end of its standard error.
func (m *Manager) script(ctx context.Context, sandbox, name, script string, env map[string]string, args ...string) (string, error) {
	return runScript(ctx, m.client, sandbox, name, script, env, args...)
}

// runScript is Manager.script for any client.
func runScript(ctx context.Context, client Client, sandbox, name, script string, env map[string]string, args ...string) (string, error) {
	e := osewaEnv()
	maps.Copy(e, env)
	command := append([]string{"sh", "-c", script, name}, args...)
	res, err := client.Run(ctx, sandbox, command, ExecOptions{
		Env:          e,
		WorkDir:      SandboxRoot,
		NoLoginShell: true,
	})
	if err != nil {
		return "", fmt.Errorf("openshell: %s: %w", name, err)
	}
	if res.ExitCode != 0 {
		msg := trimOutput(res.Stderr)
		if msg == "" {
			msg = trimOutput(res.Stdout)
		}
		return "", fmt.Errorf("openshell: %s: exit status %d: %s", name, res.ExitCode, msg)
	}
	return string(res.Stdout), nil
}

// execEnvValue encodes a value for the exec environment: base64, which the
// scripts decode. The gateway rejects environment values with a newline
// (openshell-server grpc/validation.rs, reject_control_chars) and puts
// them into the shell command line it runs.
func execEnvValue(v string) string {
	return base64.StdEncoding.EncodeToString([]byte(v))
}

// parseKeyValues parses key=value lines.
func parseKeyValues(s string) map[string]string {
	kv := map[string]string{}
	sc := bufio.NewScanner(strings.NewReader(s))
	for sc.Scan() {
		if k, v, ok := strings.Cut(sc.Text(), "="); ok {
			kv[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return kv
}

// lastLine returns the last non-empty line of s.
func lastLine(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	return lines[len(lines)-1]
}
