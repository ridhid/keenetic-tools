// Package conf reads and edits /opt/etc/awg-monitor.conf: KEY=value lines,
// values optionally in quotes, # comments. Anything that looks like shell
// syntax ($VAR, $(...), `...`) is rejected with the line number, so a user
// who expects shell semantics gets an error instead of a silent surprise.
// Edits keep comments and the order of lines.
package conf

import (
	"fmt"
	"regexp"
	"strings"
)

// File is a parsed config that remembers its original lines.
type File struct {
	lines  []string
	values map[string]string
	keys   []string
}

// Error points at the offending line.
type Error struct {
	Name   string
	Line   int
	Text   string
	Reason string
}

func (e *Error) Error() string {
	return fmt.Sprintf("%s, строка %d: %s: %s", e.Name, e.Line, e.Reason, e.Text)
}

var keyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Parse reads config data; name is used in error messages.
func Parse(name string, data []byte) (*File, error) {
	f := &File{values: map[string]string{}}
	text := strings.TrimSuffix(string(data), "\n")
	if text != "" {
		f.lines = strings.Split(text, "\n")
	}
	for i, raw := range f.lines {
		line := strings.TrimLeft(strings.TrimSuffix(raw, "\r"), " \t")
		if line == "" || line[0] == '#' {
			continue
		}
		fail := func(reason string) error {
			return &Error{Name: name, Line: i + 1, Text: strings.TrimSpace(raw), Reason: reason}
		}
		key, rest, ok := strings.Cut(line, "=")
		if !ok || !keyRe.MatchString(key) {
			return nil, fail("ожидается НАСТРОЙКА=значение")
		}
		val, err := parseValue(rest)
		if err != "" {
			return nil, fail(err)
		}
		if _, seen := f.values[key]; !seen {
			f.keys = append(f.keys, key)
		}
		f.values[key] = val
	}
	return f, nil
}

// parseValue handles 'single', "double" (without expansions) and bare words,
// followed by optional blanks and a # comment.
func parseValue(s string) (string, string) {
	var val string
	switch {
	case strings.HasPrefix(s, "'"):
		end := strings.IndexByte(s[1:], '\'')
		if end < 0 {
			return "", "нет закрывающей кавычки '"
		}
		val, s = s[1:1+end], s[2+end:]
	case strings.HasPrefix(s, `"`):
		end := strings.IndexByte(s[1:], '"')
		if end < 0 {
			return "", `нет закрывающей кавычки "`
		}
		val, s = s[1:1+end], s[2+end:]
		if strings.ContainsAny(val, "$`\\") {
			return "", "подстановки ($, `, \\) не поддерживаются — используйте одинарные кавычки"
		}
	default:
		end := strings.IndexAny(s, " \t")
		if end < 0 {
			end = len(s)
		}
		val, s = s[:end], s[end:]
		if strings.ContainsAny(val, "$`\\\"'();&|<>") {
			return "", "недопустимые символы в значении — заключите его в одинарные кавычки"
		}
	}
	rest := strings.TrimLeft(s, " \t")
	if rest != "" && rest[0] != '#' {
		if len(rest) == len(s) {
			return "", "склеенные кавычки не поддерживаются"
		}
		return "", "несколько слов — заключите значение в одинарные кавычки"
	}
	return val, ""
}

// Get returns the last assignment of key.
func (f *File) Get(key string) (string, bool) {
	v, ok := f.values[key]
	return v, ok
}

// Keys returns the assigned keys in order of first assignment.
func (f *File) Keys() []string { return append([]string(nil), f.keys...) }

// Has reports whether some line assigns key at column 1 (`grep -q '^KEY='`).
func (f *File) Has(key string) bool {
	for _, l := range f.lines {
		if strings.HasPrefix(l, key+"=") {
			return true
		}
	}
	return false
}

// Set replaces every line starting with KEY= by KEY=raw, or appends it.
// raw is written as is: use Quote for values with spaces.
func (f *File) Set(key, raw string) {
	done := false
	for i, l := range f.lines {
		if strings.HasPrefix(l, key+"=") {
			f.lines[i] = key + "=" + raw
			done = true
		}
	}
	if !done {
		f.lines = append(f.lines, key+"="+raw)
	}
	if v, err := parseValue(raw); err == "" {
		if _, seen := f.values[key]; !seen {
			f.keys = append(f.keys, key)
		}
		f.values[key] = v
	}
}

// Append adds raw lines (comments, assignments) at the end.
func (f *File) Append(text string) error {
	g, err := Parse("", []byte(text))
	if err != nil {
		return err
	}
	f.lines = append(f.lines, g.lines...)
	for _, k := range g.keys {
		if _, seen := f.values[k]; !seen {
			f.keys = append(f.keys, k)
		}
		f.values[k] = g.values[k]
	}
	return nil
}

// Bytes returns the file content with a trailing newline.
func (f *File) Bytes() []byte {
	if len(f.lines) == 0 {
		return nil
	}
	return []byte(strings.Join(f.lines, "\n") + "\n")
}

// Quote wraps v in single quotes; v must not contain one.
func Quote(v string) string { return "'" + v + "'" }
