package forge

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// NOTE: a listing can take a forge tens of seconds (GitLab, ~800 projects:
// ~40s), so it is kept on disk and a restart does not wait for a new one.

// listingVersion is the format of a listing file; others are ignored.
const listingVersion = 1

// listingDir is where listing files live, under the data path.
const listingDir = "forge-listings"

// listingFile is a forge's last successful listing on disk. Kind, URL and
// Account must match the forge's, or the file is ignored.
type listingFile struct {
	Version int      `json:"version"`
	Kind    string   `json:"kind"`
	URL     string   `json:"url"`
	Account string   `json:"account"`
	Listing *Listing `json:"listing"`
}

// listingAccount identifies the token without storing it: another token
// can see other repositories.
func listingAccount(f *Forge) (string, error) {
	token, err := f.cfg.Token()
	if err != nil {
		return "", err
	}
	if token == "" {
		return "", ErrNoToken
	}
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:8]), nil
}

// listingPath is the forge's listing file; empty without a data path.
func (s *Service) listingPath(name string) string {
	if s.dataPath == "" {
		return ""
	}
	return filepath.Join(s.dataPath, listingDir, name+".json")
}

// restoreListing loads the forge's listing from disk, once, unless a
// discovery already set one.
func (s *Service) restoreListing(f *Forge) {
	s.mu.Lock()
	st := s.states[f.Name()]
	s.mu.Unlock()
	st.restore.Do(func() {
		l, err := s.readListing(f)
		if err != nil {
			s.logger.Debug("ignoring stored forge listing", "forge", f.Name(), "err", err)
			return
		}
		if l == nil {
			return
		}
		s.mu.Lock()
		if st.listing == nil {
			st.listing = l
		}
		s.mu.Unlock()
	})
}

// readListing reads the forge's listing file. It returns nil, nil when
// there is none. Each repository is checked again, as discovery does.
func (s *Service) readListing(f *Forge) (*Listing, error) {
	p := s.listingPath(f.Name())
	if p == "" {
		return nil, nil
	}
	b, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err //nolint:wrapcheck
	}
	var lf listingFile
	if err := json.Unmarshal(b, &lf); err != nil {
		return nil, fmt.Errorf("%s: %w", p, err)
	}
	account, err := listingAccount(f)
	if err != nil {
		return nil, err
	}
	switch {
	case lf.Version != listingVersion:
		return nil, fmt.Errorf("%s: version %d, want %d", p, lf.Version, listingVersion)
	case lf.Kind != f.Kind() || lf.URL != f.URL() || lf.Account != account:
		return nil, fmt.Errorf("%s: listed for another forge or token", p)
	case lf.Listing == nil:
		return nil, fmt.Errorf("%s: no listing", p)
	}
	l := lf.Listing
	repos := l.Repos[:0]
	for _, r := range l.Repos {
		if _, err := f.RepoName(r.Path); err != nil {
			continue
		}
		if err := s.checkRemote(f, r.CloneURL); err != nil {
			continue
		}
		repos = append(repos, r)
	}
	l.Repos = repos
	return l, nil
}

// saveListing writes the forge's listing file, unless a newer listing was
// written already. Failures are logged: the listing stays in memory.
func (s *Service) saveListing(f *Forge, l *Listing) {
	p := s.listingPath(f.Name())
	if p == "" || l == nil {
		return
	}
	s.saveMu.Lock()
	defer s.saveMu.Unlock()
	s.mu.Lock()
	st := s.states[f.Name()]
	stale := l.Time.Before(st.savedAt)
	s.mu.Unlock()
	if stale {
		return
	}
	if err := writeListing(p, f, l); err != nil {
		s.logger.Warn("cannot store forge listing", "forge", f.Name(), "err", err)
		return
	}
	s.mu.Lock()
	st.savedAt = l.Time
	s.mu.Unlock()
}

// writeListing replaces the file at p atomically (temporary file, then
// rename), so a crash never leaves half a listing.
func writeListing(p string, f *Forge, l *Listing) error {
	account, err := listingAccount(f)
	if err != nil {
		return err
	}
	b, err := json.Marshal(listingFile{Version: listingVersion, Kind: f.Kind(), URL: f.URL(), Account: account, Listing: l})
	if err != nil {
		return err //nolint:wrapcheck
	}
	dir := filepath.Dir(p)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err //nolint:wrapcheck
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(p)+".*.tmp")
	if err != nil {
		return err //nolint:wrapcheck
	}
	defer os.Remove(tmp.Name()) //nolint:errcheck
	if _, err := tmp.Write(b); err != nil {
		tmp.Close() //nolint:errcheck
		return err  //nolint:wrapcheck
	}
	if err := tmp.Close(); err != nil {
		return err //nolint:wrapcheck
	}
	return os.Rename(tmp.Name(), p) //nolint:wrapcheck
}
