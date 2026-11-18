package docker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"sync"
	"time"

	sbx "github.com/danielboothcloud/sundae/pkg/sandbox"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/client"
)

// StreamOptions is one exec in a container.
type StreamOptions struct {
	Command []string
	Env     map[string]string
	WorkDir string
	Stdin   io.Reader
	Stdout  io.Writer
	Stderr  io.Writer
	TTY     bool
	// Cols and Rows are the initial terminal size (TTY only); Sizes
	// receives later ones until it is closed.
	Cols, Rows uint
	Sizes      <-chan [2]uint
}

// StreamFunc runs an exec in the named container and returns its exit
// code once it ends.
type StreamFunc func(ctx context.Context, name string, o StreamOptions) (int, error)

// SetStream replaces how commands run in containers: for tests.
func (r *Runtime) SetStream(f StreamFunc) { r.stream = f }

// LoginWrapper, run by sh -lc, runs "$@" after the login shell's start-up
// files.
const LoginWrapper = `exec "$@"`

// command returns command, run through a login shell unless NoLoginShell.
func command(cmd []string, opts sbx.ExecOptions) []string {
	if opts.NoLoginShell {
		return cmd
	}
	return append([]string{"/bin/sh", "-lc", LoginWrapper, "sundae-exec"}, cmd...)
}

// apiStream runs the exec through the engine's API.
func (r *Runtime) apiStream(ctx context.Context, name string, o StreamOptions) (int, error) {
	env := make([]string, 0, len(o.Env))
	for _, k := range slices.Sorted(maps.Keys(o.Env)) {
		env = append(env, k+"="+o.Env[k])
	}
	var size client.ConsoleSize
	if o.TTY && o.Cols > 0 && o.Rows > 0 {
		size = client.ConsoleSize{Width: o.Cols, Height: o.Rows}
	}
	ec, err := r.api.ExecCreate(ctx, name, client.ExecCreateOptions{
		TTY: o.TTY, ConsoleSize: size,
		AttachStdin: o.Stdin != nil, AttachStdout: true, AttachStderr: true,
		Env: env, WorkingDir: o.WorkDir, Cmd: o.Command,
	})
	if err != nil {
		return 0, fmt.Errorf("docker runtime: exec in %q: %w", name, err)
	}
	att, err := r.api.ExecAttach(ctx, ec.ID, client.ExecAttachOptions{TTY: o.TTY, ConsoleSize: size})
	if err != nil {
		return 0, fmt.Errorf("docker runtime: attach exec in %q: %w", name, err)
	}
	defer att.Close()
	stop := context.AfterFunc(ctx, att.Close)
	defer stop()

	if o.Sizes != nil {
		go func() {
			for sz := range o.Sizes {
				_, _ = r.api.ExecResize(ctx, ec.ID, client.ExecResizeOptions{Width: sz[0], Height: sz[1]})
			}
		}()
	}
	if o.Stdin != nil {
		go func() {
			_, _ = io.Copy(att.Conn, o.Stdin)
			_ = att.CloseWrite()
		}()
	}
	stdout, stderr := o.Stdout, o.Stderr
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}
	if o.TTY {
		_, err = io.Copy(stdout, att.Reader)
	} else {
		_, err = stdcopy.StdCopy(stdout, stderr, att.Reader)
	}
	if ctx.Err() != nil {
		return 0, fmt.Errorf("docker runtime: exec in %q: %w", name, ctx.Err())
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return 0, fmt.Errorf("docker runtime: exec in %q: %w", name, err)
	}
	// The output ended; the exit code follows shortly.
	ictx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	for {
		in, err := r.api.ExecInspect(ictx, ec.ID, client.ExecInspectOptions{})
		if err != nil {
			return 0, fmt.Errorf("docker runtime: inspect exec in %q: %w", name, err)
		}
		if !in.Running {
			return in.ExitCode, nil
		}
		select {
		case <-ictx.Done():
			return 0, fmt.Errorf("docker runtime: exec in %q: no exit code: %w", name, ictx.Err())
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func (r *Runtime) Run(ctx context.Context, name string, cmd []string, opts sbx.ExecOptions) (*sbx.ExecResult, error) {
	var stdout, stderr bytes.Buffer
	code, err := r.stream(ctx, name, StreamOptions{
		Command: command(cmd, opts), Env: opts.Env, WorkDir: opts.WorkDir,
		Stdout: &stdout, Stderr: &stderr,
	})
	if err != nil {
		return nil, err
	}
	return &sbx.ExecResult{ExitCode: code, Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}, nil
}

// uploadScript writes standard input to $1 through a temporary file.
const uploadScript = `set -e; mkdir -p "$(dirname "$1")"; cat > "$1.part"; mv -f "$1.part" "$1"`

func (r *Runtime) Upload(ctx context.Context, name, localPath, remotePath string) error {
	f, err := os.Open(localPath)
	if err != nil {
		return fmt.Errorf("docker runtime: upload: %w", err)
	}
	defer f.Close() //nolint:errcheck
	var stderr bytes.Buffer
	code, err := r.stream(ctx, name, StreamOptions{
		Command: []string{"/bin/sh", "-c", uploadScript, "sundae-upload", remotePath},
		Stdin:   f, Stdout: io.Discard, Stderr: &stderr,
	})
	switch {
	case err != nil:
		return fmt.Errorf("docker runtime: upload %s: %w", remotePath, err)
	case code != 0:
		return fmt.Errorf("docker runtime: upload %s: exit status %d: %s", remotePath, code, bytes.TrimSpace(stderr.Bytes()))
	}
	return nil
}

func (r *Runtime) Interactive(ctx context.Context, name string, cmd []string, cols, rows uint32, opts sbx.ExecOptions) (sbx.InteractiveSession, error) {
	ctx, cancel := context.WithCancel(ctx)
	s := &session{cancel: cancel, sizes: make(chan [2]uint, 1), done: make(chan struct{})}
	s.inR, s.inW = io.Pipe()
	s.outR, s.outW = io.Pipe()
	go func() {
		s.code, s.err = r.stream(ctx, name, StreamOptions{
			Command: command(cmd, opts), Env: opts.Env, WorkDir: opts.WorkDir,
			Stdin: s.inR, Stdout: s.outW, Stderr: io.Discard, TTY: true,
			Cols: uint(cols), Rows: uint(rows), Sizes: s.sizes,
		})
		_ = s.outW.Close()
		_ = s.inR.Close()
		close(s.done)
	}()
	return s, nil
}

// session is an interactive exec.
type session struct {
	cancel    context.CancelFunc
	inR, outR *io.PipeReader
	inW, outW *io.PipeWriter
	sizes     chan [2]uint
	done      chan struct{}
	code      int
	err       error
	mu        sync.Mutex
	closed    bool
}

func (s *session) Read(p []byte) (int, error)  { return s.outR.Read(p) } //nolint:wrapcheck
func (s *session) Write(p []byte) (int, error) { return s.inW.Write(p) } //nolint:wrapcheck

// Resize sends the new size, replacing one not yet sent.
func (s *session) Resize(cols, rows uint32) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	select {
	case <-s.sizes:
	default:
	}
	s.sizes <- [2]uint{uint(cols), uint(rows)}
	return nil
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
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		s.closed = true
		s.cancel()
		close(s.sizes)
		_ = s.inW.Close()
		_ = s.outR.Close()
	}
	return nil
}
