package monitor

import (
	"context"
	"crypto/x509"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Each stage of a real request against local servers.
func TestFetchStages(t *testing.T) {
	ctx := context.Background()
	const short = 500 * time.Millisecond

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/slow":
			time.Sleep(2 * time.Second)
		case "/moved":
			http.Redirect(w, r, "/elsewhere", http.StatusMovedPermanently)
		}
	}))
	defer srv.Close()
	roots := x509.NewCertPool()
	roots.AddCert(srv.Certificate())
	trusted := realNet{roots: roots}

	closed, _ := net.Listen("tcp4", "127.0.0.1:0")
	closedAddr := closed.Addr().String()
	closed.Close()

	// Accepts TCP, never speaks TLS.
	silent, _ := net.Listen("tcp4", "127.0.0.1:0")
	defer silent.Close()
	go func() {
		for {
			c, err := silent.Accept()
			if err != nil {
				return
			}
			defer c.Close()
		}
	}()

	cases := []struct {
		name, url, iface string
		n                realNet
		stage            string
		code             int
	}{
		{"ok", srv.URL + "/", "", trusted, "ok", 200},
		{"redirect not followed", srv.URL + "/moved", "", trusted, "ok", 301},
		{"untrusted certificate", srv.URL + "/", "", realNet{}, "cert", 0},
		{"refused", "https://" + closedAddr + "/", "", trusted, "tcp", 0},
		{"tls hangs", "https://" + silent.Addr().String() + "/", "", trusted, "tls", 0},
		{"no answer", srv.URL + "/slow", "", trusted, "http", 0},
		{"no such name", "https://no-such-host.invalid/", "", trusted, "dns", 0},
		{"no such device", srv.URL + "/", "nosuchdev0", trusted, "iface", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := c.n.Fetch(ctx, c.url, c.iface, short)
			if f.Stage != c.stage || f.Code != c.code {
				t.Fatalf("got %+v, want %s %d", f, c.stage, c.code)
			}
			if c.stage == "ok" && f.IP != "127.0.0.1" {
				t.Fatalf("ip %q", f.IP)
			}
			if f.Secs > 3 {
				t.Fatalf("took %.1fs", f.Secs)
			}
		})
	}
}

func TestFetchFields(t *testing.T) {
	s, c, sec := fetchFields(Fetch{Stage: "tls", Secs: 7.999})
	if s+" "+c+" "+sec != "tls 000 8.00" {
		t.Fatal(s, c, sec)
	}
	if s, c, sec := fetchFields(noWAN); s+" "+c+" "+sec != "nowan 000 -" {
		t.Fatal(s, c, sec)
	}
	if !strings.Contains(domText(Fetch{Stage: "weird"}), "weird") {
		t.Fatal("unknown stage")
	}
}

func TestIfaceAddr(t *testing.T) {
	ip, n := realNet{}.IfaceAddr("lo")
	if ip != "127.0.0.1" || n == nil || !n.Contains(net.ParseIP("127.0.0.2")) {
		t.Fatalf("lo: %q %v", ip, n)
	}
	if ip, n := (realNet{}).IfaceAddr("nosuchdev0"); ip != "" || n != nil {
		t.Fatal("missing device has an address")
	}
}
