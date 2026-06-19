package local

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestShellEcho exercises the stdin-piped execution path against a real host
// shell. Skipped when no POSIX shell is present (e.g. a bare Windows CI box).
func TestShellEcho(t *testing.T) {
	sh, err := NewShell()
	if err != nil {
		t.Skipf("no shell available: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Single-quoted args must survive intact — this is the whole reason for
	// piping the script over stdin rather than passing it as a -c argument.
	res, err := sh.Exec(ctx, `echo 'hello world'`)
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit %d, stderr=%q", res.ExitCode, res.Stderr)
	}
	if strings.TrimSpace(res.Stdout) != "hello world" {
		t.Fatalf("got %q", res.Stdout)
	}
}

func TestShellExitCode(t *testing.T) {
	sh, err := NewShell()
	if err != nil {
		t.Skipf("no shell available: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := sh.Exec(ctx, "exit 7")
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 7 {
		t.Fatalf("expected exit 7, got %d", res.ExitCode)
	}
}

func TestShellStream(t *testing.T) {
	sh, err := NewShell()
	if err != nil {
		t.Skipf("no shell available: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var got strings.Builder
	code, err := sh.ExecStream(ctx, "printf 'a\\nb\\n'", func(s string) { got.WriteString(s) })
	if err != nil {
		t.Fatal(err)
	}
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(got.String(), "a") || !strings.Contains(got.String(), "b") {
		t.Fatalf("stream missing output: %q", got.String())
	}
}
