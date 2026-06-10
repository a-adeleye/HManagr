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
	"time"

	"vps-manager/internal/config"
	"vps-manager/internal/docker"
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
}

func NewApp() *App {
	return &App{
		pool:          sshpkg.NewPool(),
		shells:        make(map[string]*sshpkg.Session),
		sudoPasswords: make(map[string]string),
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
	a.sudoMu.Lock()
	for k := range a.sudoPasswords {
		delete(a.sudoPasswords, k)
	}
	a.sudoMu.Unlock()
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
	a.ClearSudoPassword(id)
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
	a.ClearSudoPassword(id)
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

// MakeDir creates a directory on the remote host (mkdir -p semantics). If
// useSudo is true, the call shells out to `sudo mkdir -p` instead of going
// through SFTP, since SFTP has no way to elevate.
func (a *App) MakeDir(id, path string, useSudo bool) error {
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

// ProbeSudo reports whether sudo on this VPS will work without a password
// prompt right now. Used by the migration wizard so it can avoid asking for a
// password when one isn't actually needed (NOPASSWD or already-cached).
//
// Returns one of:
//
//	"ok"       — sudo runs non-interactively (NOPASSWD or a working cached pw)
//	"password" — a password is required and we don't have a valid one cached
//	"denied"   — this user isn't in sudoers at all
func (a *App) ProbeSudo(id string) (string, error) {
	conn, err := a.pool.Get(id)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(a.ctx, 5*time.Second)
	defer cancel()

	a.sudoMu.Lock()
	pwd, hasPwd := a.sudoPasswords[id]
	a.sudoMu.Unlock()

	if hasPwd {
		res, err := conn.ExecWithStdin(ctx, "sudo -S -p '' true", pwd+"\n")
		if err == nil && res.ExitCode == 0 {
			return "ok", nil
		}
		// Cached password no longer works — fall through to a clean probe so
		// "denied" can still be distinguished from "password".
		a.sudoMu.Lock()
		delete(a.sudoPasswords, id)
		a.sudoMu.Unlock()
	}

	res, err := conn.Exec(ctx, "sudo -n true")
	if err != nil {
		return "", err
	}
	if res.ExitCode == 0 {
		return "ok", nil
	}
	out := strings.ToLower(res.Stdout + " " + res.Stderr)
	if strings.Contains(out, "not in the sudoers") || strings.Contains(out, "not allowed to execute") {
		return "denied", nil
	}
	return "password", nil
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
	a.sudoMu.Lock()
	pwd, hasPwd := a.sudoPasswords[id]
	a.sudoMu.Unlock()
	if hasPwd {
		return conn.ExecWithStdin(ctx, "sudo -S -p '' "+cmd, pwd+"\n")
	}
	return conn.Exec(ctx, "sudo -n "+cmd)
}

// execFn returns a migration.ExecFn that runs commands on the given VPS,
// optionally wrapped with sudo. The returned function is safe to call across
// goroutine boundaries — it re-resolves the connection from the pool each
// call, so it survives transient reconnects.
func (a *App) execFn(id string, useSudo bool) migration.ExecFn {
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
