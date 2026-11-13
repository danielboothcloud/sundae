package openshell

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/danielboothcloud/sundae/pkg/config"
	"github.com/danielboothcloud/sundae/pkg/proto"
)

func TestStartMultiRepoSession(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	gh := config.GitIdentity{Name: "Dan", Email: "dan@users.noreply.github.com"}
	gl := config.GitIdentity{Name: "Dan", Email: "dan@work.example"}

	s, err := h.mgr.Start(ctx, StartOptions{User: alice, Name: "bundle", Repos: []RepoSpec{
		{Name: "github/me/app", VCS: proto.VCSJJ, GitIdentity: gh},
		{Name: "gitlab/ops/infra", VCS: proto.VCSGit, GitIdentity: gl},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if s.Sandbox != SessionSandboxName("alice", "bundle") || strings.Join(s.Repos, ",") != "github/me/app,gitlab/ops/infra" {
		t.Fatalf("session = %+v", s)
	}
	// Several repositories: the program runs in /sandbox.
	if s.WorkDir != SandboxRoot || strings.Join(s.Cloned, ",") != "github/me/app,gitlab/ops/infra" {
		t.Fatalf("workdir %s cloned %v", s.WorkDir, s.Cloned)
	}
	b := h.exec.boxes[s.Sandbox]
	if b.identity["/sandbox/github/me/app"] != gh.String() || b.identity["/sandbox/gitlab/ops/infra"] != gl.String() {
		t.Fatalf("identities = %v", b.identity)
	}
	sb, _ := h.fc.Sandboxes().Get(ctx, "default", s.Sandbox)
	if sb.Annotations[AnnotationRepos] != "github/me/app,gitlab/ops/infra" || sb.Annotations[AnnotationRepo] != "github/me/app" {
		t.Fatalf("annotations = %v", sb.Annotations)
	}
	// The list matches the annotation: no file is written.
	if h.exec.count(s.Sandbox, writeReposName) != 0 {
		t.Fatal("list file written for an unchanged list")
	}
	if c := h.exec.clones(); len(c) != 2 || c[0].args()[0] != "jj" || c[1].args()[0] != "git" {
		t.Fatalf("clones = %v", c)
	}
}

func TestSessionReposAddRemove(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	s, err := h.mgr.Start(ctx, StartOptions{User: alice, Name: "work", Repo: "a"})
	if err != nil {
		t.Fatal(err)
	}
	if s.WorkDir != "/sandbox/a" {
		t.Fatalf("workdir %s", s.WorkDir)
	}

	// Adding resumes the session and clones only what is new.
	s, err = h.mgr.Resume(ctx, StartOptions{User: alice, Name: "work", Repos: []RepoSpec{{Name: "b"}}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(s.Repos, ",") != "a,b" || strings.Join(s.Cloned, ",") != "b" {
		t.Fatalf("repos %v cloned %v", s.Repos, s.Cloned)
	}
	b := h.exec.boxes[s.Sandbox]
	if b.files[reposFile] != "a\nb\n" {
		t.Fatalf("list file = %q", b.files[reposFile])
	}

	// Without a Monitor the list is read back from the sandbox on attach;
	// a repository it holds but the request does not name is resolved.
	var resolved []string
	s, err = h.mgr.Resume(ctx, StartOptions{User: alice, Name: "work", ResolveRepo: func(n string) (RepoSpec, error) {
		resolved = append(resolved, n)
		if n == "a" {
			return RepoSpec{}, errors.New("repository not found")
		}
		return RepoSpec{Name: n}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(s.Repos, ",") != "a,b" || strings.Join(resolved, ",") != "a,b" || h.exec.count(s.Sandbox, readReposName) == 0 {
		t.Fatalf("repos %v resolved %v", s.Repos, resolved)
	}

	// Removing leaves the checkout; the last one cannot go.
	var progress strings.Builder
	s, err = h.mgr.Resume(ctx, StartOptions{User: alice, Name: "work", RemoveRepos: []string{"a"}, Progress: &progress})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(s.Repos, ",") != "b" || !b.dirs["/sandbox/a"] || !strings.Contains(progress.String(), "Removed a from session work") {
		t.Fatalf("repos %v progress %q", s.Repos, progress.String())
	}
	if _, err := h.mgr.Resume(ctx, StartOptions{User: alice, Name: "work", RemoveRepos: []string{"b"}}); err == nil || !strings.Contains(err.Error(), "no repositories left") {
		t.Fatalf("removing the last: %v", err)
	}

	infos, err := h.mgr.List(ctx, alice)
	if err != nil || len(infos) != 1 {
		t.Fatalf("infos %v err %v", infos, err)
	}
}

func TestMonitorRemembersRepoLists(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	mon := NewMonitor(nil, config.DefaultConfig())
	h.mgr.monitor = mon
	s, err := h.mgr.Start(ctx, StartOptions{User: alice, Name: "w", Repos: []RepoSpec{{Name: "a"}, {Name: "b"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.mgr.Resume(ctx, StartOptions{User: alice, Name: "w", Repos: []RepoSpec{{Name: "c"}}}); err != nil {
		t.Fatal(err)
	}
	// The Monitor knew the list: it was never read back.
	if h.exec.count(s.Sandbox, readReposName) != 0 {
		t.Fatal("list read although known")
	}
	infos, _ := h.mgr.List(ctx, alice)
	if len(infos) != 1 || strings.Join(infos[0].Repos, ",") != "a,b,c" || infos[0].Repo != "a" || !infos[0].HasRepo("c") {
		t.Fatalf("infos = %+v", infos)
	}
	if _, err := h.mgr.End(ctx, alice, "w"); err != nil {
		t.Fatal(err)
	}
	if _, ok := mon.knownRepos(s.Sandbox); ok {
		t.Fatal("list kept after end")
	}
}

func TestProfileFromFirstMatchingRepo(t *testing.T) {
	ctx := context.Background()
	h := newHarnessWith(t, profileConfig())
	// No explicit profile and no repository profile: the first
	// sessions.repos match among the repositories, in order.
	s, err := h.mgr.Start(ctx, StartOptions{User: alice, Name: "mix", Repos: []RepoSpec{{Name: "docs"}, {Name: "infra/net"}}})
	if err != nil {
		t.Fatal(err)
	}
	if s.Profile != "claude" {
		t.Fatalf("profile = %q", s.Profile)
	}
	// A repository profile wins over the globs.
	s, err = h.mgr.Start(ctx, StartOptions{User: alice, Name: "mix2", RepoProfile: "shell", Repos: []RepoSpec{{Name: "infra/net"}}})
	if err != nil || s.Profile != "shell" {
		t.Fatalf("profile = %q err %v", s.Profile, err)
	}
}
