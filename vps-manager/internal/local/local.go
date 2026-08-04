// Package local runs the same POSIX-shell command strings the rest of the app
// builds for SSH, but on the local host instead. The point of going through a
// POSIX shell (rather than rebuilding every command as an argv slice) is that
// the whole backend constructs commands with POSIX single-quote quoting —
// `docker ps --format '{{json .}}'`, `sh -c '…'`, and so on. Running them
// through bash/sh means the local and remote code paths execute byte-identical
// commands, so a feature that works over SSH works locally with no second
// implementation.
//
// The command string is fed to the shell over **stdin** (`bash` reading a
// script), never as a `-c` argument. On Windows that matters: Go quotes exec
// args with CommandLineToArgvW rules while MSYS programs (Git Bash) re-parse
// with their own rules, so a `-c "…"` round-trip mangles quotes. Piping the
// script as raw bytes sidesteps host-side argument quoting entirely.
package local

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	sshpkg "vps-manager/internal/ssh"
)

func fileExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && !info.IsDir()
}

// Shell executes POSIX shell command strings on the local machine.
type Shell struct {
	bin string // resolved shell binary (bash preferred, sh fallback)
}

// NewShell locates a POSIX shell on the host. On Windows that means Git Bash /
// a bash on PATH; an error here is surfaced when the user tries to enter local
// mode, with guidance to install one.
func NewShell() (*Shell, error) {
	bin, err := findShell()
	if err != nil {
		return nil, err
	}
	return &Shell{bin: bin}, nil
}

// ShellPath reports the shell binary backing this executor (for diagnostics).
func (s *Shell) ShellPath() string { return s.bin }

func findShell() (string, error) {
	if runtime.GOOS == "windows" {
		// On stock Windows, `bash` on PATH is usually C:\Windows\System32\bash.exe
		// — the WSL launcher, which runs in a Linux namespace unrelated to the
		// host Docker the rest of the app drives (and hard-fails if no WSL distro
		// is installed). So prefer a real Git for Windows bash, whose subprocesses
		// (docker.exe / git.exe) resolve on the Windows PATH, BEFORE falling back
		// to a PATH lookup — and never accept the WSL stub.
		candidates := []string{
			`C:\Program Files\Git\bin\bash.exe`,
			`C:\Program Files\Git\usr\bin\bash.exe`,
			`C:\Program Files (x86)\Git\bin\bash.exe`,
			`C:\Program Files\Git\usr\bin\sh.exe`,
		}
		for _, c := range candidates {
			if fileExists(c) {
				return c, nil
			}
		}
		for _, name := range []string{"bash", "sh"} {
			if p, err := exec.LookPath(name); err == nil && !isWSLStub(p) {
				return p, nil
			}
		}
		return "", fmt.Errorf("no usable POSIX shell found — local mode needs Git Bash (install Git for Windows, which bundles it). The Windows-bundled WSL `bash` is not used because it runs in a separate Linux environment")
	}
	for _, name := range []string{"bash", "sh"} {
		if p, err := exec.LookPath(name); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("no POSIX shell (bash/sh) found on PATH")
}

// isWSLStub reports whether p is the Windows WSL launcher (or any System32
// shell stub) rather than a real POSIX shell that runs host executables.
func isWSLStub(p string) bool {
	lower := strings.ToLower(filepath.ToSlash(p))
	return strings.Contains(lower, "/windows/system32/") || strings.HasSuffix(lower, "/wsl.exe")
}

// run executes cmd, feeding it to the shell over stdin, and returns the result.
// extraStdin, if non-empty, is appended after the command and a newline — used
// by ExecWithStdin so a program reading its own stdin (rare locally) still can.
func (s *Shell) run(ctx context.Context, cmd string, onOutput func([]byte)) (*sshpkg.ExecResult, error) {
	c := exec.CommandContext(ctx, s.bin)
	hideConsole(c)
	c.Stdin = strings.NewReader(cmd + "\n")

	var stdout, stderr bytes.Buffer
	if onOutput != nil {
		// Stream mode: tee both streams to the callback as they arrive. A mutex
		// keeps interleaved chunks from racing in the callback.
		var mu sync.Mutex
		c.Stdout = &cbWriter{onOutput, &mu}
		c.Stderr = &cbWriter{onOutput, &mu}
	} else {
		c.Stdout = &stdout
		c.Stderr = &stderr
	}

	err := c.Run()
	res := &sshpkg.ExecResult{Stdout: stdout.String(), Stderr: stderr.String()}
	// Check ctx first: a deadline/cancel kills the process, which surfaces as an
	// *exec.ExitError with a bogus exit code. Mirroring the SSH path, a context
	// error must win so callers see context.DeadlineExceeded, not "exit 1".
	if ctx.Err() != nil {
		res.ExitCode = -1
		return res, ctx.Err()
	}
	if err == nil {
		res.ExitCode = 0
		return res, nil
	}
	if exitErr, ok := err.(*exec.ExitError); ok {
		res.ExitCode = exitErr.ExitCode()
		return res, nil // non-zero exit is not a transport error
	}
	return nil, fmt.Errorf("run shell: %w", err)
}

// Exec mirrors ssh.Connection.Exec.
func (s *Shell) Exec(ctx context.Context, cmd string) (*sshpkg.ExecResult, error) {
	return s.run(ctx, cmd, nil)
}

// ExecWithStdin mirrors ssh.Connection.ExecWithStdin: cmd is expected to read
// stdin itself at some point (e.g. `docker login --password-stdin`). Locally
// cmd is fed to bash over its OWN stdin (see the package doc), so a second
// stdin can't reliably be multiplexed onto that same stream — instead, stdin
// is written to a private temp file and cmd is run with its stdin redirected
// from that file, which is removed again before this returns.
func (s *Shell) ExecWithStdin(ctx context.Context, cmd, stdin string) (*sshpkg.ExecResult, error) {
	f, err := os.CreateTemp("", "vps-manager-stdin-*")
	if err != nil {
		return nil, fmt.Errorf("create stdin temp file: %w", err)
	}
	path := f.Name()
	defer os.Remove(path)
	_, werr := f.WriteString(stdin)
	cerr := f.Close()
	if werr != nil {
		return nil, fmt.Errorf("write stdin temp file: %w", werr)
	}
	if cerr != nil {
		return nil, fmt.Errorf("write stdin temp file: %w", cerr)
	}
	// Git Bash needs forward slashes even for a Windows-style temp path.
	quoted := "'" + strings.ReplaceAll(filepath.ToSlash(path), "'", "'\\''") + "'"
	return s.run(ctx, cmd+" < "+quoted, nil)
}

// ExecStream mirrors ssh.Connection.ExecStream: combined stdout+stderr is
// streamed to onOutput as it arrives; returns the exit code.
func (s *Shell) ExecStream(ctx context.Context, cmd string, onOutput func(string)) (int, error) {
	res, err := s.run(ctx, cmd, func(b []byte) {
		if onOutput != nil {
			onOutput(string(b))
		}
	})
	if err != nil {
		return -1, err
	}
	return res.ExitCode, nil
}

// Preflight verifies docker is reachable so entering local mode fails fast with
// a clear message if Docker Desktop isn't running.
func (s *Shell) Preflight(ctx context.Context) error {
	res, err := s.Exec(ctx, "docker version --format '{{.Server.Version}}'")
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		msg := strings.TrimSpace(res.Stderr)
		if msg == "" {
			msg = strings.TrimSpace(res.Stdout)
		}
		if msg == "" {
			msg = "docker is not available"
		}
		return fmt.Errorf("%s", msg)
	}
	return nil
}

// cbWriter is an io.Writer that forwards each Write to a callback under a mutex.
type cbWriter struct {
	cb func([]byte)
	mu *sync.Mutex
}

func (w *cbWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	w.cb(p)
	w.mu.Unlock()
	return len(p), nil
}

var _ io.Writer = (*cbWriter)(nil)
