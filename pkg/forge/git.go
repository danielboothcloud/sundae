package forge

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

const (
	// networkTimeout bounds one fetch or push.
	networkTimeout = 30 * time.Minute
	// localTimeout bounds one local git command.
	localTimeout = 2 * time.Minute
)

// gitError is a failed git command with its trimmed stderr.
type gitError struct {
	args   []string
	stderr string
	err    error
}

func (e *gitError) Error() string {
	msg := fmt.Sprintf("git %s: %v", strings.Join(e.args, " "), e.err)
	if e.stderr != "" {
		msg += ": " + e.stderr
	}
	return msg
}

func (e *gitError) Unwrap() error { return e.err }

// exitCode returns the exit code of a failed git command, or -1.
func exitCode(err error) int {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

// runGit runs git in dir with env added to the process environment and
// returns its stdout.
func runGit(ctx context.Context, dir string, env []string, timeout time.Duration, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), &gitError{args: redactArgs(args), stderr: trimStderr(stderr.String()), err: err}
	}
	return stdout.String(), nil
}

// redactArgs drops credentials from any URL argument before it is put in an
// error. Forge URLs never carry credentials, but this keeps a hand-edited
// remote from leaking one into logs.
func redactArgs(args []string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		if at := strings.Index(a, "@"); at > 0 && strings.Contains(a[:at], "://") {
			scheme, _, _ := strings.Cut(a, "://")
			a = scheme + "://***@" + a[at+1:]
		}
		out[i] = a
	}
	return out
}

func trimStderr(s string) string {
	const max = 512
	s = strings.TrimSpace(s)
	if len(s) > max {
		s = s[len(s)-max:]
	}
	return s
}

// refMap lists refs under prefix as short name (prefix removed) to object id.
func refMap(ctx context.Context, dir, prefix string) (map[string]string, error) {
	out, err := runGit(ctx, dir, nil, localTimeout, "for-each-ref", "--format=%(objectname) %(refname)", prefix)
	if err != nil {
		return nil, err
	}
	refs := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		oid, name, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		refs[strings.TrimPrefix(name, prefix)] = oid
	}
	return refs, nil
}

// resolveRef returns the object id of ref, or "" when it does not exist.
func resolveRef(ctx context.Context, dir, ref string) (string, error) {
	out, err := runGit(ctx, dir, nil, localTimeout, "rev-parse", "--verify", "--quiet", ref)
	if err != nil {
		if exitCode(err) == 1 {
			return "", nil
		}
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// isAncestor reports whether commit a is an ancestor of (or equal to) b.
func isAncestor(ctx context.Context, dir, a, b string) (bool, error) {
	_, err := runGit(ctx, dir, nil, localTimeout, "merge-base", "--is-ancestor", a, b)
	if err == nil {
		return true, nil
	}
	if exitCode(err) == 1 {
		return false, nil
	}
	return false, err
}

// aheadBehind returns how many commits local has that upstream lacks
// (ahead) and the reverse (behind).
func aheadBehind(ctx context.Context, dir, local, upstream string) (ahead, behind int, err error) {
	out, err := runGit(ctx, dir, nil, localTimeout, "rev-list", "--left-right", "--count", local+"..."+upstream)
	if err != nil {
		return 0, 0, err
	}
	fields := strings.Fields(out)
	if len(fields) != 2 {
		return 0, 0, fmt.Errorf("git rev-list: unexpected output %q", out)
	}
	ahead, err = strconv.Atoi(fields[0])
	if err != nil {
		return 0, 0, err
	}
	behind, err = strconv.Atoi(fields[1])
	return ahead, behind, err
}

// updateRef sets ref to newOID if it currently is oldOID. An empty oldOID
// requires the ref not to exist. The compare-and-swap means a push that
// lands between reading and writing a branch is never overwritten.
func updateRef(ctx context.Context, dir, ref, newOID, oldOID string) error {
	_, err := runGit(ctx, dir, nil, localTimeout, "update-ref", "-m", "forge sync", ref, newOID, oldOID)
	return err
}

// headBranch returns the branch HEAD points at, e.g. "main", or "".
func headBranch(ctx context.Context, dir string) string {
	out, err := runGit(ctx, dir, nil, localTimeout, "symbolic-ref", "--quiet", "HEAD")
	if err != nil {
		return ""
	}
	return strings.TrimPrefix(strings.TrimSpace(out), "refs/heads/")
}

// gitConfigGet returns a repository config value, or "" when unset.
func gitConfigGet(ctx context.Context, dir, key string) (string, error) {
	out, err := runGit(ctx, dir, nil, localTimeout, "config", "--file", "config", "--get", key)
	if err != nil {
		if exitCode(err) == 1 {
			return "", nil
		}
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// gitConfigSet sets a repository config value. git config takes the config
// lock, so concurrent writers do not lose each other's changes.
func gitConfigSet(ctx context.Context, dir, key, value string) error {
	_, err := runGit(ctx, dir, nil, localTimeout, "config", "--file", "config", key, value)
	return err
}

// gitConfigReplaceAll replaces every value of a multi-valued key.
func gitConfigReplaceAll(ctx context.Context, dir, key string, values ...string) error {
	if _, err := runGit(ctx, dir, nil, localTimeout, "config", "--file", "config", "--unset-all", key); err != nil && exitCode(err) != 5 {
		return err
	}
	for _, v := range values {
		if _, err := runGit(ctx, dir, nil, localTimeout, "config", "--file", "config", "--add", key, v); err != nil {
			return err
		}
	}
	return nil
}

// gitConfigUnset removes a repository config value; unset is not an error.
func gitConfigUnset(ctx context.Context, dir, key string) error {
	if _, err := runGit(ctx, dir, nil, localTimeout, "config", "--file", "config", "--unset-all", key); err != nil && exitCode(err) != 5 {
		return err
	}
	return nil
}
