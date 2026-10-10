package monitor

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// awgKeys returns the private and preshared keys from the AWG config.
func (m *M) awgKeys() []string {
	data, err := m.env.ReadFile(m.c.AwgConf)
	if err != nil {
		return nil
	}
	var keys []string
	for _, l := range lines(string(data)) {
		if secretKey.MatchString(l) {
			v := strings.TrimSpace(l[strings.IndexByte(l, '=')+1:])
			if v != "" {
				keys = append(keys, v)
			}
		}
	}
	return keys
}

func (m *M) bundle(arg string) error {
	if err := m.requireLogdir(); err != nil {
		return err
	}
	days := 3
	if arg != "" {
		n, err := strconv.Atoi(arg)
		if err != nil || n <= 0 || !isNum(arg) {
			return fail("Число дней — целое больше нуля.")
		}
		days = n
	}
	now := m.env.Now()
	cutoff := now.Add(-time.Duration(days-1) * 24 * time.Hour).Format("2006-01-02")

	var meta bytes.Buffer
	fmt.Fprintf(&meta, "# awg-monitor bundle %s, дней: %d\n", now.Format("2006-01-02 15:04:05 MST"), days)
	sec(&meta, "config: "+Conf)
	if data, err := m.env.ReadFile(Conf); err == nil {
		for _, l := range lines(string(data)) {
			if !strings.HasPrefix(strings.TrimLeft(l, " \t"), "#") {
				meta.WriteString(l + "\n")
			}
		}
	}
	sec(&meta, fmt.Sprintf("report %dd", days))
	if err := m.writeReport(&meta, strconv.Itoa(days)+"d"); err != nil {
		meta.WriteString(err.Error() + "\n")
	}

	// Paths relative to the log directory.
	var files []string
	for _, dir := range []string{"samples", "domains"} {
		for _, f := range m.logFiles(dir) {
			if strings.TrimSuffix(filepath.Base(f), ".log") >= cutoff {
				files = append(files, dir+"/"+filepath.Base(f))
			}
		}
	}
	for _, f := range []string{"events.log", "events.log.1"} {
		if m.env.IsFile(m.path(f)) {
			files = append(files, f)
		}
	}
	for _, dir := range []string{"snapshots", "dumps"} {
		root := m.env.P(m.path(dir))
		filepath.WalkDir(root, func(p string, e fs.DirEntry, err error) error {
			if err != nil || !e.Type().IsRegular() {
				return nil
			}
			fi, err := e.Info()
			if err != nil {
				return nil
			}
			// Dumps of the period only (find -mtime -DAYS).
			if dir == "dumps" && now.Sub(fi.ModTime()) >= time.Duration(days)*24*time.Hour {
				return nil
			}
			rel, _ := filepath.Rel(m.env.P(m.c.LogDir), p)
			files = append(files, filepath.ToSlash(rel))
			return nil
		})
	}

	name := "awg-monitor-" + now.Format("2006-01-02_150405") + ".tar.gz"
	final := m.env.P(m.path("bundles/" + name))
	part := final + ".part"
	if err := os.MkdirAll(filepath.Dir(final), 0o755); err != nil {
		return err
	}
	defer os.Remove(part)
	if err := m.writeTar(part, meta.Bytes(), now, files); err != nil {
		return err
	}
	// Refuse to keep the archive if a secret key slipped in.
	for _, key := range m.awgKeys() {
		found, err := gzipContains(part, key)
		if err != nil {
			return err
		}
		if found {
			return fail("В архив попал ключ из конфига AWG — архив удалён. Сообщите об ошибке.")
		}
	}
	if err := os.Rename(part, final); err != nil {
		return err
	}
	m.say("Архив: %s", m.path("bundles/"+name))
	m.say("Ключей в нём нет, но есть IP сервера и журнал роутера — передавайте только тем, кому доверяете.")
	return nil
}

func (m *M) writeTar(path string, meta []byte, now time.Time, files []string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	bw := bufio.NewWriter(f)
	gz := gzip.NewWriter(bw)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "meta.txt", Mode: 0o644, Size: int64(len(meta)), ModTime: now, Typeflag: tar.TypeReg}); err != nil {
		return err
	}
	if _, err := tw.Write(meta); err != nil {
		return err
	}
	for _, rel := range files {
		if err := addFile(tw, m.env.P(m.path(rel)), rel); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	if err := gz.Close(); err != nil {
		return err
	}
	if err := bw.Flush(); err != nil {
		return err
	}
	return f.Close()
}

func addFile(tw *tar.Writer, real, name string) error {
	src, err := os.Open(real)
	if err != nil {
		// Removed by housekeeping meanwhile: skip it.
		return nil
	}
	defer src.Close()
	fi, err := src.Stat()
	if err != nil {
		return err
	}
	hdr, err := tar.FileInfoHeader(fi, "")
	if err != nil {
		return err
	}
	hdr.Name = name
	hdr.Uname, hdr.Gname = "", ""
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	// A log may grow while we copy: take exactly the size in the header.
	_, err = io.CopyN(tw, src, fi.Size())
	return err
}

// gzipContains scans the decompressed archive for s without holding it in memory.
func gzipContains(path, s string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return false, err
	}
	needle := []byte(s)
	buf := make([]byte, 0, 64<<10+len(needle))
	chunk := make([]byte, 64<<10)
	for {
		n, err := gz.Read(chunk)
		buf = append(buf, chunk[:n]...)
		if bytes.Contains(buf, needle) {
			return true, nil
		}
		// Keep the tail that may hold the start of a match.
		if keep := len(needle) - 1; len(buf) > keep {
			buf = append(buf[:0], buf[len(buf)-keep:]...)
		}
		if err == io.EOF {
			return false, nil
		}
		if err != nil {
			return false, err
		}
	}
}
