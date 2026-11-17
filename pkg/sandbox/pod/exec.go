package pod

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"slices"
	"sync"

	sbx "github.com/danielboothcloud/sundae/pkg/sandbox"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/httpstream"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/remotecommand"
	utilexec "k8s.io/client-go/util/exec"
)

// StreamOptions is one exec in the sandbox container.
type StreamOptions struct {
	Command []string
	Stdin   io.Reader
	Stdout  io.Writer
	Stderr  io.Writer
	TTY     bool
	Sizes   remotecommand.TerminalSizeQueue
}

// StreamFunc runs an exec in the named pod and returns when it ends. A
// command that exits non-zero is an error satisfying utilexec.ExitError.
type StreamFunc func(ctx context.Context, pod string, o StreamOptions) error

// SetStream replaces how commands run in pods: for tests.
func (r *Runtime) SetStream(f StreamFunc) { r.stream = f }

// remoteStream runs the exec through the API server: WebSocket, falling
// back to SPDY, as kubectl does.
func (r *Runtime) remoteStream(ctx context.Context, pod string, o StreamOptions) error {
	req := r.cs.CoreV1().RESTClient().Post().
		Resource("pods").Namespace(r.namespace).Name(pod).SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: ContainerName,
			Command:   o.Command,
			Stdin:     o.Stdin != nil,
			Stdout:    o.Stdout != nil,
			Stderr:    o.Stderr != nil && !o.TTY,
			TTY:       o.TTY,
		}, scheme.ParameterCodec)
	ex, err := r.executor(req.URL())
	if err != nil {
		return err
	}
	opts := remotecommand.StreamOptions{Stdin: o.Stdin, Stdout: o.Stdout, Tty: o.TTY, TerminalSizeQueue: o.Sizes}
	if !o.TTY {
		opts.Stderr = o.Stderr
	}
	return ex.StreamWithContext(ctx, opts) //nolint:wrapcheck
}

func (r *Runtime) executor(u *url.URL) (remotecommand.Executor, error) {
	ws, err := remotecommand.NewWebSocketExecutor(r.rest, "GET", u.String())
	if err != nil {
		return nil, fmt.Errorf("pod runtime: exec: %w", err)
	}
	spdy, err := remotecommand.NewSPDYExecutor(r.rest, "POST", u)
	if err != nil {
		return nil, fmt.Errorf("pod runtime: exec: %w", err)
	}
	ex, err := remotecommand.NewFallbackExecutor(ws, spdy, func(err error) bool {
		return httpstream.IsUpgradeFailure(err) || httpstream.IsHTTPSProxyError(err)
	})
	if err != nil {
		return nil, fmt.Errorf("pod runtime: exec: %w", err)
	}
	return ex, nil
}

// ExecWrapper, run by sh, runs `$@` in the directory $1 (when not empty).
const ExecWrapper = `[ -z "$1" ] || cd -- "$1" || exit 126; shift; exec "$@"`

// wrapCommand returns command with opts applied: the environment through
// env(1), the working directory and the login shell through sh. The pod
// exec API has neither.
func wrapCommand(command []string, opts sbx.ExecOptions) []string {
	var out []string
	if len(opts.Env) > 0 {
		out = append(out, "env")
		keys := make([]string, 0, len(opts.Env))
		for k := range opts.Env {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		for _, k := range keys {
			out = append(out, k+"="+opts.Env[k])
		}
	}
	if opts.WorkDir != "" || !opts.NoLoginShell {
		flag := "-c"
		if !opts.NoLoginShell {
			flag = "-lc"
		}
		out = append(out, "/bin/sh", flag, ExecWrapper, "sundae-exec", opts.WorkDir)
	}
	return append(out, command...)
}

// exitCode returns the exit code err carries, and whether it is one.
func exitCode(err error) (int, bool) {
	var ee utilexec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitStatus(), true
	}
	return 0, false
}

func (r *Runtime) Run(ctx context.Context, name string, command []string, opts sbx.ExecOptions) (*sbx.ExecResult, error) {
	var stdout, stderr bytes.Buffer
	err := r.stream(ctx, name, StreamOptions{Command: wrapCommand(command, opts), Stdout: &stdout, Stderr: &stderr})
	res := &sbx.ExecResult{Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}
	if err != nil {
		code, ok := exitCode(err)
		if !ok {
			return nil, fmt.Errorf("pod runtime: exec in %q: %w", name, err)
		}
		res.ExitCode = code
	}
	return res, nil
}

// uploadScript writes standard input to $1 through a temporary file.
const uploadScript = `set -e; mkdir -p "$(dirname "$1")"; cat > "$1.part"; mv -f "$1.part" "$1"`

func (r *Runtime) Upload(ctx context.Context, name, localPath, remotePath string) error {
	f, err := os.Open(localPath)
	if err != nil {
		return fmt.Errorf("pod runtime: upload: %w", err)
	}
	defer f.Close() //nolint:errcheck
	var stderr bytes.Buffer
	err = r.stream(ctx, name, StreamOptions{
		Command: []string{"/bin/sh", "-c", uploadScript, "sundae-upload", remotePath},
		Stdin:   f, Stdout: io.Discard, Stderr: &stderr,
	})
	if err != nil {
		return fmt.Errorf("pod runtime: upload %s: %w: %s", remotePath, err, bytes.TrimSpace(stderr.Bytes()))
	}
	return nil
}

func (r *Runtime) Interactive(ctx context.Context, name string, command []string, cols, rows uint32, opts sbx.ExecOptions) (sbx.InteractiveSession, error) {
	ctx, cancel := context.WithCancel(ctx)
	s := &session{
		cancel: cancel,
		sizes:  make(chan remotecommand.TerminalSize, 1),
		done:   make(chan struct{}),
		ctx:    ctx,
	}
	s.inR, s.inW = io.Pipe()
	s.outR, s.outW = io.Pipe()
	_ = s.Resize(cols, rows)
	go func() {
		err := r.stream(ctx, name, StreamOptions{
			Command: wrapCommand(command, opts),
			Stdin:   s.inR, Stdout: s.outW, TTY: true, Sizes: s,
		})
		if code, ok := exitCode(err); ok {
			s.code, err = code, nil
		}
		s.err = err
		_ = s.outW.Close()
		_ = s.inR.Close()
		close(s.done)
	}()
	return s, nil
}

// session is an interactive exec.
type session struct {
	ctx        context.Context
	cancel     context.CancelFunc
	inR, outR  *io.PipeReader
	inW, outW  *io.PipeWriter
	sizes      chan remotecommand.TerminalSize
	done       chan struct{}
	code       int
	err        error
	closeOnce  sync.Once
	sizesMutex sync.Mutex
}

func (s *session) Read(p []byte) (int, error)  { return s.outR.Read(p) } //nolint:wrapcheck
func (s *session) Write(p []byte) (int, error) { return s.inW.Write(p) } //nolint:wrapcheck

// Resize sends the new size, replacing one not yet sent.
func (s *session) Resize(cols, rows uint32) error {
	s.sizesMutex.Lock()
	defer s.sizesMutex.Unlock()
	select {
	case <-s.sizes:
	default:
	}
	s.sizes <- remotecommand.TerminalSize{Width: uint16(min(cols, 1<<16-1)), Height: uint16(min(rows, 1<<16-1))} //nolint:gosec
	return nil
}

// Next is the remotecommand.TerminalSizeQueue; nil ends it.
func (s *session) Next() *remotecommand.TerminalSize {
	select {
	case sz := <-s.sizes:
		return &sz
	case <-s.ctx.Done():
		return nil
	}
}

func (s *session) ExitCode() (int, error) {
	<-s.done
	return s.code, s.err
}

// CloseWrite ends the input; output continues.
func (s *session) CloseWrite() error { return s.inW.Close() } //nolint:wrapcheck

// Cancel aborts the exec.
func (s *session) Cancel() error {
	s.cancel()
	return nil
}

func (s *session) Close() error {
	s.closeOnce.Do(func() {
		s.cancel()
		_ = s.inW.Close()
		_ = s.outR.Close()
	})
	return nil
}
