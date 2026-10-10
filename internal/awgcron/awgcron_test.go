package awgcron

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ridhid/keenetic-tools/internal/markers"
	"github.com/ridhid/keenetic-tools/internal/selfbin"
	"github.com/ridhid/keenetic-tools/internal/sys"
	"github.com/ridhid/keenetic-tools/internal/sys/systest"
)

const (
	hourly = "01 * * * * root /opt/bin/keenetic-tools awg-cron run " + markers.Restart
	daily  = "02 4 * * * root /opt/bin/keenetic-tools awg-cron run " + markers.Restart
	other  = "*/10 * * * * root /opt/bin/something\n"
)

type router struct {
	*systest.Router
	self string
}

// newRouter is Entware with the tunnel service and cron installed.
func newRouter(t *testing.T) *router {
	r := &router{Router: systest.New(t)}
	r.Write(Service, "#!/bin/sh\n", 0o755)
	r.Write(CronInit, "#!/bin/sh\n", 0o755)
	r.Write(Crontab, other, 0o600)
	r.Runner.On(CronInit+" restart", "", 0)
	r.self = filepath.Join(t.TempDir(), "keenetic-tools.part")
	if err := os.WriteFile(r.self, []byte("\x7fELF"+markers.Embedded), 0o755); err != nil {
		t.Fatal(err)
	}
	return r
}

func (r *router) main(args ...string) int {
	r.Out.Reset()
	r.Err.Reset()
	return Main(r.Env, args, r.self)
}

func (r *router) mustRun(args ...string) {
	r.T.Helper()
	if code := r.main(args...); code != 0 {
		r.T.Fatalf("%v: exit %d\nstdout: %s\nstderr: %s", args, code, r.Out, r.Err)
	}
}

func (r *router) mustFail(want string, args ...string) {
	r.T.Helper()
	if code := r.main(args...); code == 0 {
		r.T.Fatalf("%v succeeded, want failure", args)
	}
	if !strings.Contains(r.Err.String(), want) {
		r.T.Fatalf("%v stderr = %q, want %q", args, r.Err, want)
	}
}

func (r *router) backups() int { return len(r.Glob(Crontab + ".awg-backup.*")) }

func TestInstallFresh(t *testing.T) {
	r := newRouter(t)
	r.mustRun("install")
	if got := r.Read(Crontab); got != other+hourly+"\n" {
		t.Fatalf("crontab = %q", got)
	}
	if !r.Owned(selfbin.Bin, markers.Binary) {
		t.Fatal("binary not installed")
	}
	if !r.Runner.Ran(CronInit + " restart") {
		t.Fatal("cron not restarted")
	}
	if r.Runner.Ran(Service + " restart") {
		t.Fatal("tunnel restarted during install")
	}
	if !strings.Contains(r.Out.String(), "Установлено: 01 * * * *") || r.backups() != 1 {
		t.Fatalf("stdout %q, backups %d", r.Out, r.backups())
	}
}

func TestInstallIdempotentAndSwitchSchedule(t *testing.T) {
	r := newRouter(t)
	r.mustRun("install")
	r.mustRun("install", "hourly")
	if r.backups() != 1 {
		t.Fatalf("repeat install made a backup: %d", r.backups())
	}
	r.mustRun("install", "daily")
	if got := r.Read(Crontab); got != other+daily+"\n" {
		t.Fatalf("crontab = %q", got)
	}
}

func TestInstallsCronPackage(t *testing.T) {
	r := newRouter(t)
	os.Remove(r.P(CronInit))
	os.Remove(r.P(Crontab))
	r.Runner.On("opkg update", "Downloading...\n", 0)
	r.Runner.OnFunc("opkg install cron", func(sys.Cmd) sys.Result {
		r.Write(CronInit, "#!/bin/sh\n", 0o755)
		r.Write(Crontab, "", 0o600)
		return systest.Exit("Installing cron\n", 0)
	})
	r.mustRun("install")
	if !r.Runner.Ran("opkg update") || r.Read(Crontab) != hourly+"\n" {
		t.Fatalf("calls %v, crontab %q", r.Runner.Calls(), r.Read(Crontab))
	}
	if !strings.Contains(r.Out.String(), "Installing cron") {
		t.Fatal("opkg output not shown")
	}
}

func TestInstallCronPackageFails(t *testing.T) {
	r := newRouter(t)
	os.Remove(r.P(CronInit))
	r.Runner.On("opkg update", "", 0)
	r.Runner.On("opkg install cron", "", 255)
	r.mustFail("opkg install cron", "install")
}

func TestRefusals(t *testing.T) {
	cases := []struct {
		name  string
		setup func(r *router)
		want  string
	}{
		{"no service", func(r *router) { os.Remove(r.P(Service)) }, "Не найден исполняемый " + Service},
		{"not root", func(r *router) { r.Euid = func() int { return 1000 } }, "под root"},
		{"foreign binary", func(r *router) { r.Write(selfbin.Bin, "#!/bin/sh\n", 0o755) }, "не принадлежит keenetic-tools"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newRouter(t)
			c.setup(r)
			r.mustFail(c.want, "install")
			if r.Read(Crontab) != other || r.backups() != 0 {
				t.Fatalf("crontab changed: %q", r.Read(Crontab))
			}
			if r.Runner.Ran(CronInit + " restart") {
				t.Fatal("cron restarted")
			}
		})
	}
}

func TestArgs(t *testing.T) {
	r := newRouter(t)
	if code := r.main("install", "weekly"); code != 1 || !strings.Contains(r.Out.String(), "Использование") {
		t.Fatalf("install weekly: %d %q", code, r.Out)
	}
	r.mustFail("Слишком много", "install", "daily", "x")
	r.mustFail("uninstall не принимает", "uninstall", "x")
	if code := r.main("bogus"); code != 1 {
		t.Fatalf("bogus: %d", code)
	}
}

func TestUninstall(t *testing.T) {
	r := newRouter(t)
	r.mustRun("install")
	r.mustRun("uninstall")
	if r.Read(Crontab) != other {
		t.Fatalf("crontab = %q", r.Read(Crontab))
	}
	if r.Exists(selfbin.Bin) {
		t.Fatal("binary left although awg-monitor is not installed")
	}
	r.mustRun("uninstall") // nothing left: still fine
}

func TestUninstallKeepsBinaryForMonitor(t *testing.T) {
	r := newRouter(t)
	r.mustRun("install")
	r.Write(Crontab, r.Read(Crontab)+"* * * * * root /opt/bin/awg-monitor collect >/dev/null 2>&1 "+markers.Monitor+"\n", 0o600)
	r.mustRun("uninstall")
	if !r.Exists(selfbin.Bin) || !strings.Contains(r.Out.String(), "нужен awg-monitor") {
		t.Fatalf("binary removed while awg-monitor uses it: %q", r.Out)
	}
	if !strings.Contains(r.Read(Crontab), markers.Monitor) {
		t.Fatal("monitor line removed")
	}
}

// ---------- run ----------

func TestRunSuccessAndFailure(t *testing.T) {
	for _, code := range []int{0, 3} {
		r := newRouter(t)
		r.Runner.On(Service+" restart", "Stopping...\nStarting...\n", code)
		if got := r.main("run"); got != code {
			t.Fatalf("exit %d, want %d", got, code)
		}
		want := "2026-10-10 12:00:00 " + systest.Epoch.Format("MST") + "\nStopping...\nStarting...\nrestart exit code: " + strconv.Itoa(code) + "\n"
		if got := r.Read(Log); got != want {
			t.Fatalf("log = %q, want %q", got, want)
		}
		if r.LockHolder(LockFile) != 0 {
			t.Fatal("lock still held")
		}
	}
}

func TestRunTruncatesPreviousLog(t *testing.T) {
	r := newRouter(t)
	r.Write(Log, "old run\n", 0o644)
	r.Runner.On(Service+" restart", "", 0)
	r.main("run")
	if strings.Contains(r.Read(Log), "old run") {
		t.Fatal("previous log kept")
	}
}

func TestRunSkipsWhileRunning(t *testing.T) {
	r := newRouter(t)
	r.Write(Log, "running\n", 0o644)
	held, err := r.TryLock(LockFile)
	if held == nil || err != nil {
		t.Fatal(err)
	}
	defer held.Release()
	if code := r.main("run"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if r.Runner.Ran(Service + " restart") {
		t.Fatal("restarted while a run is active")
	}
	if !strings.HasSuffix(r.Read(Log), "skipped: previous run (PID "+strconv.Itoa(os.Getpid())+") is still active\n") ||
		!strings.HasPrefix(r.Read(Log), "running\n") {
		t.Fatalf("log = %q", r.Read(Log))
	}
}

func TestRunLeftoverLockFile(t *testing.T) {
	r := newRouter(t)
	r.Write(LockFile, "999999999\n", 0o644)
	r.Runner.On(Service+" restart", "", 0)
	r.main("run")
	if !r.Runner.Ran(Service + " restart") {
		t.Fatal("a lock file nobody holds blocked the run")
	}
}

func TestRunServiceMissing(t *testing.T) {
	r := newRouter(t)
	if code := r.main("run"); code != 127 {
		t.Fatalf("exit %d, want 127", code)
	}
	if !strings.Contains(r.Read(Log), "restart exit code: 127") {
		t.Fatalf("log = %q", r.Read(Log))
	}
}

// Real processes: the timeout kills only the init script, and a daemon it
// leaves running with the log open does not hold the job.
func TestRunRealProcesses(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("no /bin/sh")
	}
	old := Timeout
	Timeout = 300 * time.Millisecond
	defer func() { Timeout = old }()

	t.Run("timeout", func(t *testing.T) {
		r := newRouter(t)
		r.Run = sys.ExecRunner{}
		r.Write(Service, "#!/bin/sh\necho stopping\nsleep 5\n", 0o755)
		start := time.Now()
		if code := r.main("run"); code != 137 {
			t.Fatalf("exit %d, want 137; log %q", code, r.Read(Log))
		}
		if time.Since(start) > 3*time.Second {
			t.Fatalf("took %s", time.Since(start))
		}
		want := "stopping\nrestart timed out after 0s and was killed\nrestart exit code: 137\n"
		if !strings.HasSuffix(r.Read(Log), want) {
			t.Fatalf("log = %q", r.Read(Log))
		}
	})
	t.Run("daemon keeps log open", func(t *testing.T) {
		r := newRouter(t)
		r.Run = sys.ExecRunner{}
		r.Write(Service, "#!/bin/sh\nsleep 3 &\necho started\n", 0o755)
		start := time.Now()
		if code := r.main("run"); code != 0 {
			t.Fatalf("exit %d; log %q", code, r.Read(Log))
		}
		if time.Since(start) > 2*time.Second {
			t.Fatalf("waited for the daemon: %s", time.Since(start))
		}
	})
}
