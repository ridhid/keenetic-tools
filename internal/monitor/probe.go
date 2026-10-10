package monitor

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"math"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/asiforis/keenetic-tools/internal/sys"
)

// State is the collect state kept between runs in LOG_DIR/state.
type State struct {
	State, Cause, Since, Ts, PID, Ticks, Rx, Tx, Ep, Hash string
}

func (m *M) readState() State {
	var s State
	data, err := m.env.ReadFile(m.path("state"))
	if err != nil {
		return s
	}
	for _, line := range strings.Split(string(data), "\n") {
		k, v, _ := strings.Cut(line, "=")
		switch k {
		case "state":
			s.State = v
		case "cause":
			s.Cause = v
		case "since":
			s.Since = v
		case "ts":
			s.Ts = v
		case "pid":
			s.PID = v
		case "ticks":
			s.Ticks = v
		case "rx":
			s.Rx = v
		case "tx":
			s.Tx = v
		case "ep":
			s.Ep = v
		case "hash":
			s.Hash = v
		}
	}
	return s
}

func (m *M) writeState(s State) error {
	text := fmt.Sprintf("state=%s\ncause=%s\nsince=%s\nts=%s\npid=%s\nticks=%s\nrx=%s\ntx=%s\nep=%s\nhash=%s\n",
		s.State, s.Cause, s.Since, s.Ts, s.PID, s.Ticks, s.Rx, s.Tx, s.Ep, s.Hash)
	return m.env.WriteAtomic(m.path("state"), []byte(text), 0o644)
}

// Sample is one measurement. Fields hold their log form; "-" means unknown.
type Sample struct {
	Stamp   string // local "2006-01-02 15:04:05"
	Now     int64
	Min     int64
	PID     string
	RSS     string
	Ticks   string
	CPU     string
	IfUp    bool
	MTU     string
	HSAge   string
	Rx, Tx  string
	RxD     string
	TxD     string
	EP      string
	Tun     string
	TunLoss int
	TunRTT  string
	TunBest int
	HTTP    string
	EPLoss  string
	EPRTT   string
	Load    string
	Mem     string
	CT      string
	State   string
	Cause   string
}

func isNum(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// delta is a counter increase, "-" when unknown or reset.
func delta(a, b string) string {
	x, err1 := strconv.ParseUint(a, 10, 64)
	y, err2 := strconv.ParseUint(b, 10, 64)
	if err1 != nil || err2 != nil || x < y {
		return "-"
	}
	return strconv.FormatUint(x-y, 10)
}

// Line is the samples/DAY.log record; report and status read it, so new
// keys may be added but the meaning of existing ones must not change.
func (s *Sample) Line() string {
	return fmt.Sprintf("%s ts=%d state=%s cause=%s hs_age=%s rx_d=%s tx_d=%s "+
		"tun=%s tun_loss=%d tun_rtt=%s http=%s ep=%s ep_loss=%s ep_rtt=%s "+
		"pid=%s rss_kb=%s cpu=%s mtu=%s load=%s mem_kb=%s ct=%s",
		s.Stamp, s.Now, s.State, s.Cause, s.HSAge, s.RxD, s.TxD,
		s.Tun, s.TunLoss, s.TunRTT, s.HTTP, s.EP, s.EPLoss, s.EPRTT,
		s.PID, s.RSS, s.CPU, s.MTU, s.Load, s.Mem, s.CT)
}

const stampFormat = "2006-01-02 15:04:05"

// probe measures the tunnel; forceHTTP runs the HTTP check regardless of HTTP_EVERY.
func (m *M) probe(prev State, forceHTTP bool) *Sample {
	now := m.env.Now()
	s := &Sample{Stamp: now.Format(stampFormat), Now: now.Unix(), Min: now.Unix() / 60}

	s.PID, s.RSS, s.Ticks = "-", "-", ""
	if pid := m.findProcess(); pid != "" {
		s.PID = pid
		if st, err := m.env.ReadFile("/proc/" + pid + "/stat"); err == nil {
			s.Ticks = procTicks(string(st))
		}
		if st, err := m.env.ReadFile("/proc/" + pid + "/status"); err == nil {
			s.RSS = dash(field(string(st), "VmRSS:"))
		}
	}
	s.CPU = "-"
	// CPU% over the last interval, assuming USER_HZ=100.
	if s.PID != "-" && s.PID == prev.PID && isNum(s.Ticks) && isNum(prev.Ticks) && isNum(prev.Ts) {
		t, _ := strconv.ParseInt(s.Ticks, 10, 64)
		pt, _ := strconv.ParseInt(prev.Ticks, 10, 64)
		pts, _ := strconv.ParseInt(prev.Ts, 10, 64)
		if s.Now > pts && t >= pt {
			s.CPU = strconv.FormatInt((t-pt)/(s.Now-pts), 10)
		}
	}

	s.MTU = "-"
	if fi, err := os.Stat(m.env.P("/sys/class/net/" + m.c.Iface)); err == nil && fi.IsDir() {
		s.IfUp = true
		if b, err := m.env.ReadFile("/sys/class/net/" + m.c.Iface + "/mtu"); err == nil {
			s.MTU = dash(strings.TrimSpace(string(b)))
		}
	}

	hs := ""
	s.EP = "-"
	if s.IfUp && m.env.Have("awg") {
		hs = m.awgField("latest-handshakes", 1)
		s.Rx = m.awgField("transfer", 1)
		s.Tx = m.awgField("transfer", 2)
		s.EP = m.awgField("endpoints", 1)
	}
	if !isNum(s.Rx) {
		s.Rx = ""
	}
	if !isNum(s.Tx) {
		s.Tx = ""
	}
	if s.EP == "" || s.EP == "(none)" {
		s.EP = "-"
	}
	switch {
	case hs == "0":
		s.HSAge = "never"
	case isNum(hs):
		h, _ := strconv.ParseInt(hs, 10, 64)
		s.HSAge = strconv.FormatInt(s.Now-h, 10)
	default:
		s.HSAge = "-"
	}
	s.RxD, s.TxD = "-", "-"
	if s.PID == prev.PID {
		s.RxD, s.TxD = delta(s.Rx, prev.Rx), delta(s.Tx, prev.Tx)
	}

	var tun []string
	lossSum, best, rttSum, rttN := 0.0, math.Inf(1), 0.0, 0
	if s.IfUp {
		for _, t := range m.c.PingTargets {
			loss, rtt := m.ping(t, m.c.Iface)
			tun = append(tun, loss+"/"+rtt)
			l, _ := strconv.ParseFloat(loss, 64)
			lossSum += l
			best = math.Min(best, l)
			if rtt != "-" {
				r, _ := strconv.ParseFloat(rtt, 64)
				rttSum += r
				rttN++
			}
		}
	}
	if len(tun) == 0 {
		s.Tun, s.TunLoss, s.TunRTT, s.TunBest = "-", 100, "-", 100
	} else {
		s.Tun = strings.Join(tun, ",")
		s.TunLoss = int(math.Round(lossSum / float64(len(tun))))
		s.TunRTT = "-"
		if rttN > 0 {
			s.TunRTT = fmt.Sprintf("%.1f", rttSum/float64(rttN))
		}
		s.TunBest = int(best)
	}

	s.HTTP = "-"
	if m.c.HTTPURL != "" && s.IfUp {
		if s.TunBest >= 100 || forceHTTP || (m.c.HTTPEvery > 0 && s.Min%int64(m.c.HTTPEvery) == 0) {
			f := m.net.Fetch(context.Background(), m.c.HTTPURL, m.c.Iface, 8*time.Second)
			s.HTTP = fmt.Sprintf("%03d:%.3f", f.Code, f.Secs)
		}
	}

	s.EPLoss, s.EPRTT = "-", "-"
	if s.EP != "-" {
		s.EPLoss, s.EPRTT = m.ping(epHost(s.EP), "")
	}

	s.Load = "-"
	if b, err := m.env.ReadFile("/proc/loadavg"); err == nil {
		if f := strings.Fields(string(b)); len(f) > 0 {
			s.Load = f[0]
		}
	}
	s.Mem = "-"
	if b, err := m.env.ReadFile("/proc/meminfo"); err == nil {
		if v := field(string(b), "MemAvailable:"); v != "" {
			s.Mem = v
		} else {
			s.Mem = dash(field(string(b), "MemFree:"))
		}
	}
	s.CT = "-"
	if b, err := m.env.ReadFile("/proc/sys/net/netfilter/nf_conntrack_count"); err == nil {
		s.CT = dash(strings.TrimSpace(string(b)))
	}
	return s
}

// field returns the first value after a "Key:" line prefix.
func field(text, key string) string {
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, key) {
			if f := strings.Fields(line[len(key):]); len(f) > 0 {
				return f[0]
			}
		}
	}
	return ""
}

// procTicks is utime+stime from /proc/PID/stat.
func procTicks(stat string) string {
	// The command name may contain spaces; fields start after ")".
	i := strings.LastIndexByte(stat, ')')
	if i < 0 {
		return ""
	}
	f := strings.Fields(stat[i+1:])
	// f[0] is field 3 (state); utime and stime are fields 14 and 15.
	if len(f) < 13 {
		return ""
	}
	u, err1 := strconv.ParseInt(f[11], 10, 64)
	st, err2 := strconv.ParseInt(f[12], 10, 64)
	if err1 != nil || err2 != nil {
		return ""
	}
	return strconv.FormatInt(u+st, 10)
}

// findProcess returns the PID of `amneziawg-go IFACE`, matching argv
// exactly rather than a substring (a shell running pgrep would match too).
func (m *M) findProcess() string {
	dirs, err := os.ReadDir(m.env.P("/proc"))
	if err != nil {
		return ""
	}
	best := 0
	for _, d := range dirs {
		pid, err := strconv.Atoi(d.Name())
		if err != nil {
			continue
		}
		b, err := m.env.ReadFile("/proc/" + d.Name() + "/cmdline")
		if err != nil {
			continue
		}
		argv := strings.Split(strings.TrimRight(string(b), "\x00"), "\x00")
		if len(argv) >= 2 && filepath.Base(argv[0]) == "amneziawg-go" && argv[1] == m.c.Iface {
			if best == 0 || pid < best {
				best = pid
			}
		}
	}
	if best == 0 {
		return ""
	}
	return strconv.Itoa(best)
}

// awgField returns column col of the first line of `awg show IFACE what`.
func (m *M) awgField(what string, col int) string {
	out, err := m.env.Output(10*time.Second, "awg", "show", m.c.Iface, what)
	if err != nil {
		return ""
	}
	line, _, _ := strings.Cut(string(out), "\n")
	f := strings.Fields(line)
	if col < len(f) {
		return f[col]
	}
	return ""
}

// epHost strips the port and IPv6 brackets from an endpoint.
func epHost(ep string) string {
	if h, _, err := net.SplitHostPort(ep); err == nil {
		return h
	}
	return strings.Trim(ep, "[]")
}

// ping returns loss percent and average RTT ("-" if none) for a target,
// through device iface unless it is empty.
func (m *M) ping(target, iface string) (loss, rtt string) {
	args := []string{"-c", strconv.Itoa(m.c.PingCount), "-w", strconv.Itoa(m.c.PingCount + 2)}
	if iface != "" {
		args = append(args, "-I", iface)
	}
	args = append(args, target)
	r := m.env.Run.Run(context.Background(), sys.Cmd{Name: "ping", Args: args, Timeout: time.Duration(m.c.PingCount+5) * time.Second})
	return pingStats(r.Output)
}

// pingStats reads BusyBox or iputils ping output.
func pingStats(out []byte) (loss, rtt string) {
	loss, rtt = "100", "-"
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		line := sc.Text()
		if strings.Contains(line, "packet loss") {
			for _, f := range strings.Fields(line) {
				if i := strings.IndexByte(f, '%'); i >= 0 {
					loss = f[:i]
				}
			}
		}
		if strings.Contains(line, "min/avg/max") {
			_, v, ok := strings.Cut(line, "=")
			if p := strings.Split(strings.TrimSpace(v), "/"); ok && len(p) > 1 {
				rtt = p[1]
			}
		}
	}
	return loss, rtt
}

// classify sets State and Cause.
func (m *M) classify(s *Sample) {
	s.State, s.Cause = "OK", "-"
	httpOK := len(s.HTTP) >= 4 && s.HTTP[3] == ':' && (s.HTTP[0] == '2' || s.HTTP[0] == '3')
	if s.TunBest < 100 || httpOK {
		rtt, err := strconv.ParseFloat(s.TunRTT, 64)
		switch {
		case s.TunLoss > 0:
			s.State, s.Cause = "DEGRADED", "packet_loss"
		case err == nil && rtt > m.c.RTTWarn:
			s.State, s.Cause = "DEGRADED", "high_rtt"
		case s.HTTP != "-" && !httpOK:
			s.State, s.Cause = "DEGRADED", "http_fail"
		}
		return
	}
	s.State = "DOWN"
	age, err := strconv.Atoi(s.HSAge)
	switch {
	case s.PID == "-":
		s.Cause = "process_dead"
	case !s.IfUp:
		s.Cause = "iface_missing"
	case err != nil:
		s.Cause = "handshake_never"
	case age > m.c.HandshakeStale:
		if s.EPLoss == "100" || strings.HasPrefix(s.EPLoss, "100.") {
			s.Cause = "endpoint_unreachable"
		} else {
			s.Cause = "handshake_stale"
		}
	default:
		s.Cause = "tunnel_no_traffic"
	}
}
