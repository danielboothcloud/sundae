package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	v1 "github.com/NVIDIA/OpenShell/sdk/go/openshell/v1"
	"github.com/NVIDIA/OpenShell/sdk/go/openshell/v1/fake"
	"github.com/danielboothcloud/sundae/pkg/config"
	"github.com/danielboothcloud/sundae/pkg/openshell"
	"github.com/danielboothcloud/sundae/pkg/proto"
	"github.com/matryer/is"
)

func runOpenShell(t *testing.T, ctx context.Context, args ...string) (string, error) {
	t.Helper()
	c := OpenShellCommand()
	var out bytes.Buffer
	c.SetOut(&out)
	c.SetErr(&out)
	c.SetArgs(args)
	err := c.ExecuteContext(ctx)
	return out.String(), err
}

func TestOpenShellProvidersCommand(t *testing.T) {
	is := is.New(t)
	ctx, be := newAuthTestContext(t)
	cfg := config.FromContext(ctx)
	dir := filepath.Join(t.TempDir(), "my-github")
	is.NoErr(os.MkdirAll(dir, 0o755))
	is.NoErr(os.WriteFile(filepath.Join(dir, "GITHUB_TOKEN"), []byte("ghp_topsecret\n"), 0o600))
	cfg.OpenShell.Providers = []config.OpenShellProvider{
		{Name: "my-github", Type: "github", CredentialsDir: dir},
		{Name: "taken", Type: "anthropic", CredentialsDir: dir},
	}
	cfg.Sessions.Profiles = map[string]config.SessionProfile{
		"claude": {Providers: []string{"my-github", "by-hand"}},
	}

	fc := fake.NewClient()
	t.Cleanup(func() { _ = fc.Close() })
	_, err := fc.Providers().Create(ctx, "default", &v1.Provider{Name: "taken", Type: "anthropic", Spec: v1.ProviderSpec{Credentials: map[string]string{"K": "v"}}})
	is.NoErr(err)
	r := openshell.NewProviderReconciler(func() (openshell.ProviderAPI, io.Closer, error) {
		return fc.Providers(), io.NopCloser(nil), nil
	}, cfg, openshell.WithProviderSyncKey([]byte("k")))
	r.Sync(ctx)
	ctx = openshell.ContextWithProviderReconciler(ctx, r)

	admin := withUser(t, ctx, be, "admin1", true)
	alice := withUser(t, ctx, be, "alice", false)

	// Admins only: users and anonymous callers are refused.
	for _, c := range []context.Context{alice, ctx} {
		for _, args := range [][]string{{"providers"}, {"providers", "--json"}} {
			_, err := runOpenShell(t, c, args...)
			is.True(errors.Is(err, proto.ErrUnauthorized))
		}
	}

	out, err := runOpenShell(t, admin, "providers")
	is.NoErr(err)
	for _, want := range []string{"my-github", "github", "synced", "sundae", "GITHUB_TOKEN", "claude", "taken", "conflict", "other", "was not created by Sundae", "by-hand", "manual"} {
		is.True(strings.Contains(out, want)) // missing: want
	}
	is.True(!strings.Contains(out, "ghp_topsecret"))

	out, err = runOpenShell(t, admin, "providers", "--json")
	is.NoErr(err)
	is.True(!strings.Contains(out, "ghp_topsecret"))
	var got []openshell.ProviderStatus
	is.NoErr(json.Unmarshal([]byte(out), &got))
	is.Equal(len(got), 3)
	is.Equal(got[0].Name, "my-github")
	is.Equal(got[0].State, openshell.ProviderSynced)
	is.True(got[0].Managed && got[0].Declared)
	is.Equal(got[0].CredentialKeys, []string{"GITHUB_TOKEN"})
	is.Equal(got[0].Profiles, []string{"claude"})
	is.True(got[0].LastSync != nil)
	is.Equal(got[1].State, openshell.ProviderConflict)
	is.Equal(got[2], openshell.ProviderStatus{Name: "by-hand", State: openshell.ProviderManual, Profiles: []string{"claude"}})

	// Nothing declared or listed.
	cfg.OpenShell.Providers = nil
	cfg.Sessions.Profiles = nil
	plain := openshell.ContextWithProviderReconciler(admin, nil)
	out, err = runOpenShell(t, plain, "providers")
	is.NoErr(err)
	is.True(strings.Contains(out, "No OpenShell providers"))
	out, err = runOpenShell(t, plain, "providers", "--json")
	is.NoErr(err)
	is.Equal(strings.TrimSpace(out), "[]")
}
