//go:build windows

package local

import (
	"os/exec"
	"syscall"
)

// CREATE_NO_WINDOW: run the child process without allocating a console. Without
// it, every shell command we spawn briefly flashes a console window on screen —
// very noticeable in local mode, where the app fires a bash process per action.
const createNoWindow = 0x08000000

// hideConsole suppresses the console window for a child process on Windows.
func hideConsole(c *exec.Cmd) {
	if c.SysProcAttr == nil {
		c.SysProcAttr = &syscall.SysProcAttr{}
	}
	c.SysProcAttr.HideWindow = true
	c.SysProcAttr.CreationFlags |= createNoWindow
}
