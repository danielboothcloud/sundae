package forge

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/config"
)

// fakeGitHub serves GET /api/v3/user/repos (the GitHub Enterprise Server
// layout, which a non-github.com url selects) from pages, linking each page
// to the next with a Link header as GitHub documents.
func fakeGitHub(t *testing.T, token string, pagesFor func(base string) [][]map[string]any) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v3/user/repos" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"message":"Bad credentials"}`)
			return
		}
		if r.Header.Get("Accept") != "application/vnd.github+json" || r.Header.Get("X-GitHub-Api-Version") == "" {
			t.Errorf("missing GitHub API headers: %v", r.Header)
		}
		q := r.URL.Query()
		if q.Get("per_page") != "100" || q.Get("affiliation") != "owner,collaborator,organization_member" {
			t.Errorf("unexpected query %v", q)
		}
		pages := pagesFor(srv.URL)
		page, _ := strconv.Atoi(q.Get("page"))
		if page == 0 {
			page = 1
		}
		if page < len(pages) {
			next := fmt.Sprintf("%s/api/v3/user/repos?per_page=100&affiliation=owner,collaborator,organization_member&page=%d", srv.URL, page+1)
			w.Header().Set("Link", fmt.Sprintf(`<%s>; rel="next", <%s>; rel="last"`, next, next))
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(pages[page-1]) //nolint:errcheck
	}))
	t.Cleanup(srv.Close)
	return srv
}

func ghRepo(base, path string, private bool) map[string]any {
	return map[string]any{
		"full_name":      path,
		"clone_url":      base + "/" + path + ".git",
		"html_url":       base + "/" + path,
		"default_branch": "main",
		"private":        private,
		"archived":       false,
		"description":    "repo " + path,
	}
}

func TestDiscoverGitHub(t *testing.T) {
	srv := fakeGitHub(t, "ghp_test", func(base string) [][]map[string]any {
		return [][]map[string]any{
			{ghRepo(base, "me/a", false), ghRepo(base, "me/b", true)},
			{
				ghRepo(base, "org/c", false),
				// A clone URL on another host is rejected.
				{"full_name": "me/evil", "clone_url": "https://evil.example/me/evil.git", "default_branch": "main"},
				// So is a path that would escape the prefix.
				{"full_name": "../escape", "clone_url": base + "/x.git"},
			},
			{ghRepo(base, "me/d", false)},
		}
	})
	base := srv.URL

	f := newTestForge(t, config.ForgeConfig{Name: "gh", Kind: "github", URL: base, TokenFile: writeToken(t, "ghp_test")})
	listing, err := f.Discover(t.Context(), nil)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	var paths []string
	for _, r := range listing.Repos {
		paths = append(paths, r.Path)
	}
	if strings.Join(paths, ",") != "me/a,me/b,me/d,org/c" {
		t.Errorf("paths = %v", paths)
	}
	b, _ := listing.Find("ME/B")
	if !b.Private || b.DefaultBranch != "main" || b.CloneURL != base+"/me/b.git" || b.Description != "repo me/b" {
		t.Errorf("me/b = %+v", b)
	}
	if len(listing.Rejected) != 2 {
		t.Fatalf("rejected = %+v, want 2", listing.Rejected)
	}
	for _, rj := range listing.Rejected {
		switch rj.Path {
		case "me/evil":
			if !strings.Contains(rj.Reason, ErrForeignCloneURL.Error()) {
				t.Errorf("me/evil rejected for %q", rj.Reason)
			}
		case "../escape":
			if !strings.Contains(rj.Reason, ErrInvalidPath.Error()) {
				t.Errorf("../escape rejected for %q", rj.Reason)
			}
		default:
			t.Errorf("unexpected rejection %+v", rj)
		}
	}
}

func TestDiscoverGitHubErrors(t *testing.T) {
	srv := fakeGitHub(t, "right", func(string) [][]map[string]any { return [][]map[string]any{{}} })
	f := newTestForge(t, config.ForgeConfig{Name: "gh", Kind: "github", URL: srv.URL, TokenFile: writeToken(t, "wrong")})
	_, err := f.Discover(t.Context(), nil)
	if err == nil || !strings.Contains(err.Error(), "401") || !strings.Contains(err.Error(), "Bad credentials") {
		t.Errorf("err = %v, want 401 with the API message", err)
	}
	if strings.Contains(fmt.Sprint(err), "wrong") {
		t.Errorf("token leaked into error: %v", err)
	}

	noToken := newTestForge(t, config.ForgeConfig{Name: "gh", Kind: "github", URL: srv.URL})
	if _, err := noToken.Discover(t.Context(), nil); !errors.Is(err, ErrNoToken) {
		t.Errorf("err = %v, want ErrNoToken", err)
	}
}

// TestDiscoverRefusesForeignNextPage checks that pagination never carries
// the token to another host.
func TestDiscoverRefusesForeignNextPage(t *testing.T) {
	var leaked bool
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaked = r.Header.Get("PRIVATE-TOKEN") != "" || r.Header.Get("Authorization") != ""
		fmt.Fprint(w, "[]")
	}))
	t.Cleanup(other.Close)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Link", `<`+other.URL+`/api/v4/projects?page=2>; rel="next"`)
		fmt.Fprint(w, "[]")
	}))
	t.Cleanup(srv.Close)

	f := newTestForge(t, config.ForgeConfig{Name: "gl", Kind: "gitlab", URL: srv.URL, TokenFile: writeToken(t, "glpat")})
	_, err := f.Discover(t.Context(), nil)
	if err == nil || !strings.Contains(err.Error(), "off the API host") {
		t.Errorf("err = %v, want refusal of the next page link", err)
	}
	if leaked {
		t.Error("token sent to another host")
	}
}

// TestDiscoverRefusesForeignRedirect checks that a redirect off the forge
// host is not followed with the token.
func TestDiscoverRefusesForeignRedirect(t *testing.T) {
	var leaked bool
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaked = r.Header.Get("PRIVATE-TOKEN") != ""
		fmt.Fprint(w, "[]")
	}))
	t.Cleanup(other.Close)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+r.URL.Path, http.StatusFound)
	}))
	t.Cleanup(srv.Close)

	f := newTestForge(t, config.ForgeConfig{Name: "gl", Kind: "gitlab", URL: srv.URL, TokenFile: writeToken(t, "glpat")})
	if _, err := f.Discover(t.Context(), nil); err == nil || !strings.Contains(err.Error(), "refusing redirect") {
		t.Errorf("err = %v, want refused redirect", err)
	}
	if leaked {
		t.Error("token sent to another host")
	}
}

func glProject(base, path, visibility string) map[string]any {
	return map[string]any{
		"path_with_namespace": path,
		"http_url_to_repo":    base + "/" + path + ".git",
		"web_url":             base + "/" + path,
		"default_branch":      "trunk",
		"visibility":          visibility,
		"archived":            path == "grp/old",
		"description":         "",
	}
}

func TestDiscoverGitLab(t *testing.T) {
	var srv *httptest.Server
	var calls int
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/gitlab/api/v4/projects" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("PRIVATE-TOKEN") != "glpat-test" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		q := r.URL.Query()
		if q.Get("membership") != "true" || q.Get("per_page") != "100" {
			t.Errorf("unexpected query %v", q)
		}
		base := srv.URL + "/gitlab"
		var page []map[string]any
		switch q.Get("page") {
		case "", "1":
			// Page 1 links with a Link header.
			w.Header().Set("Link", `<`+srv.URL+`/gitlab/api/v4/projects?membership=true&per_page=100&page=2>; rel="next"`)
			page = []map[string]any{glProject(base, "me/a", "private"), glProject(base, "grp/sub/b", "internal")}
		case "2":
			// Page 2 only has GitLab's X-Next-Page header.
			w.Header().Set("X-Next-Page", "3")
			page = []map[string]any{glProject(base, "grp/old", "public")}
		case "3":
			w.Header().Set("X-Next-Page", "")
			page = []map[string]any{glProject(base, "me/a", "private")} // duplicate across pages
		}
		json.NewEncoder(w).Encode(page) //nolint:errcheck
	}))
	t.Cleanup(srv.Close)

	f := newTestForge(t, config.ForgeConfig{Name: "gl", Kind: "gitlab", URL: srv.URL + "/gitlab", TokenFile: writeToken(t, "glpat-test")})
	listing, err := f.Discover(t.Context(), nil)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if calls != 3 {
		t.Errorf("API called %d times, want 3", calls)
	}
	if len(listing.Repos) != 3 || len(listing.Rejected) != 0 {
		t.Fatalf("listing = %+v", listing)
	}
	b, ok := listing.Find("grp/sub/b")
	if !ok || !b.Private || b.DefaultBranch != "trunk" {
		t.Errorf("grp/sub/b = %+v (internal must count as private)", b)
	}
	old, _ := listing.Find("grp/old")
	if old.Private || !old.Archived {
		t.Errorf("grp/old = %+v", old)
	}
}

func TestLinkNext(t *testing.T) {
	for h, want := range map[string]string{
		``: ``,
		`<https://a/x?page=2>; rel="next", <https://a/x?page=9>; rel="last"`:  `https://a/x?page=2`,
		`<https://a/x?page=1>; rel="prev", <https://a/x?page=3>; rel="next"`:  `https://a/x?page=3`,
		`<https://a/x?page=1>; rel="first"`:                                   ``,
		`<https://a/x?page=4>; rel="prev next"`:                               `https://a/x?page=4`,
		`<https://a/x?a=1,2&page=5>; rel=next`:                                `https://a/x?a=1,2&page=5`,
		`<https://a/x?page=6>; title="a, b"; rel="next"`:                      `https://a/x?page=6`,
		`<https://a/x?page=7>; rel="nextish", <https://a/x?page=8>; rel=next`: `https://a/x?page=8`,
	} {
		if got := linkNext(h); got != want {
			t.Errorf("linkNext(%q) = %q, want %q", h, got, want)
		}
	}
}
