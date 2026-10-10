package selfbin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ridhid/keenetic-tools/internal/markers"
	"github.com/ridhid/keenetic-tools/internal/sys/systest"
)

func fakeBinary(t *testing.T, version string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "keenetic-tools.part")
	if err := os.WriteFile(p, []byte("\x7fELF"+markers.Embedded+version), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestInstallAndUpgrade(t *testing.T) {
	r := systest.New(t)
	if err := Install(r.Env, fakeBinary(t, "v1")); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(r.P(Bin))
	if err != nil || fi.Mode().Perm() != 0o755 {
		t.Fatalf("installed: %v %v", fi, err)
	}
	if err := Link(r.Env, MonitorLink); err != nil {
		t.Fatal(err)
	}
	if err := Install(r.Env, fakeBinary(t, "v2")); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(r.Read(MonitorLink), "v2") {
		t.Fatalf("awg-monitor does not see the new binary: %q", r.Read(MonitorLink))
	}
	if err := Link(r.Env, MonitorLink); err != nil {
		t.Fatalf("relink: %v", err)
	}
}

func TestInstallFromItselfIsNoop(t *testing.T) {
	r := systest.New(t)
	if err := Install(r.Env, fakeBinary(t, "v1")); err != nil {
		t.Fatal(err)
	}
	if err := Install(r.Env, r.P(Bin)); err != nil {
		t.Fatal(err)
	}
}

func TestRefusesForeign(t *testing.T) {
	r := systest.New(t)
	r.Write(Bin, "#!/bin/sh\necho not ours\n", 0o755)
	if err := Install(r.Env, fakeBinary(t, "v1")); err == nil {
		t.Fatal("foreign binary replaced")
	}
	r.Write(MonitorLink, "#!/bin/sh\n", 0o755)
	if err := Link(r.Env, MonitorLink); err == nil {
		t.Fatal("foreign awg-monitor replaced")
	}
	if err := Unlink(r.Env, MonitorLink); err != nil || !r.Exists(MonitorLink) {
		t.Fatal("foreign awg-monitor removed")
	}
}

func TestRefusesNonBinary(t *testing.T) {
	r := systest.New(t)
	p := filepath.Join(t.TempDir(), "x")
	os.WriteFile(p, []byte("hello"), 0o755)
	if err := Install(r.Env, p); err == nil {
		t.Fatal("installed a file without the marker")
	}
}

func TestUnlink(t *testing.T) {
	r := systest.New(t)
	if err := Install(r.Env, fakeBinary(t, "v1")); err != nil {
		t.Fatal(err)
	}
	Link(r.Env, MonitorLink)
	if err := Unlink(r.Env, MonitorLink); err != nil || r.Exists(MonitorLink) {
		t.Fatal("symlink not removed")
	}
}
