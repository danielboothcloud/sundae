package forge

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/log/v2"
)

// restart returns a new service on the fixture's config and data path, as
// after a server restart, with the same test seam.
func (fx *serviceFixture) restart(t *testing.T) *Service {
	t.Helper()
	svc, err := NewService(fx.cfg, fx.be, log.New(io.Discard))
	if err != nil {
		t.Fatal(err)
	}
	svc.checkRemote = fx.svc.checkRemote
	return svc
}

func (ff *fakeForge) callCount() int {
	ff.mu.Lock()
	defer ff.mu.Unlock()
	return ff.calls
}

func listedPaths(t *testing.T, svc *Service) []string {
	t.Helper()
	repos, err := svc.KnownRepos(t.Context(), "gh", false)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, r := range repos {
		if r.Listed {
			out = append(out, r.Path)
		}
	}
	return out
}

func TestListingSurvivesRestart(t *testing.T) {
	fx := newServiceFixture(t)
	fx.upstream(t, "me/proj", false)
	fx.upstream(t, "me/other", false)
	if _, err := fx.svc.Discover(fx.ctx, "gh"); err != nil {
		t.Fatal(err)
	}
	calls := fx.ff.callCount()

	svc := fx.restart(t)
	st, err := svc.Status(fx.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st[0].Listed == nil || *st[0].Listed != 2 || st[0].ListedAt == nil {
		t.Fatalf("status after restart = %+v, want 2 listed with a time", st[0])
	}
	if got := listedPaths(t, svc); strings.Join(got, ",") != "me/other,me/proj" {
		t.Fatalf("listed after restart = %v", got)
	}
	if fx.ff.callCount() != calls {
		t.Fatalf("the restarted service called the forge %d times", fx.ff.callCount()-calls)
	}
	info, err := os.Stat(svc.listingPath("gh"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("listing file mode = %v, want 0600", info.Mode().Perm())
	}
}

func TestListingFailedDiscoveryKeepsFile(t *testing.T) {
	fx := newServiceFixture(t)
	fx.upstream(t, "me/proj", false)
	if _, err := fx.svc.Discover(fx.ctx, "gh"); err != nil {
		t.Fatal(err)
	}
	fx.ff.mu.Lock()
	fx.ff.fail = true
	fx.ff.mu.Unlock()
	if _, err := fx.svc.Discover(fx.ctx, "gh"); err == nil {
		t.Fatal("discovery against a failing forge succeeded")
	}
	if got := listedPaths(t, fx.restart(t)); len(got) != 1 || got[0] != "me/proj" {
		t.Fatalf("listed after a failed discovery and a restart = %v", got)
	}
}

func TestListingIgnoredForOtherToken(t *testing.T) {
	fx := newServiceFixture(t)
	fx.upstream(t, "me/proj", false)
	if _, err := fx.svc.Discover(fx.ctx, "gh"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fx.cfg.Forges[0].TokenFile, []byte("ghp_other\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := listedPaths(t, fx.restart(t)); len(got) != 0 {
		t.Fatalf("listed with another token = %v, want none", got)
	}
}

func TestListingIgnoresBadFiles(t *testing.T) {
	fx := newServiceFixture(t)
	fx.upstream(t, "me/proj", false)
	if _, err := fx.svc.Discover(fx.ctx, "gh"); err != nil {
		t.Fatal(err)
	}
	p := fx.svc.listingPath("gh")
	good, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"corrupt":     "{not json",
		"old version": strings.Replace(string(good), `"version":1`, `"version":0`, 1),
		"other url":   strings.Replace(string(good), fx.ff.srv.URL, "https://elsewhere.example", 1),
		"foreign url": strings.Replace(string(good), fx.root, "/elsewhere", 1),
	} {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if got := listedPaths(t, fx.restart(t)); len(got) != 0 {
				t.Fatalf("listed from a bad file = %v, want none", got)
			}
		})
	}
}

func TestListingWithoutDataPath(t *testing.T) {
	fx := newServiceFixture(t)
	fx.svc.dataPath = ""
	fx.upstream(t, "me/proj", false)
	if _, err := fx.svc.Discover(fx.ctx, "gh"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(fx.cfg.DataPath, listingDir)); !os.IsNotExist(err) {
		t.Fatalf("listing dir without a data path: %v", err)
	}
	if got := listedPaths(t, fx.svc); len(got) != 1 {
		t.Fatalf("listed in memory = %v", got)
	}
}
