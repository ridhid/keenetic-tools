package monitor

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// domNorm strips scheme, path and port; it returns the lowercased host or
// false on bad input.
func domNorm(s string) (string, bool) {
	if _, rest, ok := strings.Cut(s, "://"); ok {
		s = rest
	}
	s, _, _ = strings.Cut(s, "/")
	s, _, _ = strings.Cut(s, ":")
	if s == "" || s[0] == '-' || s[0] == '.' {
		return "", false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '-') {
			return "", false
		}
	}
	return strings.ToLower(s), true
}

// defaultRoute is the device of the IPv4 default route.
func (m *M) defaultRoute() string {
	data, err := m.env.ReadFile("/proc/net/route")
	if err != nil {
		return ""
	}
	for _, l := range lines(string(data)) {
		f := strings.Fields(l)
		if len(f) > 7 && f[1] == "00000000" && f[7] == "00000000" {
			return f[0]
		}
	}
	return ""
}

// wanIface is the "direct" path for domain checks; "" when unknown or
// when the default route goes into the tunnel.
func (m *M) wanIface() string {
	wan := m.c.WanIface
	if wan == "" {
		wan = m.defaultRoute()
	}
	if wan == m.c.Iface {
		return ""
	}
	return wan
}

// DomResult is a domain checked directly (W) and through the tunnel (T).
type DomResult struct {
	Domain string
	W, T   Fetch
}

var noWAN = Fetch{Stage: "nowan"}

// domRun probes domains via WAN and the tunnel in parallel.
func (m *M) domRun(domains []string) []DomResult {
	wan := m.wanIface()
	timeout := time.Duration(m.c.DomainTimeout) * time.Second
	res := make([]DomResult, len(domains))
	var wg sync.WaitGroup
	for i, d := range domains {
		res[i] = DomResult{Domain: d, W: noWAN}
		url := "https://" + d + "/"
		if wan != "" {
			wg.Go(func() { res[i].W = m.net.Fetch(bgctx(), url, wan, timeout) })
		}
		wg.Go(func() { res[i].T = m.net.Fetch(bgctx(), url, m.c.Iface, timeout) })
	}
	wg.Wait()
	return res
}

// ip is the address a check connected to, preferring the direct one.
func (r DomResult) ip() string {
	if r.W.IP != "" {
		return r.W.IP
	}
	return dash(r.T.IP)
}

func domOK(stage string) bool { return stage == "ok" || stage == "nowan" }

func okFail(stage string) string {
	if domOK(stage) {
		return "ok"
	}
	return "fail"
}

func domText(f Fetch) string {
	code, secs := fmtFetch(f)
	switch f.Stage {
	case "ok":
		return fmt.Sprintf("ответил, HTTP %s, %s с", code, secs)
	case "nowan":
		return "не проверено: не найден интерфейс провайдера (WAN_IFACE в конфиге)"
	case "dns":
		return "сбой DNS: имя не разрешилось"
	case "tcp":
		return fmt.Sprintf("сбой TCP: сервер не принял соединение (%s с)", secs)
	case "tls":
		return fmt.Sprintf("сбой TLS: рукопожатие оборвано или зависло (%s с) — похоже на DPI", secs)
	case "http":
		return fmt.Sprintf("сбой HTTP: соединение есть, ответа нет (%s с)", secs)
	case "cert":
		return "неверный сертификат — возможна подмена (заглушка провайдера)"
	case "iface":
		return "интерфейс недоступен"
	}
	return fmt.Sprintf("ошибка проверки (%s)", f.Stage)
}

func domVerdict(w, t string) string {
	switch {
	case w == "dns" && t == "dns":
		return "имя не разрешается — проблема DNS (AdGuard Home / DNS роутера), а не маршрута"
	case w == "nowan" && domOK(t):
		return "через туннель работает"
	case w == "nowan":
		return "через туннель не открывается"
	case domOK(w) && domOK(t):
		return "доступен обоими путями"
	case domOK(t):
		return "напрямую не открывается, через туннель — да: домен должен идти в туннель (проверьте список маршрутизации)"
	case domOK(w):
		return "через туннель не открывается, напрямую — да: проблема туннеля или его сервера"
	}
	return "не открывается ни напрямую, ни через туннель: сайт недоступен или блокировка глубже"
}

// fetchFields is "stage code seconds" for logs; a WAN that was not
// checked has no code or time.
func fetchFields(f Fetch) (stage, code, secs string) {
	if f.Stage == "nowan" {
		return "nowan", "000", "-"
	}
	code, secs = fmtFetch(f)
	return f.Stage, code, secs
}

// domCollect is the DOMAINS_WATCH check from collect.
func (m *M) domCollect(s *Sample) {
	var list []string
	for _, a := range m.c.DomainsWatch {
		if h, ok := domNorm(a); ok {
			list = append(list, h)
		}
	}
	if len(list) == 0 {
		return
	}
	old := map[string]string{}
	if data, err := m.env.ReadFile(m.path("domains.state")); err == nil {
		for _, l := range lines(string(data)) {
			if f := strings.Fields(l); len(f) == 2 {
				if _, seen := old[f[0]]; !seen {
					old[f[0]] = f[1]
				}
			}
		}
	}
	var log, state strings.Builder
	for _, r := range m.domRun(list) {
		ws, wc, wt := fetchFields(r.W)
		ts, tc, tt := fetchFields(r.T)
		fmt.Fprintf(&log, "%s ts=%d domain=%s wan=%s wan_code=%s wan_t=%s tun=%s tun_code=%s tun_t=%s ip=%s\n",
			s.Stamp, s.Now, r.Domain, ws, wc, wt, ts, tc, tt, r.ip())
		flags := okFail(ws) + "/" + okFail(ts)
		was, ok := old[r.Domain]
		if !ok {
			was = "ok/ok"
		}
		if flags != was {
			m.event(fmt.Sprintf("domain_changed domain=%s wan=%s tun=%s", r.Domain, ws, ts))
		}
		fmt.Fprintf(&state, "%s %s\n", r.Domain, flags)
	}
	m.appendFile(m.path("domains/"+s.Stamp[:10]+".log"), log.String())
	m.env.WriteAtomic(m.path("domains.state"), []byte(state.String()), 0o644)
}

func (m *M) checkDomain(args []string) error {
	if len(args) == 0 {
		return fail("Укажите домен: awg-monitor check-domain example.com")
	}
	var list []string
	for _, a := range args {
		h, ok := domNorm(a)
		if !ok {
			return fail("Некорректный домен: '%s'.", a)
		}
		list = append(list, h)
	}
	res := m.domRun(list)
	m.say("Напрямую — через %s, туннель — через %s; таймаут %d с.", dashText(m.wanIface(), "?"), m.c.Iface, m.c.DomainTimeout)
	for _, r := range res {
		m.say("")
		if ip := r.ip(); ip != "-" {
			m.say("%s (%s)", r.Domain, ip)
		} else {
			m.say("%s", r.Domain)
		}
		m.say("  напрямую: %s", domText(r.W))
		m.say("  туннель:  %s", domText(r.T))
		m.say("  итог: %s", domVerdict(r.W.Stage, r.T.Stage))
	}
	return nil
}
