//go:build unix

package forge

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"syscall"
)

// lockDir holds the per-repository lock files under the data path.
const lockDir = "forge-locks"

// fileLock takes an exclusive lock on the repository's lock file, shared by
// every Sundae process, so a forward in a hook process and a sync or push in
// the server never overlap. Without a data path it is a no-op.
func (s *Service) fileLock(repo string) func() {
	if s.dataPath == "" {
		return func() {}
	}
	dir := filepath.Join(s.dataPath, lockDir)
	sum := sha256.Sum256([]byte(repo))
	path := filepath.Join(dir, hex.EncodeToString(sum[:16])+".lock")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		s.logger.Warn("could not create forge lock directory", "err", err)
		return func() {}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec
	if err != nil {
		s.logger.Warn("could not open forge lock", "repo", repo, "err", err)
		return func() {}
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close() //nolint:errcheck,gosec
		s.logger.Warn("could not take forge lock", "repo", repo, "err", err)
		return func() {}
	}
	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN) //nolint:errcheck,gosec
		f.Close()                                   //nolint:errcheck,gosec
	}
}
