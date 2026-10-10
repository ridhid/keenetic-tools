// keenetic-tools: AmneziaWG restart job and tunnel monitor for Keenetic / Entware.
// Called as awg-monitor (a symlink to the same binary) it runs the
// monitor commands directly.
package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime/debug"

	"github.com/asiforis/keenetic-tools/internal/awgcron"
	"github.com/asiforis/keenetic-tools/internal/markers"
	"github.com/asiforis/keenetic-tools/internal/monitor"
	"github.com/asiforis/keenetic-tools/internal/selfbin"
	"github.com/asiforis/keenetic-tools/internal/sys"
)

var version = "dev"

func usage(w io.Writer) {
	fmt.Fprint(w, `Использование:
  keenetic-tools awg-cron install [hourly|daily] | uninstall
  keenetic-tools awg-cron run          (задание cron: перезапуск туннеля)
  keenetic-tools awg-monitor КОМАНДА   (то же, что awg-monitor КОМАНДА)
  keenetic-tools version
`)
}

func main() {
	// Keep the marker lines in the binary (see markers.Embedded).
	if len(markers.Embedded) == 0 {
		panic("markers")
	}
	os.Exit(run(os.Args, os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if filepath.Base(args[0]) == "awg-monitor" {
		return runMonitor(args[1:], stdout, stderr)
	}
	if len(args) < 2 {
		usage(stderr)
		return 1
	}
	switch args[1] {
	case "version", "--version":
		fmt.Fprintln(stdout, version)
		return 0
	case "-h", "--help", "help":
		usage(stdout)
		return 0
	case "awg-cron":
		env, self, ok := setup(stdout, stderr)
		if !ok {
			return 1
		}
		return awgcron.Main(env, args[2:], self)
	case "awg-monitor":
		return runMonitor(args[2:], stdout, stderr)
	}
	usage(stderr)
	return 1
}

func setup(stdout, stderr io.Writer) (*sys.Env, string, bool) {
	self, err := selfbin.Self()
	if err != nil {
		fmt.Fprintf(stderr, "Ошибка: %v\n", err)
		return nil, "", false
	}
	env := sys.New()
	env.Stdout, env.Stderr = stdout, stderr
	return env, self, true
}

func runMonitor(args []string, stdout, stderr io.Writer) int {
	// Runs every minute next to the tunnel on a small router.
	debug.SetMemoryLimit(16 << 20)
	env, self, ok := setup(stdout, stderr)
	if !ok {
		return 1
	}
	return monitor.Main(env, args, self)
}
