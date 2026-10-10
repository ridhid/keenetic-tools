package monitor

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/ridhid/keenetic-tools/internal/conf"
	"github.com/ridhid/keenetic-tools/internal/crontab"
	"github.com/ridhid/keenetic-tools/internal/markers"
	"github.com/ridhid/keenetic-tools/internal/selfbin"
)

// Filesystems that live in the router's RAM or internal flash.
var badFS = map[string]bool{"tmpfs": true, "ramfs": true, "rootfs": true, "squashfs": true, "jffs2": true,
	"ubifs": true, "proc": true, "sysfs": true, "devtmpfs": true}

func (m *M) confOwned() bool { return m.env.Owned(Conf, markers.Monitor) }

func (m *M) cronOn() bool {
	data, err := m.env.ReadFile(crontab.Path)
	return err == nil && crontab.Has(data, markers.Monitor)
}

// restartJob reports whether the awg-cron job still needs the binary.
func (m *M) restartJob() bool {
	data, err := m.env.ReadFile(crontab.Path)
	return err == nil && crontab.Has(data, markers.Restart)
}

func (m *M) updateCrontab(line string) error {
	b, err := crontab.Update(m.env, crontab.Path, markers.Monitor, line, backupPrefix)
	if b != "" {
		m.say("Резервная копия расписания: %s", b)
	}
	return err
}

// lockWait waits for a running collect to finish and holds the lock.
func (m *M) lockWait() (func(), error) {
	for range 90 {
		l, err := m.env.TryLock(LockFile)
		if err != nil {
			return nil, err
		}
		if l != nil {
			return func() { l.Release() }, nil
		}
		m.env.Sleep(1e9)
	}
	return nil, fail("Сбор (collect) не завершается: занят %s.", LockFile)
}

func (m *M) install(args []string) error {
	dir := ""
	for len(args) > 0 {
		switch {
		case args[0] == "--log-dir":
			if len(args) < 2 {
				return fail("--log-dir требует путь.")
			}
			dir, args = args[1], args[2:]
		case strings.HasPrefix(args[0], "--log-dir="):
			dir, args = strings.TrimPrefix(args[0], "--log-dir="), args[1:]
		default:
			m.usage()
			return exitCode(1)
		}
	}
	if err := m.requireRoot(); err != nil {
		return err
	}
	if err := selfbin.Check(m.env); err != nil {
		return err
	}
	if err := selfbin.CheckLink(m.env, selfbin.MonitorLink); err != nil {
		return err
	}
	var cf *conf.File
	if m.env.Exists(Conf) {
		if !m.confOwned() {
			return fail("%s уже существует и не принадлежит awg-monitor. Файл не изменён.", Conf)
		}
		data, err := m.env.ReadFile(Conf)
		if err != nil {
			return err
		}
		if cf, err = conf.Parse(Conf, data); err != nil {
			return err
		}
	}
	oldDir := ""
	if cf != nil {
		oldDir, _ = cf.Get("LOG_DIR")
		if _, err := m.c.apply(cf); err != nil {
			return err
		}
	}
	if dir == "" {
		dir = dashText(oldDir, DefaultLogDir)
	}
	if dir != "/" {
		dir = strings.TrimSuffix(dir, "/")
	}
	if !strings.HasPrefix(dir, "/") || dir == "/" {
		return fail("Путь к логам должен быть абсолютным: '%s'.", dir)
	}
	if strings.ContainsAny(dir, "'\n") {
		return fail("Путь к логам не должен содержать кавычки и переводы строк.")
	}
	switch fst := m.env.FSType(dir); {
	case fst == "":
		return fail("Не удалось определить файловую систему для %s.", dir)
	case badFS[fst]:
		return fail("%s находится в памяти роутера (%s). Укажите каталог на внешнем диске, например /tmp/mnt/<диск>/awg-monitor.", dir, fst)
	}

	if err := crontab.Ensure(m.env); err != nil {
		return err
	}
	if !m.env.Have("awg") {
		m.say("Предупреждение: awg не найден — handshake и Endpoint собираться не будут.")
	}
	if !m.env.IsExecutable(m.c.Service) {
		m.say("Предупреждение: не найден %s — проверьте IFACE/SERVICE в %s.", m.c.Service, Conf)
	}

	if err := os.MkdirAll(m.env.P(dir), 0o755); err != nil {
		return err
	}
	if !m.env.IsFile(dir + "/" + DirMarker) {
		if err := m.env.WriteAtomic(dir+"/"+DirMarker, []byte(markers.Monitor+" — каталог логов awg-monitor\n"), 0o644); err != nil {
			return err
		}
	}
	if cf == nil {
		var err error
		if cf, err = conf.Parse(Conf, []byte(confText(dir))); err != nil {
			return err
		}
	} else {
		cf.Set("LOG_DIR", conf.Quote(dir))
	}
	if !cf.Has("DOMAINS_WATCH") {
		if err := cf.Append(confDomains()); err != nil {
			return err
		}
	}
	if err := m.env.WriteAtomic(Conf, cf.Bytes(), 0o644); err != nil {
		return err
	}
	if err := selfbin.Install(m.env, m.self); err != nil {
		return err
	}
	if err := selfbin.Link(m.env, selfbin.MonitorLink); err != nil {
		return err
	}
	if err := m.updateCrontab(cronLine); err != nil {
		return err
	}
	if err := crontab.Restart(m.env); err != nil {
		return err
	}

	m.say("Установлено: %s (%s), настройки %s", selfbin.MonitorLink, selfbin.Bin, Conf)
	m.say("Логи: %s", dir)
	if oldDir != "" && oldDir != dir {
		m.say("Прежний каталог %s не тронут.", oldDir)
	}
	m.say("Замер — каждую минуту. Первые данные: awg-monitor status (через минуту).")
	// The first snapshot is the baseline for config_changed.
	m.c = Defaults()
	if err := m.loadConf(); err == nil {
		if name, _, err := m.snapshotWrite("install"); err == nil {
			m.say("Снимок: %s", m.path("snapshots/"+name))
		}
	}
	return nil
}

func (m *M) uninstall(args []string) error {
	purge := false
	switch {
	case len(args) == 1 && args[0] == "--purge":
		purge = true
	case len(args) > 0:
		m.usage()
		return exitCode(1)
	}
	if err := m.requireRoot(); err != nil {
		return err
	}
	if err := m.loadConf(); err != nil {
		fmt.Fprintf(m.env.Stderr, "Предупреждение: %v — каталог логов неизвестен, логи не тронуты.\n", err)
		m.c = Defaults()
	}
	if m.env.IsFile(crontab.Path) {
		if err := m.updateCrontab(""); err != nil {
			return err
		}
		if err := crontab.Restart(m.env); err != nil {
			return err
		}
	}
	release, err := m.lockWait()
	if err != nil {
		return err
	}
	defer release()
	m.capClean()
	selfbin.Unlink(m.env, selfbin.MonitorLink)
	if m.confOwned() {
		m.env.Remove(Conf)
	}
	if m.env.Owned(SrcCopy, markers.Binary) {
		m.env.Remove(SrcCopy)
	}
	m.env.Remove(NoDirFlag)
	if purge {
		m.removeBackups()
	}
	if m.logdirOK() {
		if purge {
			for _, d := range []string{"samples", "domains", "missed", "dumps", "snapshots", "bundles"} {
				os.RemoveAll(m.env.P(m.path(d)))
			}
			for _, f := range []string{"events.log", "events.log.1", "state", "domains.state", "missed.cache", DirMarker} {
				m.env.Remove(m.path(f))
			}
			for _, pat := range []string{"*.tmp.*"} {
				matches, _ := filepath.Glob(m.env.P(m.path(pat)))
				for _, f := range matches {
					os.Remove(f)
				}
			}
			os.Remove(m.env.P(m.c.LogDir))
			m.say("Логи удалены: %s", m.c.LogDir)
		} else {
			m.say("Логи сохранены: %s (удалить: uninstall --purge)", m.c.LogDir)
		}
	}
	m.say("awg-monitor удалён. Другие задания cron сохранены.")
	if m.restartJob() {
		m.say("%s оставлен: он нужен заданию awg-cron.", selfbin.Bin)
	} else if m.env.Owned(selfbin.Bin, markers.Binary) {
		if err := m.env.Remove(selfbin.Bin); err != nil {
			return err
		}
		m.say("%s удалён.", selfbin.Bin)
	}
	// Nobody else runs collect now: the lock file can go.
	m.env.Remove(LockFile)
	if left := m.ramFiles(); len(left) > 0 {
		m.say("Осталось в /tmp: %s", strings.Join(left, " "))
	}
	if !purge {
		m.say("Резервные копии crontab (%s*) сохранены — удаляются с --purge.", backupPrefix)
	}
	if pk := m.pkgList(); pk != "" {
		m.say("Пакеты Entware не удалялись (могут быть нужны другим): %s. Удалить ненужные: opkg remove ИМЯ", pk)
	}
	m.say("Записи в системном журнале роутера (тег awg-monitor) остаются до его очистки/перезагрузки.")
	return nil
}

func (m *M) backups() []string {
	matches, _ := filepath.Glob(m.env.P(backupPrefix) + "*")
	var out []string
	for _, f := range matches {
		if fi, err := os.Lstat(f); err == nil && fi.Mode().IsRegular() {
			out = append(out, f)
		}
	}
	return out
}

func (m *M) removeBackups() {
	for _, f := range m.backups() {
		os.Remove(f)
	}
}

// ramFiles lists /tmp/awg-monitor* (router paths).
func (m *M) ramFiles() []string {
	matches, _ := filepath.Glob(m.env.P("/tmp/awg-monitor") + "*")
	for i := range matches {
		matches[i] = strings.TrimPrefix(matches[i], m.env.Root)
	}
	return matches
}

func (m *M) pkgList() string {
	var out []string
	for _, l := range lines(m.run(60e9, "opkg", "list-installed")) {
		f := strings.Fields(l)
		if len(f) > 0 && contains([]string{"cron", "curl", "ca-bundle", "tcpdump"}, f[0]) {
			out = append(out, f[0])
		}
	}
	return strings.Join(out, ", ")
}

// confSet changes one setting in the config, keeping everything else.
func (m *M) confSet(key, raw string) error {
	if !m.confOwned() {
		return fail("Нет %s — сначала установите: keenetic-tools awg-monitor install", Conf)
	}
	data, err := m.env.ReadFile(Conf)
	if err != nil {
		return err
	}
	f, err := conf.Parse(Conf, data)
	if err != nil {
		return err
	}
	f.Set(key, raw)
	return m.env.WriteAtomic(Conf, f.Bytes(), 0o644)
}

func (m *M) installed() bool {
	return selfbin.IsLink(m.env, selfbin.MonitorLink) && m.env.Owned(selfbin.Bin, markers.Binary)
}

func (m *M) enable(args []string) error {
	if len(args) != 1 {
		return fail("Укажите: awg-monitor enable collect|capture")
	}
	if err := m.requireRoot(); err != nil {
		return err
	}
	if !m.installed() {
		return fail("awg-monitor не установлен (%s). Установите: keenetic-tools awg-monitor install", selfbin.MonitorLink)
	}
	switch args[0] {
	case "collect":
		if !m.env.IsFile(crontab.Path) {
			return fail("Не найден %s.", crontab.Path)
		}
		if err := m.updateCrontab(cronLine); err != nil {
			return err
		}
		if err := crontab.Restart(m.env); err != nil {
			return err
		}
		m.say("Сбор по cron включён (раз в минуту).")
	case "capture":
		if !m.env.Have("tcpdump") {
			return fail("Нужен tcpdump: opkg install tcpdump")
		}
		if err := m.requireLogdir(); err != nil {
			return err
		}
		if err := m.confSet("DNS_CAPTURE", "1"); err != nil {
			return err
		}
		m.say("Поиск мимо туннеля включён: захват DNS запустится при следующем сборе (в течение минуты).")
		m.say("Результаты: awg-monitor missed. Выключить и удалить данные: awg-monitor disable capture --purge")
		if !m.cronOn() {
			m.say("Внимание: задание cron выключено — включите: awg-monitor enable collect")
		}
	default:
		return fail("Неизвестная функция '%s'. Доступно: collect, capture.", args[0])
	}
	return nil
}

func (m *M) disable(args []string) error {
	if len(args) < 1 || len(args) > 2 {
		return fail("Укажите: awg-monitor disable collect|capture [--purge]")
	}
	purge := false
	if len(args) == 2 {
		if args[1] != "--purge" {
			return fail("Неизвестный параметр '%s'.", args[1])
		}
		purge = true
	}
	if err := m.requireRoot(); err != nil {
		return err
	}
	switch args[0] {
	case "collect":
		if purge {
			return fail("Для collect нет --purge: удалить всё — awg-monitor uninstall --purge")
		}
		if m.env.IsFile(crontab.Path) {
			if err := m.updateCrontab(""); err != nil {
				return err
			}
			if err := crontab.Restart(m.env); err != nil {
				return err
			}
		}
		release, err := m.lockWait()
		if err != nil {
			return err
		}
		defer release()
		// Without collect nobody harvests or bounds the capture.
		m.capClean()
		m.say("Сбор по cron выключен, захват DNS остановлен. Данные на диске сохранены.")
		m.say("Включить снова: awg-monitor enable collect")
	case "capture":
		release, err := m.lockWait()
		if err != nil {
			return err
		}
		defer release()
		if m.confOwned() {
			if err := m.confSet("DNS_CAPTURE", "0"); err != nil {
				return err
			}
		}
		m.capClean()
		m.say("Поиск мимо туннеля выключен, tcpdump остановлен, данные в памяти удалены.")
		switch {
		case purge && m.logdirOK():
			os.RemoveAll(m.env.P(m.path("missed")))
			m.env.Remove(m.path("missed.cache"))
			m.say("Удалены собранные данные: %s, missed.cache", m.path("missed"))
		case purge:
			m.say("Каталог логов недоступен — данные на диске не удалены.")
		case m.logdirOK() && m.env.Exists(m.path("missed")):
			m.say("Собранные данные остались в %s (удалить: awg-monitor disable capture --purge)", m.path("missed"))
		}
		if strings.Contains(m.pkgList(), "tcpdump") {
			m.say("tcpdump больше не нужен монитору: opkg remove tcpdump")
		}
	default:
		return fail("Неизвестная функция '%s'. Доступно: collect, capture.", args[0])
	}
	return nil
}

// sizeOf is the total size of a file or directory, like `du -sh`.
func (m *M) sizeOf(path string) string {
	var total int64
	err := filepath.WalkDir(m.env.P(path), func(_ string, e fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if fi, err := e.Info(); err == nil && !fi.IsDir() {
			total += fi.Size()
		}
		return nil
	})
	if err != nil && errors.Is(err, fs.ErrNotExist) {
		return "-"
	}
	return human(total)
}

func human(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	f, suffix := float64(n), "KMGT"
	i := -1
	for f >= unit && i < len(suffix)-1 {
		f /= unit
		i++
	}
	if f < 10 {
		return fmt.Sprintf("%.1f%c", f, suffix[i])
	}
	return fmt.Sprintf("%.0f%c", f, suffix[i])
}

func row(m *M, k, v string) { m.say("  %s: %s", k, v) }

func (m *M) status() error {
	m.say("Установлено:")
	if m.env.Owned(selfbin.Bin, markers.Binary) {
		row(m, "программа", selfbin.Bin)
	} else {
		row(m, "программа", "нет")
	}
	if selfbin.IsLink(m.env, selfbin.MonitorLink) {
		row(m, "команда awg-monitor", selfbin.MonitorLink)
	} else {
		row(m, "команда awg-monitor", "нет")
	}
	if m.confOwned() {
		row(m, "настройки", Conf)
	} else {
		row(m, "настройки", "нет")
	}
	if m.env.Owned(SrcCopy, markers.Binary) {
		row(m, "копия установщика", SrcCopy+" (не нужна после установки)")
	}
	if m.cronOn() {
		row(m, "задание cron (раз в минуту)", "ВКЛЮЧЕНО")
	} else {
		row(m, "задание cron (раз в минуту)", "выключено")
	}
	if m.restartJob() {
		row(m, "перезапуск туннеля по cron (awg-cron)", "установлен")
	}
	if n := len(m.backups()); n > 0 {
		row(m, "резервные копии crontab", fmt.Sprintf("%d шт. (%s*)", n, backupPrefix))
	}

	m.say("")
	m.say("Запущено сейчас:")
	if pid := m.env.LockHolder(LockFile); pid != 0 {
		row(m, "сбор (collect)", fmt.Sprintf("выполняется, PID %d", pid))
	} else {
		row(m, "сбор (collect)", "не выполняется")
	}
	if pid := m.capPID(); pid != 0 {
		row(m, "захват DNS (tcpdump)", fmt.Sprintf("ЗАПУЩЕН, PID %d", pid))
	} else {
		row(m, "захват DNS (tcpdump)", "не запущен")
	}

	m.say("")
	m.say("Функции:")
	if m.cronOn() {
		row(m, "замеры туннеля", "включены")
	} else {
		row(m, "замеры туннеля", "выключены (нет задания cron)")
	}
	if n := len(m.c.DomainsWatch); n > 0 {
		row(m, "проверка DOMAINS_WATCH", fmt.Sprintf("%d доменов, раз в %d мин", n, m.c.DomainsEvery))
	} else {
		row(m, "проверка DOMAINS_WATCH", "выключена (список пуст)")
	}
	if m.c.DNSCapture {
		t := "ВКЛЮЧЁН"
		if !m.env.Have("tcpdump") {
			t += ", но tcpdump не установлен"
		}
		if !m.cronOn() {
			t += ", но задание cron выключено"
		}
		row(m, "поиск мимо туннеля (capture)", t)
	} else {
		row(m, "поиск мимо туннеля (capture)", "выключен")
	}

	m.say("")
	m.say("Данные:")
	switch {
	case m.logdirOK():
		row(m, "каталог логов", fmt.Sprintf("%s (%s)", m.c.LogDir, m.sizeOf(m.c.LogDir)))
		for _, d := range []string{"samples", "domains", "missed", "dumps", "snapshots", "bundles"} {
			if m.env.Exists(m.path(d)) {
				m.say("    %s (%s)", d, m.sizeOf(m.path(d)))
			}
		}
	case m.c.LogDir != "":
		row(m, "каталог логов", m.c.LogDir+" — недоступен (диск не подключён или удалён)")
	default:
		row(m, "каталог логов", "не задан")
	}
	ram := m.ramFiles()
	if len(ram) > 0 {
		row(m, "в памяти (/tmp)", "")
		for _, f := range ram {
			m.say("    %s (%s)", f, m.sizeOf(f))
		}
	} else {
		row(m, "в памяти (/tmp)", "ничего")
	}
	row(m, "пакеты Entware", dashText(m.pkgList(), "нет")+" (удаляются только вручную: opkg remove ...)")

	m.say("")
	m.say("Управление:")
	if !m.installed() && !m.confOwned() && !m.cronOn() && m.capPID() == 0 && len(ram) == 0 {
		m.say("  awg-monitor не установлен и ничего не запущено.")
		m.say("  Установить: keenetic-tools awg-monitor install --log-dir DIR")
		return nil
	}
	if m.c.DNSCapture || m.capPID() != 0 {
		m.say("  awg-monitor disable capture --purge   — остановить поиск мимо туннеля и удалить его данные")
	} else {
		m.say("  awg-monitor enable capture            — включить поиск мимо туннеля (нужен tcpdump)")
	}
	if m.cronOn() {
		m.say("  awg-monitor disable collect           — выключить весь сбор (cron), данные остаются")
	} else if m.installed() {
		m.say("  awg-monitor enable collect            — включить сбор по cron")
	}
	m.say("  awg-monitor uninstall --purge         — удалить всё и все следы")

	if !m.logdirOK() {
		return nil
	}
	m.say("")
	m.say("=== Туннель ===")
	if st := m.readState(); st.State != "" {
		since := st.Since
		if ts, err := parseUnix(st.Since); err == nil {
			since = ts
		}
		m.say("Текущее состояние: %s (причина: %s), с %s", st.State, st.Cause, since)
	}
	if files := m.logFiles("samples"); len(files) > 0 {
		data, _ := os.ReadFile(files[len(files)-1])
		if l := lines(string(data)); len(l) > 0 {
			f := strings.Fields(l[len(l)-1])
			m.say("")
			m.say("Последний замер:")
			if len(f) >= 2 {
				m.say("  %s %s", f[0], f[1])
				for _, kv := range f[2:] {
					m.say("  %s", kv)
				}
			}
		}
	}
	if data, err := m.env.ReadFile(m.path("events.log")); err == nil {
		m.say("")
		m.say("Последние события:")
		for _, l := range lines(tail(string(data), 10)) {
			m.say("  %s", l)
		}
	}
	return nil
}
