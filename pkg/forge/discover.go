package forge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/soft-serve/pkg/config"
)

// ErrNoToken is returned when discovery is attempted without a token.
var ErrNoToken = errors.New("no token_file configured; listing repositories needs a token")

const (
	// perPage is the page size requested from both APIs (their maximum).
	perPage = 100
	// maxPages bounds pagination, so a misbehaving API cannot loop forever.
	maxPages = 200
	// maxResponse bounds the size of one API response body.
	maxResponse = 32 << 20
)

// RemoteRepo is a repository listed by a forge.
type RemoteRepo struct {
	// Path is the forge path, "owner/repo" or "group/subgroup/project".
	Path string `json:"path"`
	// CloneURL is the HTTPS clone URL. It has passed CheckCloneURL.
	CloneURL      string `json:"clone_url"`
	DefaultBranch string `json:"default_branch"`
	// Private is true for anything not public (GitLab "internal" included).
	Private     bool   `json:"private"`
	Archived    bool   `json:"archived"`
	Description string `json:"description"`
	WebURL      string `json:"web_url"`
}

// Rejected is a listed repository that cannot be imported.
type Rejected struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// Listing is the result of a discovery.
type Listing struct {
	// Repos are the importable repositories, sorted by path, before the
	// include and exclude filter.
	Repos []RemoteRepo `json:"repos"`
	// Rejected are listed repositories with a clone URL off the forge host
	// or a path that cannot be a repository name.
	Rejected []Rejected `json:"rejected,omitempty"`
	// Time is when the listing was taken.
	Time time.Time `json:"time"`
}

// Find returns the listed repository with the given path, compared
// case-insensitively.
func (l *Listing) Find(path string) (RemoteRepo, bool) {
	if l == nil {
		return RemoteRepo{}, false
	}
	for _, r := range l.Repos {
		if strings.EqualFold(r.Path, path) {
			return r, true
		}
	}
	return RemoteRepo{}, false
}

// newAPIClient returns the HTTP client for forge APIs. It follows redirects
// only within the same scheme and host: GitLab's token header is not one Go
// strips on a cross-host redirect, and the forge host is the only one
// trusted.
func newAPIClient() *http.Client {
	return &http.Client{
		Timeout: 30 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("stopped after 5 redirects")
			}
			if !sameOrigin(req.URL, via[0].URL) {
				return fmt.Errorf("refusing redirect off the forge host to %s", req.URL.Redacted())
			}
			return nil
		},
	}
}

func sameOrigin(a, b *url.URL) bool {
	return strings.EqualFold(a.Scheme, b.Scheme) && hostPort(a) == hostPort(b)
}

// Discover lists the repositories the token's owner can access: GitHub's
// GET /user/repos (owned, collaborator and organization member) and GitLab's
// GET /projects?membership=true, following the Link rel="next" pagination
// both document. Repositories whose clone URL is not on the configured host,
// or whose path cannot be a repository name, are rejected.
func (f *Forge) Discover(ctx context.Context, client *http.Client) (*Listing, error) {
	return f.discover(ctx, client, (*Forge).CheckCloneURL)
}

// discover is Discover with the clone URL check passed in (see
// Service.checkRemote).
func (f *Forge) discover(ctx context.Context, client *http.Client, checkRemote func(*Forge, string) error) (*Listing, error) {
	token, err := f.cfg.Token()
	if err != nil {
		return nil, err
	}
	if token == "" {
		return nil, fmt.Errorf("forge %s: %w", f.Name(), ErrNoToken)
	}
	if client == nil {
		client = newAPIClient()
	}

	var next string
	var decode func(io.Reader) ([]RemoteRepo, error)
	header := http.Header{}
	switch f.Kind() {
	case config.ForgeKindGitHub:
		q := url.Values{
			"per_page":    {fmt.Sprint(perPage)},
			"affiliation": {"owner,collaborator,organization_member"},
			"sort":        {"full_name"},
		}
		next = f.apiBase() + "/user/repos?" + q.Encode()
		header.Set("Accept", "application/vnd.github+json")
		header.Set("X-GitHub-Api-Version", "2022-11-28")
		header.Set("Authorization", "Bearer "+token)
		decode = decodeGitHub
	case config.ForgeKindGitLab:
		q := url.Values{
			"membership": {"true"},
			"per_page":   {fmt.Sprint(perPage)},
			"order_by":   {"id"},
			"sort":       {"asc"},
		}
		next = f.apiBase() + "/projects?" + q.Encode()
		header.Set("PRIVATE-TOKEN", token)
		decode = decodeGitLab
	default:
		return nil, fmt.Errorf("forge %s: unsupported kind %q", f.Name(), f.Kind())
	}

	first, err := url.Parse(next)
	if err != nil {
		return nil, err
	}

	listing := &Listing{Time: time.Now()}
	seen := map[string]bool{}
	for page := 0; next != ""; page++ {
		if page >= maxPages {
			return nil, fmt.Errorf("forge %s: more than %d pages of repositories", f.Name(), maxPages)
		}
		repos, nextURL, err := f.getPage(ctx, client, next, header, decode)
		if err != nil {
			return nil, err
		}
		for _, r := range repos {
			key := strings.ToLower(r.Path)
			if seen[key] {
				continue
			}
			seen[key] = true
			if _, err := f.RepoName(r.Path); err != nil {
				listing.Rejected = append(listing.Rejected, Rejected{Path: r.Path, Reason: err.Error()})
				continue
			}
			if err := checkRemote(f, r.CloneURL); err != nil {
				listing.Rejected = append(listing.Rejected, Rejected{Path: r.Path, Reason: err.Error()})
				continue
			}
			listing.Repos = append(listing.Repos, r)
		}

		next = ""
		if nextURL != "" {
			u, err := first.Parse(nextURL)
			if err != nil {
				return nil, fmt.Errorf("forge %s: bad next page link %q: %w", f.Name(), nextURL, err)
			}
			// The token goes with every page, so the next page must be on
			// the API host the first one was.
			if !sameOrigin(u, first) {
				return nil, fmt.Errorf("forge %s: refusing next page link off the API host: %s", f.Name(), u.Redacted())
			}
			next = u.String()
		}
	}

	sort.Slice(listing.Repos, func(i, j int) bool {
		return strings.ToLower(listing.Repos[i].Path) < strings.ToLower(listing.Repos[j].Path)
	})
	return listing, nil
}

// getPage fetches one page and returns its repositories and the next page
// link, if any.
func (f *Forge) getPage(ctx context.Context, client *http.Client, u string, header http.Header, decode func(io.Reader) ([]RemoteRepo, error)) ([]RemoteRepo, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header = header.Clone()
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("forge %s: %w", f.Name(), err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		msg := strings.TrimSpace(string(body))
		return nil, "", fmt.Errorf("forge %s: GET %s: %s: %s", f.Name(), req.URL.Path, resp.Status, msg)
	}

	repos, err := decode(io.LimitReader(resp.Body, maxResponse))
	if err != nil {
		return nil, "", fmt.Errorf("forge %s: decode %s: %w", f.Name(), req.URL.Path, err)
	}

	next := linkNext(resp.Header.Get("Link"))
	if next == "" && f.Kind() == config.ForgeKindGitLab {
		// GitLab also sends the next page number on its own. The Link
		// header is preferred, as GitLab's docs ask.
		if p := resp.Header.Get("X-Next-Page"); p != "" {
			nu := *req.URL
			q := nu.Query()
			q.Set("page", p)
			nu.RawQuery = q.Encode()
			next = nu.String()
		}
	}
	return repos, next, nil
}

// linkRe matches one entry of a Link header.
var linkRe = regexp.MustCompile(`<([^>]*)>((?:\s*;\s*[^;,="]+(?:=(?:"[^"]*"|[^;,"]*))?)*)`)

// linkNext returns the rel="next" URL of a Link header, or "".
func linkNext(h string) string {
	for _, m := range linkRe.FindAllStringSubmatch(h, -1) {
		for _, param := range strings.Split(m[2], ";") {
			k, v, ok := strings.Cut(strings.TrimSpace(param), "=")
			if !ok || !strings.EqualFold(strings.TrimSpace(k), "rel") {
				continue
			}
			for _, rel := range strings.Fields(strings.Trim(strings.TrimSpace(v), `"`)) {
				if strings.EqualFold(rel, "next") {
					return m[1]
				}
			}
		}
	}
	return ""
}

// githubRepo is the part of GitHub's Repository object discovery uses.
type githubRepo struct {
	FullName      string `json:"full_name"`
	CloneURL      string `json:"clone_url"`
	DefaultBranch string `json:"default_branch"`
	Private       bool   `json:"private"`
	Visibility    string `json:"visibility"`
	Archived      bool   `json:"archived"`
	Description   string `json:"description"`
	HTMLURL       string `json:"html_url"`
}

func decodeGitHub(r io.Reader) ([]RemoteRepo, error) {
	var page []githubRepo
	if err := json.NewDecoder(r).Decode(&page); err != nil {
		return nil, err
	}
	repos := make([]RemoteRepo, 0, len(page))
	for _, g := range page {
		repos = append(repos, RemoteRepo{
			Path:          g.FullName,
			CloneURL:      g.CloneURL,
			DefaultBranch: g.DefaultBranch,
			Private:       g.Private || (g.Visibility != "" && g.Visibility != "public"),
			Archived:      g.Archived,
			Description:   g.Description,
			WebURL:        g.HTMLURL,
		})
	}
	return repos, nil
}

// gitlabProject is the part of GitLab's project object discovery uses.
type gitlabProject struct {
	PathWithNamespace string `json:"path_with_namespace"`
	HTTPURLToRepo     string `json:"http_url_to_repo"`
	DefaultBranch     string `json:"default_branch"`
	Visibility        string `json:"visibility"`
	Archived          bool   `json:"archived"`
	Description       string `json:"description"`
	WebURL            string `json:"web_url"`
}

func decodeGitLab(r io.Reader) ([]RemoteRepo, error) {
	var page []gitlabProject
	if err := json.NewDecoder(r).Decode(&page); err != nil {
		return nil, err
	}
	repos := make([]RemoteRepo, 0, len(page))
	for _, g := range page {
		repos = append(repos, RemoteRepo{
			Path:          g.PathWithNamespace,
			CloneURL:      g.HTTPURLToRepo,
			DefaultBranch: g.DefaultBranch,
			// Anything not explicitly public stays private on Soft Serve.
			Private:     g.Visibility != "public",
			Archived:    g.Archived,
			Description: g.Description,
			WebURL:      g.WebURL,
		})
	}
	return repos, nil
}
