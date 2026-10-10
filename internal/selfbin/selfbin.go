// Package selfbin installs the running binary as /opt/bin/keenetic-tools
// and the awg-monitor symlink to it.
package selfbin

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"

	"github.com/asiforis/keenetic-tools/internal/markers"
	"github.com/asiforis/keenetic-tools/internal/sys"
)

const (
	// Bin is where the binary lives on the router.
	Bin = "/opt/bin/keenetic-tools"
	// MonitorLink is the awg-monitor name of the same binary.
	MonitorLink = "/opt/bin/awg-monitor"
	linkTarget  = "keenetic-tools"
)

// Check refuses early when Bin is somebody else's file.
func Check(env *sys.Env) error {
	if env.Exists(Bin) && !env.Owned(Bin, markers.Binary) {
		return foreign(Bin)
	}
	return nil
}

// CheckLink refuses early when path is something other than our symlink.
func CheckLink(env *sys.Env, path string) error {
	if env.Exists(path) && !IsLink(env, path) {
		return foreign(path)
	}
	return nil
}

func foreign(path string) error {
	return fmt.Errorf("%s уже существует и не принадлежит keenetic-tools. Файл не изменён", path)
}

// Install copies self (a host path) to Bin unless it already is that file.
// A foreign file at Bin is never replaced.
func Install(env *sys.Env, self string) error {
	if env.Exists(Bin) && !env.Owned(Bin, markers.Binary) {
		return foreign(Bin)
	}
	if same(self, env.P(Bin)) {
		return nil
	}
	data, err := os.ReadFile(self)
	if err != nil {
		return err
	}
	if !sys.HasMarkerLine(data, markers.Binary) {
		return fmt.Errorf("%s — не бинарник keenetic-tools", self)
	}
	if old, err := env.ReadFile(Bin); err == nil && bytes.Equal(old, data) {
		return nil
	}
	return env.WriteAtomic(Bin, data, 0o755)
}

// Link points path at Bin with a relative symlink. Anything else already
// at path is refused.
func Link(env *sys.Env, path string) error {
	if IsLink(env, path) {
		return nil
	}
	if env.Exists(path) {
		return foreign(path)
	}
	return os.Symlink(linkTarget, env.P(path))
}

// Unlink removes path if it is our symlink.
func Unlink(env *sys.Env, path string) error {
	if !IsLink(env, path) {
		return nil
	}
	return env.Remove(path)
}

// IsLink reports whether path is our symlink to Bin.
func IsLink(env *sys.Env, path string) bool {
	t, err := os.Readlink(env.P(path))
	return err == nil && (t == linkTarget || t == Bin)
}

func same(a, b string) bool {
	fa, err := os.Stat(a)
	if err != nil {
		return false
	}
	fb, err := os.Stat(b)
	if err != nil {
		return false
	}
	return os.SameFile(fa, fb)
}

// Self returns the path of the running executable.
func Self() (string, error) {
	p, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.Clean(p), nil
}
