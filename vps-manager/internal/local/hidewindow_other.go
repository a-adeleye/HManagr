//go:build !windows

package local

import "os/exec"

// hideConsole is a no-op off Windows — there is no stray console window to hide.
func hideConsole(c *exec.Cmd) {}
