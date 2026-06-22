package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"vps-manager/internal/backup"
	"vps-manager/internal/caddy"
	"vps-manager/internal/config"
	"vps-manager/internal/db"
	"vps-manager/internal/deploy"
	"vps-manager/internal/docker"
	"vps-manager/internal/local"
	"vps-manager/internal/maintenance"
	"vps-manager/internal/migration"
	"vps-manager/internal/provision"
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
		pool:          sshpkg.NewPool(config.KnownHostsPath()),
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

// ConnectStatus is the result of a connection attempt. A successful connect sets
// Connected. An unverified SSH host key is surfaced (not thrown) so the frontend
// can show the fingerprint and prompt: NeedsTrust for a never-seen host,
// KeyChanged for a host whose key no longer matches (a possible MITM).
type ConnectStatus struct {
	Connected   bool   `json:"connected"`
	NeedsTrust  bool   `json:"needsTrust"`
	KeyChanged  bool   `json:"keyChanged"`
	Fingerprint string `json:"fingerprint,omitempty"`
	KeyType     string `json:"keyType,omitempty"`
	Host        string `json:"host,omitempty"`
	Message     string `json:"message,omitempty"`
}

// Connect dials the VPS, verifying its SSH host key against known_hosts. An
// unknown or changed key returns a ConnectStatus (no error) describing what the
// user must confirm; call TrustHostKey to proceed once they've reviewed it.
func (a *App) Connect(id string) (*ConnectStatus, error) {
	return a.connect(id, false)
}

// TrustHostKey retries the connection, trusting (and persisting) a host key the
// user has reviewed. For a changed key, call ForgetHostKey first.
func (a *App) TrustHostKey(id string) (*ConnectStatus, error) {
	return a.connect(id, true)
}

// ForgetHostKey drops the stored host key for a server so it can be re-trusted
// (e.g. after a legitimate rebuild that triggered a KeyChanged status).
func (a *App) ForgetHostKey(id string) error {
	if isLocal(id) {
		return nil
	}
	v, ok := a.store.Get(id)
	if !ok {
		return fmt.Errorf("vps %s not found", id)
	}
	return a.pool.ForgetHostKey(v.Host, v.Port)
}

func (a *App) connect(id string, trustNew bool) (*ConnectStatus, error) {
	if isLocal(id) {
		if a.local == nil {
			if a.localErr != nil {
				return nil, a.localErr
			}
			return nil, fmt.Errorf("local mode is unavailable on this machine")
		}
		ctx, cancel := context.WithTimeout(a.ctx, 15*time.Second)
		defer cancel()
		if err := a.local.Preflight(ctx); err != nil {
			return nil, fmt.Errorf("docker not reachable locally: %w", err)
		}
		a.localReady.Store(true)
		return &ConnectStatus{Connected: true}, nil
	}
	v, ok := a.store.Get(id)
	if !ok {
		return nil, fmt.Errorf("vps %s not found", id)
	}
	err := a.pool.Connect(sshpkg.ConnectOptions{
		ID:              v.ID,
		Host:            v.Host,
		Port:            v.Port,
		User:            v.User,
		AuthType:        v.AuthType,
		KeyPath:         v.KeyPath,
		Password:        v.Password,
		TrustNewHostKey: trustNew,
	})
	if err != nil {
		var unknown *sshpkg.UnknownHostKeyError
		var changed *sshpkg.ChangedHostKeyError
		switch {
		case errors.As(err, &unknown):
			return &ConnectStatus{
				NeedsTrust:  true,
				Fingerprint: unknown.Fingerprint,
				KeyType:     unknown.KeyType,
				Host:        unknown.Host,
				Message:     "This server's SSH host key isn't trusted yet.",
			}, nil
		case errors.As(err, &changed):
			return &ConnectStatus{
				KeyChanged:  true,
				Fingerprint: changed.Fingerprint,
				KeyType:     changed.KeyType,
				Host:        changed.Host,
				Message:     "This server's SSH host key has CHANGED — possible man-in-the-middle.",
			}, nil
		}
		return nil, err
	}
	return &ConnectStatus{Connected: true}, nil
}

// connectErr connects without trusting new host keys and collapses the
// needs-trust / key-changed statuses into errors, for internal callers (deploy,
// cleanup, backup) that can't prompt the user interactively.
func (a *App) connectErr(id string) error {
	st, err := a.connect(id, false)
	if err != nil {
		return err
	}
	if st != nil && !st.Connected {
		return errors.New(st.Message + " Connect to this server from the sidebar first to review and trust its key.")
	}
	return nil
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
func (a *App) InspectMigration(id, sourcePath string, composeFiles []string, projectName string, useSudo bool) (*migration.Inventory, error) {
	// 120s: inspect probes each service image's registry availability (docker
	// manifest inspect) to decide which must be copied, which adds round-trips.
	ctx, cancel := context.WithTimeout(a.ctx, 120*time.Second)
	defer cancel()
	return migration.Inspect(ctx, a.execFn(id, useSudo), sourcePath, composeFiles, projectName)
}

// DiscoverComposeContext recovers the compose project name + file a running
// stack actually uses (from its container labels), so the migration can drive
// `docker compose` correctly when the file has a non-standard name or the
// project name differs from the directory basename.
func (a *App) DiscoverComposeContext(id, dir string, useSudo bool) migration.ComposeContext {
	ctx, cancel := context.WithTimeout(a.ctx, 20*time.Second)
	defer cancel()
	return migration.DiscoverComposeContext(ctx, a.execFn(id, useSudo), dir)
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

// DiscoverStacks scans the immediate subdirectories of a path for compose
// stacks. The UI calls this when the path itself has no compose file (a
// multi-stack parent folder) so the user can pick which sub-stacks to migrate.
func (a *App) DiscoverStacks(id, dir string, useSudo bool) ([]migration.SubStack, error) {
	ctx, cancel := context.WithTimeout(a.ctx, 30*time.Second)
	defer cancel()
	return migration.DiscoverStacks(ctx, a.execFn(id, useSudo), dir)
}

// RunMultiMigration migrates several sub-stacks (subdirectories of sourceRoot)
// in one run, each to <targetRoot>/<subdir>. It inspects and migrates them one
// at a time, streaming through the same migration:log/done events, with a header
// per stack and a summary of any failures at the end. A failing stack doesn't
// stop the others.
func (a *App) RunMultiMigration(srcID, dstID, sourceRoot, targetRoot string, subdirs []string, useSudo bool) error {
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
	if len(subdirs) == 0 {
		return fmt.Errorf("no sub-stacks selected")
	}
	srcExec := a.execFn(srcID, useSudo)
	dstExec := a.execFn(dstID, useSudo)
	srcRoot := strings.TrimRight(sourceRoot, "/")
	tgtRoot := strings.TrimRight(targetRoot, "/")
	go func() {
		log := func(line string) { wailsruntime.EventsEmit(a.ctx, "migration:log", line) }
		var failed []string
		for i, sub := range subdirs {
			log(fmt.Sprintf("\n══════════ Stack %d/%d: %s ══════════", i+1, len(subdirs), sub))
			srcPath := srcRoot + "/" + sub
			tgtPath := tgtRoot + "/" + sub
			ictx, icancel := context.WithTimeout(a.ctx, 120*time.Second)
			cc := migration.DiscoverComposeContext(ictx, srcExec, srcPath)
			inv, err := migration.Inspect(ictx, srcExec, srcPath, cc.ComposeFiles, cc.Project)
			icancel()
			if err != nil {
				log("✗ inspect failed: " + err.Error())
				failed = append(failed, sub)
				continue
			}
			vols := make([]string, 0, len(inv.Volumes))
			for _, v := range inv.Volumes {
				vols = append(vols, v.Name)
			}
			opts := migration.RunOptions{
				SourcePath:       srcPath,
				TargetPath:       tgtPath,
				ComposeFiles:     cc.ComposeFiles,
				ProjectName:      cc.Project,
				Volumes:          vols,
				EnvFiles:         inv.EnvFiles,
				ExternalNetworks: inv.ExternalNetworks,
				BuildImages:      inv.BuildImages,
			}
			if err := migration.Run(a.ctx, src, dst, srcExec, dstExec, opts, log); err != nil {
				log("✗ " + sub + " failed: " + err.Error())
				failed = append(failed, sub)
				continue
			}
			log("✓ " + sub + " migrated")
		}
		msg := ""
		if len(failed) > 0 {
			msg = fmt.Sprintf("%d of %d stack(s) failed: %s", len(failed), len(subdirs), strings.Join(failed, ", "))
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
// The unnamed return type is assignable to deploy.StreamFn, docker.StreamFn and
// backup.StreamFn alike, so the same helper feeds all three packages.
func (a *App) streamFn(id string, useSudo bool) func(context.Context, string, func(string)) (int, error) {
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
		if err := a.connectErr(LocalID); err != nil {
			return err
		}
	} else {
		if _, ok := a.store.Get(d.VPSID); !ok {
			return fmt.Errorf("the VPS for this deployment no longer exists")
		}
		if !a.pool.IsConnected(d.VPSID) {
			if err := a.connectErr(d.VPSID); err != nil {
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

// ensureConnected dials the VPS if it isn't already connected, so cleanup and
// backup actions work straight after selecting a server.
func (a *App) ensureConnected(id string) error {
	if isLocal(id) {
		return a.connectErr(LocalID)
	}
	if _, ok := a.store.Get(id); !ok {
		return fmt.Errorf("vps %s not found", id)
	}
	if !a.pool.IsConnected(id) {
		return a.connectErr(id)
	}
	return nil
}

// ───────── Cleanup / teardown ─────────
//
// Removes a docker-compose stack from a VPS: `docker compose down` (always),
// optionally its named volumes and images, and optionally the stack directory.
// External volumes/networks are left untouched by compose down. Progress streams
// via "cleanup:log"/"cleanup:done", mirroring migration.

// InspectTeardown previews what a stack contains (services, named volumes,
// external networks) so the confirm screen can show exactly what will be removed.
func (a *App) InspectTeardown(id, sourcePath string, composeFiles []string, projectName string, useSudo bool) (*migration.Inventory, error) {
	ctx, cancel := context.WithTimeout(a.ctx, 90*time.Second)
	defer cancel()
	return migration.Inspect(ctx, a.execFn(id, useSudo), sourcePath, composeFiles, projectName)
}

// TeardownStack runs the teardown in a goroutine, streaming progress.
func (a *App) TeardownStack(id string, opts docker.TeardownOptions, useSudo bool) error {
	if err := a.ensureConnected(id); err != nil {
		return err
	}
	exec := a.execFn(id, useSudo)
	stream := a.streamFn(id, useSudo)
	go func() {
		log := func(line string) { wailsruntime.EventsEmit(a.ctx, "cleanup:log", line) }
		err := func() error {
			log("→ Stopping and removing stack at " + opts.Path + " …")
			if opts.RemoveVolumes {
				log("  named volumes WILL be removed")
			} else {
				log("  named volumes kept")
			}
			exit, err := docker.ComposeDown(a.ctx, stream, opts.Path, opts.Project, opts.ComposeFiles, opts.RemoveVolumes, opts.RemoveImages,
				func(chunk string) { log(strings.TrimRight(chunk, "\n")) })
			if err != nil {
				return err
			}
			if exit != 0 {
				return fmt.Errorf("docker compose down exited with code %d — see log above", exit)
			}
			if opts.RemoveDir {
				log("→ Removing stack directory " + opts.Path + " …")
				if err := docker.RemoveDir(a.ctx, exec, opts.Path); err != nil {
					return err
				}
			}
			log("✓ Cleanup complete.")
			return nil
		}()
		msg := ""
		if err != nil {
			msg = err.Error()
		}
		wailsruntime.EventsEmit(a.ctx, "cleanup:done", msg)
	}()
	return nil
}

// ───────── Backups (restic, VPS-side cron) ─────────
//
// Backup jobs live on the VPS under /etc/vps-manager/backups + /etc/cron.d, so
// they run on schedule even when this app is closed. These methods are a
// management layer over those files. Backups require a remote VPS (restic + cron
// don't apply to the local desktop), so each method rejects the local env.

func (a *App) backupExec(id string, useSudo bool, d time.Duration) (backup.ExecFn, context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(a.ctx, d)
	return a.execFn(id, useSudo), ctx, cancel
}

// secretWrite writes sensitive content (the bucket env: S3 keys + restic
// password) to a root-owned path WITHOUT ever placing it on a command line. It
// stages the bytes over the SFTP channel (so they never appear in `ps` or sudo's
// syslog), then moves the file into place and fixes ownership/mode via the
// (sudo'd) exec.
func (a *App) secretWrite(id string, useSudo bool) backup.SecretWriteFn {
	return func(ctx context.Context, p, content, mode string) error {
		conn, err := a.pool.Get(id)
		if err != nil {
			return err
		}
		tmp := "/tmp/vpsm-" + backup.NewID() + ".tmp"
		if err := sftp.WriteFile(conn.Client, tmp, content); err != nil {
			return fmt.Errorf("stage secret: %w", err)
		}
		_ = sftp.Chmod(conn.Client, tmp, 0o600)
		cleanup := func() { _, _ = a.execFn(id, false)(ctx, "rm -f "+shellQuote(tmp)) }
		cmd := fmt.Sprintf("mkdir -p %s && mv %s %s && chown root:root %s && chmod %s %s",
			shellQuote(path.Dir(p)), shellQuote(tmp), shellQuote(p), shellQuote(p), mode, shellQuote(p))
		res, err := a.execFn(id, useSudo)(ctx, cmd)
		if err != nil {
			cleanup()
			return err
		}
		if res.ExitCode != 0 {
			cleanup()
			msg := strings.TrimSpace(res.Stderr)
			if msg == "" {
				msg = fmt.Sprintf("exit %d", res.ExitCode)
			}
			return fmt.Errorf("install secret file: %s", msg)
		}
		return nil
	}
}

// ListBackupJobs reads the backup jobs configured on the VPS.
func (a *App) ListBackupJobs(id string, useSudo bool) ([]backup.Job, error) {
	if isLocal(id) {
		return nil, nil
	}
	if err := a.ensureConnected(id); err != nil {
		return nil, err
	}
	exec, ctx, cancel := a.backupExec(id, useSudo, 30*time.Second)
	defer cancel()
	return backup.ListJobs(ctx, exec)
}

// SaveBackupJob installs restic if needed, writes the job's files + cron entry,
// and ensures the restic repository exists. On edit, blank secrets are kept.
func (a *App) SaveBackupJob(id string, job backup.Job, secrets backup.Secrets, useSudo bool) (backup.Job, error) {
	if isLocal(id) {
		return job, fmt.Errorf("backups run on a remote VPS, not the local environment")
	}
	if err := a.ensureConnected(id); err != nil {
		return job, err
	}
	exec, ctx, cancel := a.backupExec(id, useSudo, 8*time.Minute) // a cold restic install downloads
	defer cancel()
	if _, err := backup.EnsureRestic(ctx, exec); err != nil {
		return job, err
	}
	saved, err := backup.SaveJob(ctx, exec, a.secretWrite(id, useSudo), job, secrets)
	if err != nil {
		return job, err
	}
	if err := backup.InitRepo(ctx, exec, saved.ID); err != nil {
		return saved, fmt.Errorf("repository init: %w", err)
	}
	return saved, nil
}

// DeleteBackupJob removes a job's files and cron entry (the bucket data is left
// intact).
func (a *App) DeleteBackupJob(id, jobID string, useSudo bool) error {
	if err := a.ensureConnected(id); err != nil {
		return err
	}
	exec, ctx, cancel := a.backupExec(id, useSudo, 30*time.Second)
	defer cancel()
	return backup.DeleteJob(ctx, exec, jobID)
}

// RunBackupNow triggers a job immediately, streaming output via
// "backup:log:<jobID>" / "backup:done:<jobID>", and records the outcome.
func (a *App) RunBackupNow(id, jobID string, useSudo bool) error {
	if isLocal(id) {
		return fmt.Errorf("backups run on a remote VPS, not the local environment")
	}
	if err := a.ensureConnected(id); err != nil {
		return err
	}
	stream := a.streamFn(id, useSudo)
	go func() {
		log := func(line string) { wailsruntime.EventsEmit(a.ctx, "backup:log:"+jobID, line) }
		exit, err := backup.RunNow(a.ctx, stream, jobID, func(chunk string) { log(strings.TrimRight(chunk, "\n")) })
		status, msg := "success", ""
		if err != nil {
			status, msg = "failed", err.Error()
		} else if exit != 0 {
			status, msg = "failed", fmt.Sprintf("backup script exited with code %d — see log above", exit)
		}
		statusCtx, cancel := context.WithTimeout(a.ctx, 30*time.Second)
		_ = backup.SetJobStatus(statusCtx, a.execFn(id, useSudo), jobID, time.Now().Format(time.RFC3339), status)
		cancel()
		wailsruntime.EventsEmit(a.ctx, "backup:done:"+jobID, msg)
	}()
	return nil
}

// ListBackupSnapshots lists the restic snapshots for a job (newest first).
func (a *App) ListBackupSnapshots(id, jobID string, useSudo bool) ([]backup.Snapshot, error) {
	if err := a.ensureConnected(id); err != nil {
		return nil, err
	}
	exec, ctx, cancel := a.backupExec(id, useSudo, 90*time.Second)
	defer cancel()
	return backup.ListSnapshots(ctx, exec, jobID)
}

// ForgetBackupSnapshot deletes one snapshot from a job's repository and prunes.
func (a *App) ForgetBackupSnapshot(id, jobID, snapshotID string, useSudo bool) error {
	if err := a.ensureConnected(id); err != nil {
		return err
	}
	exec, ctx, cancel := a.backupExec(id, useSudo, 5*time.Minute)
	defer cancel()
	return backup.ForgetSnapshot(ctx, exec, jobID, snapshotID)
}

// TestBackupTarget validates bucket credentials by initializing/opening the
// restic repo with a throwaway env, after ensuring restic is installed.
func (a *App) TestBackupTarget(id string, bucket backup.BucketRef, secrets backup.Secrets, useSudo bool) error {
	if isLocal(id) {
		return fmt.Errorf("backups run on a remote VPS, not the local environment")
	}
	if err := a.ensureConnected(id); err != nil {
		return err
	}
	exec, ctx, cancel := a.backupExec(id, useSudo, 8*time.Minute)
	defer cancel()
	if _, err := backup.EnsureRestic(ctx, exec); err != nil {
		return err
	}
	return backup.TestTarget(ctx, exec, a.secretWrite(id, useSudo), bucket, secrets)
}

// RestoreBackup restores a snapshot to targetVpsID (the job's own VPS or a
// different one), streaming progress via "restore:log"/"restore:done". When
// targetVpsID is the job's origin VPS, leave secrets blank to reuse the job's
// stored credentials; for any other VPS, secrets (S3 keys + restic password)
// must be supplied so the target can read the bucket. Restoring overwrites
// existing volume data — the UI gates this behind a confirmation.
func (a *App) RestoreBackup(targetVpsID string, job backup.Job, secrets backup.Secrets, opts backup.RestoreOptions, useSudo bool) error {
	if isLocal(targetVpsID) {
		return fmt.Errorf("restore runs on a remote VPS, not the local environment")
	}
	if opts.SnapshotID == "" {
		return fmt.Errorf("no snapshot selected")
	}
	if err := a.ensureConnected(targetVpsID); err != nil {
		return err
	}
	exec := a.execFn(targetVpsID, useSudo)
	stream := a.streamFn(targetVpsID, useSudo)
	hasSecrets := secrets.AccessKey != "" || secrets.SecretKey != "" || secrets.ResticPassword != ""
	go func() {
		log := func(line string) { wailsruntime.EventsEmit(a.ctx, "restore:log", line) }
		err := func() error {
			ctx, cancel := context.WithTimeout(a.ctx, 8*time.Minute)
			defer cancel()
			if _, err := backup.EnsureRestic(ctx, exec); err != nil {
				return err
			}
			// Same VPS reuses the job's stored env; another VPS needs a temp one.
			envPath := backup.JobEnvPath(job.ID)
			cleanupEnv := false
			if hasSecrets {
				p, err := backup.WriteRestoreEnv(ctx, a.secretWrite(targetVpsID, useSudo), job.Bucket, secrets)
				if err != nil {
					return err
				}
				envPath = p
				cleanupEnv = true
				defer func() {
					// Use a fresh context so app shutdown / cancellation can't skip
					// removing the secret env (the script's trap also rm's it).
					rmCtx, rmCancel := context.WithTimeout(context.Background(), 15*time.Second)
					_, _ = exec(rmCtx, "rm -f "+shellQuote(p))
					rmCancel()
				}()
			} else {
				// Reusing the job's stored credentials — confirm they exist here.
				if res, err := exec(ctx, "test -f "+shellQuote(envPath)); err != nil {
					return err
				} else if res.ExitCode != 0 {
					return fmt.Errorf("this VPS has no stored credentials for backup job %s — enter the bucket access key, secret and restic password to restore here", job.ID)
				}
			}
			script := backup.RenderRestoreScript(job, opts, envPath, cleanupEnv)
			exit, serr := stream(a.ctx, script, func(chunk string) { log(strings.TrimRight(chunk, "\n")) })
			if serr != nil {
				return serr
			}
			if exit != 0 {
				return fmt.Errorf("restore exited with code %d — see log above", exit)
			}
			return nil
		}()
		msg := ""
		if err != nil {
			msg = err.Error()
		}
		wailsruntime.EventsEmit(a.ctx, "restore:done", msg)
	}()
	return nil
}

// ───────── System maintenance (disk / docker usage / prune) ─────────

// SystemUsage gathers root-fs + Docker disk usage for the maintenance panel.
func (a *App) SystemUsage(id string, useSudo bool) (*maintenance.Usage, error) {
	if err := a.ensureConnected(id); err != nil {
		return nil, err
	}
	return maintenance.GetUsage(a.ctx, maintenance.ExecFn(a.execFn(id, useSudo)))
}

// PruneDocker streams `docker system prune` (with the opt-in destructive flags
// in opts) to the "maintenance-prune" Wails event. -a / --volumes are only
// passed when the caller explicitly sets them in opts.
func (a *App) PruneDocker(id string, opts maintenance.PruneOptions, useSudo bool) error {
	if err := a.ensureConnected(id); err != nil {
		return err
	}
	stream := maintenance.StreamFn(a.streamFn(id, useSudo))
	exit, err := maintenance.Prune(a.ctx, stream, opts, func(chunk string) {
		wailsruntime.EventsEmit(a.ctx, "maintenance-prune", strings.TrimRight(chunk, "\n"))
	})
	if err != nil {
		return err
	}
	if exit != 0 {
		return fmt.Errorf("docker system prune exited with code %d", exit)
	}
	return nil
}

// SystemLargestImages lists the biggest Docker images (cheap "what's using space").
func (a *App) SystemLargestImages(id string, limit int, useSudo bool) ([]maintenance.Image, error) {
	if err := a.ensureConnected(id); err != nil {
		return nil, err
	}
	return maintenance.ListLargestImages(a.ctx, maintenance.ExecFn(a.execFn(id, useSudo)), limit)
}

// ───────── Database provisioning ─────────

// ProvisionDatabaseEngines returns the static engine catalog for the form.
func (a *App) ProvisionDatabaseEngines() []provision.Engine {
	return provision.Engines()
}

// ProvisionDatabase stands up a managed database on the given host as a
// docker-compose stack, streaming progress to the "provision-db" Wails event.
// The returned Result carries the generated password — show it to the user ONCE.
func (a *App) ProvisionDatabase(id string, spec provision.Spec, useSudo bool) (*provision.Result, error) {
	if err := a.ensureConnected(id); err != nil {
		return nil, err
	}
	exec := provision.ExecFn(a.execFn(id, useSudo))
	stream := provision.StreamFn(a.streamFn(id, useSudo))
	secretWrite := provision.SecretWriteFn(a.secretWrite(id, useSudo))
	return provision.Provision(a.ctx, exec, stream, secretWrite, spec, func(line string) {
		wailsruntime.EventsEmit(a.ctx, "provision-db", line)
	})
}

// ───────── Domains (Caddy label proxy + Cloudflare DNS) ─────────

// DetectCaddyProxy reports whether a caddy-docker-proxy is running on the host.
func (a *App) DetectCaddyProxy(id string, useSudo bool) (*caddy.ProxyInfo, error) {
	if err := a.ensureConnected(id); err != nil {
		return nil, err
	}
	return caddy.DetectProxy(a.ctx, caddy.ExecFn(a.execFn(id, useSudo)))
}

// ExposeServiceDomain writes a caddy-docker-proxy label override next to the
// stack and re-ups it, streaming progress to the "caddy-expose" Wails event.
func (a *App) ExposeServiceDomain(id string, spec caddy.ExposeSpec, useSudo bool) error {
	if err := a.ensureConnected(id); err != nil {
		return err
	}
	exec := caddy.ExecFn(a.execFn(id, useSudo))
	stream := caddy.StreamFn(a.streamFn(id, useSudo))
	return caddy.ApplyOverride(a.ctx, exec, stream, spec, func(line string) {
		wailsruntime.EventsEmit(a.ctx, "caddy-expose", line)
	})
}

// CloudflareUpsert creates/updates an A record from the DESKTOP. The token is
// never sent to the VPS. proxied toggles Cloudflare's orange-cloud proxying.
func (a *App) CloudflareUpsert(apiToken, zone, name, ipv4 string, proxied bool) error {
	return caddy.CloudflareUpsertA(a.ctx, apiToken, zone, name, ipv4, proxied)
}
