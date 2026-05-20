package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"vps-manager/internal/config"
	"vps-manager/internal/docker"
	"vps-manager/internal/sftp"
	sshpkg "vps-manager/internal/ssh"
)

// App is the struct bound to the frontend. Every exported method is callable
// from JavaScript via the generated wailsjs bindings.
type App struct {
	ctx   context.Context
	store *config.Store
	pool  *sshpkg.Pool
}

func NewApp() *App {
	return &App{pool: sshpkg.NewPool()}
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
