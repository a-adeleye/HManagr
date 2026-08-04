//go:build !windows

package main

import "os/exec"

// hideAIConsole is a no-op outside Windows — there's no console window to hide.
func hideAIConsole(c *exec.Cmd) {}

// killAITree terminates codex. On POSIX, codex is normally a real binary or
// an exec'd shebang script rather than a shim that forks a child, so killing
// the direct process is enough.
func killAITree(c *exec.Cmd) {
	if c.Process == nil {
		return
	}
	_ = c.Process.Kill()
}
