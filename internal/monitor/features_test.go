package monitor

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/ridhid/keenetic-tools/internal/sys"
	"github.com/ridhid/keenetic-tools/internal/sys/systest"
)

func TestDomNorm(t *testing.T) {
	for in, want := range map[string]string{
		"YouTube.com": "youtube.com", "https://a.b.c/path?x": "a.b.c", "a.b:443": "a.b",
		"-x.com": "", ".x.com": "", "a b": "", "a_b.com": "", "": "",
	} {
		got, ok := domNorm(in)
		if got != want || ok != (want != "") {
			t.Errorf("domNorm(%q) = %q, %v", in, got, ok)
		}
	}
}

func TestCheckDomain(t *testing.T) {
	r := newRouter(t)
	r.net.fetch["https://blocked.example/ ppp0"] = Fetch{Stage: "tls", Secs: 8, IP: "198.51.100.5"}
	r.net.fetch["https://blocked.example/ opkgtun0"] = Fetch{Stage: "ok", Code: 301, Secs: 0.4, IP: "198.51.100.5"}
	r.net.fetch["https://dead.example/"] = Fetch{Stage: "dns", Secs: 0.1}
	out := r.mustRun("check-domain", "https://Blocked.example/x", "dead.example")
	contain(t, out, "Напрямую — через ppp0, туннель — через opkgtun0; таймаут 8 с.",
		"blocked.example (198.51.100.5)", "напрямую: сбой TLS: рукопожатие оборвано или зависло (8.00 с)",
		"туннель:  ответил, HTTP 301, 0.40 с", "домен должен идти в туннель",
		"dead.example\n", "проблема DNS")
	r.mustFail("Некорректный домен", "check-domain", "a b")
	r.mustFail("Укажите домен", "check-domain")
}

func TestCheckDomainNoWAN(t *testing.T) {
	r := newRouter(t)
	// Default route into the tunnel: no direct path.
	r.Write("/proc/net/route", "Iface\tDestination\tGateway\tFlags\tRefCnt\tUse\tMetric\tMask\nopkgtun0\t00000000\t00000000\t0001\t0\t0\t0\t00000000\n", 0o644)
	out := r.mustRun("check-domain", "a.example")
	contain(t, out, "через ?", "не проверено: не найден интерфейс провайдера", "через туннель работает")
	for _, c := range r.net.calls() {
		if strings.HasSuffix(c, " opkgtun0") == false {
			t.Errorf("direct probe without WAN: %s", c)
		}
	}
}

func TestDomainsWatch(t *testing.T) {
	r := newRouter(t)
	r.Write(Conf, confText(logDir)+"DOMAINS_WATCH='youtube.com bad_domain https://x.org/'\n", 0o644)
	r.net.fetch["https://youtube.com/ ppp0"] = Fetch{Stage: "tls", Secs: 8, IP: "142.250.1.1"}
	r.mustRun("collect")
	log := r.Read(logDir + "/domains/2026-10-10.log")
	contain(t, log, "domain=youtube.com wan=tls wan_code=000 wan_t=8.00 tun=ok tun_code=204 tun_t=0.12 ip=142.250.1.1",
		"domain=x.org wan=ok")
	lack(t, log, "bad_domain")
	contain(t, r.events(), "domain_changed domain=youtube.com wan=tls tun=ok")
	lack(t, r.events(), "domain=x.org")
	if got := r.Read(logDir + "/domains.state"); got != "youtube.com fail/ok\nx.org ok/ok\n" {
		t.Fatalf("domains.state %q", got)
	}
	// Same result: no new event.
	r.at(systest.Epoch.Add(5 * time.Minute))
	r.mustRun("collect")
	if strings.Count(r.events(), "domain_changed") != 1 {
		t.Fatal(r.events())
	}
	out := r.mustRun("domains")
	contain(t, out, "youtube.com — проверок 2", "напрямую: 0% (сбои: tls 2)", "туннель:  100%",
		"x.org — проверок 2", "Изменения доступности: 1")
}

func TestRouteCheck(t *testing.T) {
	r := newRouter(t)
	r.net.dns["www.youtube.com"] = []string{"142.250.1.1", "142.250.1.2", "2a00::1"}
	r.Write("/proc/net/nf_conntrack", ct(
		"ipv4 2 tcp 6 100 ESTABLISHED src=192.168.1.50 dst=142.250.1.1 sport=50000 dport=443 packets=5 bytes=900 src=142.250.1.1 dst=10.8.1.2 sport=443 dport=50000 packets=5 bytes=5000 [ASSURED] mark=0 use=1",
		"ipv4 2 tcp 6 100 SYN_SENT src=192.168.1.51 dst=142.250.1.2 sport=50001 dport=443 packets=1 bytes=60 [UNREPLIED] src=142.250.1.2 dst=203.0.113.9 sport=443 dport=50001 packets=0 bytes=0 mark=0 use=1",
		"ipv4 2 tcp 6 100 ESTABLISHED src=192.168.1.50 dst=8.8.8.8 sport=50002 dport=443 src=8.8.8.8 dst=203.0.113.9 sport=443 dport=50002",
		"ipv6 10 tcp 6 100 ESTABLISHED src=fd00::2 dst=2a00:1450::1 sport=1 dport=443 src=2a00:1450::1 dst=fd00::2 sport=443 dport=1",
	), 0o644)
	out := r.mustRun("route-check", "www.youtube.com")
	contain(t, out,
		"1. Список: покрыт — группа tunnel-list, запись include youtube.com",
		"2. DNS роутера (127.0.0.1): IPv4: 142.250.1.1 142.250.1.2; IPv6: 2a00::1",
		"3. 142.250.1.1 — в runtime-списке группы tunnel-list (как youtube.com)",
		"3. 142.250.1.2 — НЕТ в runtime-списке",
		"192.168.1.50 -> 142.250.1.1 tcp/443 через ТУННЕЛЬ, ответ есть, байт 900/5000, ESTABLISHED",
		"192.168.1.51 -> 142.250.1.2 tcp/443 через ПРОВАЙДЕРА, ответ НЕТ",
		"5. Проверка с роутера: напрямую — ответил",
		"IP 142.250.1.2 не попали в список", "соединения идут мимо туннеля", "1 IPv6-соединений")
	lack(t, out, "8.8.8.8", "Добавьте в группу")

	out = r.mustRun("route-check", "news.example.com")
	contain(t, out, "НЕ покрыт ни одной группой (tunnel-list)", "имя не разрешилось",
		"нечего проверять", "include example.com (или news.example.com)")
	lack(t, out, "other-list")
	r.Runner.Missing("ndmc")
	r.mustFail("только на Keenetic", "route-check", "a.com")
}

func ct(rows ...string) string { return strings.Join(rows, "\n") + "\n" }

const tcpdumpOut = `IP (tos 0x0, ttl 64, id 1, offset 0, flags [DF], proto UDP (17), length 120)
    192.168.1.1.53 > 192.168.1.50.40000: [udp sum ok] 1 q: A? Blocked.Example. 2/0/0 blocked.example. [1m] A 198.51.100.5, blocked.example. [1m] A 198.51.100.6 (90)
IP (tos 0x0, ttl 64, id 2, offset 0, flags [DF], proto UDP (17), length 90)
    192.168.1.1.53 > 192.168.1.51.40001: [udp sum ok] 2 q: A? video.youtube.com. 1/0/0 video.youtube.com. [1m] A 142.250.9.9 (60)
IP (tos 0x0, ttl 64, id 3, offset 0, flags [DF], proto UDP (17), length 90)
    192.168.1.1.53 > 192.168.1.50.40002: [udp sum ok] 3 q: AAAA? v6.example. 1/0/0 v6.example. [1m] AAAA 2001:db8::1 (60)
`

func TestParseDNS(t *testing.T) {
	got := fmt.Sprint(parseDNS(tcpdumpOut))
	want := "[[198.51.100.5 blocked.example 192.168.1.50] [198.51.100.6 blocked.example 192.168.1.50] [142.250.9.9 video.youtube.com 192.168.1.51]]"
	if got != want {
		t.Fatalf("got %s", got)
	}
}

func TestMissed(t *testing.T) {
	r := newRouter(t)
	r.Write(Conf, confText(logDir)+"DNS_CAPTURE=1\n", 0o644)
	r.Runner.OnFunc("tcpdump", func(c sys.Cmd) sys.Result {
		r.Write("/proc/"+fmt.Sprint(systest.FakePID)+"/cmdline", "tcpdump\x00-l\x00", 0o644)
		return systest.Exit(tcpdumpOut, 0)
	})
	r.Write("/proc/net/nf_conntrack", ct(
		// Blocked directly: unreplied, not in the list.
		"ipv4 2 tcp 6 100 SYN_SENT src=192.168.1.50 dst=198.51.100.5 sport=50000 dport=443 [UNREPLIED] src=198.51.100.5 dst=203.0.113.9 sport=443 dport=50000",
		// In the list but went direct.
		"ipv4 2 tcp 6 100 ESTABLISHED src=192.168.1.51 dst=142.250.9.9 sport=50001 dport=443 bytes=900 src=142.250.9.9 dst=203.0.113.9 sport=443 dport=50001 bytes=100",
		// DoH from a device.
		"ipv4 2 tcp 6 100 ESTABLISHED src=192.168.1.52 dst=1.1.1.1 sport=50002 dport=443 src=1.1.1.1 dst=203.0.113.9 sport=443 dport=50002",
		// No DNS answer seen.
		"ipv4 2 udp 17 30 src=192.168.1.52 dst=203.0.113.200 sport=5000 dport=3478 [UNREPLIED] src=203.0.113.200 dst=203.0.113.9 sport=3478 dport=5000",
		// Fine: replied.
		"ipv4 2 tcp 6 100 ESTABLISHED src=192.168.1.50 dst=93.184.216.34 sport=50003 dport=443 bytes=900 src=93.184.216.34 dst=203.0.113.9 sport=443 dport=50003 bytes=9000",
		// Another network: ignored.
		"ipv4 2 tcp 6 100 SYN_SENT src=10.1.1.1 dst=198.51.100.5 sport=1 dport=443 [UNREPLIED] src=198.51.100.5 dst=203.0.113.9 sport=443 dport=1",
	), 0o644)
	r.net.fetch["https://blocked.example/ ppp0"] = Fetch{Stage: "tcp", Secs: 8}
	r.mustRun("collect")

	if !r.Runner.Ran("tcpdump -l -n -t -vv -s 1500 -c 20000 -i br0 udp src port 53") || r.Read(CapPID) != fmt.Sprint(systest.FakePID)+"\n" {
		t.Fatalf("tcpdump not started: %v", r.Runner.Calls())
	}
	if r.Read(CapLog) != "" {
		t.Fatal("capture not harvested")
	}
	contain(t, r.Read(DNSMap), "198.51.100.5 blocked.example 192.168.1.50")
	contain(t, r.Read(Includes), "youtube.com\ngooglevideo.com\n")
	log := r.Read(logDir + "/missed/2026-10-10.log")
	contain(t, log,
		"kind=flow client=192.168.1.50 ip=198.51.100.5 proto=tcp dport=443 via=wan stage=noreply domain=blocked.example covered=no",
		"client=192.168.1.51 ip=142.250.9.9 proto=tcp dport=443 via=wan stage=stall domain=video.youtube.com covered=yes",
		"client=192.168.1.52 ip=1.1.1.1 proto=tcp dport=443 via=wan stage=doh",
		"ip=203.0.113.200 proto=udp dport=3478 via=wan stage=noreply_udp domain=- covered=-",
		"kind=probe domain=blocked.example wan=tcp wan_t=8.00 tun=ok tun_t=0.12")
	lack(t, log, "93.184.216.34", "10.1.1.1")
	contain(t, r.events(), "missed_candidate domain=blocked.example wan=tcp tun=ok")

	// The same flows a minute later are not logged or probed again.
	r.at(systest.Epoch.Add(time.Minute))
	r.mustRun("collect")
	if got := r.Read(logDir + "/missed/2026-10-10.log"); got != log {
		t.Fatalf("logged again:\n%s", got)
	}

	out := r.mustRun("missed")
	contain(t, out,
		"1. Не в списке, напрямую не открываются, через туннель — да",
		"blocked.example                          соединений 1, устройства: 192.168.1.50",
		"include blocked.example",
		"3. В списке, но соединения пошли мимо туннеля", "video.youtube.com",
		"4. Без ответа и без запроса к DNS роутера", "203.0.113.200:3478/udp",
		"5. Устройства со своим DNS", "192.168.1.52 — соединений 1: 1.1.1.1:443")
	lack(t, out, "захват DNS сейчас не запущен")

	r.mustRun("disable", "capture", "--purge")
	if len(r.Killed) != 1 || r.Exists(CapPID) || r.Exists(DNSMap) || r.Exists(logDir+"/missed") {
		t.Fatalf("capture not cleaned: killed %v", r.Killed)
	}
	if v, _ := confValue(r, "DNS_CAPTURE"); v != "0" {
		t.Fatalf("DNS_CAPTURE=%s", v)
	}
}

func TestMissedNoTcpdump(t *testing.T) {
	r := newRouter(t)
	r.Write(Conf, confText(logDir)+"DNS_CAPTURE=1\n", 0o644)
	r.mustRun("collect")
	r.at(systest.Epoch.Add(time.Minute))
	r.mustRun("collect")
	if strings.Count(strings.Join(r.logged, "\n"), "tcpdump не установлен") != 1 {
		t.Fatalf("syslog %q", r.logged)
	}
	r.mustFail("Данных пока нет", "missed")
}

func confValue(r *router, key string) (string, bool) {
	r.m.c = Defaults()
	for _, l := range lines(r.Read(Conf)) {
		if v, ok := strings.CutPrefix(l, key+"="); ok {
			return v, true
		}
	}
	return "", false
}

func TestReport(t *testing.T) {
	r := newRouter(t)
	base := systest.Epoch.Add(-3 * time.Hour)
	var b strings.Builder
	line := func(min int, state, cause, pid, rtt string) {
		ts := base.Add(time.Duration(min) * time.Minute)
		fmt.Fprintf(&b, "%s ts=%d state=%s cause=%s hs_age=10 rx_d=1 tx_d=1 tun=0/1 tun_loss=0 tun_rtt=%s http=- ep=x ep_loss=0 ep_rtt=1 pid=%s rss_kb=100 cpu=2 mtu=1376 load=0 mem_kb=1 ct=1\n",
			ts.Format(stampFormat), ts.Unix(), state, cause, rtt, pid)
	}
	line(0, "OK", "-", "100", "20.0")
	line(1, "DOWN", "handshake_stale", "100", "-")
	line(2, "DOWN", "handshake_stale", "100", "-")
	line(3, "OK", "-", "200", "40.0")
	// Gap of 10 minutes.
	line(13, "DEGRADED", "packet_loss", "200", "30.0")
	line(14, "DOWN", "process_dead", "-", "-")
	r.Write(logDir+"/samples/"+base.Format("2006-01-02")+".log", b.String(), 0o644)
	r.Write(logDir+"/events.log", fmt.Sprintf("x y ts=%d endpoint_changed old=a new=b\nx y ts=%d config_changed snapshot=s.txt\n", base.Unix(), base.Unix()), 0o644)
	out := r.mustRun("report")
	contain(t, out,
		"замеров 6", "OK 33.3% | DEGRADED 16.7% | DOWN 50.0%",
		"Пропуски данных: 1 (~9 мин)", "RTT средний 30.0 мс, максимальный 40.0 мс",
		"перезапусков 1 (в минуты :01–:03 — 1",
		"  DOWN: handshake_stale              2\n",
		"— "+base.Add(3*time.Minute).Format("2006-01-02 15:04")+"  2 мин  handshake_stale",
		"(продолжается)  1 мин  process_dead",
		"Смена Endpoint: 1", "Изменения конфигурации: 1")
	out = r.mustRun("report", "1h")
	contain(t, out, "За период 1h замеров нет.")
	r.mustFail("24h или 7d", "report", "5m")
	r.mustFail("один аргумент", "report", "1h", "2h")
}

func TestBundle(t *testing.T) {
	r := newRouter(t)
	r.mustRun("collect")
	r.Write(logDir+"/samples/2026-10-01.log", "old\n", 0o644)
	r.Write(logDir+"/missed/2026-10-10.log", "client=192.168.1.50\n", 0o644)
	out := r.mustRun("bundle")
	contain(t, out, "Архив: "+logDir+"/bundles/awg-monitor-2026-10-10_120000.tar.gz")
	names, meta := readTar(t, r.P(logDir+"/bundles/awg-monitor-2026-10-10_120000.tar.gz"))
	sort.Strings(names)
	got := strings.Join(names, " ")
	contain(t, got, "meta.txt", "samples/2026-10-10.log", "events.log", "snapshots/")
	lack(t, got, "2026-10-01", "missed", ".part")
	contain(t, meta, "### config: /opt/etc/awg-monitor.conf", "LOG_DIR=", "### report 3d", "замеров 1")
	lack(t, meta, "# Настройки")

	// A key in the logs: no archive.
	r.Write(logDir+"/events.log", "leak "+pskKey+"\n", 0o644)
	r.at(systest.Epoch.Add(time.Second))
	r.mustFail("ключ из конфига AWG", "bundle")
	if len(r.Glob(logDir+"/bundles/*")) != 1 {
		t.Fatalf("bundles: %v", r.Glob(logDir+"/bundles/*"))
	}
	r.mustFail("целое больше нуля", "bundle", "0")
}

func readTar(t *testing.T, path string) (names []string, meta string) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, h.Name)
		if h.Name == "meta.txt" {
			b, _ := io.ReadAll(tr)
			meta = string(b)
		}
	}
}

func TestSnapshotsAndDiff(t *testing.T) {
	r := newRouter(t)
	r.mustFail("минимум два", "diff")
	out := r.mustRun("snapshot", "до смены/mtu")
	contain(t, out, "2026-10-10_120000-до_смены_mtu.txt", "Хэш конфигурации: ")
	r.at(systest.Epoch.Add(time.Minute))
	r.mustRun("snapshot")
	contain(t, r.mustRun("snapshots"), "2026-10-10_120000-до_смены_mtu.txt\n2026-10-10_120100-manual.txt\n")
	contain(t, r.mustRun("diff"), "Различий нет.")
	contain(t, r.mustRun("diff", "до_смены", "manual"), "-> 2026-10-10_120100-manual.txt")
	r.mustFail("не найден", "diff", "nothing")
	r.mustFail("не больше двух", "diff", "a", "b", "c")
}

func TestUnifiedDiff(t *testing.T) {
	a := strings.Split("1 2 3 4 5 6 7 8 9 10 11 12 13 14 15", " ")
	b := strings.Split("1 2 3 4 5 X 7 8 9 10 11 12 13 14 15 16", " ")
	want := `--- a
+++ b
@@ -3,7 +3,7 @@
 3
 4
 5
-6
+X
 7
 8
 9
@@ -13,3 +13,4 @@
 13
 14
 15
+16
`
	if got := unifiedDiff("a", "b", a, b); got != want {
		t.Fatalf("got:\n%s", got)
	}
	if unifiedDiff("a", "b", a, a) != "" {
		t.Fatal("equal inputs differ")
	}
	if got := unifiedDiff("a", "b", nil, []string{"x"}); got != "--- a\n+++ b\n@@ -0,0 +1 @@\n+x\n" {
		t.Fatalf("got:\n%s", got)
	}
}

func TestPingStats(t *testing.T) {
	cases := []struct{ out, loss, rtt string }{
		{"3 packets transmitted, 3 packets received, 0% packet loss\nround-trip min/avg/max = 10.1/20.2/30.3 ms\n", "0", "20.2"},
		{"3 packets transmitted, 2 received, 33.3333% packet loss, time 2003ms\nrtt min/avg/max/mdev = 1.1/2.2/3.3/0.4 ms\n", "33.3333", "2.2"},
		{"3 packets transmitted, 0 packets received, 100% packet loss\n", "100", "-"},
		{"ping: bad address\n", "100", "-"},
	}
	for _, c := range cases {
		if l, r := pingStats([]byte(c.out)); l != c.loss || r != c.rtt {
			t.Errorf("%q: %s %s", c.out, l, r)
		}
	}
}
