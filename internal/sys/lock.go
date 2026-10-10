package sys

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
)

// Lock is an exclusive flock(2) on a file that also holds the owner PID.
// The kernel drops it when the process exits, so a crash leaves no stale lock.
type Lock struct{ f *os.File }

// TryLock takes the lock without waiting; it returns nil, nil when another
// process holds it.
func (e *Env) TryLock(path string) (*Lock, error) {
	real := e.P(path)
	if err := os.MkdirAll(filepath.Dir(real), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(real, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, nil
		}
		return nil, err
	}
	if err := f.Truncate(0); err == nil {
		f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0)
	}
	return &Lock{f: f}, nil
}

// Release drops the lock; the file stays so that nobody races on its inode.
func (l *Lock) Release() error {
	if l == nil {
		return nil
	}
	return l.f.Close()
}

// LockHolder returns the PID holding the lock at path, or 0 if it is free.
func (e *Env) LockHolder(path string) int {
	f, err := os.Open(e.P(path))
	if err != nil {
		return 0
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); err == nil {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		return 0
	}
	return e.ReadPID(path)
}
