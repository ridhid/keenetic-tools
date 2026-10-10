package sys_test

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/asiforis/keenetic-tools/internal/sys"
	"github.com/asiforis/keenetic-tools/internal/sys/systest"
)

const marker = "# keenetic-tools: awg-monitor"

func TestHasMarkerLine(t *testing.T) {
	cases := []struct {
		data string
		want bool
	}{
		{"#!/bin/sh\n" + marker + "\nset -eu\n", true},
		{marker + "\nLOG_DIR=x\n", true},
		{"a\n" + marker, true},
		{marker, true},
		{"\x7fELF\x00\x01\n" + marker + "\n\x00\x02", true},
		{"a\n" + marker + " extra\n", false},
		{"a\n  " + marker + "\n", false},
		{"", false},
	}
	for _, c := range cases {
		if got := sys.HasMarkerLine([]byte(c.data), marker); got != c.want {
			t.Errorf("HasMarkerLine(%q) = %v, want %v", c.data, got, c.want)
		}
	}
}

func TestOwned(t *testing.T) {
	r := systest.New(t)
	r.Write("/opt/bin/ours", "#!/bin/sh\n"+marker+"\n", 0o755)
	r.Write("/opt/bin/theirs", "#!/bin/sh\necho hi\n", 0o755)
	if err := os.Symlink(r.P("/opt/bin/ours"), r.P("/opt/bin/link")); err != nil {
		t.Fatal(err)
	}
	if !r.Owned("/opt/bin/ours", marker) {
		t.Error("ours: want owned")
	}
	if r.Owned("/opt/bin/theirs", marker) {
		t.Error("theirs: want not owned")
	}
	if r.Owned("/opt/bin/link", marker) {
		t.Error("symlink: want not owned")
	}
	if r.Owned("/opt/bin/missing", marker) {
		t.Error("missing: want not owned")
	}
}

func TestWriteAtomicReplacesInode(t *testing.T) {
	r := systest.New(t)
	r.Write("/opt/bin/x", "old", 0o644)
	before, _ := os.Stat(r.P("/opt/bin/x"))
	if err := r.WriteAtomic("/opt/bin/x", []byte("new"), 0o755); err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(r.P("/opt/bin/x"))
	if os.SameFile(before, after) {
		t.Error("want a new inode")
	}
	if after.Mode().Perm() != 0o755 || r.Read("/opt/bin/x") != "new" {
		t.Errorf("mode %v, content %q", after.Mode().Perm(), r.Read("/opt/bin/x"))
	}
	if left := r.Glob("/opt/bin/x.tmp.*"); len(left) != 0 {
		t.Errorf("temp files left: %v", left)
	}
}

func TestLock(t *testing.T) {
	r := systest.New(t)
	const path = "/tmp/awg-monitor.lock"
	l, err := r.TryLock(path)
	if l == nil || err != nil {
		t.Fatalf("first lock: %v %v", l, err)
	}
	if got := r.LockHolder(path); got != os.Getpid() {
		t.Fatalf("holder = %d, want %d", got, os.Getpid())
	}
	// flock is per open file: a second open conflicts even in this process.
	if l2, err := r.TryLock(path); l2 != nil || err != nil {
		t.Fatalf("second lock while held: %v %v", l2, err)
	}
	if err := l.Release(); err != nil {
		t.Fatal(err)
	}
	if got := r.LockHolder(path); got != 0 {
		t.Fatalf("holder after release = %d", got)
	}
	l, err = r.TryLock(path)
	if l == nil || err != nil {
		t.Fatalf("lock after release: %v %v", l, err)
	}
	l.Release()
}

func TestCreateTempAndBackup(t *testing.T) {
	r := systest.New(t)
	r.Write("/opt/etc/crontab", "x\n", 0o600)
	old := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := os.Chtimes(r.P("/opt/etc/crontab"), old, old); err != nil {
		t.Fatal(err)
	}
	b, err := r.Backup("/opt/etc/crontab", "/opt/etc/crontab.awg-backup.")
	if err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(r.P(b))
	if fi.Mode().Perm() != 0o600 || !fi.ModTime().Equal(old) || r.Read(b) != "x\n" {
		t.Errorf("backup %s: mode %v mtime %v content %q", b, fi.Mode().Perm(), fi.ModTime(), r.Read(b))
	}
	b2, _ := r.Backup("/opt/etc/crontab", "/opt/etc/crontab.awg-backup.")
	if b2 == b {
		t.Error("two backups got the same name")
	}
}

func TestFSType(t *testing.T) {
	r := systest.New(t)
	r.Write("/proc/mounts", ""+
		"rootfs / rootfs rw 0 0\n"+
		"tmpfs /tmp tmpfs rw 0 0\n"+
		"/dev/sda1 /tmp/mnt/my\\040disk ext4 rw 0 0\n"+
		"/dev/sda1 /opt ext4 rw 0 0\n"+
		"/dev/ubi0_0 /storage ubifs rw 0 0\n", 0o644)
	for _, d := range []string{"/tmp/mnt/my disk", "/opt", "/storage"} {
		if err := os.MkdirAll(r.P(d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cases := map[string]string{
		"/tmp/mnt/my disk/awg-monitor": "ext4",
		"/tmp/awg":                     "tmpfs",
		"/tmp/mnt/other/x":             "tmpfs",
		"/opt/var/log/awg-monitor":     "ext4",
		"/storage/x":                   "ubifs",
		"/var/log":                     "rootfs",
	}
	for p, want := range cases {
		if got := r.FSType(p); got != want {
			t.Errorf("FSType(%s) = %q, want %q", p, got, want)
		}
	}
}

func TestExecRunnerTimeoutKills(t *testing.T) {
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skip("sleep not found")
	}
	res := sys.ExecRunner{}.Run(context.Background(), sys.Cmd{Name: "sleep", Args: []string{"5"}, Timeout: 100 * time.Millisecond})
	if res.Code != 137 {
		t.Fatalf("code = %d, err %v; want 137", res.Code, res.Err)
	}
}

func TestExecRunnerExitCode(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not found")
	}
	res := sys.ExecRunner{}.Run(context.Background(), sys.Cmd{Name: "sh", Args: []string{"-c", "echo out; echo err >&2; exit 3"}})
	if res.Code != 3 || string(res.Output) != "out\nerr\n" {
		t.Fatalf("code %d output %q", res.Code, res.Output)
	}
}

// Start detaches the process into its own session and does not wait for it.
func TestExecRunnerStart(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	out, err := os.Create(t.TempDir() + "/out")
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	start := time.Now()
	pid, err := sys.ExecRunner{}.Start(sys.Cmd{Name: "sh", Args: []string{"-c", "ps -o sid= -p $$; sleep 1"}, Stdout: out})
	if err != nil || pid <= 0 {
		t.Fatalf("pid %d, %v", pid, err)
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Fatal("Start waited for the process")
	}
	for range 50 {
		if b, _ := os.ReadFile(out.Name()); len(b) > 0 {
			if sid := strings.TrimSpace(string(b)); sid != strconv.Itoa(pid) {
				t.Fatalf("session %q, want own session %d", sid, pid)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Skip("ps printed nothing")
}
