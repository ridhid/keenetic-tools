package monitor

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/syslog"
	"net"
	"net/http"
	"net/http/httptrace"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	// Root certificates for when the router has none.
	_ "golang.org/x/crypto/x509roots/fallback"

	"github.com/ridhid/keenetic-tools/internal/sys"
)

// Fetch is the outcome of one HTTP(S) request.
type Fetch struct {
	// Stage is "ok" when the server answered with any HTTP status, otherwise
	// where the request broke: dns, tcp, tls, cert, http, iface or err.
	Stage string
	Code  int
	IP    string
	Secs  float64
}

// Net is the network side of the probes; tests replace it.
type Net interface {
	// Fetch GETs url over IPv4 through device iface ("" — any), without
	// following redirects.
	Fetch(ctx context.Context, url, iface string, timeout time.Duration) Fetch
	// Resolve asks DNS server for host's A and AAAA records.
	Resolve(ctx context.Context, host, server string) ([]string, error)
	// IfaceAddr returns the first IPv4 address of a device and its network.
	IfaceAddr(name string) (ip string, network *net.IPNet)
}

type realNet struct {
	// roots overrides the system roots (tests).
	roots *x509.CertPool
}

func bindTo(iface string) func(string, string, syscall.RawConn) error {
	if iface == "" {
		return nil
	}
	return func(_, _ string, rc syscall.RawConn) error {
		var serr error
		if err := rc.Control(func(fd uintptr) { serr = syscall.BindToDevice(int(fd), iface) }); err != nil {
			return err
		}
		return serr
	}
}

// progress records how far a request got; trace hooks may run in a dial
// goroutine that outlives the request.
type progress struct {
	mu                               sync.Mutex
	resolving, connected, handshaken bool
	ip                               string
}

func (p *progress) set(f func()) {
	p.mu.Lock()
	defer p.mu.Unlock()
	f()
}

func (n realNet) Fetch(ctx context.Context, url, iface string, timeout time.Duration) Fetch {
	var r Fetch
	var p progress
	start := time.Now()
	dialer := &net.Dialer{Timeout: timeout, Control: bindTo(iface)}
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
			return dialer.DialContext(ctx, "tcp4", addr)
		},
		TLSClientConfig:     &tls.Config{RootCAs: n.roots},
		TLSHandshakeTimeout: timeout,
		DisableKeepAlives:   true,
	}
	defer tr.CloseIdleConnections()
	client := &http.Client{
		Transport:     tr,
		Timeout:       timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	trace := &httptrace.ClientTrace{
		DNSStart: func(httptrace.DNSStartInfo) { p.set(func() { p.resolving = true }) },
		DNSDone:  func(i httptrace.DNSDoneInfo) { p.set(func() { p.resolving = i.Err != nil }) },
		ConnectDone: func(_, addr string, err error) {
			if err == nil {
				p.set(func() {
					p.connected = true
					p.ip, _, _ = net.SplitHostPort(addr)
				})
			}
		},
		TLSHandshakeDone: func(_ tls.ConnectionState, err error) { p.set(func() { p.handshaken = err == nil }) },
	}
	req, err := http.NewRequestWithContext(httptrace.WithClientTrace(ctx, trace), "GET", url, nil)
	if err != nil {
		return Fetch{Stage: "err"}
	}
	req.Header.Set("User-Agent", "awg-monitor")
	resp, err := client.Do(req)
	if err == nil {
		// The status is enough; a broken body still means the server answered.
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		r.Code = resp.StatusCode
	}
	r.Secs = time.Since(start).Seconds()
	p.set(func() {
		r.IP = p.ip
		r.Stage = stage(err, strings.HasPrefix(url, "https:"), p.connected, p.handshaken)
		if r.Stage == "tcp" && p.resolving {
			// Timed out while the name was still being resolved.
			r.Stage = "dns"
		}
	})
	return r
}

// stage names where a request broke, like curl exit codes did.
func stage(err error, https, connected, handshaken bool) string {
	if err == nil {
		return "ok"
	}
	var dnsErr *net.DNSError
	var unknownCA x509.UnknownAuthorityError
	var hostErr x509.HostnameError
	var invalid x509.CertificateInvalidError
	var verify *tls.CertificateVerificationError
	switch {
	case errors.As(err, &dnsErr):
		return "dns"
	case errors.Is(err, syscall.ENODEV), errors.Is(err, syscall.EPERM):
		return "iface"
	case errors.As(err, &unknownCA), errors.As(err, &hostErr), errors.As(err, &invalid), errors.As(err, &verify):
		return "cert"
	case !connected:
		return "tcp"
	case https && !handshaken:
		return "tls"
	}
	return "http"
}

func (realNet) Resolve(ctx context.Context, host, server string) ([]string, error) {
	r := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, net.JoinHostPort(server, "53"))
		},
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	addrs, err := r.LookupIPAddr(ctx, host)
	var out []string
	for _, a := range addrs {
		out = append(out, a.IP.String())
	}
	sort.SliceStable(out, func(i, j int) bool { return !strings.Contains(out[i], ":") && strings.Contains(out[j], ":") })
	return out, err
}

func (realNet) IfaceAddr(name string) (string, *net.IPNet) {
	ifc, err := net.InterfaceByName(name)
	if err != nil {
		return "", nil
	}
	addrs, err := ifc.Addrs()
	if err != nil {
		return "", nil
	}
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil {
			return n.IP.String(), n
		}
	}
	return "", nil
}

// syslogger writes to the system log as `logger -t awg-monitor` would,
// falling back to the logger command without /dev/log.
func syslogger(env *sys.Env) func(string) {
	return func(msg string) {
		if w, err := syslog.New(syslog.LOG_NOTICE|syslog.LOG_USER, "awg-monitor"); err == nil {
			w.Notice(msg)
			w.Close()
			return
		}
		env.Run.Run(context.Background(), sys.Cmd{Name: "logger", Args: []string{"-t", "awg-monitor", msg}, Timeout: 5 * time.Second})
	}
}

// fmtFetch prints a probe as "stage code ip seconds" (the domain log fields).
func fmtFetch(f Fetch) (code, secs string) {
	code = fmt.Sprintf("%03d", f.Code)
	secs = fmt.Sprintf("%.2f", f.Secs)
	return
}
