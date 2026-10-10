package monitor

import (
	"net"
	"sort"
	"strconv"
	"strings"
)

// Routing is the domain routing of a Keenetic running-config: object-group
// fqdn includes and the groups that dns-proxy routes to an interface.
type Routing struct {
	// Groups routed to the tunnel, sorted.
	Groups []string
	// Includes of every object-group fqdn, lowercased.
	Includes map[string][]string
}

func parseRouting(cfg, ndmIface string) Routing {
	r := Routing{Includes: map[string][]string{}}
	routed := map[string]bool{}
	group, dnsProxy := "", false
	for _, l := range lines(cfg) {
		if l != "" && l[0] != ' ' && l[0] != '!' {
			group, dnsProxy = "", false
		}
		f := strings.Fields(l)
		switch {
		case strings.HasPrefix(l, "object-group fqdn ") && len(f) >= 3:
			group = f[2]
			continue
		case l == "dns-proxy":
			dnsProxy = true
			continue
		}
		if group != "" && len(f) >= 2 && f[0] == "include" {
			r.Includes[group] = append(r.Includes[group], strings.ToLower(f[1]))
		}
		if dnsProxy && len(f) >= 4 && f[0] == "route" && f[1] == "object-group" && f[3] == ndmIface {
			routed[f[2]] = true
		}
	}
	for g := range routed {
		r.Groups = append(r.Groups, g)
	}
	sort.Strings(r.Groups)
	return r
}

// Cover returns the first tunnel group and include that cover domain d.
func (r Routing) Cover(d string) (group, include string, ok bool) {
	for _, g := range r.Groups {
		for _, inc := range r.Includes[g] {
			if d == inc || strings.HasSuffix(d, "."+inc) {
				return g, inc, true
			}
		}
	}
	return "", "", false
}

// TunnelIncludes lists the includes of all tunnel groups.
func (r Routing) TunnelIncludes() []string {
	var out []string
	for _, g := range r.Groups {
		out = append(out, r.Includes[g]...)
	}
	return out
}

func (m *M) routing() Routing { return parseRouting(m.ndmc("show running-config"), m.c.NdmIface) }

// Flow is a LAN connection from conntrack.
type Flow struct {
	Client, Dst, Proto, Dport, Via string
	Replied                        bool
	OBytes, RBytes, State, Sport   string
}

var privateNets = func() []*net.IPNet {
	var out []*net.IPNet
	for _, c := range []string{"10.0.0.0/8", "192.168.0.0/16", "172.16.0.0/12", "127.0.0.0/8",
		"169.254.0.0/16", "224.0.0.0/3", "0.0.0.0/8"} {
		_, n, _ := net.ParseCIDR(c)
		out = append(out, n)
	}
	return out
}()

func private(s string) bool {
	ip := net.ParseIP(s)
	if ip == nil {
		return false
	}
	for _, n := range privateNets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// ctFlows reads IPv4 conntrack: flows from private to public addresses.
// dsts limits destinations (nil — all); lan limits clients (nil — all);
// tun is the tunnel address: replies to it mean the flow is in the tunnel.
func (m *M) ctFlows(dsts map[string]bool, tun string, lan *net.IPNet) []Flow {
	data, err := m.env.ReadFile("/proc/net/nf_conntrack")
	if err != nil {
		return nil
	}
	var out []Flow
	for _, l := range lines(string(data)) {
		f := strings.Fields(l)
		if len(f) < 4 || f[0] != "ipv4" {
			continue
		}
		o, r := map[string]string{}, map[string]string{}
		st, unreplied, side := "-", false, 0
		if f[2] == "tcp" && len(f) > 5 {
			st = f[5]
		}
		for _, t := range f[3:] {
			if t == "[UNREPLIED]" {
				unreplied = true
				continue
			}
			k, v, ok := strings.Cut(t, "=")
			if !ok || k == "" {
				continue
			}
			if k == "src" {
				side++
			}
			if side == 1 {
				if _, seen := o[k]; !seen {
					o[k] = v
				}
			} else if side == 2 {
				if _, seen := r[k]; !seen {
					r[k] = v
				}
			}
		}
		if !private(o["src"]) || private(o["dst"]) {
			continue
		}
		if lan != nil && !lan.Contains(net.ParseIP(o["src"])) {
			continue
		}
		if dsts != nil && !dsts[o["dst"]] {
			continue
		}
		via := "wan"
		if tun != "" && r["dst"] == tun {
			via = "tun"
		}
		out = append(out, Flow{Client: o["src"], Dst: o["dst"], Proto: f[2], Dport: dash(o["dport"]), Via: via,
			Replied: !unreplied, OBytes: dash(o["bytes"]), RBytes: dash(r["bytes"]), State: st, Sport: dash(o["sport"])})
	}
	return out
}

// ctV6Count counts LAN IPv6 flows to global addresses (they bypass an
// IPv4-only tunnel).
func (m *M) ctV6Count() int {
	data, err := m.env.ReadFile("/proc/net/nf_conntrack")
	if err != nil {
		return 0
	}
	n := 0
	for _, l := range lines(string(data)) {
		f := strings.Fields(l)
		if len(f) == 0 || f[0] != "ipv6" {
			continue
		}
		for _, t := range f[3:] {
			if d, ok := strings.CutPrefix(t, "dst="); ok {
				if !strings.HasPrefix(d, "fe") && !strings.HasPrefix(d, "ff") && !strings.HasPrefix(d, "0000") {
					n++
				}
				break
			}
		}
	}
	return n
}

// baseDomain is the last two labels of d.
func baseDomain(d string) string {
	p := strings.Split(d, ".")
	if len(p) > 2 {
		return p[len(p)-2] + "." + p[len(p)-1]
	}
	return d
}

func (m *M) routeCheck(args []string) error {
	if len(args) != 1 {
		return fail("Укажите один домен: awg-monitor route-check example.com")
	}
	d, ok := domNorm(args[0])
	if !ok {
		return fail("Некорректный домен: '%s'.", args[0])
	}
	if !m.env.Have("ndmc") {
		return fail("ndmc не найден — команда работает только на Keenetic.")
	}
	rt := m.routing()
	group, include, hit := rt.Cover(d)
	m.say("Домен: %s", d)
	switch {
	case len(rt.Groups) == 0:
		m.say("1. Список: не найдено групп с маршрутом в %s (dns-proxy route object-group ... %s).", m.c.NdmIface, m.c.NdmIface)
	case hit:
		m.say("1. Список: покрыт — группа %s, запись include %s", group, include)
	default:
		m.say("1. Список: НЕ покрыт ни одной группой (%s) — трафик пойдёт мимо туннеля.", strings.Join(rt.Groups, " "))
	}

	addrs, _ := m.net.Resolve(bgctx(), d, m.c.DNSServer)
	var v4, v6 []string
	for _, a := range addrs {
		if strings.Contains(a, ":") {
			v6 = append(v6, a)
		} else {
			v4 = append(v4, a)
		}
	}
	if len(addrs) > 0 {
		m.say("2. DNS роутера (%s): IPv4: %s; IPv6: %s", m.c.DNSServer, dashText(strings.Join(v4, " "), "нет"), dashText(strings.Join(v6, " "), "нет"))
	} else {
		m.say("2. DNS роутера (%s): имя не разрешилось.", m.c.DNSServer)
	}

	var missing []string
	if hit && len(v4) > 0 {
		// Give ndnproxy a moment to apply the answer to the runtime list.
		m.env.Sleep(1e9)
		listed := map[string]string{}
		fqdn := ""
		for _, l := range lines(m.ndmc("show object-group fqdn " + group)) {
			f := strings.Fields(l)
			if len(f) < 2 {
				continue
			}
			switch f[0] {
			case "fqdn:":
				fqdn = f[1]
			case "address:":
				if _, seen := listed[f[1]]; !seen {
					listed[f[1]] = fqdn
				}
			}
		}
		for _, ip := range v4 {
			if f, ok := listed[ip]; ok {
				m.say("3. %s — в runtime-списке группы %s (как %s)", ip, group, f)
			} else {
				m.say("3. %s — НЕТ в runtime-списке группы %s", ip, group)
				missing = append(missing, ip)
			}
		}
	}

	tun, _ := m.net.IfaceAddr(m.c.Iface)
	wanFlows := 0
	if len(v4) == 0 {
		m.say("4. Соединения: нечего проверять — нет IPv4-адресов.")
	} else {
		dsts := map[string]bool{}
		for _, ip := range v4 {
			dsts[ip] = true
		}
		flows := m.ctFlows(dsts, tun, nil)
		if len(flows) > 0 {
			m.say("4. Текущие соединения устройств к этим IP (адрес туннеля %s):", dashText(tun, "?"))
			for _, f := range flows {
				via := "ПРОВАЙДЕРА"
				if f.Via == "tun" {
					via = "ТУННЕЛЬ"
				} else {
					wanFlows++
				}
				reply := "НЕТ"
				if f.Replied {
					reply = "есть"
				}
				st := ""
				if f.State != "-" {
					st = ", " + f.State
				}
				m.say("   %s -> %s %s/%s через %s, ответ %s, байт %s/%s%s", f.Client, f.Dst, f.Proto, f.Dport, via, reply, f.OBytes, f.RBytes, st)
			}
		} else {
			m.say("4. Текущих соединений устройств к этим IP нет (откройте сайт на устройстве и повторите).")
		}
	}

	r := m.domRun([]string{d})[0]
	m.say("5. Проверка с роутера: напрямую — %s; туннель — %s", domText(r.W), domText(r.T))

	v6n := m.ctV6Count()
	m.say("")
	m.say("Итог:")
	if len(rt.Groups) > 0 && !hit {
		if b := baseDomain(d); b == d {
			m.say("  Домен не в списке. Добавьте в группу: include %s", d)
		} else {
			m.say("  Домен не в списке. Добавьте в группу: include %s (или %s).", b, d)
		}
	}
	if len(missing) > 0 {
		m.say("  IP %s не попали в список: проверьте, что устройство спрашивает DNS роутера.", strings.Join(missing, " "))
	}
	if hit && wanFlows > 0 {
		m.say("  Домен в списке, но соединения идут мимо туннеля: устройство получило IP раньше (кэш DNS —")
		m.say("  переподключите устройство / ipconfig /flushdns) или берёт его из своего DNS (DoH, Private DNS).")
	}
	if len(v6) > 0 && v6n > 0 {
		m.say("  У домена есть IPv6, а в сети %s IPv6-соединений в интернет: туннель только IPv4,", strconv.Itoa(v6n))
		m.say("  такой трафик идёт мимо. Отключите IPv6 в домашней сети или для этих устройств.")
	}
	m.say("  Шаг 4 видит только IP, полученные с роутера сейчас; у CDN устройство могло получить другие.")
	return nil
}
