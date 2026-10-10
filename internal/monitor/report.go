package monitor

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// period parses "24h" / "7d" (default 24h) into the time window.
func (m *M) period(p string) (name string, from, to int64, err error) {
	if p == "" {
		p = "24h"
	}
	mult := int64(0)
	switch {
	case strings.HasSuffix(p, "h"):
		mult = 3600
	case strings.HasSuffix(p, "d"):
		mult = 86400
	}
	n, perr := strconv.ParseInt(p[:max(len(p)-1, 0)], 10, 64)
	if mult == 0 || perr != nil || n <= 0 || !isNum(p[:len(p)-1]) {
		return "", 0, 0, fail("Период задаётся как 24h или 7d.")
	}
	to = m.env.Now().Unix()
	return p, to - n*mult, to, nil
}

// record is a log line: date, time and key=value fields.
type record struct {
	date, time string
	f          map[string]string
	ts         int64
}

func parseRecord(l string) (record, bool) {
	f := strings.Fields(l)
	if len(f) < 3 {
		return record{}, false
	}
	r := record{date: f[0], time: f[1], f: map[string]string{}}
	for _, kv := range f[2:] {
		if k, v, ok := strings.Cut(kv, "="); ok && k != "" {
			r.f[k] = v
		}
	}
	r.ts, _ = strconv.ParseInt(r.f["ts"], 10, 64)
	return r, true
}

// logFiles lists DIR/*.log in the log directory, oldest first.
func (m *M) logFiles(dir string) []string {
	files, _ := filepath.Glob(filepath.Join(m.env.P(m.path(dir)), "*.log"))
	sort.Strings(files)
	return files
}

// records reads DIR/*.log within [from, to].
func (m *M) records(dir string, from, to int64) []record {
	var out []record
	for _, file := range m.logFiles(dir) {
		data, err := os.ReadFile(file)
		if err != nil {
			continue
		}
		for _, l := range lines(string(data)) {
			if r, ok := parseRecord(l); ok && r.ts >= from && r.ts <= to {
				out = append(out, r)
			}
		}
	}
	return out
}

// events reads events.log lines since from: "date time ts=N name args...".
func (m *M) events(from int64) [][]string {
	data, err := m.env.ReadFile(m.path("events.log"))
	if err != nil {
		return nil
	}
	var out [][]string
	for _, l := range lines(string(data)) {
		f := strings.Fields(l)
		if len(f) < 4 {
			continue
		}
		ts, _ := strconv.ParseInt(strings.TrimPrefix(f[2], "ts="), 10, 64)
		if ts >= from {
			out = append(out, f)
		}
	}
	return out
}

func known(v string) bool { return v != "" && v != "-" }

func atof(s string) float64 {
	f, _ := strconv.ParseFloat(s, 64)
	return f
}

func (m *M) report(p string) error {
	if err := m.requireLogdir(); err != nil {
		return err
	}
	return m.writeReport(m.env.Stdout, p)
}

func (m *M) writeReport(w io.Writer, p string) error {
	name, from, to, err := m.period(p)
	if err != nil {
		return err
	}
	if len(m.logFiles("samples")) == 0 {
		return fail("Замеров пока нет. Первый появится в течение минуты после установки.")
	}
	recs := m.records("samples", from, to)
	if len(recs) == 0 {
		fmt.Fprintf(w, "За период %s замеров нет.\n", name)
		return nil
	}
	type outage struct {
		start, end, cause string
		secs              int64
	}
	var (
		cnt                 = map[string]int{}
		why                 = map[string]int{}
		outages             []outage
		down                *outage
		downTs, pts         int64
		gaps, gapSec        int64
		lastPID             string
		restarts, scheduled int
		rtt, rttMax, loss   float64
		rttN, lossN         int
		rssFirst, rssLast   string
		rssMax              float64
		cpu, cpuMax         float64
		cpuN                int
		first, last         string
	)
	for i, r := range recs {
		st := r.f["state"]
		cnt[st]++
		if st != "OK" {
			why[st+": "+r.f["cause"]]++
		}
		stamp := r.date + " " + r.time[:min(5, len(r.time))]
		if i == 0 {
			first = stamp
		}
		if pts != 0 && r.ts-pts > 150 {
			gaps++
			gapSec += r.ts - pts - 60
		}
		if st == "DOWN" && down == nil {
			down = &outage{start: stamp, cause: r.f["cause"]}
			downTs = r.ts
		} else if st != "DOWN" && down != nil {
			down.end, down.secs = stamp, r.ts-downTs
			outages = append(outages, *down)
			down = nil
		}
		if p := r.f["pid"]; known(p) {
			if lastPID != "" && p != lastPID {
				restarts++
				if len(r.time) >= 5 {
					if mm := r.time[3:5]; mm == "01" || mm == "02" || mm == "03" {
						scheduled++
					}
				}
			}
			lastPID = p
		}
		if v := r.f["tun_rtt"]; known(v) {
			rtt += atof(v)
			rttN++
			rttMax = max(rttMax, atof(v))
		}
		if v := r.f["tun_loss"]; known(v) {
			loss += atof(v)
			lossN++
		}
		if v := r.f["rss_kb"]; known(v) {
			if rssFirst == "" {
				rssFirst = v
			}
			rssLast = v
			rssMax = max(rssMax, atof(v))
		}
		if v := r.f["cpu"]; known(v) {
			cpu += atof(v)
			cpuN++
			cpuMax = max(cpuMax, atof(v))
		}
		pts, last = r.ts, stamp
	}
	if down != nil {
		down.end, down.secs = last+" (продолжается)", pts+60-downTs
		outages = append(outages, *down)
	}
	n := float64(len(recs))
	fmt.Fprintf(w, "Период %s: %s — %s, замеров %d\n", name, first, last, len(recs))
	fmt.Fprintf(w, "OK %.1f%% | DEGRADED %.1f%% | DOWN %.1f%%\n", 100*float64(cnt["OK"])/n, 100*float64(cnt["DEGRADED"])/n, 100*float64(cnt["DOWN"])/n)
	if gaps > 0 {
		fmt.Fprintf(w, "Пропуски данных: %d (~%d мин) — роутер, диск или cron не работали\n", gaps, gapSec/60)
	}
	avgLoss, avgRTT, maxRTT := 0.0, "-", "-"
	if lossN > 0 {
		avgLoss = loss / float64(lossN)
	}
	if rttN > 0 {
		avgRTT, maxRTT = fmt.Sprintf("%.1f", rtt/float64(rttN)), fmt.Sprintf("%.1f", rttMax)
	}
	fmt.Fprintf(w, "Туннель: потери в среднем %.1f%%, RTT средний %s мс, максимальный %s мс\n", avgLoss, avgRTT, maxRTT)
	fmt.Fprintf(w, "amneziawg-go: перезапусков %d (в минуты :01–:03 — %d, похоже на плановый рестарт)\n", restarts, scheduled)
	if rssFirst != "" {
		fmt.Fprintf(w, "Память amneziawg-go: %s → %s КБ (максимум %d)\n", rssFirst, rssLast, int64(rssMax))
	}
	if cpuN > 0 {
		fmt.Fprintf(w, "CPU amneziawg-go: в среднем %.0f%%, максимум %d%%\n", cpu/float64(cpuN), int64(cpuMax))
	}
	if len(why) > 0 {
		fmt.Fprintln(w, "\nНе-OK замеры по причинам (1 замер ≈ 1 мин):")
		for _, k := range sortedKeys(why) {
			fmt.Fprintf(w, "  %-34s %d\n", k, why[k])
		}
	}
	if len(outages) > 0 {
		fmt.Fprintln(w, "\nСбои (DOWN), последние 30:")
		for _, o := range outages[max(len(outages)-30, 0):] {
			mins := max((o.secs+30)/60, 1)
			fmt.Fprintf(w, "  %s — %s  %d мин  %s\n", o.start, o.end, mins, o.cause)
		}
	} else {
		fmt.Fprintln(w, "\nСбоев (DOWN) не было.")
	}
	var eps, cfgs []string
	for _, e := range m.events(from) {
		switch {
		case e[3] == "endpoint_changed" && len(e) >= 6:
			eps = append(eps, "  "+strings.Join([]string{e[0], e[1], e[4], e[5]}, " "))
		case e[3] == "config_changed" && len(e) >= 5:
			cfgs = append(cfgs, "  "+strings.Join([]string{e[0], e[1], e[4]}, " "))
		}
	}
	if len(eps) > 0 {
		fmt.Fprintf(w, "\nСмена Endpoint: %d\n%s\n", len(eps), strings.Join(eps, "\n"))
	}
	if len(cfgs) > 0 {
		fmt.Fprintf(w, "\nИзменения конфигурации: %d\n%s\n(сравнить: awg-monitor diff)\n", len(cfgs), strings.Join(cfgs, "\n"))
	}
	return nil
}

func sortedKeys[V any](mp map[string]V) []string {
	keys := make([]string, 0, len(mp))
	for k := range mp {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func pct(ok, total int) string {
	if total == 0 {
		return "-"
	}
	return fmt.Sprintf("%.0f%%", 100*float64(ok)/float64(total))
}

func (m *M) domains(p string) error {
	if err := m.requireLogdir(); err != nil {
		return err
	}
	name, from, to, err := m.period(p)
	if err != nil {
		return err
	}
	if len(m.logFiles("domains")) == 0 {
		return fail("Проверок доменов пока нет. Задайте DOMAINS_WATCH в /opt/etc/awg-monitor.conf.")
	}
	type stat struct {
		n, wn, wok, tok int
		fail            map[string]int
		last            string
	}
	var order []string
	stats := map[string]*stat{}
	for _, r := range m.records("domains", from, to) {
		d := r.f["domain"]
		if d == "" {
			continue
		}
		s := stats[d]
		if s == nil {
			s = &stat{fail: map[string]int{}}
			stats[d] = s
			order = append(order, d)
		}
		s.n++
		w, t := r.f["wan"], r.f["tun"]
		if w != "nowan" {
			s.wn++
			if w == "ok" {
				s.wok++
			} else {
				s.fail["w "+w]++
			}
		}
		if t == "ok" {
			s.tok++
		} else {
			s.fail["t "+t]++
		}
		s.last = r.date + " " + r.time[:min(5, len(r.time))] + " напрямую=" + w + " туннель=" + t
	}
	out := m.env.Stdout
	if len(order) == 0 {
		fmt.Fprintf(out, "За период %s проверок доменов нет.\n", name)
	} else {
		fmt.Fprintf(out, "Период %s, доля успешных проверок:\n", name)
		for _, d := range order {
			s := stats[d]
			fmt.Fprintf(out, "\n%s — проверок %d\n", d, s.n)
			fmt.Fprintf(out, "  напрямую: %s%s\n", pct(s.wok, s.wn), failures(s.fail, "w"))
			fmt.Fprintf(out, "  туннель:  %s%s\n", pct(s.tok, s.n), failures(s.fail, "t"))
			fmt.Fprintf(out, "  последняя: %s\n", s.last)
		}
	}
	var changes []string
	for _, e := range m.events(from) {
		if e[3] == "domain_changed" && len(e) >= 7 {
			changes = append(changes, "  "+strings.Join([]string{e[0], e[1], e[4], e[5], e[6]}, " "))
		}
	}
	if len(changes) > 0 {
		fmt.Fprintf(out, "\nИзменения доступности: %d\n%s\n", len(changes), strings.Join(changes, "\n"))
	}
	return nil
}

// failures is " (сбои: dns 2, tls 1)" for one side, "" without failures.
func failures(fail map[string]int, side string) string {
	var parts []string
	for _, st := range []string{"dns", "tcp", "tls", "http", "cert", "iface", "err"} {
		if n := fail[side+" "+st]; n > 0 {
			parts = append(parts, fmt.Sprintf("%s %d", st, n))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return " (сбои: " + strings.Join(parts, ", ") + ")"
}

// set is an ordered set of words.
type set []string

func (s *set) add(v string) {
	if !contains(*s, v) {
		*s = append(*s, v)
	}
}

func (s set) String() string { return strings.Join(s, " ") }

func (m *M) missed(p string) error {
	if err := m.requireLogdir(); err != nil {
		return err
	}
	name, from, to, err := m.period(p)
	if err != nil {
		return err
	}
	if len(m.logFiles("missed")) == 0 {
		if !m.c.DNSCapture {
			return fail("Сбор выключен. Включите: opkg install tcpdump, затем awg-monitor enable capture.")
		}
		return fail("Данных пока нет — подождите несколько минут.")
	}
	var (
		probeW, probeT = map[string]string{}, map[string]string{}
		clients        = map[string]*set{}
		uncovered      = map[string]int{}
		coveredWan     = map[string]int{}
		noDNS          = map[string]int{}
		tunnel         = map[string]int{}
		dohN           = map[string]int{}
		dohDst         = map[string]*set{}
	)
	addTo := func(mp map[string]*set, k, v string) {
		if mp[k] == nil {
			mp[k] = &set{}
		}
		mp[k].add(v)
	}
	for _, r := range m.records("missed", from, to) {
		f := r.f
		switch f["kind"] {
		case "probe":
			probeW[f["domain"]], probeT[f["domain"]] = f["wan"], f["tun"]
			continue
		case "flow":
		default:
			continue
		}
		st, d, c := f["stage"], f["domain"], f["client"]
		switch {
		case st == "doh" || st == "dot":
			dohN[c]++
			addTo(dohDst, c, f["ip"]+":"+f["dport"])
		case f["via"] == "tun":
			k := d
			if d == "-" {
				k = f["ip"]
			}
			tunnel[k]++
			addTo(clients, k, c)
		case d == "-":
			k := f["ip"] + ":" + f["dport"] + "/" + f["proto"]
			noDNS[k]++
			addTo(clients, k, c)
		default:
			addTo(clients, d, c)
			if f["covered"] == "yes" {
				coveredWan[d]++
			} else {
				uncovered[d]++
			}
		}
	}
	out := m.env.Stdout
	top := func(cnt map[string]int, limit int, title string, extra bool) int {
		if len(cnt) == 0 {
			return 0
		}
		keys := sortedKeys(cnt)
		sort.SliceStable(keys, func(i, j int) bool { return cnt[keys[i]] > cnt[keys[j]] })
		fmt.Fprintln(out, "\n"+title)
		for _, k := range keys[:min(limit, len(keys))] {
			x := ""
			if _, ok := probeW[k]; extra && ok {
				x = "; проверка: напрямую " + probeW[k] + ", туннель " + probeT[k]
			}
			cl := ""
			if clients[k] != nil {
				cl = clients[k].String()
			}
			fmt.Fprintf(out, "  %-40s соединений %d, устройства: %s%s\n", k, cnt[k], cl, x)
		}
		if len(keys) > limit {
			fmt.Fprintf(out, "  … и ещё %d\n", len(keys)-limit)
		}
		return len(keys)
	}
	fmt.Fprintf(out, "Период %s.\n", name)
	cand, rest := map[string]int{}, map[string]int{}
	var bases set
	for _, d := range sortedKeys(uncovered) {
		if w, ok := probeW[d]; ok && w != "ok" && w != "nowan" && probeT[d] == "ok" {
			cand[d] = uncovered[d]
			bases.add(baseDomain(d))
		} else {
			rest[d] = uncovered[d]
		}
	}
	found := top(cand, 50, "1. Не в списке, напрямую не открываются, через туннель — да (добавить в список):", false)
	if len(cand) > 0 {
		fmt.Fprintln(out, "   Предлагается добавить в группу:")
		for _, b := range bases {
			fmt.Fprintf(out, "     include %s\n", b)
		}
	}
	found += top(rest, 30, "2. Не в списке, соединения мимо туннеля без ответа (не подтверждено проверкой):", true)
	found += top(coveredWan, 30, "3. В списке, но соединения пошли мимо туннеля (кэш DNS на устройстве, DoH, IPv6):", false)
	found += top(noDNS, 20, "4. Без ответа и без запроса к DNS роутера (DoH на устройстве или вшитые IP):", false)
	if len(dohN) > 0 {
		fmt.Fprintln(out, "\n5. Устройства со своим DNS (DoH/DoT) — их домены роутер не видит:")
		for _, c := range sortedKeys(dohN) {
			fmt.Fprintf(out, "  %s — соединений %d: %s\n", c, dohN[c], dohDst[c])
		}
	}
	found += top(tunnel, 10, "6. Через туннель без ответа (проблема туннеля или сервера):", false)
	if found == 0 && len(dohN) == 0 {
		fmt.Fprintln(out, "Соединений мимо туннеля без ответа не найдено.")
	}
	if m.c.DNSCapture && m.capPID() == 0 {
		fmt.Fprintln(out, "\nВнимание: захват DNS сейчас не запущен (появится в течение минуты; нужен tcpdump).")
	}
	return nil
}
