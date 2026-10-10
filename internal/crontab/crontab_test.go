package crontab

import (
	"os"
	"strings"
	"testing"

	"github.com/ridhid/keenetic-tools/internal/sys/systest"
)

const (
	tab    = "/opt/etc/crontab"
	marker = "# keenetic-tools: awg-monitor"
	ours   = "* * * * * root /opt/bin/awg-monitor collect >/dev/null 2>&1 " + marker
	prefix = tab + ".awg-monitor-backup."
)

func TestStrip(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"empty", "", ""},
		{"keeps others", "a\nb\n", "a\nb\n"},
		{"drops ours", "a\n" + ours + "\nb\n", "a\nb\n"},
		{"adds final newline", "a\nb", "a\nb\n"},
		{"marker in the middle is kept", "x " + marker + " y\n", "x " + marker + " y\n"},
		{"line equal to marker", marker + "\n", ""},
		{"other marker kept", "01 * * * * root /x # keenetic-awg-restart: managed by install-awg-cron.sh\n" + ours + "\n",
			"01 * * * * root /x # keenetic-awg-restart: managed by install-awg-cron.sh\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := string(Strip([]byte(c.in), marker)); got != c.want {
				t.Errorf("Strip(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestUpdateAddIsIdempotent(t *testing.T) {
	r := systest.New(t)
	r.Write(tab, "SHELL=/bin/sh\n*/5 * * * * root /other\n", 0o600)

	backup, err := Update(r.Env, tab, marker, ours, prefix)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(backup, prefix) || len(backup) != len(prefix)+6 {
		t.Fatalf("backup = %q", backup)
	}
	want := "SHELL=/bin/sh\n*/5 * * * * root /other\n" + ours + "\n"
	if got := r.Read(tab); got != want {
		t.Fatalf("crontab = %q, want %q", got, want)
	}
	if got := r.Read(backup); got != "SHELL=/bin/sh\n*/5 * * * * root /other\n" {
		t.Fatalf("backup content = %q", got)
	}
	fi, _ := os.Stat(r.P(backup))
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("backup mode = %v, want 0600", fi.Mode().Perm())
	}

	backup, err = Update(r.Env, tab, marker, ours, prefix)
	if err != nil || backup != "" {
		t.Fatalf("second Update: backup %q, err %v; want no change", backup, err)
	}
	if n := len(r.Glob(prefix + "*")); n != 1 {
		t.Fatalf("%d backups, want 1", n)
	}
}

func TestUpdateRemoveKeepsInode(t *testing.T) {
	r := systest.New(t)
	r.Write(tab, "a\n"+ours+"\n", 0o644)
	before, _ := os.Stat(r.P(tab))

	if _, err := Update(r.Env, tab, marker, "", prefix); err != nil {
		t.Fatal(err)
	}
	if got := r.Read(tab); got != "a\n" {
		t.Fatalf("crontab = %q", got)
	}
	after, _ := os.Stat(r.P(tab))
	if !os.SameFile(before, after) {
		t.Fatal("crontab was replaced, want rewritten in place")
	}
}

func TestUpdateMissingCrontab(t *testing.T) {
	r := systest.New(t)
	if _, err := Update(r.Env, tab, marker, ours, prefix); err == nil {
		t.Fatal("want error for missing crontab")
	}
}

func TestHas(t *testing.T) {
	if !Has([]byte("a\n"+ours+"\n"), marker) || Has([]byte("a\n"), marker) {
		t.Fatal("Has")
	}
}
