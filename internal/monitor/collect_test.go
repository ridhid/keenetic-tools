package monitor

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ridhid/keenetic-tools/internal/markers"
	"github.com/ridhid/keenetic-tools/internal/sys"
	"github.com/ridhid/keenetic-tools/internal/sys/systest"
)

func TestCollectOK(t *testing.T) {
	r := newRouter(t)
	r.mustRun("collect")
	s := r.lastSample()
	want := map[string]string{
		"state": "OK", "cause": "-", "hs_age": "60", "rx_d": "-", "tun": "0/20.5,0/30.5",
		"tun_loss": "0", "tun_rtt": "25.5", "http": "204:0.120", "ep": "203.0.113.7:51820",
		"ep_loss": "0", "ep_rtt": "40.0", "pid": awgPID, "rss_kb": "9876", "cpu": "-",
		"mtu": "1376", "load": "0.12", "mem_kb": "250000", "ct": "321",
	}
	for k, v := range want {
		if s[k] != v {
			t.Errorf("%s = %q, want %q", k, s[k], v)
		}
	}
	contain(t, r.events(), "state start->OK cause=-", "config_changed snapshot=")
	if len(r.m.listSnapshots()) != 1 {
		t.Fatalf("snapshots: %v", r.m.listSnapshots())
	}
	st := r.m.readState()
	if st.State != "OK" || st.PID != awgPID || st.Ticks != "2000" || st.Rx != "1000000" || st.Hash == "" {
		t.Fatalf("state %+v", st)
	}
	// The ping through the tunnel is bound to it; the endpoint ping is not.
	contain(t, strings.Join(r.Runner.Calls(), "\n"), "ping -c 3 -w 5 -I opkgtun0 1.1.1.1", "ping -c 3 -w 5 203.0.113.7")
	if len(r.Glob(LockFile)) != 1 || r.LockHolder(LockFile) != 0 {
		t.Fatal("lock not released")
	}
	contain(t, r.mustRun("status"), "Текущее состояние: OK (причина: -), с 2026-10-10 12:00:00",
		"Последний замер:", "  tun_rtt=25.5", "Последние события:", "сбор (collect): не выполняется")
}

func TestCollectSecondRun(t *testing.T) {
	r := newRouter(t)
	r.mustRun("collect")
	r.at(systest.Epoch.Add(time.Minute))
	r.Write("/proc/"+awgPID+"/stat", awgPID+" (amneziawg-go) S 1 1 1 0 -1 4194560 100 0 0 0 1800 800 0 0 20 0 8 0 100 0 0", 0o644)
	r.Runner.OnFunc("awg show opkgtun0 transfer", func(sys.Cmd) sys.Result { return systest.Exit("k\t1500000\t2100000\n", 0) })
	r.mustRun("collect")
	s := r.lastSample()
	if s["cpu"] != "10" || s["rx_d"] != "500000" || s["tx_d"] != "100000" || s["http"] != "-" {
		t.Fatalf("sample %v", s)
	}
	if n := strings.Count(r.events(), "\n"); n != 2 {
		t.Fatalf("events:\n%s", r.events())
	}
}

func TestCollectDown(t *testing.T) {
	old := fmt.Sprint(systest.Epoch.Unix() - 600)
	cases := []struct {
		cause string
		setup func(r *router)
	}{
		{"process_dead", func(r *router) { os.RemoveAll(r.P("/proc/" + awgPID)) }},
		{"iface_missing", func(r *router) { os.RemoveAll(r.P("/sys/class/net/opkgtun0")) }},
		{"handshake_never", func(r *router) { r.hs = "0" }},
		{"endpoint_unreachable", func(r *router) { r.hs = old; delete(r.pings, "203.0.113.7") }},
		{"handshake_stale", func(r *router) { r.hs = old }},
		{"tunnel_no_traffic", func(r *router) {}},
	}
	for _, c := range cases {
		t.Run(c.cause, func(t *testing.T) {
			r := newRouter(t)
			delete(r.pings, "1.1.1.1")
			delete(r.pings, "8.8.8.8")
			r.net.def = Fetch{Stage: "tcp", Secs: 8}
			c.setup(r)
			r.mustRun("collect")
			s := r.lastSample()
			if s["state"] != "DOWN" || s["cause"] != c.cause {
				t.Fatalf("state %s cause %s", s["state"], s["cause"])
			}
			dumps := r.Glob(logDir + "/dumps/*-" + c.cause + ".txt")
			if len(dumps) != 1 {
				t.Fatalf("dumps %v", dumps)
			}
			contain(t, r.events(), "state start->DOWN cause="+c.cause+" dump=dumps/")
			contain(t, r.Read(dumps[0]), "# awg-monitor dump", "### sample", "### dmesg (tail)", "### top")
			if !strings.Contains(strings.Join(r.logged, "\n"), "DOWN") {
				t.Fatal("not in syslog")
			}
		})
	}
}

func TestCollectRecoveryAndChanges(t *testing.T) {
	r := newRouter(t)
	delete(r.pings, "1.1.1.1")
	delete(r.pings, "8.8.8.8")
	r.net.def = Fetch{Stage: "tcp", Secs: 8}
	r.mustRun("collect")

	r.at(systest.Epoch.Add(3 * time.Minute))
	r.pings["1.1.1.1"], r.pings["8.8.8.8"] = "0/20", "0/20"
	r.Write("/proc/5678/cmdline", "/opt/sbin/amneziawg-go\x00opkgtun0\x00", 0o644)
	os.RemoveAll(r.P("/proc/" + awgPID))
	r.ep = "198.51.100.1:51820"
	r.mustRun("collect")
	contain(t, r.events(), "state DOWN->OK cause=- down_for=180s", "process_restarted old=1234 new=5678",
		"endpoint_changed old=203.0.113.7:51820 new=198.51.100.1:51820")
}

func TestCollectDegraded(t *testing.T) {
	cases := []struct {
		cause string
		setup func(r *router)
	}{
		{"packet_loss", func(r *router) { r.pings["1.1.1.1"] = "33/20" }},
		{"high_rtt", func(r *router) { r.pings["1.1.1.1"], r.pings["8.8.8.8"] = "0/600", "0/700" }},
		{"http_fail", func(r *router) { r.net.def = Fetch{Stage: "http", Secs: 8} }},
	}
	for _, c := range cases {
		t.Run(c.cause, func(t *testing.T) {
			r := newRouter(t)
			c.setup(r)
			r.mustRun("collect")
			if s := r.lastSample(); s["state"] != "DEGRADED" || s["cause"] != c.cause {
				t.Fatalf("state %s cause %s", s["state"], s["cause"])
			}
		})
	}
}

func TestCollectDiskMissing(t *testing.T) {
	r := newRouter(t)
	r.Write(CapPID, "777\n", 0o644)
	r.Write("/proc/777/cmdline", "tcpdump\x00-l\x00", 0o644)
	os.Remove(r.P(logDir + "/" + DirMarker))
	r.mustRun("collect")
	r.mustRun("collect")
	if len(r.Glob(logDir+"/samples/*")) != 0 || r.Exists(logDir+"/state") {
		t.Fatal("wrote into an unmounted log dir")
	}
	if len(r.logged) != 1 || !strings.Contains(r.logged[0], "недоступен") {
		t.Fatalf("syslog %q", r.logged)
	}
	if len(r.Killed) != 1 || r.Killed[0] != "777 terminated" || r.Exists(CapPID) {
		t.Fatalf("tcpdump not stopped: %v", r.Killed)
	}
	// The disk is back: the flag goes.
	r.Write(logDir+"/"+DirMarker, "x\n", 0o644)
	r.mustRun("collect")
	if r.Exists(NoDirFlag) {
		t.Fatal("flag left")
	}
}

func TestCollectLocked(t *testing.T) {
	r := newRouter(t)
	l, err := r.TryLock(LockFile)
	if l == nil || err != nil {
		t.Fatal(err)
	}
	defer l.Release()
	r.mustRun("collect")
	if r.Exists(logDir + "/state") {
		t.Fatal("collect ran while locked")
	}
}

func TestCollectHousekeeping(t *testing.T) {
	r := newRouter(t)
	old := systest.Epoch.Add(-15 * 24 * time.Hour)
	keep := systest.Epoch.Add(-14 * 24 * time.Hour).Add(time.Hour)
	for _, f := range []string{"/samples/old.log", "/dumps/old.txt", "/bundles/old.tar.gz", "/samples/keep.log", "/snapshots/old.txt"} {
		r.Write(logDir+f, "x\n", 0o644)
		mt := old
		if strings.Contains(f, "keep") {
			mt = keep
		}
		os.Chtimes(r.P(logDir+f), mt, mt)
	}
	r.Write(logDir+"/events.log", strings.Repeat("x", eventsMax+1), 0o644)
	r.mustRun("collect")
	for _, f := range []string{"/samples/old.log", "/dumps/old.txt", "/bundles/old.tar.gz"} {
		if r.Exists(logDir + f) {
			t.Errorf("%s kept", f)
		}
	}
	for _, f := range []string{"/samples/keep.log", "/snapshots/old.txt", "/events.log.1"} {
		if !r.Exists(logDir + f) {
			t.Errorf("%s removed", f)
		}
	}
}

func TestCollectConfigChange(t *testing.T) {
	r := newRouter(t)
	r.mustRun("collect")
	// Not a CONFIG_EVERY minute: no check.
	r.at(systest.Epoch.Add(time.Minute))
	r.Write("/opt/etc/amnezia/amneziawg/awg0-opkgtun0.conf", "[Interface]\nPrivateKey = "+privKey+"\nJc = 8\n", 0o600)
	r.mustRun("collect")
	if len(r.m.listSnapshots()) != 1 {
		t.Fatal("snapshot outside CONFIG_EVERY")
	}
	r.at(systest.Epoch.Add(15 * time.Minute))
	r.mustRun("collect")
	list := r.m.listSnapshots()
	if len(list) != 2 {
		t.Fatalf("snapshots %v", list)
	}
	snap := r.Read(logDir + "/snapshots/" + list[1])
	contain(t, snap, "PrivateKey = <redacted>", "Jc = 8", "release: 5.1.5", "interface OpkgTun0", "ip mtu 1376", "cron - 4.1")
	lack(t, snap, privKey, "uptime:", "tcpdump - ")
	out := r.mustRun("diff")
	contain(t, out, "-Jc = 4", "+Jc = 8", "@@")
	lack(t, out, "### ndm")
}

func TestCheck(t *testing.T) {
	r := newRouter(t)
	out := r.mustRun("check")
	contain(t, out, "Состояние: OK", "Интерфейс opkgtun0: есть, MTU 1376", "PID 1234, RSS 9876 КБ",
		"Handshake: 60 с назад", "HTTP через туннель (код:время): 204:0.120", "state=OK")
	if r.Exists(logDir+"/state") || len(r.Glob(logDir+"/samples/*")) != 0 {
		t.Fatal("check wrote files")
	}
}

func TestConfErrors(t *testing.T) {
	r := newRouter(t)
	head := markers.Monitor + "\n"
	r.Write(Conf, head+"PING_COUNT=$(reboot)\n", 0o644)
	r.mustFail("строка 2", "check")
	r.mustFail("строка 2", "collect")
	if len(r.logged) != 1 || !strings.Contains(r.logged[0], "строка 2") {
		t.Fatalf("collect did not log a broken config: %q", r.logged)
	}
	r.Write(Conf, head+"PING_COUNT=abc\n", 0o644)
	r.mustFail("PING_COUNT=abc: ожидается целое число", "check")
	r.Write(Conf, head+"LOG_DIR="+logDir+"\nFOO=1\n", 0o644)
	r.mustRun("snapshots")
	contain(t, r.Err.String(), "неизвестная настройка FOO")
}
