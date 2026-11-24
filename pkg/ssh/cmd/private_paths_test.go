package cmd

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danielboothcloud/sundae/pkg/access"
	"github.com/danielboothcloud/sundae/pkg/backend"
	"github.com/danielboothcloud/sundae/pkg/config"
	"github.com/danielboothcloud/sundae/pkg/openshell"
	"github.com/danielboothcloud/sundae/pkg/proto"
	sbx "github.com/danielboothcloud/sundae/pkg/sandbox"
	"github.com/matryer/is"
)

// linkRepo links repo to forge gh as an import does.
func linkRepo(t *testing.T, ctx context.Context, repo string) {
	t.Helper()
	cfgFile := filepath.Join(config.FromContext(ctx).DataPath, "repos", repo+".git", "config")
	for _, kv := range [][2]string{{"forge.name", "gh"}, {"forge.path", "me/" + repo}, {"forge.defaultbranch", "main"}} {
		if out, err := exec.CommandContext(t.Context(), "git", "config", "--file", cfgFile, kv[0], kv[1]).CombinedOutput(); err != nil {
			t.Fatalf("git config %s: %v: %s", kv[0], err, out)
		}
	}
}

// withPrivateForge declares forge gh with private_paths.
func withPrivateForge(t *testing.T, ctx context.Context) {
	t.Helper()
	cfg := config.FromContext(ctx)
	// As validated: normalized, no trailing "/".
	cfg.Forges = []config.ForgeConfig{{Name: "gh", Kind: "github", URL: "http://127.0.0.1:1", PrivatePaths: []string{"AGENTS.md", "_bmad"}}}
}

func TestRepoPrivatePaths(t *testing.T) {
	is := is.New(t)
	ctx, be := newAuthTestContext(t)
	withPrivateForge(t, ctx)
	admin := withUser(t, ctx, be, "admin1", true)
	alice := withUser(t, ctx, be, "alice", false)
	bob := withUser(t, ctx, be, "bob", false)
	for _, r := range []string{"linked", "plain"} {
		_, err := be.CreateRepository(admin, r, proto.UserFromContext(admin), proto.RepositoryOptions{})
		is.NoErr(err)
	}
	is.NoErr(be.AddCollaborator(admin, "linked", "alice", access.ReadWriteAccess))
	linkRepo(t, ctx, "linked")

	// The forge's list, until the repository has its own.
	out, _, err := runRepoOutput(t, bob, "private-paths", "linked")
	is.NoErr(err)
	is.Equal(out, "Source: forge\nPaths:\n  - AGENTS.md\n  - _bmad\n")

	// Setting needs write access; paths are checked and normalized.
	is.True(errors.Is(runRepo(t, bob, "private-paths", "linked", "notes"), proto.ErrUnauthorized))
	is.True(errors.Is(runRepo(t, bob, "private-paths", "linked", "--clear"), proto.ErrUnauthorized))
	err = runRepo(t, alice, "private-paths", "linked", "../up")
	is.True(err != nil && strings.Contains(err.Error(), `".."`))
	err = runRepo(t, alice, "private-paths", "linked", ".git/hooks")
	is.True(err != nil && strings.Contains(err.Error(), ".git or .jj"))
	is.NoErr(runRepo(t, alice, "private-paths", "linked", "notes/", "AGENTS.md", "notes"))
	out, _, err = runRepoOutput(t, bob, "private-paths", "linked")
	is.NoErr(err)
	is.Equal(out, "Source: repo\nPaths:\n  - notes\n  - AGENTS.md\n")

	// Replacing replaces; --clear goes back to the forge's.
	is.NoErr(runRepo(t, admin, "private-paths", "linked", ".claude/"))
	out, _, _ = runRepoOutput(t, bob, "private-paths", "linked")
	is.Equal(out, "Source: repo\nPaths:\n  - .claude\n")
	err = runRepo(t, alice, "private-paths", "linked", "--clear", "x")
	is.True(err != nil && strings.Contains(err.Error(), "--clear takes no PATH"))
	is.NoErr(runRepo(t, alice, "private-paths", "linked", "--clear"))
	out, _, _ = runRepoOutput(t, bob, "private-paths", "linked")
	is.Equal(out, "Source: forge\nPaths:\n  - AGENTS.md\n  - _bmad\n")

	// Forge-linked repositories only.
	err = runRepo(t, admin, "private-paths", "plain")
	is.True(err != nil && strings.Contains(err.Error(), "not linked to a configured forge"))
	err = runRepo(t, admin, "private-paths", "plain", "AGENTS.md")
	is.True(err != nil && strings.Contains(err.Error(), "not linked to a configured forge"))
	is.True(errors.Is(runRepo(t, bob, "private-paths", "nope"), proto.ErrRepoNotFound))
}

// newPrivateSessionContext is a session test context with a forge-linked
// repository r whose private paths are AGENTS.md and _bmad; save answers
// the private save script.
func newPrivateSessionContext(t *testing.T, save func() *openshell.ExecResult) (context.Context, *backend.Backend, *fakeOpenShell, *[]string) {
	t.Helper()
	is := is.New(t)
	ctx, be, user, fc := newSessionTestContext(t)
	withPrivateForge(t, ctx)
	_, err := be.CreateRepository(ctx, "r", user, proto.RepositoryOptions{})
	is.NoErr(err)
	linkRepo(t, ctx, "r")
	var ran []string
	fc.scripts = func(_ string, command []string) (*openshell.ExecResult, bool) {
		if len(command) < 4 || command[0] != "sh" {
			return nil, false
		}
		switch command[3] {
		case "sundae-private-restore":
			ran = append(ran, "restore "+strings.Join(command[4:], " "))
			return &openshell.ExecResult{Stdout: []byte("restored\n")}, true
		case "sundae-private-save":
			ran = append(ran, "save "+strings.Join(command[4:], " "))
			return save(), true
		}
		return nil, false
	}
	return ctx, be, fc, &ran
}

func TestSessionPrivateFiles(t *testing.T) {
	is := is.New(t)
	saveResult := &openshell.ExecResult{Stdout: []byte("saved 0123456789abcdef\n")}
	ctx, _, fc, ran := newPrivateSessionContext(t, func() *openshell.ExecResult { return saveResult })
	sandbox := openshell.SessionSandboxName("alice", "r")

	// A start restores them.
	_, _, err := runSessionCmd(t, ctx, "start", "r", "-d")
	is.NoErr(err)
	is.Equal(*ran, []string{"restore /sandbox/r AGENTS.md _bmad"})

	stdout, _, err := runSessionCmd(t, ctx, "save-private", "r")
	is.NoErr(err)
	is.Equal(stdout, "r: saved 0123456 (AGENTS.md, _bmad)\n")
	is.Equal((*ran)[1], "save /sandbox/r AGENTS.md _bmad")

	// A failed save refuses the delete, before anything is lost.
	saveResult = &openshell.ExecResult{ExitCode: 1, Stderr: []byte("error: failed to push some refs")}
	_, _, err = runSessionCmd(t, ctx, "delete", "r")
	is.True(errors.Is(err, openshell.ErrPrivateNotSaved))
	is.True(strings.Contains(err.Error(), "session r was not deleted"))
	is.True(strings.Contains(err.Error(), "failed to push some refs"))
	is.True(strings.Contains(err.Error(), "session delete --discard-private r"))
	is.Equal(fc.phase(sandbox), sbx.Ready)

	// A failed save only warns on suspend: the storage keeps the files.
	_, stderr, err := runSessionCmd(t, ctx, "suspend", "r")
	is.NoErr(err)
	is.True(strings.Contains(stderr, "Warning: could not save the private files"))
	is.Equal(fc.phase(sandbox), sbx.Suspended)
	_, _, err = runSessionCmd(t, ctx, "save-private", "r")
	is.True(err != nil && strings.Contains(err.Error(), "resume it first"))

	// A suspended session cannot save (its suspend's save failed): the delete is
	// refused, unless --discard-private.
	n := len(*ran)
	_, _, err = runSessionCmd(t, ctx, "delete", "r")
	is.True(errors.Is(err, openshell.ErrPrivateNotSaved))
	is.True(strings.Contains(err.Error(), `resume it and run "session save-private r"`))
	is.True(strings.Contains(err.Error(), "session delete --discard-private r"))
	is.Equal(fc.phase(sandbox), sbx.Suspended)
	_, stderr, err = runSessionCmd(t, ctx, "delete", "r", "--discard-private")
	is.NoErr(err)
	is.True(strings.Contains(stderr, "deleting it loses the private files of r not saved before"))
	is.Equal(len(*ran), n)
	is.Equal(fc.phase(sandbox), sbx.Phase(""))

	// --discard-private deletes a running session whose save fails.
	_, _, err = runSessionCmd(t, ctx, "start", "r", "-d")
	is.NoErr(err)
	_, stderr, err = runSessionCmd(t, ctx, "delete", "r", "--discard-private")
	is.NoErr(err)
	is.True(strings.Contains(stderr, "loses its unsaved private files"))
	is.True(strings.Contains(stderr, "Session deleted"))
	is.Equal(fc.phase(sandbox), sbx.Phase(""))

	// A saving delete deletes.
	saveResult = &openshell.ExecResult{Stdout: []byte("unchanged 0123456789abcdef\n")}
	_, _, err = runSessionCmd(t, ctx, "start", "r", "-d")
	is.NoErr(err)
	n = len(*ran)
	_, _, err = runSessionCmd(t, ctx, "delete", "r")
	is.NoErr(err)
	is.Equal((*ran)[n], "save /sandbox/r AGENTS.md _bmad")
	is.Equal(fc.phase(sandbox), sbx.Phase(""))
}
