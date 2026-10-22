package git

import (
	"regexp"
	"strings"

	"github.com/aymanbagabas/git-module"
)

// ZeroID is the zero hash.
const ZeroID = git.EmptyID

// IsZeroHash returns whether the hash is a zero hash.
func IsZeroHash(h string) bool {
	pattern := regexp.MustCompile(`^0{40,}$`)
	return pattern.MatchString(h)
}

// Commit is a wrapper around git.Commit with helper methods.
type Commit = git.Commit

// Commits is a list of commits.
type Commits []*Commit

// Len implements sort.Interface.
func (cl Commits) Len() int { return len(cl) }

// Swap implements sort.Interface.
func (cl Commits) Swap(i, j int) { cl[i], cl[j] = cl[j], cl[i] }

// Less implements sort.Interface.
func (cl Commits) Less(i, j int) bool {
	return cl[i].Author.When.After(cl[j].Author.When)
}

// changeIDHeader is the header jj writes into the git commits it pushes.
const changeIDHeader = "change-id "

var changeIDPattern = regexp.MustCompile(`^[A-Za-z0-9]{1,64}$`)

// ShortChangeIDLen is the length of an abbreviated jj change ID (jj's own
// default).
const ShortChangeIDLen = 8

// ParseChangeID returns the jj change ID found in the header of a raw git
// commit object (as printed by `git cat-file commit`), or "" when the commit
// has no change-id header. Only the header block is inspected; the message is
// never searched.
func ParseChangeID(raw []byte) string {
	for _, line := range strings.Split(string(raw), "\n") {
		if line == "" {
			// End of headers.
			return ""
		}
		if id, ok := strings.CutPrefix(line, changeIDHeader); ok {
			id = strings.TrimSpace(id)
			// The header is pusher-controlled: only accept a short
			// alphanumeric token so it can't carry escape sequences.
			if !changeIDPattern.MatchString(id) {
				return ""
			}
			return id
		}
	}
	return ""
}

// ChangeID returns the jj change ID of the given commit, or "" if the commit
// carries no change-id header (e.g. it was created by plain git).
func (r *Repository) ChangeID(c *Commit) string {
	if c == nil || c.ID == nil {
		return ""
	}
	out, err := git.NewCommand("cat-file", "commit", c.ID.String()).RunInDir(r.Path)
	if err != nil {
		return ""
	}
	return ParseChangeID(out)
}

// ShortChangeID abbreviates a jj change ID. An empty ID stays empty.
func ShortChangeID(id string) string {
	if len(id) > ShortChangeIDLen {
		return id[:ShortChangeIDLen]
	}
	return id
}
