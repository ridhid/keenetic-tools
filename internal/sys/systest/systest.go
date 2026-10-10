// Package systest provides a fake router for tests: a temporary root
// directory, scripted external commands and a fixed clock.
package systest

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/asiforis/keenetic-tools/internal/sys"
)

// Handler produces the result of a fake command.
type Handler func(c sys.Cmd) sys.Result

// Runner is a scripted sys.Runner. Commands are matched by the full command
// line first ("opkg install cron"), then by name only ("opkg").
type Runner struct {
	// Root is stripped from command paths, so tests use router paths.
	Root     string
	mu       sync.Mutex
	handlers map[string]Handler
	missing  map[string]bool
	calls    []string
}

// On makes a command print out and exit with code.
func (r *Runner) On(cmdline, out string, code int) {
	r.OnFunc(cmdline, func(sys.Cmd) sys.Result { return Exit(out, code) })
}

// OnFunc registers a handler for a command line or a command name.
func (r *Runner) OnFunc(cmdline string, h Handler) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.handlers == nil {
		r.handlers = map[string]Handler{}
	}
	r.handlers[cmdline] = h
}

// Missing makes LookPath fail for a command even if it has a handler.
func (r *Runner) Missing(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.missing == nil {
		r.missing = map[string]bool{}
	}
	r.missing[name] = true
}

// Calls returns the command lines run so far.
func (r *Runner) Calls() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.calls...)
}

// Ran reports whether a command line was run.
func (r *Runner) Ran(cmdline string) bool {
	for _, c := range r.Calls() {
		if c == cmdline {
			return true
		}
	}
	return false
}

func (r *Runner) handler(c sys.Cmd) (Handler, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.missing[c.Name] {
		return nil, false
	}
	if h, ok := r.handlers[strings.Join(append([]string{c.Name}, c.Args...), " ")]; ok {
		return h, true
	}
	h, ok := r.handlers[c.Name]
	return h, ok
}

func (r *Runner) Run(ctx context.Context, c sys.Cmd) sys.Result {
	if r.Root != "" {
		c.Name = strings.TrimPrefix(c.Name, r.Root)
	}
	line := strings.Join(append([]string{c.Name}, c.Args...), " ")
	r.mu.Lock()
	r.calls = append(r.calls, line)
	r.mu.Unlock()
	h, ok := r.handler(c)
	if !ok {
		return sys.Result{Code: -1, Err: &exec.Error{Name: c.Name, Err: exec.ErrNotFound}}
	}
	if c.OnStart != nil {
		c.OnStart(os.Getpid())
	}
	res := h(c)
	if c.Stdout != nil {
		c.Stdout.Write(res.Output)
		res.Output = nil
	}
	return res
}

// Start runs the handler synchronously (its output goes to c.Stdout) and
// returns a fake PID.
func (r *Runner) Start(c sys.Cmd) (int, error) {
	res := r.Run(context.Background(), c)
	if res.Code == -1 {
		return 0, res.Err
	}
	return FakePID, nil
}

// FakePID is the PID of every process started by Runner.Start.
const FakePID = 424242

func (r *Runner) LookPath(name string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.missing[name] {
		return "", exec.ErrNotFound
	}
	for k := range r.handlers {
		if k == name || strings.HasPrefix(k, name+" ") {
			return "/opt/bin/" + name, nil
		}
	}
	return "", exec.ErrNotFound
}

// Exit builds a result with an exit code (non-zero codes carry an error).
func Exit(out string, code int) sys.Result {
	res := sys.Result{Output: []byte(out), Code: code}
	if code != 0 {
		res.Err = &exitError{code}
	}
	return res
}

type exitError struct{ code int }

func (e *exitError) Error() string { return "exit status " + strconv.Itoa(e.code) }

// Router is a fake router: Env plus handles to its parts.
type Router struct {
	*sys.Env
	T      *testing.T
	Runner *Runner
	Out    *bytes.Buffer
	Err    *bytes.Buffer
	// Killed lists "PID SIGNAL" of every Kill call; no real signal is sent.
	Killed []string
}

// Epoch is the fixed "now" of a fake router.
var Epoch = time.Date(2026, 10, 10, 12, 0, 0, 0, time.Local)

// New returns a fake router running as root with an empty filesystem.
func New(t *testing.T) *Router {
	t.Helper()
	root := t.TempDir()
	r := &Runner{Root: root}
	out, errb := &bytes.Buffer{}, &bytes.Buffer{}
	env := &sys.Env{
		Root:   root,
		Run:    r,
		Now:    func() time.Time { return Epoch },
		Euid:   func() int { return 0 },
		Sleep:  func(time.Duration) {},
		Stdout: out,
		Stderr: errb,
	}
	// A router always has /tmp.
	if err := os.MkdirAll(filepath.Join(root, "tmp"), 0o1777); err != nil {
		t.Fatal(err)
	}
	rt := &Router{Env: env, T: t, Runner: r, Out: out, Err: errb}
	env.Kill = func(pid int, sig syscall.Signal) error {
		rt.Killed = append(rt.Killed, strconv.Itoa(pid)+" "+sig.String())
		return nil
	}
	return rt
}

// Write creates a router file with its parent directories.
func (r *Router) Write(path, content string, perm os.FileMode) {
	r.T.Helper()
	real := r.P(path)
	if err := os.MkdirAll(filepath.Dir(real), 0o755); err != nil {
		r.T.Fatal(err)
	}
	if err := os.WriteFile(real, []byte(content), perm); err != nil {
		r.T.Fatal(err)
	}
	if err := os.Chmod(real, perm); err != nil {
		r.T.Fatal(err)
	}
}

// Read returns a router file, failing the test if it is missing.
func (r *Router) Read(path string) string {
	r.T.Helper()
	b, err := os.ReadFile(r.P(path))
	if err != nil {
		r.T.Fatal(err)
	}
	return string(b)
}

// Glob lists router paths matching a pattern.
func (r *Router) Glob(pattern string) []string {
	r.T.Helper()
	m, err := filepath.Glob(r.P(pattern))
	if err != nil {
		r.T.Fatal(err)
	}
	for i := range m {
		m[i] = strings.TrimPrefix(m[i], r.Root)
	}
	return m
}
