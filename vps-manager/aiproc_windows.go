//go:build windows

package main

import (
	"os/exec"
	"strconv"
	"syscall"
)

// createNoWindow / CREATE_NEW_PROCESS_GROUP: run codex without flashing a
// console window (matches internal/local's convention for spawned shells),
// and put it in its own process group so killAITree can reach the whole
// tree — codex on Windows resolves through an npm .cmd shim, which spawns
// the real codex.exe as a CHILD process; killing just the shim's PID would
// leave codex.exe (and the API call it's mid-flight on) running orphaned.
const createNoWindow = 0x08000000
const createNewProcessGroup = 0x00000200

func hideAIConsole(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: createNoWindow | createNewProcessGroup,
	}
}

// killAITree terminates codex and any child it spawned. taskkill /T walks
// the process tree rooted at the PID; plain Process.Kill() would only hit
// the shim.
func killAITree(c *exec.Cmd) {
	if c.Process == nil {
		return
	}
	_ = exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(c.Process.Pid)).Run()
}
