// Package monitor is awg-monitor: a once-a-minute probe of the AmneziaWG
// tunnel with logs on an external disk, failure dumps, config snapshots,
// domain checks and a search for flows that bypass the tunnel.
package monitor

import (
	"errors"
	"fmt"
	"io/fs"
	"strconv"
	"strings"

	"github.com/asiforis/keenetic-tools/internal/conf"
	"github.com/asiforis/keenetic-tools/internal/crontab"
	"github.com/asiforis/keenetic-tools/internal/markers"
	"github.com/asiforis/keenetic-tools/internal/selfbin"
	"github.com/asiforis/keenetic-tools/internal/sys"
)

const (
	Conf      = "/opt/etc/awg-monitor.conf"
	LockFile  = "/tmp/awg-monitor.lock"
	NoDirFlag = "/tmp/awg-monitor.nodir"
	// DNS capture for "missed" (RAM only).
	CapLog   = "/tmp/awg-monitor.dns.log"
	CapPID   = "/tmp/awg-monitor.dns.pid"
	DNSMap   = "/tmp/awg-monitor.dnsmap"
	CTSeen   = "/tmp/awg-monitor.ctseen"
	Includes = "/tmp/awg-monitor.includes"
	capMax   = 4 << 20
	// DirMarker in the log directory proves the disk is mounted.
	DirMarker     = ".awg-monitor"
	DefaultLogDir = "/opt/var/log/awg-monitor"
	eventsMax     = 1 << 20
	// SrcCopy is where the README one-liner downloads the binary.
	SrcCopy      = "/opt/keenetic-tools.part"
	backupPrefix = crontab.Path + ".awg-monitor-backup."
	cronLine     = "* * * * * root " + selfbin.MonitorLink + " collect >/dev/null 2>&1 " + markers.Monitor
)

// Public DoH resolvers: HTTPS to them is a device's own DNS, not a site.
var dohIPs = strings.Fields("8.8.8.8 8.8.4.4 1.1.1.1 1.0.0.1 9.9.9.9 149.112.112.112 94.140.14.14 94.140.15.15 208.67.222.222 208.67.220.220 77.88.8.8 77.88.8.1 76.76.2.0 76.76.10.0")

// M is one invocation of awg-monitor.
type M struct {
	env  *sys.Env
	c    Config
	net  Net
	log  func(msg string) // system log
	self string
}

// New returns a monitor for the real system; self is the running binary.
func New(env *sys.Env, self string) *M {
	return &M{env: env, c: Defaults(), net: realNet{}, log: syslogger(env), self: self}
}

// Error is a user-facing failure; Main prints it as "Ошибка: ...".
type Error string

func (e Error) Error() string { return string(e) }

func fail(format string, a ...any) error { return Error(fmt.Sprintf(format, a...)) }

type exitCode int

func (c exitCode) Error() string { return "exit " + strconv.Itoa(int(c)) }

func (m *M) say(format string, a ...any) { fmt.Fprintf(m.env.Stdout, format+"\n", a...) }

func (m *M) usage() {
	fmt.Fprint(m.env.Stdout, `Использование:
  keenetic-tools awg-monitor install [--log-dir DIR] — установить или обновить
  awg-monitor uninstall [--purge]   — удалить; с --purge — все следы: логи, копии crontab
  awg-monitor status                — что установлено, что запущено, сколько данных; состояние туннеля
  awg-monitor enable collect|capture        — включить сбор по cron / поиск доменов мимо туннеля
  awg-monitor disable collect|capture [--purge] — выключить и остановить; --purge удаляет данные функции
  awg-monitor check                 — замер прямо сейчас, ничего не записывает
  awg-monitor report [24h|7d]       — сводка за период (по умолчанию 24h)
  awg-monitor snapshot [метка]      — снять конфигурацию
  awg-monitor snapshots             — список снимков
  awg-monitor diff [A] [B]          — разница снимков (по умолчанию два последних)
  awg-monitor bundle [дней]         — архив логов для разбора (по умолчанию 3 дня)
  awg-monitor check-domain ДОМЕН... — проверить домены напрямую и через туннель
  awg-monitor domains [24h|7d]      — сводка проверок доменов из DOMAINS_WATCH
  awg-monitor route-check ДОМЕН     — почему домен идёт (или не идёт) через туннель
  awg-monitor missed [24h|7d]       — домены и соединения мимо туннеля без ответа (DNS_CAPTURE=1)
  awg-monitor collect               — один замер (его раз в минуту запускает cron)

Каталог логов по умолчанию — из конфига, иначе /opt/var/log/awg-monitor.
Настройки: /opt/etc/awg-monitor.conf
`)
}

// Main runs `awg-monitor ARGS...` and returns the exit code.
func Main(env *sys.Env, args []string, self string) int {
	return New(env, self).Main(args)
}

// Main runs one command and returns the exit code.
func (m *M) Main(args []string) int {
	err := m.dispatch(args)
	var code exitCode
	switch {
	case err == nil:
		return 0
	case errors.As(err, &code):
		return int(code)
	}
	fmt.Fprintf(m.env.Stderr, "Ошибка: %v\n", err)
	return 1
}

func maxArgs(args []string, n int, cmd string) error {
	if len(args) > n {
		if n == 0 {
			return fail("%s не принимает аргументов.", cmd)
		}
		return fail("%s принимает один аргумент.", cmd)
	}
	return nil
}

func arg(args []string, i int) string {
	if i < len(args) {
		return args[i]
	}
	return ""
}

func (m *M) dispatch(args []string) error {
	action := arg(args, 0)
	if len(args) > 0 {
		args = args[1:]
	}
	switch action {
	case "", "-h", "--help", "help":
		m.usage()
		return nil
	case "install":
		return m.install(args)
	case "uninstall":
		return m.uninstall(args)
	}
	cmds := map[string]func([]string) error{
		"enable":       m.enable,
		"disable":      m.disable,
		"collect":      func([]string) error { return m.collect() },
		"check":        func([]string) error { return m.check() },
		"status":       func([]string) error { return m.status() },
		"report":       func(a []string) error { return m.withMax(a, 1, action, m.report) },
		"snapshot":     func(a []string) error { return m.withMax(a, 1, action, m.snapshot) },
		"snapshots":    func(a []string) error { return m.snapshots() },
		"diff":         m.diff,
		"bundle":       func(a []string) error { return m.withMax(a, 1, action, m.bundle) },
		"check-domain": m.checkDomain,
		"route-check":  m.routeCheck,
		"missed":       func(a []string) error { return m.withMax(a, 1, action, m.missed) },
		"domains":      func(a []string) error { return m.withMax(a, 1, action, m.domains) },
	}
	cmd, ok := cmds[action]
	if !ok {
		m.usage()
		return exitCode(1)
	}
	if err := m.loadConf(); err != nil {
		if action == "collect" {
			m.log(err.Error())
		}
		return err
	}
	return cmd(args)
}

func (m *M) withMax(args []string, n int, cmd string, f func(string) error) error {
	if err := maxArgs(args, n, cmd); err != nil {
		return err
	}
	return f(arg(args, 0))
}

// loadConf applies Conf over the defaults; a missing file keeps them.
func (m *M) loadConf() error {
	data, err := m.env.ReadFile(Conf)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	f, err := conf.Parse(Conf, data)
	if err != nil {
		return err
	}
	unknown, err := m.c.apply(f)
	if err != nil {
		return err
	}
	for _, k := range unknown {
		fmt.Fprintf(m.env.Stderr, "Предупреждение: %s: неизвестная настройка %s\n", Conf, k)
	}
	return nil
}

func (m *M) logdirOK() bool {
	return m.c.LogDir != "" && m.env.IsFile(m.c.LogDir+"/"+DirMarker)
}

func (m *M) requireLogdir() error {
	if m.logdirOK() {
		return nil
	}
	dir := m.c.LogDir
	if dir == "" {
		dir = "не задан"
	}
	return fail("Каталог логов '%s' недоступен (нет маркера %s). Диск подключён? Переустановите: keenetic-tools awg-monitor install --log-dir DIR", dir, DirMarker)
}

// path joins the log directory with a relative path.
func (m *M) path(rel string) string { return m.c.LogDir + "/" + rel }

func (m *M) requireRoot() error {
	if !m.env.IsRoot() {
		return fail("Запустите под root в shell Entware.")
	}
	return nil
}
