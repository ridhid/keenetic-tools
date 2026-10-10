package monitor

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ridhid/keenetic-tools/internal/markers"
	"github.com/ridhid/keenetic-tools/internal/sys"
	"github.com/ridhid/keenetic-tools/internal/sys/systest"
)

const (
	logDir = "/tmp/mnt/HDD/awg-monitor"
	awgPID = "1234"
	// Private key of the fake AWG config; must never leave it.
	privKey = "cGrivAtEkEyFoRtEsTsOnLyAAAAAAAAAAAAAAAAAAAA="
	pskKey  = "cHJlc2hhcmVkS2V5Rm9yVGVzdHNPbmx5QUFBQUFBQUE="
)

type fakeNet struct {
	mu     sync.Mutex
	fetch  map[string]Fetch // by "url iface", then by url
	def    Fetch
	dns    map[string][]string
	addrs  map[string]string // iface -> CIDR
	called []string
}

func (n *fakeNet) Fetch(_ context.Context, url, iface string, _ time.Duration) Fetch {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.called = append(n.called, url+" "+iface)
	if f, ok := n.fetch[url+" "+iface]; ok {
		return f
	}
	if f, ok := n.fetch[url]; ok {
		return f
	}
	return n.def
}

func (n *fakeNet) Resolve(_ context.Context, host, _ string) ([]string, error) {
	return n.dns[host], nil
}

func (n *fakeNet) IfaceAddr(name string) (string, *net.IPNet) {
	c, ok := n.addrs[name]
	if !ok {
		return "", nil
	}
	ip, nw, _ := net.ParseCIDR(c)
	return ip.String(), &net.IPNet{IP: ip, Mask: nw.Mask}
}

func (n *fakeNet) calls() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]string(nil), n.called...)
}

// ping results by target: "loss/rtt" ("100/-" for no answer).
type router struct {
	*systest.Router
	m      *M
	net    *fakeNet
	pings  map[string]string
	hs     string // latest handshake, unix seconds
	ep     string
	logged []string
	self   string
}

// newRouter is a healthy tunnel with awg-monitor installed.
func newRouter(t *testing.T) *router {
	r := &router{Router: systest.New(t)}
	r.net = &fakeNet{
		fetch: map[string]Fetch{},
		def:   Fetch{Stage: "ok", Code: 204, IP: "104.16.1.1", Secs: 0.12},
		dns:   map[string][]string{},
		addrs: map[string]string{"opkgtun0": "10.8.1.2/32", "br0": "192.168.1.1/24"},
	}
	r.pings = map[string]string{"1.1.1.1": "0/20.5", "8.8.8.8": "0/30.5", "203.0.113.7": "0/40.0"}
	r.hs = fmt.Sprint(systest.Epoch.Unix() - 60)
	r.ep = "203.0.113.7:51820"
	r.self = filepath.Join(t.TempDir(), "keenetic-tools.part")
	if err := os.WriteFile(r.self, []byte("\x7fELF"+markers.Embedded), 0o755); err != nil {
		t.Fatal(err)
	}

	r.Write("/proc/mounts", "/dev/root / squashfs ro 0 0\ntmpfs /tmp tmpfs rw 0 0\n/dev/sda1 /tmp/mnt/HDD ext4 rw 0 0\n/dev/sda2 /opt ext4 rw 0 0\n", 0o644)
	r.Write("/proc/"+awgPID+"/cmdline", "amneziawg-go\x00opkgtun0\x00", 0o644)
	r.Write("/proc/"+awgPID+"/stat", awgPID+" (amneziawg-go) S 1 1 1 0 -1 4194560 100 0 0 0 1500 500 0 0 20 0 8 0 100 0 0", 0o644)
	r.Write("/proc/"+awgPID+"/status", "Name:\tamneziawg-go\nVmRSS:\t  9876 kB\n", 0o644)
	// A shell whose command line mentions the tunnel must not count.
	r.Write("/proc/999/cmdline", "sh\x00-c\x00pgrep -f amneziawg-go opkgtun0\x00", 0o644)
	r.Write("/sys/class/net/opkgtun0/mtu", "1376\n", 0o644)
	r.Write("/proc/loadavg", "0.12 0.10 0.05 1/100 1234\n", 0o644)
	r.Write("/proc/meminfo", "MemTotal: 500000 kB\nMemFree: 100000 kB\nMemAvailable: 250000 kB\n", 0o644)
	r.Write("/proc/sys/net/netfilter/nf_conntrack_count", "321\n", 0o644)
	r.Write("/proc/net/route", "Iface\tDestination\tGateway\tFlags\tRefCnt\tUse\tMetric\tMask\nppp0\t00000000\t01020304\t0003\t0\t0\t0\t00000000\nbr0\t0001A8C0\t00000000\t0001\t0\t0\t0\t00FFFFFF\n", 0o644)
	r.Write("/opt/etc/amnezia/amneziawg/awg0-opkgtun0.conf", "[Interface]\nPrivateKey = "+privKey+"\nAddress = 10.8.1.2/32\nJc = 4\n\n[Peer]\nPublicKey = pub\nPresharedKey = "+pskKey+"\nEndpoint = 203.0.113.7:51820\n", 0o600)
	r.Write("/opt/etc/init.d/S52awg-opkgtun0", "#!/bin/sh\nOPKGTUN_IFACE=opkgtun0\nAWG_CONF=/opt/etc/amnezia/amneziawg/awg0-opkgtun0.conf\n", 0o755)
	r.Write("/opt/etc/init.d/S10cron", "#!/bin/sh\n", 0o755)
	r.Write("/opt/etc/crontab", "SHELL=/bin/sh\n", 0o600)
	r.Write(logDir+"/"+DirMarker, markers.Monitor+"\n", 0o644)
	r.Write(Conf, confText(logDir), 0o644)

	r.Runner.On("/opt/etc/init.d/S10cron restart", "", 0)
	r.Runner.OnFunc("awg", func(c sys.Cmd) sys.Result {
		switch strings.Join(c.Args, " ") {
		case "show opkgtun0 latest-handshakes":
			return systest.Exit("peerkey=\t"+r.hs+"\n", 0)
		case "show opkgtun0 transfer":
			return systest.Exit("peerkey=\t1000000\t2000000\n", 0)
		case "show opkgtun0 endpoints":
			return systest.Exit("peerkey=\t"+r.ep+"\n", 0)
		}
		return systest.Exit("interface: opkgtun0\n  public key: pub\n  private key: (hidden)\n", 0)
	})
	r.Runner.OnFunc("ping", func(c sys.Cmd) sys.Result {
		res, ok := r.pings[c.Args[len(c.Args)-1]]
		if !ok {
			res = "100/-"
		}
		loss, rtt, _ := strings.Cut(res, "/")
		out := "PING x (x): 56 data bytes\n\n--- x ping statistics ---\n3 packets transmitted, 3 packets received, " + loss + "% packet loss\n"
		if rtt != "-" {
			out += "round-trip min/avg/max = 1.000/" + rtt + "/99.000 ms\n"
		}
		return systest.Exit(out, 0)
	})
	r.Runner.OnFunc("ndmc", func(c sys.Cmd) sys.Result {
		switch c.Args[1] {
		case "show running-config":
			return systest.Exit("\x1b[K"+runningConfig, 0)
		case "show version":
			return systest.Exit("\x1b[Krelease: 5.1.5\nmodel: Ultra\nhw_id: KN-1811\nuptime: 100\n", 0)
		case "show object-group fqdn tunnel-list":
			return systest.Exit("\x1b[K  group-name: tunnel-list\n    entry:\n      fqdn: youtube.com\n        type: config\n        address: 142.250.1.1\n        ttl: 300\n", 0)
		}
		return systest.Exit("\x1b[K", 0)
	})
	r.Runner.On("opkg list-installed", "amneziawg-go - 0.2\ncron - 4.1\ncurl - 8.0\ntcpdump - 4.99\n", 0)
	for _, c := range []string{"ip", "dmesg", "top", "free", "logger"} {
		r.Runner.On(c, c+" output\n", 0)
	}

	r.m = New(r.Env, r.self)
	r.m.net = r.net
	r.m.log = func(msg string) { r.logged = append(r.logged, msg) }
	return r
}

const runningConfig = `! $$$ Model: Keenetic Ultra
interface OpkgTun0
    description awg
    ip address 10.8.1.2 255.255.255.255
    ip mtu 1376
!
object-group fqdn tunnel-list
    include youtube.com
    include googlevideo.com
!
object-group fqdn other-list
    include example.org
!
dns-proxy
    route object-group tunnel-list OpkgTun0 auto reject
    route object-group other-list Wireguard0 auto
!
`

// main runs a command with a fresh M (config reloaded) and returns its code.
func (r *router) main(args ...string) int {
	r.Out.Reset()
	r.Err.Reset()
	m := New(r.Env, r.self)
	m.net = r.net
	m.log = r.m.log
	r.m = m
	return m.Main(args)
}

func (r *router) mustRun(args ...string) string {
	r.T.Helper()
	if code := r.main(args...); code != 0 {
		r.T.Fatalf("%v: exit %d\nstdout: %s\nstderr: %s", args, code, r.Out, r.Err)
	}
	return r.Out.String()
}

func (r *router) mustFail(want string, args ...string) {
	r.T.Helper()
	if code := r.main(args...); code == 0 {
		r.T.Fatalf("%v succeeded, want failure; stdout %s", args, r.Out)
	}
	if !strings.Contains(r.Err.String(), want) {
		r.T.Fatalf("%v stderr = %q, want %q", args, r.Err, want)
	}
}

// at moves the clock.
func (r *router) at(t time.Time) { r.Now = func() time.Time { return t } }

func (r *router) samples() []string {
	return lines(r.Read(logDir + "/samples/" + r.Now().Format("2006-01-02") + ".log"))
}

func (r *router) lastSample() map[string]string {
	r.T.Helper()
	s := r.samples()
	rec, ok := parseRecord(s[len(s)-1])
	if !ok {
		r.T.Fatalf("bad sample %q", s[len(s)-1])
	}
	return rec.f
}

func (r *router) events() string {
	b, _ := os.ReadFile(r.P(logDir + "/events.log"))
	return string(b)
}

func contain(t *testing.T, got string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(got, w) {
			t.Errorf("missing %q in:\n%s", w, got)
		}
	}
}

func lack(t *testing.T, got string, unwanted ...string) {
	t.Helper()
	for _, w := range unwanted {
		if strings.Contains(got, w) {
			t.Errorf("unexpected %q in:\n%s", w, got)
		}
	}
}
