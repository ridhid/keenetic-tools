// Package sys is the boundary to the router: file paths, external commands,
// clock and process checks. Everything above it takes an *Env, so tests run
// whole scenarios in a temporary directory with fake commands.
package sys

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Path is the PATH that every command runs with: Entware first.
const Path = "/opt/sbin:/opt/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// Env carries everything that touches the system.
type Env struct {
	// Root prefixes every absolute path; empty on the router.
	Root  string
	Run   Runner
	Now   func() time.Time
	Euid  func() int
	Sleep func(time.Duration)
	// Kill sends a signal to a process.
	Kill   func(pid int, sig syscall.Signal) error
	Stdout io.Writer
	Stderr io.Writer
}

// New returns the Env for the real system.
func New() *Env {
	os.Setenv("PATH", Path)
	return &Env{
		Run:    ExecRunner{},
		Now:    time.Now,
		Euid:   os.Geteuid,
		Sleep:  time.Sleep,
		Kill:   syscall.Kill,
		Stdout: os.Stdout,
		Stderr: os.Stderr,
	}
}

// P maps an absolute router path to the real one.
func (e *Env) P(path string) string {
	if e.Root == "" {
		return path
	}
	return filepath.Join(e.Root, path)
}

// Cmd describes one external command.
type Cmd struct {
	Name    string
	Args    []string
	Stdin   io.Reader
	Stdout  io.Writer // nil: collected into Result.Output
	Stderr  io.Writer // nil: merged into Stdout
	Timeout time.Duration
	// OnStart is called with the PID once the process runs.
	OnStart func(pid int)
}

// Result is the outcome of a command.
type Result struct {
	Output []byte
	// Code is the exit status; 128+N when killed by signal N, -1 when the
	// command did not start (see Err).
	Code int
	Err  error
}

// Runner runs external commands.
type Runner interface {
	Run(ctx context.Context, c Cmd) Result
	// Start launches a detached background process (own session, stdin
	// from /dev/null) and returns its PID without waiting for it.
	Start(c Cmd) (int, error)
	LookPath(name string) (string, error)
}

// ExecRunner runs real processes.
type ExecRunner struct{}

func (ExecRunner) LookPath(name string) (string, error) { return exec.LookPath(name) }

func (ExecRunner) Run(ctx context.Context, c Cmd) Result {
	if c.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.Timeout)
		defer cancel()
	}
	// On timeout only the process itself gets SIGKILL, never its children.
	cmd := exec.CommandContext(ctx, c.Name, c.Args...)
	// A daemon started by an init script may inherit our pipe and never close it.
	cmd.WaitDelay = 5 * time.Second
	cmd.Stdin = c.Stdin
	var buf strings.Builder
	cmd.Stdout = c.Stdout
	if cmd.Stdout == nil {
		cmd.Stdout = &buf
	}
	cmd.Stderr = c.Stderr
	if cmd.Stderr == nil {
		cmd.Stderr = cmd.Stdout
	}
	if err := cmd.Start(); err != nil {
		return Result{Code: -1, Err: err}
	}
	if c.OnStart != nil {
		c.OnStart(cmd.Process.Pid)
	}
	err := cmd.Wait()
	return Result{Output: []byte(buf.String()), Code: exitCode(cmd.ProcessState), Err: err}
}

func (ExecRunner) Start(c Cmd) (int, error) {
	cmd := exec.Command(c.Name, c.Args...)
	cmd.Stdout, cmd.Stderr = c.Stdout, c.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	pid := cmd.Process.Pid
	// Reap it when it exits so it does not linger as a zombie while we run.
	go cmd.Wait()
	return pid, nil
}

func exitCode(ps *os.ProcessState) int {
	if ps == nil {
		return -1
	}
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return ps.ExitCode()
}

// Output runs a command with a timeout and returns its combined output.
func (e *Env) Output(timeout time.Duration, name string, args ...string) ([]byte, error) {
	r := e.Run.Run(context.Background(), Cmd{Name: name, Args: args, Timeout: timeout})
	return r.Output, r.Err
}

// Have reports whether a command is on PATH.
func (e *Env) Have(name string) bool {
	_, err := e.Run.LookPath(name)
	return err == nil
}

// IsRoot reports whether the process runs as root.
func (e *Env) IsRoot() bool { return e.Euid() == 0 }
