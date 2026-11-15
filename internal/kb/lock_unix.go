//go:build unix

package kb

import (
	"os"
	"syscall"
)

// Lock takes an exclusive advisory lock on the bundle for the duration of a
// write. Concurrent agents on the same machine serialise here; writers on
// other machines are reconciled by git (one file per claim, merge=union log).
func (s *Store) Lock() (func(), error) { return s.lockFile("lock") }

// lockFile takes an exclusive advisory lock on a named file in the local
// state directory, blocking until it is free.
func (s *Store) lockFile(name string) (func(), error) {
	p := s.LocalPath(name)
	if err := os.MkdirAll(s.LocalPath(), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}
