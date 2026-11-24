//go:build !unix

package forge

// fileLock is a no-op where flock is missing; only the in-process lock
// applies, so hook processes and the server may overlap there.
func (s *Service) fileLock(string) func() { return func() {} }
