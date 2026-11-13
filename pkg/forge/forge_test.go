package forge

import (
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/danielboothcloud/sundae/pkg/config"
)

// newTestForge validates fc the way the server does and returns its Forge.
func newTestForge(t *testing.T, fc config.ForgeConfig) *Forge {
	t.Helper()
	cfg := config.DefaultConfig()
	cfg.DataPath = t.TempDir()
	cfg.Forges = []config.ForgeConfig{fc}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	f, err := New(cfg.Forges[0])
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func writeToken(t *testing.T, token string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(p, []byte(token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestMatches(t *testing.T) {
	f := newTestForge(t, config.ForgeConfig{
		Name:    "gh",
		Kind:    "github",
		Include: []string{"me/*", "org/**"},
		Exclude: []string{"me/old-*", "org/secret/**"},
	})
	for path, want := range map[string]bool{
		"me/tool":              true,
		"Me/Tool":              true, // case-insensitive
		"me/old-tool":          false,
		"me/a/b":               false, // * does not cross /
		"org/x":                true,
		"org/team/project":     true, // ** does
		"org/secret/project":   false,
		"someone/else":         false,
		"org/secret-ish/thing": true,
	} {
		if got := f.Matches(path); got != want {
			t.Errorf("Matches(%q) = %v, want %v", path, got, want)
		}
	}

	all := newTestForge(t, config.ForgeConfig{Name: "gl", Kind: "gitlab"})
	if !all.Matches("any/thing/at/all") {
		t.Error("no include patterns should include everything")
	}
}

func TestRepoName(t *testing.T) {
	f := newTestForge(t, config.ForgeConfig{Name: "gh", Kind: "github"})
	got, err := f.RepoName("me/tool")
	if err != nil || got != "gh/me/tool" {
		t.Errorf("RepoName(me/tool) = %q, %v", got, err)
	}

	p := newTestForge(t, config.ForgeConfig{Name: "gl", Kind: "gitlab", Prefix: "code/gl"})
	got, err = p.RepoName("group/sub/project")
	if err != nil || got != "code/gl/group/sub/project" {
		t.Errorf("RepoName(group/sub/project) = %q, %v", got, err)
	}

	for _, bad := range []string{"tool", "me/../../etc", "me/./x", "me//x", "me/x.git", "me/x y", "/me/x", "me/x/"} {
		if _, err := f.RepoName(bad); !errors.Is(err, ErrInvalidPath) {
			t.Errorf("RepoName(%q) err = %v, want ErrInvalidPath", bad, err)
		}
	}
}

func TestCheckCloneURL(t *testing.T) {
	gh := newTestForge(t, config.ForgeConfig{Name: "gh", Kind: "github"})
	gl := newTestForge(t, config.ForgeConfig{Name: "gl", Kind: "gitlab", URL: "http://10.0.0.5:8080/gitlab"})
	cases := []struct {
		f    *Forge
		url  string
		want bool
	}{
		{gh, "https://github.com/me/tool.git", true},
		{gh, "https://GitHub.com:443/me/tool.git", true},
		{gh, "https://github.com.evil.example/me/tool.git", false},
		{gh, "https://evil.example/me/tool.git", false},
		{gh, "http://github.com/me/tool.git", false},
		{gh, "https://github.com:8443/me/tool.git", false},
		{gh, "https://user:pw@github.com/me/tool.git", false},
		{gh, "https://github.com/me/tool.git?x=1", false},
		{gh, "ssh://git@github.com/me/tool.git", false},
		{gh, "git@github.com:me/tool.git", false},
		{gh, "file:///etc/passwd", false},
		{gh, "/srv/repo.git", false},
		{gh, "https://github.com/me/../../x.git", false},
		// A self-hosted GitLab on a private address is trusted because it
		// is the declared host; its relative URL root is enforced.
		{gl, "http://10.0.0.5:8080/gitlab/group/project.git", true},
		{gl, "http://10.0.0.5:8080/other/group/project.git", false},
		{gl, "http://10.0.0.6:8080/gitlab/group/project.git", false},
		{gl, "http://169.254.169.254/gitlab/x.git", false},
	}
	for _, tc := range cases {
		err := tc.f.CheckCloneURL(tc.url)
		if (err == nil) != tc.want {
			t.Errorf("%s CheckCloneURL(%q) = %v, want ok=%v", tc.f.Name(), tc.url, err, tc.want)
		}
		if err != nil && !errors.Is(err, ErrForeignCloneURL) {
			t.Errorf("CheckCloneURL(%q) error %v is not ErrForeignCloneURL", tc.url, err)
		}
	}
}

func TestAPIBase(t *testing.T) {
	for _, tc := range []struct {
		fc   config.ForgeConfig
		want string
	}{
		{config.ForgeConfig{Name: "gh", Kind: "github"}, "https://api.github.com"},
		{config.ForgeConfig{Name: "ghes", Kind: "github", URL: "https://ghe.example.com"}, "https://ghe.example.com/api/v3"},
		{config.ForgeConfig{Name: "gl", Kind: "gitlab"}, "https://gitlab.com/api/v4"},
		{config.ForgeConfig{Name: "glsub", Kind: "gitlab", URL: "https://example.com/gitlab/"}, "https://example.com/gitlab/api/v4"},
	} {
		if got := newTestForge(t, tc.fc).apiBase(); got != tc.want {
			t.Errorf("%s apiBase() = %q, want %q", tc.fc.Name, got, tc.want)
		}
	}
}

func TestGitEnv(t *testing.T) {
	envValue := func(env []string, key string) (string, bool) {
		for _, e := range env {
			if k, v, ok := strings.Cut(e, "="); ok && k == key {
				return v, true
			}
		}
		return "", false
	}
	configEntries := func(env []string) map[string]string {
		m := map[string]string{}
		for i := 0; ; i++ {
			k, ok := envValue(env, "GIT_CONFIG_KEY_"+strconv.Itoa(i))
			if !ok {
				return m
			}
			v, _ := envValue(env, "GIT_CONFIG_VALUE_"+strconv.Itoa(i))
			m[k] = v
		}
	}

	for _, tc := range []struct {
		kind, url, user, scope string
	}{
		{"github", "", "x-access-token", "http.https://github.com/.extraHeader"},
		{"gitlab", "https://gitlab.example.com/gl", "oauth2", "http.https://gitlab.example.com/gl/.extraHeader"},
	} {
		f := newTestForge(t, config.ForgeConfig{Name: "f", Kind: tc.kind, URL: tc.url, TokenFile: writeToken(t, "s3cret")})
		env, err := f.gitEnv()
		if err != nil {
			t.Fatal(err)
		}
		entries := configEntries(env)
		want := "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte(tc.user+":s3cret"))
		if entries[tc.scope] != want {
			t.Errorf("%s: %s = %q, want %q (entries %v)", tc.kind, tc.scope, entries[tc.scope], want, entries)
		}
		if entries["http.followRedirects"] != "false" {
			t.Errorf("%s: redirects not disabled: %v", tc.kind, entries)
		}
		if v, ok := entries["credential.helper"]; !ok || v != "" {
			t.Errorf("%s: credential helpers not reset: %v", tc.kind, entries)
		}
		if count, _ := envValue(env, "GIT_CONFIG_COUNT"); count != strconv.Itoa(len(entries)) {
			t.Errorf("%s: GIT_CONFIG_COUNT = %s, want %d", tc.kind, count, len(entries))
		}
		if !slices.Contains(env, "GIT_TERMINAL_PROMPT=0") {
			t.Errorf("%s: git may prompt", tc.kind)
		}
		for _, e := range env {
			if strings.Contains(e, "s3cret") {
				t.Errorf("%s: token in clear in %q", tc.kind, e)
			}
		}
	}

	// No token: no header, still no redirects.
	f := newTestForge(t, config.ForgeConfig{Name: "f", Kind: "github"})
	env, err := f.gitEnv()
	if err != nil {
		t.Fatal(err)
	}
	for k := range configEntries(env) {
		if strings.HasSuffix(k, "extraHeader") {
			t.Errorf("header set without a token: %v", env)
		}
	}
}
