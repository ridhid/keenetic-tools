// Package awgcron is a cron job that restarts the AmneziaWG tunnel through
// its init script, with a timeout and one run at a time.
package awgcron

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/ridhid/keenetic-tools/internal/crontab"
	"github.com/ridhid/keenetic-tools/internal/markers"
	"github.com/ridhid/keenetic-tools/internal/selfbin"
	"github.com/ridhid/keenetic-tools/internal/sys"
)

const (
	Crontab  = crontab.Path
	CronInit = crontab.Init
	Service  = "/opt/etc/init.d/S52awg-opkgtun0"
	Log      = "/tmp/awg-restart.log"
	LockFile = "/tmp/awg-restart.lock"
	backup   = Crontab + ".awg-backup."
	// MonitorConf tells whether awg-monitor still needs the binary.
	MonitorConf = "/opt/etc/awg-monitor.conf"
)

// Timeout bounds one restart; a variable so tests can shorten it.
var Timeout = 120 * time.Second

var schedules = map[string]string{"hourly": "01 * * * *", "daily": "02 4 * * *"}

// Error is a user-facing failure; Main prints it as "Ошибка: ...".
type Error string

func (e Error) Error() string { return string(e) }

func fail(format string, a ...any) error { return Error(fmt.Sprintf(format, a...)) }

func say(env *sys.Env, format string, a ...any) { fmt.Fprintf(env.Stdout, format+"\n", a...) }

func usage(env *sys.Env) {
	fmt.Fprint(env.Stdout, `Использование: keenetic-tools awg-cron install [hourly|daily] | uninstall
  install         — каждый час в :01 (по умолчанию)
  install daily   — раз в сутки в 04:02 по времени cron
  uninstall       — удалить задание; оставить пакет cron
`)
}

type exitCode int

func (c exitCode) Error() string { return "exit " + strconv.Itoa(int(c)) }

// Main runs `awg-cron ARGS...` and returns the exit code.
func Main(env *sys.Env, args []string, self string) int {
	err := dispatch(env, args, self)
	var code exitCode
	switch {
	case err == nil:
		return 0
	case errors.As(err, &code):
		return int(code)
	}
	fmt.Fprintf(env.Stderr, "Ошибка: %v\n", err)
	return 1
}

func dispatch(env *sys.Env, args []string, self string) error {
	action := "install"
	if len(args) > 0 {
		action = args[0]
	}
	switch action {
	case "-h", "--help", "help":
		usage(env)
		return nil
	case "install":
		if len(args) > 2 {
			return fail("Слишком много аргументов.")
		}
		freq := "hourly"
		if len(args) == 2 {
			freq = args[1]
		}
		sched, ok := schedules[freq]
		if !ok {
			usage(env)
			return exitCode(1)
		}
		return Install(env, sched, self)
	case "uninstall":
		if len(args) != 1 {
			return fail("uninstall не принимает дополнительных аргументов.")
		}
		return Uninstall(env)
	case "run":
		if len(args) != 1 {
			return fail("run не принимает аргументов.")
		}
		if code := Run(env); code != 0 {
			return exitCode(code)
		}
		return nil
	}
	usage(env)
	return exitCode(1)
}

// cronLine is the crontab entry that runs the job.
func cronLine(schedule string) string {
	return schedule + " root " + selfbin.Bin + " awg-cron run " + markers.Restart
}

// Install sets up the job: binary in /opt/bin, crontab line, cron running.
// The tunnel itself is not restarted.
func Install(env *sys.Env, schedule, self string) error {
	if !env.IsRoot() {
		return fail("Запустите под root в shell Entware.")
	}
	if !env.IsExecutable(Service) {
		return fail("Не найден исполняемый %s. Проверьте имя и регистр букв.", Service)
	}
	if err := selfbin.Check(env); err != nil {
		return err
	}
	if err := crontab.Ensure(env); err != nil {
		return err
	}
	if err := selfbin.Install(env, self); err != nil {
		return err
	}
	b, err := crontab.Update(env, Crontab, markers.Restart, cronLine(schedule), backup)
	if err != nil {
		return err
	}
	if b != "" {
		say(env, "Резервная копия расписания: %s", b)
	}
	if err := crontab.Restart(env); err != nil {
		return err
	}
	say(env, "Установлено: %s — %s restart", schedule, Service)
	say(env, "Лог последнего запуска: %s", Log)
	say(env, "Туннель сейчас не перезапускался. Первый запуск — по расписанию.")
	return nil
}

// Uninstall removes the crontab line; the binary goes too unless
// awg-monitor still uses it.
func Uninstall(env *sys.Env) error {
	if !env.IsRoot() {
		return fail("Запустите под root в shell Entware.")
	}
	if env.IsFile(Crontab) {
		b, err := crontab.Update(env, Crontab, markers.Restart, "", backup)
		if err != nil {
			return err
		}
		if b != "" {
			say(env, "Резервная копия расписания: %s", b)
		}
	}
	if err := crontab.Restart(env); err != nil {
		return err
	}
	say(env, "Задание удалено. Пакет cron и другие задания сохранены.")
	if monitorInstalled(env) {
		say(env, "%s оставлен: он нужен awg-monitor.", selfbin.Bin)
	} else if env.Owned(selfbin.Bin, markers.Binary) {
		if err := env.Remove(selfbin.Bin); err != nil {
			return err
		}
		say(env, "%s удалён.", selfbin.Bin)
	}
	return nil
}

func monitorInstalled(env *sys.Env) bool {
	if selfbin.IsLink(env, selfbin.MonitorLink) || env.Owned(MonitorConf, markers.Monitor) {
		return true
	}
	data, err := env.ReadFile(Crontab)
	return err == nil && crontab.Has(data, markers.Monitor)
}

func stamp(t time.Time) string { return t.Format("2006-01-02 15:04:05 MST") }

// Run is the cron job: restart the tunnel with a timeout, one run at a
// time, output of the latest run in Log. It returns the exit code.
func Run(env *sys.Env) int {
	lock, err := env.TryLock(LockFile)
	if err != nil {
		fmt.Fprintf(env.Stderr, "%s: %v\n", LockFile, err)
		return 1
	}
	if lock == nil {
		appendLog(env, fmt.Sprintf("%s skipped: previous run (PID %d) is still active\n", stamp(env.Now()), env.LockHolder(LockFile)))
		return 0
	}
	defer lock.Release()

	// Keep only the latest run's output in RAM; append mode so a skip note survives.
	log, err := os.OpenFile(env.P(Log), os.O_WRONLY|os.O_CREATE|os.O_TRUNC|os.O_APPEND, 0o644)
	if err != nil {
		fmt.Fprintf(env.Stderr, "%s: %v\n", Log, err)
		return 1
	}
	defer log.Close()
	fmt.Fprintln(log, stamp(env.Now()))
	// The log is a file, not a pipe: a daemon the script leaves running
	// keeps it open without holding us.
	res := env.Run.Run(context.Background(), sys.Cmd{
		Name: env.P(Service), Args: []string{"restart"}, Stdout: log, Stderr: log, Timeout: Timeout,
	})
	status := res.Code
	if status < 0 {
		// Did not start: report like a shell would.
		fmt.Fprintf(log, "%s: %v\n", Service, res.Err)
		status = 127
	}
	if status == 137 {
		fmt.Fprintf(log, "restart timed out after %ds and was killed\n", int(Timeout.Seconds()))
	}
	fmt.Fprintf(log, "restart exit code: %d\n", status)
	return status
}

func appendLog(env *sys.Env, line string) {
	f, err := os.OpenFile(env.P(Log), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	f.WriteString(line)
}
