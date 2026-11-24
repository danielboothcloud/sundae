package docker

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/danielboothcloud/sundae/pkg/config"
	sbx "github.com/danielboothcloud/sundae/pkg/sandbox"
	"github.com/danielboothcloud/sundae/pkg/sandbox/docker/dockertest"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
)

var owner = map[string]string{sbx.LabelManaged: "true", sbx.LabelUserID: "1"}

func testConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg := config.DefaultConfig()
	cfg.DataPath = t.TempDir()
	cfg.Sessions.Runtime = config.RuntimeDocker
	cfg.Docker.Image = "sandbox:1"
	cfg.Docker.Network = "agents"
	cfg.Docker.Labels = map[string]string{"team": "a"}
	cfg.Docker.VolumeDriver = "local"
	cfg.Sessions.Profiles = map[string]config.SessionProfile{
		"nested": {Docker: &config.DockerSettings{
			Runtime: "runsc", Memory: "2Gi", CPUs: "1500m",
			SecurityOpt: []string{"seccomp=unconfined"}, CapAdd: []string{"SETUID"},
			Devices: []string{"/dev/fuse"},
		}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func request(profile string) sbx.Request {
	return sbx.Request{
		Runtime: config.RuntimeDocker, Profile: profile, Image: "img:2", Labels: owner,
		Annotations: map[string]string{"sundae.repo": "r", "sundae.session": "main", "other": "x"},
	}
}

func TestLifecycle(t *testing.T) {
	ctx := context.Background()
	e := dockertest.New()
	r := NewWithAPI(testConfig(t), e)
	r.poll = time.Millisecond

	sb, created, err := r.EnsureSandbox(ctx, "sd-a", request("nested"))
	if err != nil || !created {
		t.Fatalf("created=%v err=%v", created, err)
	}
	if sb.Phase != sbx.Ready || sb.Runtime != config.RuntimeDocker || sb.Image != "img:2" {
		t.Fatalf("sandbox = %+v", sb)
	}
	if sb.Annotations["sundae.repo"] != "r" || sb.Annotations["other"] != "" || sb.Labels[sbx.LabelUserID] != "1" {
		t.Fatalf("labels %v annotations %v", sb.Labels, sb.Annotations)
	}
	// The image was missing: pulled.
	if !slices.Equal(e.Pulls, []string{"img:2"}) {
		t.Fatalf("pulls = %v", e.Pulls)
	}
	c := e.Containers["sd-a"]
	hc := c.HostConfig
	if hc.Runtime != "runsc" || hc.NetworkMode != "agents" || hc.Memory != 2<<30 || hc.NanoCPUs != 1_500_000_000 {
		t.Fatalf("host config = %+v", hc)
	}
	if !slices.Equal(hc.SecurityOpt, []string{"seccomp=unconfined"}) || !slices.Equal(hc.CapAdd, []string{"SETUID"}) || !slices.Equal(hc.CapDrop, []string{"ALL"}) {
		t.Fatalf("security = %v %v %v", hc.SecurityOpt, hc.CapAdd, hc.CapDrop)
	}
	if len(hc.Devices) != 1 || hc.Devices[0].PathInContainer != "/dev/fuse" || hc.Devices[0].CgroupPermissions != "rwm" {
		t.Fatalf("devices = %+v", hc.Devices)
	}
	if len(hc.Mounts) != 1 || hc.Mounts[0].Type != mount.TypeVolume || hc.Mounts[0].Source != "sd-a" || hc.Mounts[0].Target != SandboxRoot {
		t.Fatalf("mounts = %+v", hc.Mounts)
	}
	if hc.RestartPolicy.Name != container.RestartPolicyUnlessStopped {
		t.Fatalf("restart = %+v", hc.RestartPolicy)
	}
	if c.Config.Labels["team"] != "a" || c.Config.Labels[LabelRuntime] != config.RuntimeDocker || !slices.Equal(c.Config.Entrypoint, keepAlive) {
		t.Fatalf("config = %+v", c.Config)
	}
	if v := e.Volumes["sd-a"]; v.Driver != "local" || v.Labels[sbx.LabelUserID] != "1" {
		t.Fatalf("volume = %+v", v)
	}

	// Existing: resumed as is, ownership checked.
	if _, created, err := r.EnsureSandbox(ctx, "sd-a", request("")); err != nil || created {
		t.Fatalf("again: %v %v", created, err)
	}
	other := map[string]string{sbx.LabelUserID: "2"}
	if _, err := r.ResumeSandbox(ctx, "sd-a", other); !errors.Is(err, sbx.ErrNotOwned) {
		t.Fatalf("foreign: %v", err)
	}

	// Suspend stops the container; resume starts it.
	if sb, err := r.SuspendSandbox(ctx, "sd-a"); err != nil || sb.Phase != sbx.Suspended {
		t.Fatalf("suspend: %+v %v", sb, err)
	}
	list, err := r.ListSandboxes(ctx, map[string]string{sbx.LabelManaged: "true"})
	if err != nil || len(list) != 1 || list[0].Phase != sbx.Suspended || list[0].Annotations["sundae.session"] != "main" {
		t.Fatalf("list: %+v %v", list, err)
	}
	if sb, err := r.ResumeSandbox(ctx, "sd-a", owner); err != nil || sb.Phase != sbx.Ready {
		t.Fatalf("resume: %+v %v", sb, err)
	}

	// Delete removes container and volume.
	if err := r.DeleteSandbox(ctx, "sd-a"); err != nil {
		t.Fatal(err)
	}
	if len(e.Containers)+len(e.Volumes) != 0 {
		t.Fatalf("left: %v %v", e.Containers, e.Volumes)
	}
	if err := r.DeleteSandbox(ctx, "sd-a"); err != nil {
		t.Fatalf("delete absent: %v", err)
	}
	if _, err := r.GetSandbox(ctx, "sd-a"); !errors.Is(err, sbx.ErrNotFound) {
		t.Fatalf("get absent: %v", err)
	}
}

func TestStartFailureCleansUp(t *testing.T) {
	ctx := context.Background()
	e := dockertest.New()
	e.StartFails = errors.New("no such runtime")
	r := NewWithAPI(testConfig(t), e)
	if _, _, err := r.EnsureSandbox(ctx, "sd-f", request("")); err == nil || !strings.Contains(err.Error(), "no such runtime") {
		t.Fatalf("err = %v", err)
	}
	if len(e.Containers)+len(e.Volumes) != 0 {
		t.Fatalf("left: %v %v", e.Containers, e.Volumes)
	}
	req := request("")
	req.Image = "missing:1"
	e.StartFails = nil
	if _, _, err := r.EnsureSandbox(ctx, "sd-m", req); err == nil || len(e.Volumes) != 0 {
		t.Fatalf("missing image: %v, volumes %v", err, e.Volumes)
	}
}

func TestNotOurs(t *testing.T) {
	ctx := context.Background()
	e := dockertest.New()
	e.Images["x"] = true
	e.Containers["sd-x"] = &dockertest.Container{Name: "sd-x", Config: container.Config{Image: "x"}, Status: container.StateRunning}
	r := NewWithAPI(testConfig(t), e)
	if _, err := r.GetSandbox(ctx, "sd-x"); !errors.Is(err, sbx.ErrNotFound) {
		t.Fatalf("foreign container: %v", err)
	}
	if err := r.DeleteSandbox(ctx, "sd-x"); err != nil || e.Containers["sd-x"] == nil {
		t.Fatalf("foreign container deleted: %v", err)
	}
}

func TestRunUploadInteractive(t *testing.T) {
	ctx := context.Background()
	r := NewWithAPI(testConfig(t), dockertest.New())
	var got []StreamOptions
	r.SetStream(func(_ context.Context, name string, o StreamOptions) (int, error) {
		got = append(got, o)
		if o.Stdin != nil {
			b, _ := io.ReadAll(o.Stdin)
			_, _ = o.Stdout.Write(bytes.ToUpper(b))
			return 0, nil
		}
		_, _ = io.WriteString(o.Stderr, "err")
		return 3, nil
	})

	res, err := r.Run(ctx, "sd-a", []string{"x"}, sbx.ExecOptions{Env: map[string]string{"A": "1"}, WorkDir: "/sandbox/r"})
	if err != nil || res.ExitCode != 3 || string(res.Stderr) != "err" {
		t.Fatalf("run: %+v %v", res, err)
	}
	if o := got[0]; !slices.Equal(o.Command, []string{"/bin/sh", "-lc", LoginWrapper, "sundae-exec", "x"}) || o.WorkDir != "/sandbox/r" || o.Env["A"] != "1" {
		t.Fatalf("run options = %+v", o)
	}
	if _, err := r.Run(ctx, "sd-a", []string{"x"}, sbx.ExecOptions{NoLoginShell: true}); err != nil || !slices.Equal(got[1].Command, []string{"x"}) {
		t.Fatalf("no login shell: %q", got[1].Command)
	}
	// Stdin reaches the exec's input, not its command or environment.
	if res, err := r.Run(ctx, "sd-a", []string{"cat"}, sbx.ExecOptions{NoLoginShell: true, Stdin: strings.NewReader("secret")}); err != nil || string(res.Stdout) != "SECRET" || len(got[2].Env) != 0 {
		t.Fatalf("stdin: %+v %v", res, err)
	}

	f := filepath.Join(t.TempDir(), "f")
	_ = os.WriteFile(f, []byte("data"), 0o600)
	if err := r.Upload(ctx, "sd-a", f, "/sandbox/x"); err != nil {
		t.Fatal(err)
	}

	is, err := r.Interactive(ctx, "sd-a", []string{"cat"}, 100, 30, sbx.ExecOptions{NoLoginShell: true})
	if err != nil {
		t.Fatal(err)
	}
	_ = is.Resize(120, 40)
	_, _ = is.Write([]byte("hi"))
	_ = sbx.CloseInput(is)
	out, _ := io.ReadAll(is)
	if code, err := is.ExitCode(); string(out) != "HI" || code != 0 || err != nil {
		t.Fatalf("interactive: %q %d %v", out, code, err)
	}
	if o := got[len(got)-1]; !o.TTY || o.Cols != 100 || o.Rows != 30 {
		t.Fatalf("interactive options = %+v", o)
	}
	_ = is.Close()
	_ = is.Close()
}

// TestLiveEngine runs a session's sandbox on a real engine: set
// SUNDAE_TEST_DOCKER=1 (and DOCKER_HOST if needed).
func TestLiveEngine(t *testing.T) {
	if os.Getenv("SUNDAE_TEST_DOCKER") == "" {
		t.Skip("SUNDAE_TEST_DOCKER is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cfg := testConfig(t)
	cfg.Docker.Network = ""
	cfg.Docker.Image = "docker.io/library/alpine:3.20"
	r, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close() //nolint:errcheck
	name := "sd-sundaetest" + strings.ReplaceAll(time.Now().Format("150405.000"), ".", "")
	t.Cleanup(func() { _ = r.DeleteSandbox(context.Background(), name) })

	req := request("")
	req.Image = cfg.Docker.Image
	sb, created, err := r.EnsureSandbox(ctx, name, req)
	if err != nil || !created || sb.Phase != sbx.Ready {
		t.Fatalf("ensure: %+v %v %v", sb, created, err)
	}
	res, err := r.Run(ctx, name, []string{"sh", "-c", `echo "$HOME $PWD $A"; echo oops >&2; exit 4`}, sbx.ExecOptions{Env: map[string]string{"A": "x"}, WorkDir: "/tmp", NoLoginShell: true})
	if err != nil || res.ExitCode != 4 || strings.TrimSpace(string(res.Stdout)) != "/sandbox /tmp x" || strings.TrimSpace(string(res.Stderr)) != "oops" {
		t.Fatalf("run: %+v %q %q %v", res, res.Stdout, res.Stderr, err)
	}
	f := filepath.Join(t.TempDir(), "f")
	_ = os.WriteFile(f, bytes.Repeat([]byte("z"), 300_000), 0o600)
	if err := r.Upload(ctx, name, f, "/sandbox/a/b"); err != nil {
		t.Fatal(err)
	}
	if res, err := r.Run(ctx, name, []string{"wc", "-c", "/sandbox/a/b"}, sbx.ExecOptions{NoLoginShell: true}); err != nil || !strings.HasPrefix(strings.TrimSpace(string(res.Stdout)), "300000") {
		t.Fatalf("upload: %q %v", res.Stdout, err)
	}
	is, err := r.Interactive(ctx, name, []string{"sh", "-c", "stty size; read x; echo got-$x"}, 90, 33, sbx.ExecOptions{NoLoginShell: true})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = is.Write([]byte("yes\n"))
	out, _ := io.ReadAll(is)
	if code, err := is.ExitCode(); code != 0 || err != nil || !strings.Contains(string(out), "33 90") || !strings.Contains(string(out), "got-yes") {
		t.Fatalf("interactive: %q %d %v", out, code, err)
	}
	_ = is.Close()

	// Suspend keeps /sandbox.
	if sb, err := r.SuspendSandbox(ctx, name); err != nil || sb.Phase != sbx.Suspended {
		t.Fatalf("suspend: %+v %v", sb, err)
	}
	if sb, err := r.ResumeSandbox(ctx, name, owner); err != nil || sb.Phase != sbx.Ready {
		t.Fatalf("resume: %+v %v", sb, err)
	}
	if res, err := r.Run(ctx, name, []string{"test", "-f", "/sandbox/a/b"}, sbx.ExecOptions{NoLoginShell: true}); err != nil || res.ExitCode != 0 {
		t.Fatalf("after resume: %+v %v", res, err)
	}
	if err := r.DeleteSandbox(ctx, name); err != nil {
		t.Fatal(err)
	}
	if _, err := r.GetSandbox(ctx, name); !errors.Is(err, sbx.ErrNotFound) {
		t.Fatalf("after delete: %v", err)
	}
}
