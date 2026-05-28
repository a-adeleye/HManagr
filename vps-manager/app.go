package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"vps-manager/internal/config"
	"vps-manager/internal/docker"
	"vps-manager/internal/sftp"
	sshpkg "vps-manager/internal/ssh"

	wailsruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

// App is the struct bound to the frontend. Every exported method is callable
// from JavaScript via the generated wailsjs bindings.
type App struct {
	ctx   context.Context
	store *config.Store
	pool  *sshpkg.Pool

	shmu   sync.Mutex
	shells map[string]*sshpkg.Session // interactive shells keyed by VPS ID
}

func NewApp() *App {
	return &App{
		pool:   sshpkg.NewPool(),
		shells: make(map[string]*sshpkg.Session),
	}
}

func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
	store, err := config.New()
	if err != nil {
		// Fatal at startup — config is required.
		panic(err)
	}
	a.store = store
}

func (a *App) shutdown(ctx context.Context) {
	a.shmu.Lock()
	for id, sess := range a.shells {
		_ = sess.Close()
		delete(a.shells, id)
	}
	a.shmu.Unlock()
	a.pool.Close()
}

// ───────── VPS config CRUD ─────────

func (a *App) ListVPS() []config.VPS {
	return a.store.List()
}

func (a *App) AddVPS(v config.VPS) error {
	return a.store.Add(v)
}

func (a *App) UpdateVPS(v config.VPS) error {
	return a.store.Update(v)
}

func (a *App) DeleteVPS(id string) error {
	a.closeShell(id)
	_ = a.pool.Disconnect(id)
	return a.store.Delete(id)
}

// ───────── Connection lifecycle ─────────

func (a *App) Connect(id string) error {
	v, ok := a.store.Get(id)
	if !ok {
		return fmt.Errorf("vps %s not found", id)
	}
	return a.pool.Connect(sshpkg.ConnectOptions{
		ID:       v.ID,
		Host:     v.Host,
		Port:     v.Port,
		User:     v.User,
		AuthType: v.AuthType,
		KeyPath:  v.KeyPath,
		Password: v.Password,
	})
}

func (a *App) Disconnect(id string) error {
	a.closeShell(id)
	return a.pool.Disconnect(id)
}

func (a *App) IsConnected(id string) bool {
	return a.pool.IsConnected(id)
}

// ───────── Files ─────────

func (a *App) ListFiles(id, dir string) ([]sftp.FileInfo, error) {
	conn, err := a.pool.Get(id)
	if err != nil {
		return nil, err
	}
	if dir == "" {
		dir = "/"
	}
	return sftp.List(conn.Client, dir)
}

func (a *App) DownloadFile(id, remotePath, localPath string) error {
	conn, err := a.pool.Get(id)
	if err != nil {
		return err
	}
	return sftp.Download(conn.Client, remotePath, localPath)
}

func (a *App) UploadFile(id, localPath, remotePath string) error {
	conn, err := a.pool.Get(id)
	if err != nil {
		return err
	}
	return sftp.Upload(conn.Client, localPath, remotePath)
}

func (a *App) ReadRemoteFile(id, remotePath string) (string, error) {
	conn, err := a.pool.Get(id)
	if err != nil {
		return "", err
	}
	return sftp.ReadFile(conn.Client, remotePath)
}

func (a *App) WriteRemoteFile(id, remotePath, content string) error {
	conn, err := a.pool.Get(id)
	if err != nil {
		return err
	}
	return sftp.WriteFile(conn.Client, remotePath, content)
}

func (a *App) DeleteRemoteFile(id, path string) error {
	conn, err := a.pool.Get(id)
	if err != nil {
		return err
	}
	return sftp.Delete(conn.Client, path)
}

// DefaultDownloadDir returns ~/Downloads for the current user, creating it if needed.
func (a *App) DefaultDownloadDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, "Downloads")
	_ = os.MkdirAll(dir, 0755)
	return dir, nil
}

// ChooseSavePath opens a native save-file dialog and returns the chosen path.
func (a *App) ChooseSavePath(defaultDir, defaultFilename string) (string, error) {
	return wailsruntime.SaveFileDialog(a.ctx, wailsruntime.SaveDialogOptions{
		DefaultDirectory: defaultDir,
		DefaultFilename:  defaultFilename,
		Title:            "Save file as",
	})
}

// ChooseOpenPath opens a native open-file dialog and returns the chosen path.
func (a *App) ChooseOpenPath() (string, error) {
	return wailsruntime.OpenFileDialog(a.ctx, wailsruntime.OpenDialogOptions{
		Title: "Select file to upload",
	})
}

// ───────── Docker ─────────

func (a *App) ListContainers(id string) ([]docker.Container, error) {
	conn, err := a.pool.Get(id)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(a.ctx, 15*time.Second)
	defer cancel()
	return docker.List(ctx, conn)
}

func (a *App) RestartContainer(id, containerID string) error {
	return a.containerAction(id, "restart", containerID)
}

func (a *App) StopContainer(id, containerID string) error {
	return a.containerAction(id, "stop", containerID)
}

func (a *App) StartContainer(id, containerID string) error {
	return a.containerAction(id, "start", containerID)
}

func (a *App) containerAction(id, action, containerID string) error {
	conn, err := a.pool.Get(id)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(a.ctx, 30*time.Second)
	defer cancel()
	switch action {
	case "restart":
		return docker.Restart(ctx, conn, containerID)
	case "stop":
		return docker.Stop(ctx, conn, containerID)
	case "start":
		return docker.Start(ctx, conn, containerID)
	}
	return fmt.Errorf("unknown action: %s", action)
}

func (a *App) ContainerLogs(id, containerID string, tail int) (string, error) {
	conn, err := a.pool.Get(id)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(a.ctx, 15*time.Second)
	defer cancel()
	if tail <= 0 {
		tail = 200
	}
	return docker.Logs(ctx, conn, containerID, tail)
}

// ───────── Shell commands ─────────

type CommandResult struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exitCode"`
}

func (a *App) RunCommand(id, cmd string) (*CommandResult, error) {
	conn, err := a.pool.Get(id)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(a.ctx, 60*time.Second)
	defer cancel()
	res, err := conn.Exec(ctx, cmd)
	if err != nil {
		return nil, err
	}
	return &CommandResult{
		Stdout:   res.Stdout,
		Stderr:   res.Stderr,
		ExitCode: res.ExitCode,
	}, nil
}

// ───────── Interactive shell (PTY) ─────────
//
// Unlike RunCommand, these drive a single long-lived PTY shell per VPS, so the
// working directory and any foreground program (psql, nano, …) persist across
// keystrokes. Output is pushed to the frontend via Wails events rather than
// returned, since it arrives asynchronously and continuously:
//
//	"shell:output:<id>"  – base64-encoded chunk of raw terminal bytes
//	"shell:exit:<id>"    – the remote shell has exited
//
// Output is base64-encoded because PTY streams carry arbitrary bytes (including
// incomplete UTF-8 sequences mid-chunk) that JSON string marshaling would
// corrupt.

// StartShell opens an interactive PTY shell for the VPS and begins streaming its
// output. If a shell already exists for this id, it is closed and replaced.
func (a *App) StartShell(id string, cols, rows int) error {
	conn, err := a.pool.Get(id)
	if err != nil {
		return err
	}

	a.shmu.Lock()
	if old, ok := a.shells[id]; ok {
		_ = old.Close()
		delete(a.shells, id)
	}
	a.shmu.Unlock()

	sess, err := conn.Shell(cols, rows,
		func(data []byte) {
			wailsruntime.EventsEmit(a.ctx, "shell:output:"+id, base64.StdEncoding.EncodeToString(data))
		},
		func() {
			a.shmu.Lock()
			delete(a.shells, id)
			a.shmu.Unlock()
			wailsruntime.EventsEmit(a.ctx, "shell:exit:"+id)
		},
	)
	if err != nil {
		return err
	}

	a.shmu.Lock()
	a.shells[id] = sess
	a.shmu.Unlock()
	return nil
}

// WriteShell forwards keystrokes / pasted text to the shell's stdin.
func (a *App) WriteShell(id, data string) error {
	a.shmu.Lock()
	sess, ok := a.shells[id]
	a.shmu.Unlock()
	if !ok {
		return fmt.Errorf("no active shell for %s", id)
	}
	return sess.Write([]byte(data))
}

// ResizeShell tells the remote PTY the terminal was resized.
func (a *App) ResizeShell(id string, cols, rows int) error {
	a.shmu.Lock()
	sess, ok := a.shells[id]
	a.shmu.Unlock()
	if !ok {
		return nil
	}
	return sess.Resize(cols, rows)
}

// CloseShell terminates the shell for the VPS, if any.
func (a *App) CloseShell(id string) error {
	a.shmu.Lock()
	sess, ok := a.shells[id]
	delete(a.shells, id)
	a.shmu.Unlock()
	if !ok {
		return nil
	}
	return sess.Close()
}

// ClipboardText returns the current system clipboard contents. Used by the
// terminal's paste handling, since the WebView doesn't surface native paste
// events to xterm.
func (a *App) ClipboardText() (string, error) {
	return wailsruntime.ClipboardGetText(a.ctx)
}

// SetClipboardText writes text to the system clipboard (terminal copy).
func (a *App) SetClipboardText(text string) error {
	return wailsruntime.ClipboardSetText(a.ctx, text)
}

// closeShell is the internal helper used by lifecycle hooks (disconnect,
// shutdown) to tear down a shell without surfacing an error.
func (a *App) closeShell(id string) {
	a.shmu.Lock()
	sess, ok := a.shells[id]
	delete(a.shells, id)
	a.shmu.Unlock()
	if ok {
		_ = sess.Close()
	}
}

// RunContainerCommand runs `cmd` inside `containerID` via `docker exec sh -c`.
func (a *App) RunContainerCommand(id, containerID, cmd string) (*CommandResult, error) {
	conn, err := a.pool.Get(id)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(a.ctx, 60*time.Second)
	defer cancel()
	res, err := docker.Exec(ctx, conn, containerID, cmd)
	if err != nil {
		return nil, err
	}
	return &CommandResult{
		Stdout:   res.Stdout,
		Stderr:   res.Stderr,
		ExitCode: res.ExitCode,
	}, nil
}
