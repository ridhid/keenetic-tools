package monitor

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"github.com/asiforis/keenetic-tools/internal/sys"
)

// capPID returns the PID of our running tcpdump, 0 if none.
func (m *M) capPID() int {
	pid := m.env.ReadPID(CapPID)
	if pid <= 0 {
		return 0
	}
	cmd, err := m.env.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline")
	if err != nil || !strings.Contains(string(cmd), "tcpdump") {
		return 0
	}
	return pid
}

func (m *M) capStop() {
	if pid := m.capPID(); pid != 0 {
		m.env.Kill(pid, syscall.SIGTERM)
	}
	for _, f := range []string{CapPID, CapLog, CapLog + ".work"} {
		m.env.Remove(f)
	}
}

// capClean removes all RAM state of the DNS capture.
func (m *M) capClean() {
	m.capStop()
	for _, f := range []string{DNSMap, CTSeen, Includes, CapPID + ".missing"} {
		m.env.Remove(f)
	}
}

func (m *M) size(path string) int64 {
	fi, err := os.Stat(m.env.P(path))
	if err != nil {
		return 0
	}
	return fi.Size()
}

// capEnsure keeps tcpdump capturing DNS answers to LAN clients; -c bounds
// RAM if collect stops running.
func (m *M) capEnsure() {
	if m.capPID() != 0 {
		if m.size(CapLog) <= capMax {
			return
		}
		m.capStop()
		m.log("захват DNS: файл вырос сверх лимита, перезапуск")
	}
	// Append mode: after the harvest truncates the file tcpdump writes from the start.
	f, err := os.OpenFile(m.env.P(CapLog), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	pid, err := m.env.Run.Start(sys.Cmd{Name: "tcpdump",
		Args:   []string{"-l", "-n", "-t", "-vv", "-s", "1500", "-c", "20000", "-i", m.c.LanIface, "udp src port 53"},
		Stdout: f,
	})
	if err != nil {
		m.log("захват DNS: " + err.Error())
		return
	}
	m.env.WriteAtomic(CapPID, []byte(strconv.Itoa(pid)+"\n"), 0o644)
}

// parseDNS reads tcpdump -vv lines with A answers to LAN clients and
// returns "ip domain client" records.
func parseDNS(text string) [][3]string {
	var out [][3]string
	for _, l := range lines(text) {
		if !strings.Contains(l, " q: ") {
			continue
		}
		t := strings.Fields(l)
		client, qname, q := "", "", -1
		for i := 0; i < len(t); i++ {
			if t[i] == ">" && client == "" && i+1 < len(t) {
				client = strings.TrimSuffix(t[i+1], ":")
				if j := strings.LastIndexByte(client, '.'); j >= 0 && isNum(client[j+1:]) {
					client = client[:j]
				}
			}
			if t[i] == "q:" {
				if i+2 >= len(t) || t[i+1] != "A?" {
					break
				}
				qname = strings.TrimSuffix(strings.ToLower(t[i+2]), ".")
				q = i + 3
				break
			}
		}
		if qname == "" {
			continue
		}
		for j := q; j < len(t)-1; j++ {
			if t[j] == "A" {
				out = append(out, [3]string{strings.TrimSuffix(t[j+1], ","), qname, client})
			}
		}
	}
	return out
}

// dnsHarvest moves captured answers into DNSMap: "ts ip domain client",
// the latest per IP, for the last 6 hours.
func (m *M) dnsHarvest(now int64) map[string]string {
	type rec struct{ ts, domain, client string }
	recs := map[string]rec{}
	from := now - 6*3600
	if data, err := m.env.ReadFile(DNSMap); err == nil {
		for _, l := range lines(string(data)) {
			f := strings.Fields(l)
			if len(f) == 4 {
				if ts, err := strconv.ParseInt(f[0], 10, 64); err == nil && ts >= from {
					recs[f[1]] = rec{f[0], f[2], f[3]}
				}
			}
		}
	}
	if m.size(CapLog) > 0 {
		data, _ := m.env.ReadFile(CapLog)
		// Truncate in place: tcpdump appends, so it continues at offset 0.
		if f, err := os.OpenFile(m.env.P(CapLog), os.O_WRONLY|os.O_TRUNC, 0); err == nil {
			f.Close()
		}
		ts := strconv.FormatInt(now, 10)
		for _, r := range parseDNS(string(data)) {
			recs[r[0]] = rec{ts, r[1], r[2]}
		}
	}
	ips := make([]string, 0, len(recs))
	for ip := range recs {
		ips = append(ips, ip)
	}
	sort.Strings(ips)
	var b strings.Builder
	domains := map[string]string{}
	for _, ip := range ips {
		r := recs[ip]
		fmt.Fprintf(&b, "%s %s %s %s\n", r.ts, ip, r.domain, r.client)
		domains[ip] = r.domain
	}
	m.env.WriteAtomic(DNSMap, []byte(b.String()), 0o600)
	return domains
}

// includes returns the tunnel group includes, refreshed every 10 minutes.
func (m *M) includes(min int64) map[string]bool {
	var list []string
	if data, err := m.env.ReadFile(Includes); err == nil && len(data) > 0 && min%10 != 0 {
		list = lines(string(data))
	} else {
		list = m.routing().TunnelIncludes()
		text := strings.Join(list, "\n")
		if text != "" {
			text += "\n"
		}
		m.env.WriteAtomic(Includes, []byte(text), 0o600)
	}
	set := map[string]bool{}
	for _, l := range list {
		set[l] = true
	}
	return set
}

func covered(d string, inc map[string]bool) bool {
	for d != "" {
		if inc[d] {
			return true
		}
		_, rest, ok := strings.Cut(d, ".")
		if !ok {
			break
		}
		d = rest
	}
	return false
}

// flowStage tells why a flow is suspicious; "" means it is not.
func (m *M) flowStage(f Flow) string {
	switch {
	case f.Dport == "853":
		return "dot"
	case f.Dport == "443" && contains(dohIPs, f.Dst):
		return "doh"
	case !f.Replied && f.Proto == "udp":
		return "noreply_udp"
	case !f.Replied:
		return "noreply"
	}
	ob, _ := strconv.Atoi(f.OBytes)
	rb, _ := strconv.Atoi(f.RBytes)
	if m.c.MissedStall && f.Proto == "tcp" && f.State == "ESTABLISHED" && f.Dport == "443" && ob >= 600 && rb < 400 {
		return "stall"
	}
	return ""
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// missedCollect is one minute of the bypass search: log new failed or
// suspicious LAN flows, then probe new domains outside the tunnel lists.
func (m *M) missedCollect(s *Sample) {
	if !m.env.Have("tcpdump") {
		if !m.env.Exists(CapPID + ".missing") {
			m.env.WriteAtomic(CapPID+".missing", nil, 0o644)
			m.log("DNS_CAPTURE=1, но tcpdump не установлен: opkg install tcpdump")
		}
		return
	}
	m.env.Remove(CapPID + ".missing")
	m.capEnsure()
	dom := m.dnsHarvest(s.Now)
	inc := m.includes(s.Min)
	seen := map[string]bool{}
	if data, err := m.env.ReadFile(CTSeen); err == nil {
		for _, l := range lines(string(data)) {
			seen[l] = true
		}
	}
	tun, _ := m.net.IfaceAddr(m.c.Iface)
	_, lan := m.net.IfaceAddr(m.c.LanIface)
	var log, keys strings.Builder
	var fresh []string
	for _, f := range m.ctFlows(nil, tun, lan) {
		stage := m.flowStage(f)
		if stage == "" {
			continue
		}
		key := f.Client + ":" + f.Sport + ">" + f.Dst + ":" + f.Dport + "/" + f.Proto
		keys.WriteString(key + "\n")
		if seen[key] {
			continue
		}
		seen[key] = true
		d, cov := "-", "-"
		if v, ok := dom[f.Dst]; ok {
			d, cov = v, "no"
			if covered(d, inc) {
				cov = "yes"
			}
		}
		fmt.Fprintf(&log, "%s ts=%d kind=flow client=%s ip=%s proto=%s dport=%s via=%s stage=%s domain=%s covered=%s\n",
			s.Stamp, s.Now, f.Client, f.Dst, f.Proto, f.Dport, f.Via, stage, d, cov)
		if f.Via == "wan" && cov == "no" && stage != "doh" && stage != "dot" {
			fresh = append(fresh, d)
		}
	}
	m.env.WriteAtomic(CTSeen, []byte(keys.String()), 0o600)
	out := m.path("missed/" + s.Stamp[:10] + ".log")
	if log.Len() > 0 {
		m.appendFile(out, log.String())
	}
	if len(fresh) == 0 || m.c.MissedProbes <= 0 {
		return
	}

	cachePath := m.path("missed.cache")
	cache := map[string]string{}
	var order []string
	recent := map[string]bool{}
	from := s.Now - int64(m.c.MissedRecheck)*3600
	if data, err := m.env.ReadFile(cachePath); err == nil {
		for _, l := range lines(string(data)) {
			f := strings.Fields(l)
			if len(f) < 2 {
				continue
			}
			if _, ok := cache[f[0]]; !ok {
				order = append(order, f[0])
			}
			cache[f[0]] = l
			if ts, err := strconv.ParseInt(f[1], 10, 64); err == nil && ts >= from {
				recent[f[0]] = true
			}
		}
	}
	sort.Strings(fresh)
	var list []string
	for i, d := range fresh {
		if (i > 0 && d == fresh[i-1]) || recent[d] {
			continue
		}
		if len(list) == m.c.MissedProbes {
			break
		}
		list = append(list, d)
	}
	if len(list) == 0 {
		return
	}
	log.Reset()
	for _, r := range m.domRun(list) {
		ws, _, wt := fetchFields(r.W)
		ts, _, tt := fetchFields(r.T)
		fmt.Fprintf(&log, "%s ts=%d kind=probe domain=%s wan=%s wan_t=%s tun=%s tun_t=%s\n", s.Stamp, s.Now, r.Domain, ws, wt, ts, tt)
		if _, ok := cache[r.Domain]; !ok {
			order = append(order, r.Domain)
		}
		cache[r.Domain] = fmt.Sprintf("%s %d %s %s", r.Domain, s.Now, ws, ts)
		if !domOK(ws) && ts == "ok" {
			m.event(fmt.Sprintf("missed_candidate domain=%s wan=%s tun=ok", r.Domain, ws))
		}
	}
	m.appendFile(out, log.String())
	var b strings.Builder
	for _, d := range order {
		b.WriteString(cache[d] + "\n")
	}
	m.env.WriteAtomic(cachePath, []byte(b.String()), 0o644)
}
