package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/forge"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/matryer/is"
)

// newForgeTestContext returns an auth test context with one forge
// configured and the forge service attached. The forge URL points nowhere:
// nothing here may reach it.
func newForgeTestContext(t *testing.T) (context.Context, *backend.Backend) {
	t.Helper()
	is := is.New(t)
	ctx, be := newAuthTestContext(t)
	cfg := config.FromContext(ctx)
	cfg.Forges = []config.ForgeConfig{{Name: "gh", Kind: "github", URL: "http://127.0.0.1:1"}}
	is.NoErr(cfg.Validate())
	svc, err := forge.NewService(cfg, be, nil)
	is.NoErr(err)
	return forge.WithContext(ctx, svc), be
}

func runForge(t *testing.T, ctx context.Context, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	c := ForgeCommand()
	var outBuf, errBuf bytes.Buffer
	c.SetOut(&outBuf)
	c.SetErr(&errBuf)
	c.SetArgs(args)
	err = c.ExecuteContext(ctx)
	return outBuf.String(), errBuf.String(), err
}

func TestForgeCommandPermissions(t *testing.T) {
	ctx, be := newForgeTestContext(t)
	is := is.New(t)
	admin := withUser(t, ctx, be, "admin1", true)
	alice := withUser(t, ctx, be, "alice", false)
	bob := withUser(t, ctx, be, "bob", false)
	_, err := be.CreateRepository(admin, "plain", proto.UserFromContext(admin), proto.RepositoryOptions{})
	is.NoErr(err)
	is.NoErr(be.AddCollaborator(admin, "plain", "alice", access.ReadWriteAccess))
	_, err = be.CreateRepository(admin, "secret", proto.UserFromContext(admin), proto.RepositoryOptions{Private: true})
	is.NoErr(err)

	// Listing, importing and syncing are admin-only, for users and
	// anonymous callers alike. The gate runs before the forge is contacted.
	for _, c := range []context.Context{alice, ctx} {
		for _, args := range [][]string{
			{"ls"},
			{"ls", "--json"},
			{"repos", "gh"},
			{"import", "gh", "me/x"},
			{"import", "gh", "--all"},
			{"sync"},
			{"sync", "gh"},
			{"sync", "gh", "me/x"},
		} {
			_, _, err := runForge(t, c, args...)
			if !errors.Is(err, proto.ErrUnauthorized) {
				t.Errorf("forge %v as non-admin: err = %v, want unauthorized", args, err)
			}
		}
	}

	// Pushing needs write access to the repository.
	_, _, err = runForge(t, bob, "push", "plain")
	is.True(errors.Is(err, proto.ErrUnauthorized))
	_, _, err = runForge(t, bob, "push", "secret")
	is.True(errors.Is(err, proto.ErrRepoNotFound)) // same as other repo commands
	_, _, err = runForge(t, ctx, "push", "plain")
	is.True(errors.Is(err, proto.ErrUnauthorized))

	// A collaborator passes the gate; the repository is not imported.
	_, _, err = runForge(t, alice, "push", "plain", "main")
	is.True(errors.Is(err, forge.ErrNotLinked))

	// Admins pass the gate.
	_, _, err = runForge(t, admin, "push", "plain")
	is.True(errors.Is(err, forge.ErrNotLinked))
	_, _, err = runForge(t, admin, "import", "gh")
	is.True(err != nil && err.Error() == "give either repository paths or --all")
	_, _, err = runForge(t, admin, "import", "gh", "me/x", "--all")
	is.True(err != nil && err.Error() == "give either repository paths or --all")
	_, _, err = runForge(t, admin, "sync", "nope")
	is.True(errors.Is(err, forge.ErrForgeNotFound))
}

func TestForgeListJSON(t *testing.T) {
	ctx, be := newForgeTestContext(t)
	is := is.New(t)
	admin := withUser(t, ctx, be, "admin1", true)

	out, _, err := runForge(t, admin, "ls", "--json")
	is.NoErr(err)
	var status []forge.ForgeStatus
	is.NoErr(json.Unmarshal([]byte(out), &status))
	is.Equal(len(status), 1)
	is.Equal(status[0].Name, "gh")
	is.Equal(status[0].Kind, "github")
	is.Equal(status[0].Imported, 0)
	is.True(status[0].Listed == nil)
	is.True(status[0].LastSync == nil)

	// Nothing imported: a forge-wide sync has nothing to do (no token, so
	// no listing either).
	out, _, err = runForge(t, admin, "sync", "gh")
	is.NoErr(err)
	is.Equal(out, "No imported repositories to sync\n")
}

func TestForgeCommandWithoutService(t *testing.T) {
	ctx, be := newAuthTestContext(t)
	admin := withUser(t, ctx, be, "admin1", true)
	_, _, err := runForge(t, admin, "ls")
	if !errors.Is(err, errForgesUnavailable) {
		t.Errorf("err = %v, want errForgesUnavailable", err)
	}
}
