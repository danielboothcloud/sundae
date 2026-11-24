package hook

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/danielboothcloud/sundae/pkg/hooks"
)

func TestRunCustomHookPropagatesExitError(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(executable)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&0o111 == 0 {
		t.Skip("test binary is not executable on this platform")
	}

	t.Setenv("SUNDAE_TEST_HELPER", "1")
	t.Setenv("SUNDAE_TEST_HELPER_EXIT", "1")
	err = runCustomHook(
		context.Background(),
		executable,
		io.Reader(nil),
		io.Discard,
		io.Discard,
		"-test.run=TestCustomHookHelperProcess",
	)
	if err == nil {
		t.Fatal("runCustomHook returned nil for a failing custom hook")
	}

	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("runCustomHook error = %T %v, want exec.ExitError", err, err)
	}
}

func TestRunCustomHookIgnoresMissingOrNonExecutableHook(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing-hook")
	if err := runCustomHook(context.Background(), missing, nil, io.Discard, io.Discard); err != nil {
		t.Fatalf("runCustomHook(missing) error = %v", err)
	}

	nonExecutable := filepath.Join(t.TempDir(), "non-executable-hook")
	if err := os.WriteFile(nonExecutable, []byte("exit 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runCustomHook(context.Background(), nonExecutable, nil, io.Discard, io.Discard); err != nil {
		t.Fatalf("runCustomHook(non-executable) error = %v", err)
	}
}

// recordingHooks records PreReceive calls and returns err from them.
type recordingHooks struct {
	hooks.Hooks
	calls int
	err   error
}

func (h *recordingHooks) PreReceive(context.Context, io.Writer, io.Writer, string, []hooks.HookArg) error {
	h.calls++
	return h.err
}

// TestRunPreReceiveCustomHookFirst checks the builtin pre-receive, which
// forwards to the forge, runs only once the custom hook accepts the push.
func TestRunPreReceiveCustomHookFirst(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("SUNDAE_TEST_HELPER", "1")
	run := func(h *recordingHooks) error {
		return runPreReceive(context.Background(), h, executable, []byte("a b refs/heads/x\n"), io.Discard, io.Discard,
			"repo", nil, "-test.run=TestCustomHookHelperProcess")
	}

	t.Setenv("SUNDAE_TEST_HELPER_EXIT", "1")
	refusing := &recordingHooks{}
	if err := run(refusing); err == nil || refusing.calls != 0 {
		t.Fatalf("custom hook refused: err = %v, builtin calls = %d; want an error and no builtin call", err, refusing.calls)
	}

	t.Setenv("SUNDAE_TEST_HELPER_EXIT", "0")
	accepting := &recordingHooks{}
	if err := run(accepting); err != nil || accepting.calls != 1 {
		t.Fatalf("custom hook accepted: err = %v, builtin calls = %d", err, accepting.calls)
	}

	builtin := &recordingHooks{err: errors.New("sundae: refused")}
	var refused *preReceiveError
	if err := run(builtin); !errors.As(err, &refused) {
		t.Fatalf("builtin refused: err = %T %v, want a preReceiveError", err, err)
	}
}

func TestCustomHookHelperProcess(t *testing.T) {
	if os.Getenv("SUNDAE_TEST_HELPER") != "1" {
		return
	}
	if os.Getenv("SUNDAE_TEST_HELPER_EXIT") == "1" {
		os.Exit(1)
	}
	os.Exit(0)
}
