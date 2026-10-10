package monitor

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/asiforis/keenetic-tools/internal/conf"
	"github.com/asiforis/keenetic-tools/internal/markers"
)

// Config is /opt/etc/awg-monitor.conf over the defaults.
type Config struct {
	LogDir         string
	Iface          string
	NdmIface       string
	Service        string
	AwgConf        string
	PingTargets    []string
	PingCount      int
	HTTPURL        string
	HTTPEvery      int
	HandshakeStale int
	RTTWarn        float64
	ConfigEvery    int
	KeepDays       int
	DomainsWatch   []string
	DomainsEvery   int
	DomainTimeout  int
	WanIface       string
	DNSServer      string
	DNSCapture     bool
	LanIface       string
	MissedProbes   int
	MissedRecheck  int
	MissedStall    bool
}

// Defaults are used for settings missing from the config.
func Defaults() Config {
	return Config{
		Iface:          "opkgtun0",
		NdmIface:       "OpkgTun0",
		Service:        "/opt/etc/init.d/S52awg-opkgtun0",
		AwgConf:        "/opt/etc/amnezia/amneziawg/awg0-opkgtun0.conf",
		PingTargets:    []string{"1.1.1.1", "8.8.8.8"},
		PingCount:      3,
		HTTPURL:        "http://cp.cloudflare.com/generate_204",
		HTTPEvery:      5,
		HandshakeStale: 180,
		RTTWarn:        500,
		ConfigEvery:    15,
		KeepDays:       14,
		DomainsEvery:   5,
		DomainTimeout:  8,
		DNSServer:      "127.0.0.1",
		LanIface:       "br0",
		MissedProbes:   3,
		MissedRecheck:  24,
		MissedStall:    true,
	}
}

type setting struct {
	key string
	set func(c *Config, v string) error
}

func str(p func(*Config) *string) func(*Config, string) error {
	return func(c *Config, v string) error { *p(c) = v; return nil }
}

func words(p func(*Config) *[]string) func(*Config, string) error {
	return func(c *Config, v string) error { *p(c) = strings.Fields(v); return nil }
}

func num(p func(*Config) *int, min int) func(*Config, string) error {
	return func(c *Config, v string) error {
		n, err := strconv.Atoi(v)
		if err != nil || n < min {
			return fmt.Errorf("ожидается целое число не меньше %d", min)
		}
		*p(c) = n
		return nil
	}
}

func flag(p func(*Config) *bool) func(*Config, string) error {
	return func(c *Config, v string) error {
		switch v {
		case "1":
			*p(c) = true
		case "0", "":
			*p(c) = false
		default:
			return fmt.Errorf("ожидается 0 или 1")
		}
		return nil
	}
}

var settings = []setting{
	{"LOG_DIR", str(func(c *Config) *string { return &c.LogDir })},
	{"IFACE", str(func(c *Config) *string { return &c.Iface })},
	{"NDM_IFACE", str(func(c *Config) *string { return &c.NdmIface })},
	{"SERVICE", str(func(c *Config) *string { return &c.Service })},
	{"AWG_CONF", str(func(c *Config) *string { return &c.AwgConf })},
	{"PING_TARGETS", words(func(c *Config) *[]string { return &c.PingTargets })},
	{"PING_COUNT", num(func(c *Config) *int { return &c.PingCount }, 1)},
	{"HTTP_URL", str(func(c *Config) *string { return &c.HTTPURL })},
	{"HTTP_EVERY", num(func(c *Config) *int { return &c.HTTPEvery }, 0)},
	{"HANDSHAKE_STALE", num(func(c *Config) *int { return &c.HandshakeStale }, 1)},
	{"RTT_WARN", func(c *Config, v string) error {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || f <= 0 {
			return fmt.Errorf("ожидается число больше нуля")
		}
		c.RTTWarn = f
		return nil
	}},
	{"CONFIG_EVERY", num(func(c *Config) *int { return &c.ConfigEvery }, 0)},
	{"KEEP_DAYS", num(func(c *Config) *int { return &c.KeepDays }, 1)},
	{"DOMAINS_WATCH", words(func(c *Config) *[]string { return &c.DomainsWatch })},
	{"DOMAINS_EVERY", num(func(c *Config) *int { return &c.DomainsEvery }, 0)},
	{"DOMAIN_TIMEOUT", num(func(c *Config) *int { return &c.DomainTimeout }, 1)},
	{"WAN_IFACE", str(func(c *Config) *string { return &c.WanIface })},
	{"DNS_SERVER", str(func(c *Config) *string { return &c.DNSServer })},
	{"DNS_CAPTURE", flag(func(c *Config) *bool { return &c.DNSCapture })},
	{"LAN_IFACE", str(func(c *Config) *string { return &c.LanIface })},
	{"MISSED_PROBES", num(func(c *Config) *int { return &c.MissedProbes }, 0)},
	{"MISSED_RECHECK", num(func(c *Config) *int { return &c.MissedRecheck }, 0)},
	{"MISSED_STALL", flag(func(c *Config) *bool { return &c.MissedStall })},
}

// apply sets the values from f; it returns the keys it does not know.
func (c *Config) apply(f *conf.File) ([]string, error) {
	known := map[string]func(*Config, string) error{}
	for _, s := range settings {
		known[s.key] = s.set
	}
	var unknown []string
	for _, k := range f.Keys() {
		set, ok := known[k]
		if !ok {
			unknown = append(unknown, k)
			continue
		}
		v, _ := f.Get(k)
		if err := set(c, v); err != nil {
			return nil, fmt.Errorf("%s: %s=%s: %v", Conf, k, v, err)
		}
	}
	return unknown, nil
}

func b01(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

func ftoa(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }

// confText is a new config with the given log directory and default values.
func confText(dir string) string {
	d := Defaults()
	return markers.Monitor + `
# Настройки awg-monitor. Читаются при каждом запуске, перезапуск не нужен.
# Формат: НАСТРОЙКА=значение; значения с пробелами — в одинарных кавычках.
# Каталог логов (на внешнем диске). Сменить: keenetic-tools awg-monitor install --log-dir DIR
LOG_DIR=` + conf.Quote(dir) + `
# Интерфейс туннеля, интерфейс в NDM, init-скрипт и конфиг AWG.
IFACE=` + d.Iface + `
NDM_IFACE=` + d.NdmIface + `
SERVICE=` + d.Service + `
AWG_CONF=` + d.AwgConf + `
# Адреса для ping через туннель и число пакетов.
PING_TARGETS=` + conf.Quote(strings.Join(d.PingTargets, " ")) + `
PING_COUNT=` + strconv.Itoa(d.PingCount) + `
# HTTP-проверка через туннель раз в HTTP_EVERY минут (и всегда, если ping не прошёл).
# Пустой HTTP_URL — отключить; HTTP_EVERY=0 — только когда ping не прошёл.
HTTP_URL=` + d.HTTPURL + `
HTTP_EVERY=` + strconv.Itoa(d.HTTPEvery) + `
# Handshake старше стольких секунд считается устаревшим.
HANDSHAKE_STALE=` + strconv.Itoa(d.HandshakeStale) + `
# RTT через туннель выше этого (мс) — состояние DEGRADED.
RTT_WARN=` + ftoa(d.RTTWarn) + `
# Проверять изменения конфигурации раз в столько минут.
CONFIG_EVERY=` + strconv.Itoa(d.ConfigEvery) + `
# Сколько дней хранить замеры, дампы и архивы.
KEEP_DAYS=` + strconv.Itoa(d.KeepDays) + `
` + confDomains()
}

// confDomains is the domain-check part of the config.
func confDomains() string {
	d := Defaults()
	return `# Домены (через пробел), которые раз в DOMAINS_EVERY минут проверяются напрямую
# и через туннель. Пусто — проверки выключены. Проверки идут параллельно.
DOMAINS_WATCH=''
DOMAINS_EVERY=` + strconv.Itoa(d.DomainsEvery) + `
# Таймаут одной проверки домена, с.
DOMAIN_TIMEOUT=` + strconv.Itoa(d.DomainTimeout) + `
# Интерфейс провайдера для проверки «напрямую»; пусто — из маршрута по умолчанию.
WAN_IFACE=
# DNS роутера для route-check.
DNS_SERVER=` + d.DNSServer + `
# Поиск доменов мимо туннеля (awg-monitor missed): 1 — включить. Нужен tcpdump
# (opkg install tcpdump); он слушает ответы DNS роутера клиентам на LAN_IFACE.
DNS_CAPTURE=` + b01(d.DNSCapture) + `
LAN_IFACE=` + d.LanIface + `
# Сколько новых доменов в минуту проверять напрямую и через туннель; повтор через столько часов.
MISSED_PROBES=` + strconv.Itoa(d.MissedProbes) + `
MISSED_RECHECK=` + strconv.Itoa(d.MissedRecheck) + `
# Считать подозрительными TLS-соединения, где сервер почти ничего не ответил (0 — выключить).
MISSED_STALL=` + b01(d.MissedStall) + `
`
}
