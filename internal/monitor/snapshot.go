package monitor

import (
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/asiforis/keenetic-tools/internal/sys"
)

func sec(b *bytes.Buffer, name string) { fmt.Fprintf(b, "\n### %s\n", name) }

// run returns a command's combined output with ndmc's terminal escapes
// and CRs removed; a command that fails still yields what it printed.
func (m *M) run(timeout time.Duration, name string, args ...string) string {
	r := m.env.Run.Run(bgctx(), sys.Cmd{Name: name, Args: args, Timeout: timeout})
	out := strings.ReplaceAll(string(r.Output), "\x1b[K", "")
	out = strings.ReplaceAll(out, "\r", "")
	if r.Code == -1 && out == "" && r.Err != nil {
		return name + ": " + r.Err.Error() + "\n"
	}
	return out
}

func (m *M) ndmc(cmd string) string { return m.run(30*time.Second, "ndmc", "-c", cmd) }

func lines(s string) []string {
	s = strings.TrimSuffix(s, "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

var (
	secretKey   = regexp.MustCompile(`(?i)^[ \t]*(privatekey|presharedkey)[ \t]*=`)
	serviceVar  = regexp.MustCompile(`^[ \t]*(OPKGTUN_[A-Z_]*|AWG_CONF|VER)=`)
	versionLine = regexp.MustCompile(`^[ \t]*(release|title|model|hw_id|arch):`)
	packageLine = regexp.MustCompile(`^(amneziawg|wireguard|cron )`)
)

// redact replaces the values of private and preshared keys.
func redact(line string) string {
	if secretKey.MatchString(line) {
		return line[:strings.IndexByte(line, '=')] + "= <redacted>"
	}
	return line
}

func grep(text string, re *regexp.Regexp) []string {
	var out []string
	for _, l := range lines(text) {
		if re.MatchString(l) {
			out = append(out, l)
		}
	}
	return out
}

// configStable is the part of the configuration that defines behaviour;
// its hash detects changes.
func (m *M) configStable() []byte {
	var b bytes.Buffer
	sec(&b, "awg config: "+m.c.AwgConf+" (keys redacted)")
	if data, err := m.env.ReadFile(m.c.AwgConf); err == nil {
		for _, l := range lines(string(data)) {
			b.WriteString(redact(strings.TrimSuffix(l, "\r")) + "\n")
		}
	} else {
		b.WriteString("missing\n")
	}
	sec(&b, "service settings: "+m.c.Service)
	data, _ := m.env.ReadFile(m.c.Service)
	if vars := grep(string(data), serviceVar); len(vars) > 0 {
		b.WriteString(strings.Join(vars, "\n") + "\n")
	} else {
		b.WriteString("missing\n")
	}
	sec(&b, "ndm: interface "+m.c.NdmIface)
	b.WriteString(interfaceBlock(m.ndmc("show running-config"), m.c.NdmIface))
	sec(&b, "firmware")
	for _, l := range grep(m.ndmc("show version"), versionLine) {
		b.WriteString(l + "\n")
	}
	sec(&b, "packages")
	for _, l := range grep(m.run(60*time.Second, "opkg", "list-installed"), packageLine) {
		b.WriteString(l + "\n")
	}
	return b.Bytes()
}

// interfaceBlock cuts "interface NAME" ... "!" out of a running-config.
func interfaceBlock(cfg, name string) string {
	var b strings.Builder
	in := false
	for _, l := range lines(cfg) {
		if l == "interface "+name {
			in = true
		}
		if in {
			b.WriteString(l + "\n")
			if strings.HasPrefix(l, "!") {
				break
			}
		}
	}
	return b.String()
}

// configInfo is volatile context, kept for reference only.
func (m *M) configInfo(b *bytes.Buffer) {
	t := 15 * time.Second
	sec(b, "awg show "+m.c.Iface)
	b.WriteString(m.run(t, "awg", "show", m.c.Iface))
	sec(b, "ip addr "+m.c.Iface)
	b.WriteString(m.run(t, "ip", "addr", "show", "dev", m.c.Iface))
	sec(b, "ip rule")
	b.WriteString(m.run(t, "ip", "rule", "show"))
	sec(b, "ip route get")
	for _, target := range m.c.PingTargets {
		b.WriteString(m.run(t, "ip", "route", "get", target))
	}
	sec(b, "ndm: show interface "+m.c.NdmIface)
	b.WriteString(m.ndmc("show interface " + m.c.NdmIface))
}

func md5hex(b []byte) string {
	h := md5.Sum(b)
	return hex.EncodeToString(h[:])
}

func (m *M) configHash() string { return md5hex(m.configStable()) }

// listSnapshots returns snapshot file names, oldest first.
func (m *M) listSnapshots() []string {
	ents, err := os.ReadDir(m.env.P(m.path("snapshots")))
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		if e.Type().IsRegular() && strings.HasSuffix(e.Name(), ".txt") && !strings.HasPrefix(e.Name(), ".") {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

var labelChars = strings.NewReplacer("/", "_", " ", "_", "\t", "_")

// snapshotWrite saves a snapshot and returns its name and hash.
func (m *M) snapshotWrite(label string) (name, hash string, err error) {
	label = strings.TrimPrefix(labelChars.Replace(label), ".")
	now := m.env.Now()
	stable := m.configStable()
	hash = md5hex(stable)
	name = now.Format("2006-01-02_150405")
	if label != "" {
		name += "-" + label
	}
	name += ".txt"
	var b bytes.Buffer
	b.WriteString("# awg-monitor snapshot\n")
	fmt.Fprintf(&b, "date: %s\n", now.Format("2006-01-02 15:04:05 MST"))
	fmt.Fprintf(&b, "label: %s\n", dash(label))
	fmt.Fprintf(&b, "hash: %s\n\n## stable\n", hash)
	b.Write(stable)
	b.WriteString("\n## info\n")
	m.configInfo(&b)
	return name, hash, m.env.WriteAtomic(m.path("snapshots/"+name), b.Bytes(), 0o644)
}

func (m *M) snapshot(label string) error {
	if err := m.requireLogdir(); err != nil {
		return err
	}
	if label == "" {
		label = "manual"
	}
	name, hash, err := m.snapshotWrite(label)
	if err != nil {
		return err
	}
	m.say("Снимок: %s", m.path("snapshots/"+name))
	m.say("Хэш конфигурации: %s", hash)
	return nil
}

func (m *M) snapshots() error {
	if err := m.requireLogdir(); err != nil {
		return err
	}
	list := m.listSnapshots()
	if len(list) == 0 {
		m.say("Снимков пока нет.")
	}
	for _, s := range list {
		m.say("%s", s)
	}
	return nil
}

// snapshotHash reads the "hash:" header of a snapshot.
func (m *M) snapshotHash(name string) string {
	data, _ := m.env.ReadFile(m.path("snapshots/" + name))
	for _, l := range lines(string(data)) {
		if h, ok := strings.CutPrefix(l, "hash: "); ok {
			return h
		}
	}
	return ""
}

func (m *M) resolveSnapshot(q string, list []string) (string, error) {
	for _, n := range []string{q, q + ".txt"} {
		if !strings.Contains(n, "/") && m.env.IsFile(m.path("snapshots/"+n)) {
			return n, nil
		}
	}
	for i := len(list) - 1; i >= 0; i-- {
		if strings.Contains(list[i], q) {
			return list[i], nil
		}
	}
	return "", fail("Снимок '%s' не найден. Список: awg-monitor snapshots", q)
}

// stablePart returns the lines between "## stable" and "## info".
func stablePart(text string) []string {
	var out []string
	in := false
	for _, l := range lines(text) {
		switch {
		case l == "## stable":
			in = true
		case l == "## info":
			in = false
		case in:
			out = append(out, l)
		}
	}
	return out
}

func (m *M) diff(args []string) error {
	if err := m.requireLogdir(); err != nil {
		return err
	}
	list := m.listSnapshots()
	var a, b string
	var err error
	switch len(args) {
	case 0:
		if len(list) < 2 {
			return fail("Нужно минимум два снимка.")
		}
		a, b = list[len(list)-2], list[len(list)-1]
	case 1:
		if len(list) == 0 {
			return fail("Снимков пока нет.")
		}
		if a, err = m.resolveSnapshot(args[0], list); err != nil {
			return err
		}
		b = list[len(list)-1]
	case 2:
		if a, err = m.resolveSnapshot(args[0], list); err != nil {
			return err
		}
		if b, err = m.resolveSnapshot(args[1], list); err != nil {
			return err
		}
	default:
		return fail("diff принимает не больше двух снимков.")
	}
	ta, _ := m.env.ReadFile(m.path("snapshots/" + a))
	tb, _ := m.env.ReadFile(m.path("snapshots/" + b))
	m.say("Сравнение (только стабильная часть): %s -> %s", a, b)
	d := unifiedDiff(a, b, stablePart(string(ta)), stablePart(string(tb)))
	if d == "" {
		m.say("Различий нет.")
		return nil
	}
	fmt.Fprint(m.env.Stdout, d)
	return nil
}
