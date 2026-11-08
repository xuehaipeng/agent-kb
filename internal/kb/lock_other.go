//go:build !unix

package kb

import (
	"errors"
	"os"
	"time"
)

// Lock takes an exclusive lock using an O_EXCL lock file (non-unix fallback).
func (s *Store) Lock() (func(), error) {
	if err := os.MkdirAll(s.LocalPath(), 0o755); err != nil {
		return nil, err
	}
	p := s.LocalPath("lock.excl")
	deadline := time.Now().Add(10 * time.Second)
	for {
		f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			f.Close()
			return func() { os.Remove(p) }, nil
		}
		if time.Now().After(deadline) {
			return nil, errors.New("timed out waiting for " + p)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
