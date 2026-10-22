package proto

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/soft-serve/git"
)

// VCS is the kind of version control system a repository is presented as.
// Storage and transport are always a bare Git repository; the kind only
// affects presentation.
type VCS string

const (
	// VCSGit is a plain Git repository.
	VCSGit VCS = "git"
	// VCSJJ is a Jujutsu (jj) repository backed by Git.
	VCSJJ VCS = "jj"
	// DefaultVCS is the VCS used for new and imported repositories when none
	// is specified.
	DefaultVCS = VCSJJ
)

// ParseVCS parses a VCS name. An empty string yields DefaultVCS.
func ParseVCS(s string) (VCS, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "":
		return DefaultVCS, nil
	case string(VCSGit):
		return VCSGit, nil
	case string(VCSJJ):
		return VCSJJ, nil
	default:
		return "", fmt.Errorf("invalid vcs %q: must be one of git, jj", s)
	}
}

// String returns the VCS name.
func (v VCS) String() string { return string(v) }

// Repository is a Git repository interface.
type Repository interface {
	// ID returns the repository's ID.
	ID() int64
	// Name returns the repository's name.
	Name() string
	// ProjectName returns the repository's project name.
	ProjectName() string
	// Description returns the repository's description.
	Description() string
	// IsPrivate returns whether the repository is private.
	IsPrivate() bool
	// IsMirror returns whether the repository is a mirror.
	IsMirror() bool
	// IsHidden returns whether the repository is hidden.
	IsHidden() bool
	// Vcs returns the repository's VCS kind.
	Vcs() VCS
	// UserID returns the ID of the user who owns the repository.
	// It returns 0 if the repository is not owned by a user.
	UserID() int64
	// CreatedAt returns the time the repository was created.
	CreatedAt() time.Time
	// UpdatedAt returns the time the repository was last updated.
	// If the repository has never been updated, it returns the time it was created.
	UpdatedAt() time.Time
	// Open returns the underlying git.Repository.
	Open() (*git.Repository, error)
}

// RepositoryOptions are options for creating a new repository.
type RepositoryOptions struct {
	Private     bool
	Description string
	ProjectName string
	Mirror      bool
	Hidden      bool
	// Vcs is the repository's VCS kind. Empty means DefaultVCS.
	Vcs         VCS
	LFS         bool
	LFSEndpoint string
}

// RepositoryDefaultBranch returns the default branch of a repository.
func RepositoryDefaultBranch(repo Repository) (string, error) {
	r, err := repo.Open()
	if err != nil {
		return "", err
	}

	ref, err := r.HEAD()
	if err != nil {
		return "", err
	}

	return ref.Name().Short(), nil
}

// CloneCommand returns the command a user runs to clone url, in the idiom of
// the repository's VCS: "jj git clone <url>" for jj, or the bare url for git
// (callers prepend "git clone" themselves, see ui/common.Common.CloneCmd).
func CloneCommand(vcs VCS, url string) string {
	if vcs == VCSJJ {
		return "jj git clone " + url
	}
	return url
}
