package sys

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// HasMarkerLine reports whether data has a line exactly equal to marker,
// like `grep -Fqx`. It also matches marker lines embedded in a binary.
func HasMarkerLine(data []byte, marker string) bool {
	m := []byte(marker)
	if bytes.HasPrefix(data, append(m, '\n')) || bytes.Equal(data, m) {
		return true
	}
	if bytes.Contains(data, append(append([]byte{'\n'}, m...), '\n')) {
		return true
	}
	return bytes.HasSuffix(data, append([]byte{'\n'}, m...))
}

// Owned reports whether path is a regular file (not a symlink) carrying the
// marker line: only such files may be replaced or removed.
func (e *Env) Owned(path, marker string) bool {
	fi, err := os.Lstat(e.P(path))
	if err != nil || !fi.Mode().IsRegular() {
		return false
	}
	data, err := os.ReadFile(e.P(path))
	return err == nil && HasMarkerLine(data, marker)
}

// Exists reports whether path exists, a dangling symlink included.
func (e *Env) Exists(path string) bool {
	_, err := os.Lstat(e.P(path))
	return err == nil
}

// IsExecutable reports whether path is a file with an execute bit.
func (e *Env) IsExecutable(path string) bool {
	fi, err := os.Stat(e.P(path))
	return err == nil && fi.Mode().IsRegular() && fi.Mode().Perm()&0o111 != 0
}

// IsFile reports whether path is a regular file (symlinks followed).
func (e *Env) IsFile(path string) bool {
	fi, err := os.Stat(e.P(path))
	return err == nil && fi.Mode().IsRegular()
}

// ReadFile reads a router file.
func (e *Env) ReadFile(path string) ([]byte, error) { return os.ReadFile(e.P(path)) }

// Remove deletes a file; a missing file is not an error.
func (e *Env) Remove(path string) error {
	err := os.Remove(e.P(path))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// WriteAtomic writes data to a temporary file next to path and renames it,
// so a running reader keeps the old inode.
func (e *Env) WriteAtomic(path string, data []byte, perm fs.FileMode) error {
	real := e.P(path)
	if err := os.MkdirAll(filepath.Dir(real), 0o755); err != nil {
		return err
	}
	tmp := fmt.Sprintf("%s.tmp.%d", real, os.Getpid())
	if err := os.WriteFile(tmp, data, perm); err != nil {
		os.Remove(tmp)
		return err
	}
	// WriteFile honours umask; set the mode explicitly.
	if err := os.Chmod(tmp, perm); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, real); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// WriteInPlace truncates and rewrites an existing file, keeping its inode,
// owner and mode (like `cat new > file`).
func (e *Env) WriteInPlace(path string, data []byte) error {
	f, err := os.OpenFile(e.P(path), os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

const tempChars = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"

// CreateTemp creates a new file named prefix + 6 random characters, like
// `mktemp prefixXXXXXX`, and returns its router path.
func (e *Env) CreateTemp(prefix string, data []byte, perm fs.FileMode) (string, error) {
	for range 100 {
		b := make([]byte, 6)
		if _, err := rand.Read(b); err != nil {
			return "", err
		}
		for i := range b {
			b[i] = tempChars[int(b[i])%len(tempChars)]
		}
		path := prefix + string(b)
		f, err := os.OpenFile(e.P(path), os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		_, werr := f.Write(data)
		cerr := f.Close()
		if werr != nil || cerr != nil {
			os.Remove(e.P(path))
			return "", errors.Join(werr, cerr)
		}
		return path, nil
	}
	return "", fmt.Errorf("не удалось создать временный файл %sXXXXXX", prefix)
}

// Backup copies path to prefix+XXXXXX keeping its mode and mtime (`cp -p`).
func (e *Env) Backup(path, prefix string) (string, error) {
	fi, err := os.Stat(e.P(path))
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(e.P(path))
	if err != nil {
		return "", err
	}
	b, err := e.CreateTemp(prefix, data, fi.Mode().Perm())
	if err != nil {
		return "", err
	}
	if err := os.Chmod(e.P(b), fi.Mode().Perm()); err != nil {
		return b, err
	}
	return b, os.Chtimes(e.P(b), fi.ModTime(), fi.ModTime())
}

// ReadPID reads a PID file; 0 when missing or malformed.
func (e *Env) ReadPID(path string) int {
	data, err := os.ReadFile(e.P(path))
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0
	}
	return pid
}

// FSType returns the filesystem type holding path, checking its deepest
// existing ancestor against /proc/mounts.
func (e *Env) FSType(path string) string {
	p := filepath.Clean(path)
	for {
		if fi, err := os.Stat(e.P(p)); err == nil && fi.IsDir() {
			break
		}
		if p == "/" {
			break
		}
		p = filepath.Dir(p)
	}
	f, err := os.Open(e.P("/proc/mounts"))
	if err != nil {
		return ""
	}
	defer f.Close()
	best, typ := -1, ""
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fl := strings.Fields(sc.Text())
		if len(fl) < 3 {
			continue
		}
		m := strings.ReplaceAll(fl[1], `\040`, " ")
		if (m == "/" || p == m || strings.HasPrefix(p, m+"/")) && len(m) >= best {
			best, typ = len(m), fl[2]
		}
	}
	return typ
}
