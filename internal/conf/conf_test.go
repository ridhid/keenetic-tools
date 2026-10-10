package conf

import (
	"errors"
	"strings"
	"testing"
)

// A config as written by awg-monitor install.
const sampleConf = `# keenetic-tools: awg-monitor
# Настройки awg-monitor. Читаются при каждом запуске, перезапуск не нужен.
LOG_DIR='/tmp/mnt/3d92c22d/awg monitor'
IFACE=opkgtun0
SERVICE=/opt/etc/init.d/S52awg-opkgtun0
PING_TARGETS='1.1.1.1 8.8.8.8'
HTTP_URL=http://cp.cloudflare.com/generate_204
HTTP_EVERY=5
DOMAINS_WATCH=''
WAN_IFACE=
DNS_CAPTURE=0
`

func TestParseSample(t *testing.T) {
	f, err := Parse("awg-monitor.conf", []byte(sampleConf))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"LOG_DIR":       "/tmp/mnt/3d92c22d/awg monitor",
		"IFACE":         "opkgtun0",
		"PING_TARGETS":  "1.1.1.1 8.8.8.8",
		"HTTP_URL":      "http://cp.cloudflare.com/generate_204",
		"DOMAINS_WATCH": "",
		"WAN_IFACE":     "",
	}
	for k, v := range want {
		if got, ok := f.Get(k); !ok || got != v {
			t.Errorf("%s = %q (%v), want %q", k, got, ok, v)
		}
	}
	if _, ok := f.Get("KEEP_DAYS"); ok {
		t.Error("KEEP_DAYS should be unset")
	}
	if string(f.Bytes()) != sampleConf {
		t.Errorf("round trip changed the file:\n%s", f.Bytes())
	}
}

func TestParseAccepts(t *testing.T) {
	cases := map[string]string{
		`A="double quoted"`:     "double quoted",
		`A=x   # comment`:       "x",
		`A='x' # comment`:       "x",
		"  A=indented":          "indented",
		"A=windows\r":           "windows",
		"A=1\nA=2":              "2",
		`A='$NOT_EXPANDED'`:     "$NOT_EXPANDED",
		`A=/tmp/mnt/*/log`:      "/tmp/mnt/*/log",
		`A=a=b`:                 "a=b",
		"# only a comment\nA=1": "1",
	}
	for in, want := range cases {
		f, err := Parse("c", []byte(in))
		if err != nil {
			t.Errorf("Parse(%q): %v", in, err)
			continue
		}
		if got, _ := f.Get("A"); got != want {
			t.Errorf("Parse(%q) A = %q, want %q", in, got, want)
		}
	}
}

func TestParseRejects(t *testing.T) {
	cases := []string{
		`A=$HOME`,
		`A=$(id)`,
		"A=`id`",
		`A="$HOME"`,
		`A=two words`,
		`A='a'b`,
		`A='unterminated`,
		`export A=1`,
		`A = 1`,
		`1A=1`,
		`. /etc/other.conf`,
		`A=x; rm -rf /`,
		`A=a&b`,
	}
	for _, in := range cases {
		_, err := Parse("awg-monitor.conf", []byte("B=1\n"+in+"\n"))
		var e *Error
		if !errors.As(err, &e) {
			t.Errorf("Parse(%q): want *Error, got %v", in, err)
			continue
		}
		if e.Line != 2 || !strings.Contains(e.Error(), "awg-monitor.conf, строка 2") {
			t.Errorf("Parse(%q): %v", in, e)
		}
	}
}

func TestSetLikeAwk(t *testing.T) {
	f, _ := Parse("c", []byte("# m\nDNS_CAPTURE=0\nX=1\n"))
	f.Set("DNS_CAPTURE", "1")
	f.Set("LOG_DIR", Quote("/mnt/a b"))
	want := "# m\nDNS_CAPTURE=1\nX=1\nLOG_DIR='/mnt/a b'\n"
	if got := string(f.Bytes()); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if v, _ := f.Get("LOG_DIR"); v != "/mnt/a b" {
		t.Fatalf("LOG_DIR = %q", v)
	}
	if !f.Has("LOG_DIR") || f.Has("LOG") {
		t.Fatal("Has")
	}
}

func TestAppend(t *testing.T) {
	f, _ := Parse("c", []byte("A=1\n"))
	if err := f.Append("# new settings\nDOMAINS_WATCH=''\nDOMAINS_EVERY=5\n"); err != nil {
		t.Fatal(err)
	}
	if v, ok := f.Get("DOMAINS_EVERY"); !ok || v != "5" {
		t.Fatalf("DOMAINS_EVERY = %q", v)
	}
	if got := string(f.Bytes()); got != "A=1\n# new settings\nDOMAINS_WATCH=''\nDOMAINS_EVERY=5\n" {
		t.Fatalf("got %q", got)
	}
}
