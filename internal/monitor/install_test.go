package monitor

import (
	"os"
	"strings"
	"testing"

	"github.com/ridhid/keenetic-tools/internal/markers"
	"github.com/ridhid/keenetic-tools/internal/selfbin"
)

// fresh is a router without awg-monitor.
func fresh(t *testing.T) *router {
	r := newRouter(t)
	os.Remove(r.P(Conf))
	os.RemoveAll(r.P(logDir))
	return r
}

func TestInstallFresh(t *testing.T) {
	r := fresh(t)
	out := r.mustRun("install", "--log-dir", logDir+"/")
	contain(t, out, "Установлено: /opt/bin/awg-monitor", "Логи: "+logDir+"\n", "Резервная копия расписания: /opt/etc/crontab.awg-monitor-backup.", "Снимок: ")
	if !r.Owned(selfbin.Bin, markers.Binary) || !selfbin.IsLink(r.Env, selfbin.MonitorLink) {
		t.Fatal("binary or link missing")
	}
	if got := r.Read("/opt/etc/crontab"); got != "SHELL=/bin/sh\n"+cronLine+"\n" {
		t.Fatalf("crontab %q", got)
	}
	if !r.Runner.Ran("/opt/etc/init.d/S10cron restart") {
		t.Fatal("cron not restarted")
	}
	c := r.Read(Conf)
	contain(t, c, markers.Monitor+"\n", "LOG_DIR='"+logDir+"'", "PING_TARGETS='1.1.1.1 8.8.8.8'", "DOMAINS_WATCH=''", "DNS_CAPTURE=0")
	if !r.IsFile(logDir+"/"+DirMarker) || len(r.Glob(logDir+"/snapshots/*-install.txt")) != 1 {
		t.Fatal("log dir not prepared")
	}
	// The installed config is valid and complete.
	r.mustRun("status")
	contain(t, r.Out.String(), "задание cron (раз в минуту): ВКЛЮЧЕНО", "команда awg-monitor: /opt/bin/awg-monitor", "=== Туннель ===")
	if r.Err.Len() != 0 {
		t.Fatalf("stderr %q", r.Err)
	}

	// Again: no duplicates, no new backup; a new log dir keeps the old one.
	r.Write("/tmp/mnt/HDD/other/.keep", "", 0o644)
	r.Write(Conf, r.Read(Conf)+"KEEP_DAYS=30\n", 0o644)
	out = r.mustRun("install", "--log-dir=/tmp/mnt/HDD/other")
	contain(t, out, "Прежний каталог "+logDir+" не тронут.")
	lack(t, out, "Резервная копия")
	if strings.Count(r.Read("/opt/etc/crontab"), cronLine) != 1 {
		t.Fatal(r.Read("/opt/etc/crontab"))
	}
	contain(t, r.Read(Conf), "LOG_DIR='/tmp/mnt/HDD/other'", "KEEP_DAYS=30")
	if strings.Count(r.Read(Conf), "LOG_DIR=") != 1 {
		t.Fatal(r.Read(Conf))
	}
	// Without --log-dir the configured one stays.
	contain(t, r.mustRun("install"), "Логи: /tmp/mnt/HDD/other\n")
}

func TestInstallRefusals(t *testing.T) {
	cases := []struct {
		name  string
		setup func(r *router)
		args  []string
		want  string
	}{
		{"tmpfs", nil, []string{"--log-dir", "/tmp/awg"}, "в памяти роутера (tmpfs)"},
		{"relative", nil, []string{"--log-dir", "logs"}, "абсолютным"},
		{"quote", nil, []string{"--log-dir", "/opt/it's"}, "кавычки"},
		{"foreign conf", func(r *router) { r.Write(Conf, "LOG_DIR=/x\n", 0o644) }, nil, "не принадлежит awg-monitor"},
		{"foreign link", func(r *router) { r.Write(selfbin.MonitorLink, "#!/bin/sh\n", 0o755) }, nil, "/opt/bin/awg-monitor уже существует"},
		{"foreign binary", func(r *router) { r.Write(selfbin.Bin, "#!/bin/sh\n", 0o755) }, nil, "/opt/bin/keenetic-tools уже существует"},
		{"not root", func(r *router) { r.Euid = func() int { return 1000 } }, nil, "под root"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := fresh(t)
			if c.setup != nil {
				c.setup(r)
			}
			r.mustFail(c.want, append([]string{"install"}, c.args...)...)
			if r.Read("/opt/etc/crontab") != "SHELL=/bin/sh\n" || r.Runner.Ran("/opt/etc/init.d/S10cron restart") {
				t.Fatal("changed something")
			}
		})
	}
}

func TestUninstall(t *testing.T) {
	r := fresh(t)
	r.mustRun("install", "--log-dir", logDir)
	r.mustRun("collect")
	r.Write(SrcCopy, "\x7fELF"+markers.Embedded, 0o755)
	out := r.mustRun("uninstall")
	contain(t, out, "Логи сохранены: "+logDir, "/opt/bin/keenetic-tools удалён.", "Пакеты Entware не удалялись (могут быть нужны другим): cron, curl, tcpdump")
	lack(t, out, "Осталось в /tmp")
	for _, p := range []string{selfbin.Bin, selfbin.MonitorLink, Conf, SrcCopy, LockFile} {
		if r.Exists(p) {
			t.Errorf("%s left", p)
		}
	}
	if r.Read("/opt/etc/crontab") != "SHELL=/bin/sh\n" || !r.Exists(logDir+"/state") {
		t.Fatal("crontab or logs wrong")
	}
	if len(r.Glob("/opt/etc/crontab.awg-monitor-backup.*")) != 2 {
		t.Fatal("backups removed without --purge")
	}
}

func TestUninstallPurgeKeepsBinaryForAwgCron(t *testing.T) {
	r := fresh(t)
	r.mustRun("install", "--log-dir", logDir)
	r.mustRun("collect")
	r.Write("/opt/etc/crontab", r.Read("/opt/etc/crontab")+"01 * * * * root /opt/bin/keenetic-tools awg-cron run "+markers.Restart+"\n", 0o600)
	out := r.mustRun("uninstall", "--purge")
	contain(t, out, "Логи удалены", "нужен заданию awg-cron")
	if !r.Exists(selfbin.Bin) || r.Exists(logDir) || len(r.Glob("/opt/etc/crontab.awg-monitor-backup.*")) != 0 {
		t.Fatal("purge wrong")
	}
	if !strings.Contains(r.Read("/opt/etc/crontab"), markers.Restart) {
		t.Fatal("awg-cron line removed")
	}
	if code := r.main("uninstall", "--all"); code != 1 || !strings.Contains(r.Out.String(), "Использование") {
		t.Fatalf("uninstall --all: %d %q", code, r.Out)
	}
}

func TestEnableDisable(t *testing.T) {
	r := fresh(t)
	r.mustFail("не установлен", "enable", "collect")
	r.mustRun("install", "--log-dir", logDir)
	r.mustRun("disable", "collect")
	if strings.Contains(r.Read("/opt/etc/crontab"), cronLine) {
		t.Fatal("still in crontab")
	}
	contain(t, r.mustRun("status"), "задание cron (раз в минуту): выключено", "awg-monitor enable collect")
	r.mustRun("enable", "collect")
	if !strings.Contains(r.Read("/opt/etc/crontab"), cronLine) {
		t.Fatal("not in crontab")
	}
	// No tcpdump yet (the fake runner has no handler for it).
	r.mustFail("Нужен tcpdump", "enable", "capture")
	r.Runner.On("tcpdump", "", 0)
	r.mustRun("enable", "capture")
	if v, _ := confValue(r, "DNS_CAPTURE"); v != "1" {
		t.Fatalf("DNS_CAPTURE=%s", v)
	}
	contain(t, r.mustRun("status"), "поиск мимо туннеля (capture): ВКЛЮЧЁН")
	r.mustFail("Для collect нет --purge", "disable", "collect", "--purge")
	r.mustFail("Неизвестная функция", "enable", "x")
	l, _ := r.TryLock(LockFile)
	defer l.Release()
	r.mustFail("не завершается", "disable", "capture")
}
