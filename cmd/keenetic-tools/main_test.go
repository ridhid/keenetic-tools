package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/asiforis/keenetic-tools/internal/markers"
	"github.com/asiforis/keenetic-tools/internal/sys"
)

// The release flags strip symbols; the marker line must survive them on
// every target architecture.
func TestBinaryCarriesMarkers(t *testing.T) {
	if testing.Short() {
		t.Skip("builds binaries")
	}
	targets := [][]string{
		{"GOARCH=mipsle", "GOMIPS=softfloat"},
		{"GOARCH=mips", "GOMIPS=softfloat"},
		{"GOARCH=arm64"},
		{"GOARCH=arm", "GOARM=5"},
	}
	dir := t.TempDir()
	for _, env := range targets {
		t.Run(env[0], func(t *testing.T) {
			bin := filepath.Join(dir, strings.TrimPrefix(env[0], "GOARCH="))
			cmd := exec.Command("go", "build", "-trimpath", "-ldflags=-s -w -buildid=", "-o", bin, ".")
			cmd.Env = append(append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux"), env...)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("build: %v\n%s", err, out)
			}
			data, err := os.ReadFile(bin)
			if err != nil {
				t.Fatal(err)
			}
			if !sys.HasMarkerLine(data, markers.Binary) {
				t.Errorf("no line %q in the binary", markers.Binary)
			}
		})
	}
}

func TestDispatch(t *testing.T) {
	cases := []struct {
		args []string
		code int
		out  string
	}{
		{[]string{"/opt/bin/keenetic-tools", "version"}, 0, "dev\n"},
		{[]string{"/opt/keenetic-tools.part", "version"}, 0, "dev\n"},
		{[]string{"/opt/bin/keenetic-tools"}, 1, ""},
	}
	for _, c := range cases {
		var out, errb bytes.Buffer
		if code := run(c.args, &out, &errb); code != c.code || out.String() != c.out {
			t.Errorf("run(%q) = %d, %q (stderr %q); want %d, %q", c.args, code, out.String(), errb.String(), c.code, c.out)
		}
	}
	// Only commands that touch nothing on this machine.
	for _, args := range [][]string{{"/opt/bin/awg-monitor", "help"}, {"keenetic-tools", "awg-monitor", "help"}} {
		var out bytes.Buffer
		if code := run(args, &out, &bytes.Buffer{}); code != 0 || !strings.Contains(out.String(), "awg-monitor collect") {
			t.Errorf("run(%q) = %d, %q", args, code, out.String())
		}
	}
}
