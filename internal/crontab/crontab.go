// Package crontab edits /opt/etc/crontab: only lines ending with our marker
// are touched, everything else is kept byte for byte.
package crontab

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	"github.com/asiforis/keenetic-tools/internal/sys"
)

// Strip drops lines that end with marker. Every kept line gets a trailing
// newline, including a last line that had none.
func Strip(data []byte, marker string) []byte {
	var out bytes.Buffer
	for _, line := range lines(data) {
		if strings.HasSuffix(line, marker) {
			continue
		}
		out.WriteString(line)
		out.WriteByte('\n')
	}
	return out.Bytes()
}

func lines(data []byte) []string {
	if len(data) == 0 {
		return nil
	}
	s := strings.TrimSuffix(string(data), "\n")
	return strings.Split(s, "\n")
}

// Has reports whether any line mentions marker (`grep -Fq`).
func Has(data []byte, marker string) bool { return bytes.Contains(data, []byte(marker)) }

// Update removes our lines from the crontab at path and, if line is not
// empty, appends it. When the content changes the old file is first copied
// to backupPrefix+XXXXXX and that path is returned; "" means no change.
func Update(env *sys.Env, path, marker, line, backupPrefix string) (string, error) {
	old, err := env.ReadFile(path)
	if err != nil {
		return "", err
	}
	next := Strip(old, marker)
	if line != "" {
		next = append(next, line...)
		next = append(next, '\n')
	}
	if bytes.Equal(old, next) {
		return "", nil
	}
	backup, err := env.Backup(path, backupPrefix)
	if err != nil {
		return "", err
	}
	return backup, env.WriteInPlace(path, next)
}

const (
	// Path is the Entware crontab.
	Path = "/opt/etc/crontab"
	// Init is the cron init script.
	Init = "/opt/etc/init.d/S10cron"
)

// Ensure installs the Entware cron package unless its init script exists,
// and checks that the crontab is in place. opkg output goes to the user.
func Ensure(env *sys.Env) error {
	if !env.IsExecutable(Init) {
		if !env.Have("opkg") {
			return fmt.Errorf("opkg не найден. Нужен shell Entware")
		}
		if err := Stream(env, "opkg", "update"); err != nil {
			return fmt.Errorf("opkg update: %v", err)
		}
		if err := Stream(env, "opkg", "install", "cron"); err != nil {
			return fmt.Errorf("opkg install cron: %v", err)
		}
	}
	if !env.IsExecutable(Init) {
		return fmt.Errorf("после установки не найден %s", Init)
	}
	if !env.IsFile(Path) {
		return fmt.Errorf("не найден %s. Проверьте установку cron", Path)
	}
	return nil
}

// Restart makes cron reread the crontab; without cron installed it does nothing.
func Restart(env *sys.Env) error {
	if !env.IsExecutable(Init) {
		return nil
	}
	if err := Stream(env, Init, "restart"); err != nil {
		return fmt.Errorf("%s restart: %v", Init, err)
	}
	return nil
}

// Stream runs a command with its output going to the user; absolute names
// are router paths.
func Stream(env *sys.Env, name string, args ...string) error {
	if strings.HasPrefix(name, "/") {
		name = env.P(name)
	}
	r := env.Run.Run(context.Background(), sys.Cmd{Name: name, Args: args, Stdout: env.Stdout, Stderr: env.Stderr})
	return r.Err
}
