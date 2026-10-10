package monitor

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// appendFile appends text to a router file, creating it and its directory.
func (m *M) appendFile(path, text string) error {
	real := m.env.P(path)
	if err := os.MkdirAll(filepath.Dir(real), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(real, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	_, err = f.WriteString(text)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// event appends to events.log and the system log.
func (m *M) event(msg string) {
	now := m.env.Now()
	m.appendFile(m.path("events.log"), fmt.Sprintf("%s ts=%d %s\n", now.Format(stampFormat), now.Unix(), msg))
	m.log(msg)
}

// collect is one cron run: measure, log, react to changes.
func (m *M) collect() error {
	if !m.logdirOK() {
		// Disk missing: do not write into an empty mount point; warn once.
		m.capStop()
		if !m.env.Exists(NoDirFlag) {
			m.env.WriteAtomic(NoDirFlag, nil, 0o644)
			m.log(fmt.Sprintf("каталог логов '%s' недоступен, замеры не пишутся", dashText(m.c.LogDir, "не задан")))
		}
		return nil
	}
	m.env.Remove(NoDirFlag)
	lock, err := m.env.TryLock(LockFile)
	if err != nil {
		return err
	}
	if lock == nil {
		return nil
	}
	defer lock.Release()

	prev := m.readState()
	s := m.probe(prev, false)
	m.classify(s)
	day := s.Stamp[:10]
	if err := m.appendFile(m.path("samples/"+day+".log"), s.Line()+"\n"); err != nil {
		return err
	}

	st := State{State: s.State, Cause: s.Cause, Since: prev.Since, Ts: strconv.FormatInt(s.Now, 10),
		PID: s.PID, Ticks: s.Ticks, Rx: s.Rx, Tx: s.Tx, Ep: s.EP, Hash: prev.Hash}
	if s.State != prev.State {
		st.Since = st.Ts
		msg := fmt.Sprintf("state %s->%s cause=%s", dashText(prev.State, "start"), s.State, s.Cause)
		if prev.State == "DOWN" && isNum(prev.Since) {
			since, _ := strconv.ParseInt(prev.Since, 10, 64)
			msg += fmt.Sprintf(" down_for=%ds", s.Now-since)
		}
		if s.State == "DOWN" {
			msg += " dump=" + m.writeDump(s)
		}
		m.event(msg)
	}
	if prev.PID != "" && s.PID != "-" && s.PID != prev.PID {
		m.event(fmt.Sprintf("process_restarted old=%s new=%s", prev.PID, s.PID))
	}
	if prev.Ep != "" && prev.Ep != "-" && s.EP != "-" && s.EP != prev.Ep {
		m.event(fmt.Sprintf("endpoint_changed old=%s new=%s", prev.Ep, s.EP))
	}

	if st.Hash == "" || (m.c.ConfigEvery > 0 && s.Min%int64(m.c.ConfigEvery) == 0) {
		base := st.Hash
		if base == "" {
			if list := m.listSnapshots(); len(list) > 0 {
				base = m.snapshotHash(list[len(list)-1])
			}
		}
		st.Hash = m.configHash()
		if st.Hash != base {
			name, hash, err := m.snapshotWrite("auto")
			if err == nil {
				st.Hash = hash
				m.event("config_changed snapshot=" + name)
			}
		}
	}

	if m.c.DNSCapture {
		m.missedCollect(s)
	} else if m.env.Exists(CapPID) {
		m.capStop()
	}

	if len(m.c.DomainsWatch) > 0 && m.c.DomainsEvery > 0 && s.Min%int64(m.c.DomainsEvery) == 0 {
		m.domCollect(s)
	}

	// Housekeeping once a day.
	if pts, err := strconv.ParseInt(prev.Ts, 10, 64); err != nil || time.Unix(pts, 0).Format("2006-01-02") != day {
		m.cleanOld()
	}
	if fi, err := os.Stat(m.env.P(m.path("events.log"))); err == nil && fi.Size() > eventsMax {
		os.Rename(m.env.P(m.path("events.log")), m.env.P(m.path("events.log.1")))
	}
	return m.writeState(st)
}

func dashText(s, empty string) string {
	if s == "" {
		return empty
	}
	return s
}

// cleanOld removes files older than KEEP_DAYS (like find -mtime +KEEP_DAYS).
func (m *M) cleanOld() {
	limit := time.Duration(m.c.KeepDays+1) * 24 * time.Hour
	now := m.env.Now()
	for _, d := range []string{"samples", "domains", "missed", "dumps", "bundles"} {
		filepath.WalkDir(m.env.P(m.path(d)), func(p string, e fs.DirEntry, err error) error {
			if err != nil || !e.Type().IsRegular() {
				return nil
			}
			if fi, err := e.Info(); err == nil && now.Sub(fi.ModTime()) >= limit {
				os.Remove(p)
			}
			return nil
		})
	}
}

func tail(text string, n int) string {
	l := lines(text)
	if len(l) > n {
		l = l[len(l)-n:]
	}
	if len(l) == 0 {
		return ""
	}
	return strings.Join(l, "\n") + "\n"
}

func head(text string, n int) string {
	l := lines(text)
	if len(l) > n {
		l = l[:n]
	}
	if len(l) == 0 {
		return ""
	}
	return strings.Join(l, "\n") + "\n"
}

// writeDump saves the router state at a DOWN transition; returns its log path.
func (m *M) writeDump(s *Sample) string {
	t := 15 * time.Second
	name := "dumps/" + m.env.Now().Format("2006-01-02_150405") + "-" + s.Cause + ".txt"
	var b bytes.Buffer
	fmt.Fprintf(&b, "# awg-monitor dump: %s state=%s cause=%s\n", s.Stamp, s.State, s.Cause)
	sec(&b, "sample")
	b.WriteString(s.Line() + "\n")
	m.configInfo(&b)
	if s.EP != "-" {
		sec(&b, "ip route get endpoint")
		b.WriteString(m.run(t, "ip", "route", "get", epHost(s.EP)))
	}
	sec(&b, "ndm: log (tail)")
	b.WriteString(tail(m.ndmc("show log"), 150))
	sec(&b, "dmesg (tail)")
	b.WriteString(tail(m.run(t, "dmesg"), 50))
	sec(&b, "top")
	b.WriteString(head(m.run(t, "top", "-bn1"), 25))
	sec(&b, "free")
	b.WriteString(m.run(t, "free"))
	sec(&b, "recent samples")
	if data, err := m.env.ReadFile(m.path("samples/" + s.Stamp[:10] + ".log")); err == nil {
		b.WriteString(tail(string(data), 15))
	}
	m.env.WriteAtomic(m.path(name), b.Bytes(), 0o644)
	return name
}

// check measures now and prints the result without writing anything.
func (m *M) check() error {
	var prev State
	if m.logdirOK() {
		prev = m.readState()
	}
	s := m.probe(prev, true)
	m.classify(s)
	if s.Cause == "-" {
		m.say("Состояние: %s", s.State)
	} else {
		m.say("Состояние: %s (%s)", s.State, s.Cause)
	}
	if s.IfUp {
		m.say("Интерфейс %s: есть, MTU %s", m.c.Iface, s.MTU)
	} else {
		m.say("Интерфейс %s: НЕТ", m.c.Iface)
	}
	m.say("amneziawg-go: PID %s, RSS %s КБ, CPU %s%%", s.PID, s.RSS, s.CPU)
	m.say("Handshake: %s с назад (порог %d с)", s.HSAge, m.c.HandshakeStale)
	m.say("Ping через туннель (потери%%/RTT мс по целям %s): %s", strings.Join(m.c.PingTargets, " "), s.Tun)
	m.say("HTTP через туннель (код:время): %s", s.HTTP)
	m.say("Endpoint %s мимо туннеля: потери %s%%, RTT %s мс", s.EP, s.EPLoss, s.EPRTT)
	m.say("Роутер: load %s, свободно памяти %s КБ, conntrack %s", s.Load, s.Mem, s.CT)
	m.say("")
	m.say("%s", s.Line())
	return nil
}

// parseUnix formats a unix time string as a local stamp.
func parseUnix(s string) (string, error) {
	ts, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return "", err
	}
	return time.Unix(ts, 0).Format(stampFormat), nil
}
