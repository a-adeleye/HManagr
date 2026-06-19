package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"vps-manager/internal/config"
	"vps-manager/internal/db"
	"vps-manager/internal/deploy"
	"vps-manager/internal/docker"
	"vps-manager/internal/local"
	"vps-manager/internal/migration"
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

	// sudoPasswords is held in process memory only, keyed by VPS ID, and
	// cleared on Disconnect / DeleteVPS / shutdown. Never persisted.
	sudoMu        sync.Mutex
	sudoPasswords map[string]string

	// dbCache holds detected DB containers (incl. sniffed credentials, which
	// never leave the backend) keyed by vpsID+"\x00"+containerID. Refreshed on
	// every ListDBContainers, dropped on Disconnect.
	dbMu    sync.Mutex
	dbCache map[string]*db.Container

	// local is the host-shell executor for the virtual "Local" environment. It
	// is nil if no POSIX shell was found on the host; localReady flips true once
	// the user "connects" (which runs a docker preflight). localReady is atomic
	// because Wails dispatches each bound method on its own goroutine, so
	// Connect/Disconnect (writers) and IsConnected (reader) can overlap.
	local      *local.Shell
	localErr   error
	localReady atomic.Bool
}

// LocalID is the reserved VPS id for the virtual "this machine" environment.
// Commands for it run on the host shell instead of over SSH.
const LocalID = "local"

func isLocal(id string) bool { return id == LocalID }

// localVPS is the synthetic sidebar entry for local management.
func localVPS() config.VPS {
	return config.VPS{ID: LocalID, Name: "Local (this machine)", IsLocal: true}
}

func NewApp() *App {
	return &App{
		pool:          sshpkg.NewPool(),
		shells:        make(map[string]*sshpkg.Session),
		sudoPasswords: make(map[string]string),
		dbCache:       make(map[string]*db.Container),
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
	// Locate a host shell for local mode. A failure here isn't fatal — it just
	// means local mode is unavailable, surfaced when the user tries to use it.
	if sh, err := local.NewShell(); err != nil {
		a.localErr = err
	} else {
		a.local = sh
	}
}

func (a *App) shutdown(ctx context.Context) {
	a.shmu.Lock()
	for id, sess := range a.shells {
		_ = sess.Close()
		delete(a.shells, id)
	}
	a.shmu.Unlock()
	a.sudoMu.Lock()
	for k := range a.sudoPasswords {
		delete(a.sudoPasswords, k)
	}
	a.sudoMu.Unlock()
	a.pool.Close()
}

// ───────── VPS config CRUD ─────────

// ListVPS returns the saved servers with the virtual Local entry prepended, so
// "this machine" always appears first in the sidebar.
func (a *App) ListVPS() []config.VPS {
	return append([]config.VPS{localVPS()}, a.store.List()...)
}

// LocalAvailable reports whether local mode can be used (a host shell exists);
// when false, the reason explains what's missing (e.g. no bash on Windows).
func (a *App) LocalAvailable() bool { return a.local != nil }

func (a *App) LocalUnavailableReason() string {
	if a.localErr != nil {
		return a.localErr.Error()
	}
	return ""
}

func (a *App) AddVPS(v config.VPS) error {
	if isLocal(v.ID) {
		return fmt.Errorf("the local environment cannot be added or edited")
	}
	_, err := a.store.Add(v)
	return err
}

func (a *App) UpdateVPS(v config.VPS) error {
	if isLocal(v.ID) {
		return fmt.Errorf("the local environment cannot be edited")
	}
	return a.store.Update(v)
}

func (a *App) DeleteVPS(id string) error {
	if isLocal(id) {
		return fmt.Errorf("the local environment cannot be deleted")
	}
	a.closeShell(id)
	a.ClearSudoPassword(id)
	_ = a.pool.Disconnect(id)
	return a.store.Delete(id)
}

// ───────── Projects ─────────
//
// A project is a named server+path bookmark. Selecting one connects to its
// server (reusing a live connection) and roots the session at the deploy path.

func (a *App) ListProjects() []config.Project {
	return a.store.ListProjects()
}

// SaveProject inserts or updates a project. When p.VPSID is empty, newServer is
// treated as inline server details: a new server is created and the project is
// linked to it (so the same server also appears in the Servers list). When
// p.VPSID is set, newServer is ignored and the existing server is used.
func (a *App) SaveProject(p config.Project, newServer config.VPS) (config.Project, error) {
	if strings.TrimSpace(p.Name) == "" || strings.TrimSpace(p.Path) == "" {
		return config.Project{}, fmt.Errorf("project name and deploy path are required")
	}
	if p.VPSID == "" {
		if strings.TrimSpace(newServer.Host) == "" {
			return config.Project{}, fmt.Errorf("choose an existing server or enter server details")
		}
		v, err := a.store.Add(newServer)
		if err != nil {
			return config.Project{}, err
		}
		p.VPSID = v.ID
		saved, err := a.store.SaveProject(p)
		if err != nil {
			// The server was already persisted; don't leave it orphaned with no
			// project pointing at it if the project save then failed.
			_ = a.store.Delete(v.ID)
			return config.Project{}, err
		}
		return saved, nil
	}
	if !isLocal(p.VPSID) {
		if _, ok := a.store.Get(p.VPSID); !ok {
			return config.Project{}, fmt.Errorf("the selected server no longer exists")
		}
	}
	return a.store.SaveProject(p)
}

func (a *App) DeleteProject(id string) error {
	return a.store.DeleteProject(id)
}

// ───────── Connection lifecycle ─────────

func (a *App) Connect(id string) error {
	if isLocal(id) {
		if a.local == nil {
			if a.localErr != nil {
				return a.localErr
			}
			return fmt.Errorf("local mode is unavailable on this machine")
		}
		ctx, cancel := context.WithTimeout(a.ctx, 15*time.Second)
		defer cancel()
		if err := a.local.Preflight(ctx); err != nil {
			return fmt.Errorf("docker not reachable locally: %w", err)
		}
		a.localReady.Store(true)
		return nil
	}
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
	a.dropDBCache(id)
	if isLocal(id) {
		a.localReady.Store(false)
		return nil
	}
	a.closeShell(id)
	a.ClearSudoPassword(id)
	return a.pool.Disconnect(id)
}

func (a *App) IsConnected(id string) bool {
	if isLocal(id) {
		return a.localReady.Load()
	}
	return a.pool.IsConnected(id)
}

// ───────── Files ─────────

// LocalStartDir is the directory the file browser opens to in local mode
// (the user's home), forward-slashed so the frontend's "/" path logic works.
func (a *App) LocalStartDir() string { return local.HomeDir() }

func (a *App) ListFiles(id, dir string) ([]sftp.FileInfo, error) {
	if isLocal(id) {
		if dir == "" {
			dir = local.HomeDir()
		}
		return local.List(dir)
	}
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
	if isLocal(id) {
		return local.Download(remotePath, localPath)
	}
	conn, err := a.pool.Get(id)
	if err != nil {
		return err
	}
	return sftp.Download(conn.Client, remotePath, localPath)
}

func (a *App) UploadFile(id, localPath, remotePath string) error {
	if isLocal(id) {
		return local.Upload(localPath, remotePath)
	}
	conn, err := a.pool.Get(id)
	if err != nil {
		return err
	}
	return sftp.Upload(conn.Client, localPath, remotePath)
}

func (a *App) ReadRemoteFile(id, remotePath string) (string, error) {
	if isLocal(id) {
		return local.ReadFile(remotePath)
	}
	conn, err := a.pool.Get(id)
	if err != nil {
		return "", err
	}
	return sftp.ReadFile(conn.Client, remotePath)
}

func (a *App) WriteRemoteFile(id, remotePath, content string) error {
	if isLocal(id) {
		return local.WriteFile(remotePath, content)
	}
	conn, err := a.pool.Get(id)
	if err != nil {
		return err
	}
	return sftp.WriteFile(conn.Client, remotePath, content)
}

func (a *App) DeleteRemoteFile(id, path string) error {
	if isLocal(id) {
		return local.Delete(path)
	}
	conn, err := a.pool.Get(id)
	if err != nil {
		return err
	}
	return sftp.Delete(conn.Client, path)
}

// MakeDir creates a directory (mkdir -p semantics). On a remote host with
// useSudo it shells out to `sudo mkdir -p` (SFTP can't elevate); locally it
// uses the native filesystem and ignores useSudo.
func (a *App) MakeDir(id, path string, useSudo bool) error {
	if isLocal(id) {
		return local.Mkdir(path)
	}
	if useSudo {
		res, err := a.execAsSudo(id, "mkdir -p "+shellQuote(path))
		return sudoErr(res, err)
	}
	conn, err := a.pool.Get(id)
	if err != nil {
		return err
	}
	return sftp.Mkdir(conn.Client, path)
}

// ───────── Sudo ─────────
//
// Sudo passwords live in process memory only, keyed by VPS ID. They're cleared
// on Disconnect / DeleteVPS / shutdown and never persisted. If no password is
// cached, sudo runs with `-n` (non-interactive) — fine for NOPASSWD setups,
// fails otherwise with a clear error so the UI can prompt.

func (a *App) SetSudoPassword(id, password string) {
	a.sudoMu.Lock()
	defer a.sudoMu.Unlock()
	a.sudoPasswords[id] = password
}

func (a *App) HasSudoPassword(id string) bool {
	a.sudoMu.Lock()
	defer a.sudoMu.Unlock()
	_, ok := a.sudoPasswords[id]
	return ok
}

func (a *App) ClearSudoPassword(id string) {
	a.sudoMu.Lock()
	defer a.sudoMu.Unlock()
	delete(a.sudoPasswords, id)
}

// execAsSudo prefixes cmd with sudo using a 30s timeout (the default for one-
// off quick commands like mkdir / chmod). For long-running operations supply
// your own context via execAsSudoCtx.
func (a *App) execAsSudo(id, cmd string) (*sshpkg.ExecResult, error) {
	ctx, cancel := context.WithTimeout(a.ctx, 30*time.Second)
	defer cancel()
	return a.execAsSudoCtx(ctx, id, cmd)
}

// execAsSudoCtx is the underlying sudo'd exec. It piggybacks on the cached
// sudo password if there is one (`sudo -S -p ''`); otherwise falls back to
// non-interactive (`sudo -n`), which fails fast for non-NOPASSWD setups.
func (a *App) execAsSudoCtx(ctx context.Context, id, cmd string) (*sshpkg.ExecResult, error) {
	conn, err := a.pool.Get(id)
	if err != nil {
		return nil, err
	}
	// Connecting as root already has full privileges — and minimal images
	// often don't ship sudo at all — so skip the prefix entirely.
	if v, ok := a.store.Get(id); ok && v.User == "root" {
		return conn.Exec(ctx, cmd)
	}
	// Elevate the whole line under one shell. Without this, compound commands
	// like `cd X && docker compose stop` would elevate only the first word
	// (`sudo cd …`), which both fails and leaves the part that actually
	// needed root running unprivileged.
	wrapped := "sh -c " + shellQuote(cmd)
	a.sudoMu.Lock()
	pwd, hasPwd := a.sudoPasswords[id]
	a.sudoMu.Unlock()
	if hasPwd {
		return conn.ExecWithStdin(ctx, "sudo -S -p '' "+wrapped, pwd+"\n")
	}
	return conn.Exec(ctx, "sudo -n "+wrapped)
}

// execFn returns an exec function that runs commands on the given VPS,
// optionally wrapped with sudo. The unnamed func type is assignable to
// migration.ExecFn, db.ExecFn, and deploy.ExecFn alike. The returned function
// is safe to call across goroutine boundaries — it re-resolves the connection
// from the pool each call, so it survives transient reconnects.
func (a *App) execFn(id string, useSudo bool) func(context.Context, string) (*sshpkg.ExecResult, error) {
	if isLocal(id) {
		// Local mode has no sudo concept — commands run as the desktop user.
		return func(ctx context.Context, cmd string) (*sshpkg.ExecResult, error) {
			if a.local == nil {
				return nil, fmt.Errorf("local mode is unavailable on this machine")
			}
			return a.local.Exec(ctx, cmd)
		}
	}
	if useSudo {
		return func(ctx context.Context, cmd string) (*sshpkg.ExecResult, error) {
			return a.execAsSudoCtx(ctx, id, cmd)
		}
	}
	return func(ctx context.Context, cmd string) (*sshpkg.ExecResult, error) {
		conn, err := a.pool.Get(id)
		if err != nil {
			return nil, err
		}
		return conn.Exec(ctx, cmd)
	}
}

// sudoErr distills an ExecResult+err pair into one error suitable for the UI.
// It special-cases the "password required" failure mode so the frontend can
// react by prompting for one.
func sudoErr(res *sshpkg.ExecResult, err error) error {
	if err != nil {
		return err
	}
	if res.ExitCode == 0 {
		return nil
	}
	msg := strings.TrimSpace(res.Stderr)
	if msg == "" {
		msg = strings.TrimSpace(res.Stdout)
	}
	if msg == "" {
		msg = fmt.Sprintf("exit %d", res.ExitCode)
	}
	if strings.Contains(msg, "a password is required") ||
		strings.Contains(msg, "no tty present") {
		return errors.New("sudo password required")
	}
	if strings.Contains(msg, "is not in the sudoers file") ||
		strings.Contains(msg, "not allowed to execute") {
		return errors.New("this user is not allowed to use sudo")
	}
	return errors.New(msg)
}

// ───────── Permissions ─────────

// PathInfo is StatRemoteFile's response: mode + numeric owner/group plus
// resolved owner/group names (best-effort; empty string if lookup fails).
type PathInfo struct {
	Mode  string `json:"mode"`
	UID   int    `json:"uid"`
	GID   int    `json:"gid"`
	Owner string `json:"owner"`
	Group string `json:"group"`
	IsDir bool   `json:"isDir"`
}

// StatRemoteFile returns mode/owner info for the permissions modal. Owner and
// group names are resolved via getent over SSH; if that fails the numeric IDs
// are still returned.
func (a *App) StatRemoteFile(id, p string) (*PathInfo, error) {
	if isLocal(id) {
		st, err := local.Stat(p)
		if err != nil {
			return nil, err
		}
		return &PathInfo{Mode: st.Mode, IsDir: st.IsDir}, nil
	}
	conn, err := a.pool.Get(id)
	if err != nil {
		return nil, err
	}
	st, err := sftp.Stat(conn.Client, p)
	if err != nil {
		return nil, err
	}
	info := &PathInfo{Mode: st.Mode, UID: st.UID, GID: st.GID, IsDir: st.IsDir}

	ctx, cancel := context.WithTimeout(a.ctx, 10*time.Second)
	defer cancel()
	if name, err := lookupName(ctx, conn, "passwd", st.UID); err == nil {
		info.Owner = name
	}
	if name, err := lookupName(ctx, conn, "group", st.GID); err == nil {
		info.Group = name
	}
	return info, nil
}

// ChmodRemoteFile sets the mode on p. mode is parsed as octal (with or without
// a leading 0), e.g. "755" or "0755". With useSudo=true, runs `sudo chmod`
// instead of SFTP's Chmod (since SFTP can't elevate).
func (a *App) ChmodRemoteFile(id, p, mode string, useSudo bool) error {
	mode = strings.TrimSpace(mode)
	if mode == "" {
		return fmt.Errorf("mode is required")
	}
	clean := strings.TrimPrefix(strings.TrimPrefix(mode, "0o"), "0")
	if clean == "" {
		clean = "0"
	}
	m, err := strconv.ParseUint(clean, 8, 32)
	if err != nil {
		return fmt.Errorf("invalid mode (use octal like 755): %w", err)
	}
	if isLocal(id) {
		return local.Chmod(p, os.FileMode(m))
	}
	if useSudo {
		res, err := a.execAsSudo(id, fmt.Sprintf("chmod %o %s", m, shellQuote(p)))
		return sudoErr(res, err)
	}
	conn, err := a.pool.Get(id)
	if err != nil {
		return err
	}
	return sftp.Chmod(conn.Client, p, os.FileMode(m))
}

// ChownRemoteFile changes owner and/or group of p. owner and group may be
// either a name or a numeric id; an empty string leaves that side unchanged.
// With useSudo=true, runs `sudo chown` (the usual case — only root can chown).
func (a *App) ChownRemoteFile(id, p, owner, group string, useSudo bool) error {
	owner = strings.TrimSpace(owner)
	group = strings.TrimSpace(group)
	if owner == "" && group == "" {
		return nil
	}
	if isLocal(id) {
		return fmt.Errorf("changing owner/group isn't supported in local mode")
	}

	if useSudo {
		// chown accepts owner, :group, or owner:group directly — no need to
		// resolve names to numeric IDs ourselves.
		var spec string
		switch {
		case owner != "" && group != "":
			spec = owner + ":" + group
		case owner != "":
			spec = owner
		default:
			spec = ":" + group
		}
		res, err := a.execAsSudo(id, "chown "+shellQuote(spec)+" "+shellQuote(p))
		return sudoErr(res, err)
	}

	// Non-sudo path: SFTP needs numeric uid/gid, so resolve via getent.
	conn, err := a.pool.Get(id)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(a.ctx, 10*time.Second)
	defer cancel()

	uid, gid := -1, -1
	if owner != "" {
		v, err := resolveID(ctx, conn, "passwd", owner)
		if err != nil {
			return fmt.Errorf("owner: %w", err)
		}
		uid = v
	}
	if group != "" {
		v, err := resolveID(ctx, conn, "group", group)
		if err != nil {
			return fmt.Errorf("group: %w", err)
		}
		gid = v
	}
	return sftp.Chown(conn.Client, p, uid, gid)
}

// resolveID maps either a numeric id or a name to a uid (db="passwd") or
// gid (db="group") via `getent` on the remote host.
func resolveID(ctx context.Context, conn *sshpkg.Connection, db, nameOrID string) (int, error) {
	res, err := conn.Exec(ctx, "getent "+db+" "+shellQuote(nameOrID))
	if err != nil {
		return 0, err
	}
	if res.ExitCode != 0 {
		return 0, fmt.Errorf("%q not found in %s", nameOrID, db)
	}
	// getent passwd: name:x:UID:GID:...
	// getent group:  name:x:GID:...
	parts := strings.Split(strings.TrimSpace(res.Stdout), ":")
	if len(parts) < 3 {
		return 0, fmt.Errorf("malformed getent output")
	}
	return strconv.Atoi(parts[2])
}

// lookupName is the inverse: numeric id → name via getent.
func lookupName(ctx context.Context, conn *sshpkg.Connection, db string, id int) (string, error) {
	res, err := conn.Exec(ctx, fmt.Sprintf("getent %s %d", db, id))
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("not found")
	}
	parts := strings.SplitN(strings.TrimSpace(res.Stdout), ":", 2)
	return parts[0], nil
}

// shellQuote wraps s in single quotes for safe inclusion in a remote command.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
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

// ListContainers lists the server's containers. When deployPath is non-empty
// (the active project's path), the list is scoped to that compose project.
func (a *App) ListContainers(id, deployPath string) ([]docker.Container, error) {
	ctx, cancel := context.WithTimeout(a.ctx, 15*time.Second)
	defer cancel()
	return docker.List(ctx, a.execFn(id, false), docker.ComposeWorkingDirFilter(deployPath))
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
	ctx, cancel := context.WithTimeout(a.ctx, 30*time.Second)
	defer cancel()
	exec := a.execFn(id, false)
	switch action {
	case "restart":
		return docker.Restart(ctx, exec, containerID)
	case "stop":
		return docker.Stop(ctx, exec, containerID)
	case "start":
		return docker.Start(ctx, exec, containerID)
	}
	return fmt.Errorf("unknown action: %s", action)
}

func (a *App) ContainerLogs(id, containerID string, tail int) (string, error) {
	ctx, cancel := context.WithTimeout(a.ctx, 15*time.Second)
	defer cancel()
	if tail <= 0 {
		tail = 200
	}
	return docker.Logs(ctx, a.execFn(id, false), containerID, tail)
}

// ───────── Shell commands ─────────

type CommandResult struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exitCode"`
}

func (a *App) RunCommand(id, cmd string) (*CommandResult, error) {
	ctx, cancel := context.WithTimeout(a.ctx, 60*time.Second)
	defer cancel()
	res, err := a.execFn(id, false)(ctx, cmd)
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
	if isLocal(id) {
		// A real local PTY needs ConPTY plumbing we don't ship; the local
		// environment is for Docker/DB/Deploy/Files, not an interactive shell.
		return fmt.Errorf("the interactive terminal isn't available in local mode — use your own terminal on this machine")
	}
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

// ───────── Migration ─────────
//
// Migration moves a docker-compose stack from one connected VPS to another.
// The Inspect step is synchronous and returns the inventory the UI shows.
// RunMigration kicks off the actual transfer in a goroutine and streams
// progress via Wails events ("migration:log", "migration:done") so the UI can
// render a live log without blocking on a many-minute call.

// FindComposeFile picks the first known compose filename present in dir on the
// source VPS, so the wizard doesn't have to ask the user to type it.
func (a *App) FindComposeFile(id, dir string) (string, error) {
	ctx, cancel := context.WithTimeout(a.ctx, 10*time.Second)
	defer cancel()
	// File existence checks don't need elevation — the user already has SFTP
	// access to anything they can navigate to.
	return migration.FindComposeFile(ctx, a.execFn(id, false), dir)
}

// InspectMigration parses the compose stack at sourcePath via
// `docker compose config --format json` on the source VPS and returns the
// resolved inventory (services, volumes, env files, bind-mount warnings).
// useSudo prefixes the docker commands with sudo for users whose docker
// daemon isn't accessible without it.
func (a *App) InspectMigration(id, sourcePath string, useSudo bool) (*migration.Inventory, error) {
	ctx, cancel := context.WithTimeout(a.ctx, 60*time.Second)
	defer cancel()
	return migration.Inspect(ctx, a.execFn(id, useSudo), sourcePath)
}

// RunMigration starts the transfer in a goroutine, streaming each progress
// line through "migration:log" and emitting "migration:done" with the final
// outcome ("" on success, an error string otherwise). The call itself returns
// as soon as the goroutine is launched.
func (a *App) RunMigration(srcID, dstID string, opts migration.RunOptions, useSudo bool) error {
	if isLocal(srcID) || isLocal(dstID) {
		return fmt.Errorf("migration runs between two SSH servers; the local environment can't be a source or target")
	}
	src, err := a.pool.Get(srcID)
	if err != nil {
		return fmt.Errorf("source: %w", err)
	}
	dst, err := a.pool.Get(dstID)
	if err != nil {
		return fmt.Errorf("target: %w", err)
	}
	srcExec := a.execFn(srcID, useSudo)
	dstExec := a.execFn(dstID, useSudo)
	go func() {
		log := func(line string) {
			wailsruntime.EventsEmit(a.ctx, "migration:log", line)
		}
		err := migration.Run(a.ctx, src, dst, srcExec, dstExec, opts, log)
		msg := ""
		if err != nil {
			msg = err.Error()
		}
		wailsruntime.EventsEmit(a.ctx, "migration:done", msg)
	}()
	return nil
}

// ───────── Databases ─────────
//
// The DB manager talks to database engines running inside docker containers
// via `docker exec` + the engine CLI (see internal/db). The frontend only ever
// holds container IDs; credentials are sniffed from the container environment
// and cached backend-side.

// ListDBContainers discovers database containers on the VPS and refreshes the
// credential cache for them. When deployPath is non-empty (active project), the
// list is scoped to that compose project's containers.
func (a *App) ListDBContainers(id string, useSudo bool, deployPath string) ([]db.Container, error) {
	ctx, cancel := context.WithTimeout(a.ctx, 30*time.Second)
	defer cancel()
	list, err := db.ListContainers(ctx, a.execFn(id, useSudo), docker.ComposeWorkingDirFilter(deployPath))
	if err != nil {
		return nil, err
	}
	a.dbMu.Lock()
	for i := range list {
		c := list[i]
		a.dbCache[id+"\x00"+c.ID] = &c
	}
	a.dbMu.Unlock()
	return list, nil
}

// dbContainer resolves a cached container, re-listing once if the cache is
// cold (e.g. after an app restart while the UI kept its state).
func (a *App) dbContainer(vpsID, containerID string, useSudo bool) (*db.Container, error) {
	a.dbMu.Lock()
	c, ok := a.dbCache[vpsID+"\x00"+containerID]
	a.dbMu.Unlock()
	if ok {
		return c, nil
	}
	// Cold cache: re-list unscoped so the container is found regardless of any
	// project filter active in the UI.
	if _, err := a.ListDBContainers(vpsID, useSudo, ""); err != nil {
		return nil, err
	}
	a.dbMu.Lock()
	c, ok = a.dbCache[vpsID+"\x00"+containerID]
	a.dbMu.Unlock()
	if !ok {
		return nil, fmt.Errorf("container %s not found on this VPS", containerID)
	}
	return c, nil
}

func (a *App) dropDBCache(vpsID string) {
	a.dbMu.Lock()
	for k := range a.dbCache {
		if strings.HasPrefix(k, vpsID+"\x00") {
			delete(a.dbCache, k)
		}
	}
	a.dbMu.Unlock()
}

func (a *App) DBListDatabases(vpsID, containerID string, useSudo bool) ([]string, error) {
	c, err := a.dbContainer(vpsID, containerID, useSudo)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(a.ctx, 30*time.Second)
	defer cancel()
	return db.ListDatabases(ctx, a.execFn(vpsID, useSudo), c)
}

func (a *App) DBListTables(vpsID, containerID, dbName string, useSudo bool) ([]db.Table, error) {
	c, err := a.dbContainer(vpsID, containerID, useSudo)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(a.ctx, 30*time.Second)
	defer cancel()
	return db.ListTables(ctx, a.execFn(vpsID, useSudo), c, dbName)
}

func (a *App) DBTableColumns(vpsID, containerID, dbName, schema, table string, useSudo bool) ([]db.Column, error) {
	c, err := a.dbContainer(vpsID, containerID, useSudo)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(a.ctx, 30*time.Second)
	defer cancel()
	return db.TableColumns(ctx, a.execFn(vpsID, useSudo), c, dbName, schema, table)
}

func (a *App) DBTableRows(vpsID, containerID, dbName, schema, table string, limit, offset int, useSudo bool) (*db.QueryResult, error) {
	c, err := a.dbContainer(vpsID, containerID, useSudo)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(a.ctx, 60*time.Second)
	defer cancel()
	return db.TableRows(ctx, a.execFn(vpsID, useSudo), c, dbName, schema, table, limit, offset)
}

func (a *App) DBQuery(vpsID, containerID, dbName, sql string, useSudo bool) (*db.QueryResult, error) {
	c, err := a.dbContainer(vpsID, containerID, useSudo)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(a.ctx, 120*time.Second)
	defer cancel()
	return db.Query(ctx, a.execFn(vpsID, useSudo), c, dbName, sql)
}

func (a *App) DBInsertRow(vpsID, containerID, dbName, schema, table string, values map[string]*string, useSudo bool) (string, error) {
	c, err := a.dbContainer(vpsID, containerID, useSudo)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(a.ctx, 30*time.Second)
	defer cancel()
	return db.InsertRow(ctx, a.execFn(vpsID, useSudo), c, dbName, schema, table, values)
}

func (a *App) DBUpdateRow(vpsID, containerID, dbName, schema, table string, pk, values map[string]*string, useSudo bool) (string, error) {
	c, err := a.dbContainer(vpsID, containerID, useSudo)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(a.ctx, 30*time.Second)
	defer cancel()
	return db.UpdateRow(ctx, a.execFn(vpsID, useSudo), c, dbName, schema, table, pk, values)
}

func (a *App) DBDeleteRow(vpsID, containerID, dbName, schema, table string, pk map[string]*string, useSudo bool) (string, error) {
	c, err := a.dbContainer(vpsID, containerID, useSudo)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(a.ctx, 30*time.Second)
	defer cancel()
	return db.DeleteRow(ctx, a.execFn(vpsID, useSudo), c, dbName, schema, table, pk)
}

// ───────── Deployments ─────────
//
// Deploy a GitHub repo to a VPS with docker compose. Definitions persist in
// the config store; each run streams its log through "deploy:log:<id>" and
// finishes with "deploy:done:<id>" ("" on success, error string otherwise).

func (a *App) ListDeployments() []config.Deployment {
	return a.store.ListDeployments()
}

func (a *App) SaveDeployment(d config.Deployment) (config.Deployment, error) {
	if strings.TrimSpace(d.Name) == "" || strings.TrimSpace(d.RepoURL) == "" ||
		strings.TrimSpace(d.Path) == "" || d.VPSID == "" {
		return config.Deployment{}, fmt.Errorf("name, VPS, repo URL and path are required")
	}
	return a.store.SaveDeployment(d)
}

func (a *App) DeleteDeployment(id string) error {
	return a.store.DeleteDeployment(id)
}

// streamFn mirrors execFn for streaming commands, applying the same sudo
// policy (skip for root, non-interactive `sudo -n` otherwise — failures
// surface as-is, no password prompting).
func (a *App) streamFn(id string, useSudo bool) deploy.StreamFn {
	if isLocal(id) {
		return func(ctx context.Context, cmd string, onOutput func(string)) (int, error) {
			if a.local == nil {
				return -1, fmt.Errorf("local mode is unavailable on this machine")
			}
			return a.local.ExecStream(ctx, cmd, onOutput)
		}
	}
	return func(ctx context.Context, cmd string, onOutput func(string)) (int, error) {
		conn, err := a.pool.Get(id)
		if err != nil {
			return -1, err
		}
		full := cmd
		if useSudo {
			if v, ok := a.store.Get(id); !ok || v.User != "root" {
				full = "sudo -n sh -c " + shellQuote(cmd)
			}
		}
		return conn.ExecStream(ctx, full, onOutput)
	}
}

// RunDeploy starts a deploy in a goroutine. Auto-connects the target VPS if
// needed so a deploy is one click from a cold start.
func (a *App) RunDeploy(deployID string) error {
	d, ok := a.store.GetDeployment(deployID)
	if !ok {
		return fmt.Errorf("deployment %s not found", deployID)
	}
	if isLocal(d.VPSID) {
		if err := a.Connect(LocalID); err != nil {
			return err
		}
	} else {
		if _, ok := a.store.Get(d.VPSID); !ok {
			return fmt.Errorf("the VPS for this deployment no longer exists")
		}
		if !a.pool.IsConnected(d.VPSID) {
			if err := a.Connect(d.VPSID); err != nil {
				return fmt.Errorf("connect: %w", err)
			}
		}
	}

	exec := a.execFn(d.VPSID, d.UseSudo)
	stream := a.streamFn(d.VPSID, d.UseSudo)
	a.store.SetDeployStatus(d.ID, "running", "", time.Now().Format(time.RFC3339))

	// Local deploys write .env natively to dodge Git Bash's CR-stripping.
	var writeFile func(string, string) error
	if isLocal(d.VPSID) {
		writeFile = func(p, content string) error {
			if err := local.WriteFile(p, content); err != nil {
				return err
			}
			_ = os.Chmod(p, 0o600) // best-effort; a no-op on Windows ACLs
			return nil
		}
	}

	go func() {
		log := func(line string) {
			wailsruntime.EventsEmit(a.ctx, "deploy:log:"+d.ID, line)
		}
		commit, err := deploy.Run(a.ctx, exec, stream, deploy.Options{
			RepoURL:     d.RepoURL,
			Branch:      d.Branch,
			Path:        d.Path,
			ComposeFile: d.ComposeFile,
			Token:       d.GithubToken,
			EnvVars:     d.EnvVars,
			WriteFile:   writeFile,
		}, log)
		status, msg := "success", ""
		if err != nil {
			status, msg = "failed", err.Error()
		}
		a.store.SetDeployStatus(d.ID, status, commit, time.Now().Format(time.RFC3339))
		wailsruntime.EventsEmit(a.ctx, "deploy:done:"+d.ID, msg)
	}()
	return nil
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
	ctx, cancel := context.WithTimeout(a.ctx, 60*time.Second)
	defer cancel()
	res, err := docker.Exec(ctx, a.execFn(id, false), containerID, cmd)
	if err != nil {
		return nil, err
	}
	return &CommandResult{
		Stdout:   res.Stdout,
		Stderr:   res.Stderr,
		ExitCode: res.ExitCode,
	}, nil
}
