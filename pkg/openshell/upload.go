package openshell

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path"

	v1 "github.com/NVIDIA/OpenShell/sdk/go/openshell/v1"
)

// Limits of the fallback upload. The gateway refuses an exec argument over
// 32 KiB (openshell-server grpc/validation.rs, MAX_EXEC_ARG_LEN), and runs the
// exec as `<shell> -c <command>` with all arguments joined into that one
// string, which Linux limits to 128 KiB (MAX_ARG_STRLEN). So the file goes in
// base64 arguments of 32,000 bytes, three to an exec.
const (
	// uploadChunk is the file bytes in one argument. A multiple of 3, so the
	// arguments' base64 concatenates without padding in between.
	uploadChunk = 24000
	// uploadChunksPerExec is the arguments one exec carries.
	uploadChunksPerExec = 3
)

// Upload copies localPath to remotePath in the sandbox.
//
// It uses the SDK's Files().Upload when the SDK can transfer files. The Go
// SDK has no file transport yet (v0.1.6 to v0.1.8: Upload always fails with
// ErrTransportNotAvailable), so it falls back to writing the file through
// exec calls, base64 in arguments of uploadChunk bytes, and then decoding it
// into place. The sandbox needs sh, base64 and mv; the file is renamed into place
// only once complete.
func (c *sdkClient) Upload(ctx context.Context, name, localPath, remotePath string) error {
	if c.files != nil {
		err := c.files.Upload(ctx, c.workspace, name, localPath, remotePath)
		switch {
		case err == nil:
			return nil
		case !errors.Is(err, v1.ErrTransportNotAvailable) && !v1.IsUnimplemented(err):
			return fmt.Errorf("openshell: upload %s: %w", remotePath, err)
		}
	}
	return c.uploadByExec(ctx, name, localPath, remotePath)
}

// Upload scripts. Each runs as `sh -c <script> <name> <args...>`; the name
// shows in the sandbox's process list and lets tests tell the steps apart.
const (
	uploadBeginName = "soft-serve-upload-begin"
	// $1: the base64 staging file. Starts it empty, creating its directory.
	uploadBeginScript = `set -e; mkdir -p "$(dirname "$1")"; : > "$1"`

	uploadChunkName = "soft-serve-upload-chunk"
	// $1: the staging file, then base64 to append.
	uploadChunkScript = `set -e; f=$1; shift; for c in "$@"; do printf '%s' "$c"; done >> "$f"`

	uploadEndName = "soft-serve-upload-end"
	// $1: the staging file, $2: the destination. Decodes into a temporary
	// file next to the destination, then renames it into place.
	uploadEndScript = `set -e; base64 -d < "$1" > "$2.part"; rm -f "$1"; mv -f "$2.part" "$2"`
)

func (c *sdkClient) uploadByExec(ctx context.Context, name, localPath, remotePath string) error {
	f, err := os.Open(localPath)
	if err != nil {
		return fmt.Errorf("openshell: upload: %w", err)
	}
	defer f.Close() //nolint:errcheck

	staging := path.Join(path.Dir(remotePath), "."+path.Base(remotePath)+".b64")
	if err := c.runScript(ctx, name, uploadBeginName, uploadBeginScript, staging); err != nil {
		return err
	}

	buf := make([]byte, uploadChunk)
	args := []string{staging}
	flush := func() error {
		if len(args) == 1 {
			return nil
		}
		err := c.runScript(ctx, name, uploadChunkName, uploadChunkScript, args...)
		args = args[:1]
		return err
	}
	for {
		n, rerr := io.ReadFull(f, buf)
		if n > 0 {
			args = append(args, base64.StdEncoding.EncodeToString(buf[:n]))
			if len(args) > uploadChunksPerExec {
				if err := flush(); err != nil {
					return err
				}
			}
		}
		if errors.Is(rerr, io.EOF) || errors.Is(rerr, io.ErrUnexpectedEOF) {
			break
		}
		if rerr != nil {
			return fmt.Errorf("openshell: upload: %w", rerr)
		}
	}
	if err := flush(); err != nil {
		return err
	}

	return c.runScript(ctx, name, uploadEndName, uploadEndScript, staging, remotePath)
}

// runScript runs `sh -c script name args...` in the sandbox and fails on a
// non-zero exit.
func (c *sdkClient) runScript(ctx context.Context, sandbox, name, script string, args ...string) error {
	command := append([]string{"sh", "-c", script, name}, args...)
	res, err := c.Run(ctx, sandbox, command, ExecOptions{NoLoginShell: true})
	if err != nil {
		return fmt.Errorf("openshell: %s: %w", name, err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("openshell: %s: exit status %d: %s", name, res.ExitCode, trimOutput(res.Stderr))
	}
	return nil
}
