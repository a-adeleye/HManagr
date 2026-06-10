package migration

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"vps-manager/internal/sftp"
	sshpkg "vps-manager/internal/ssh"
)

// ExecFn is what migration calls to run a remote command. Letting the App
// layer supply this (instead of taking *Connection directly) is how we get
// optional sudo wrapping without making this package aware of sudo at all.
type ExecFn func(ctx context.Context, cmd string) (*sshpkg.ExecResult, error)

// RunOptions is the App-layer-friendly version of a migration request — it's
// derived from Plan + Inventory + the user's selections in the wizard.
type RunOptions struct {
	SourcePath  string
	TargetPath  string
	ComposeFile string   // filename inside SourcePath, e.g. "docker-compose.yml"
	Volumes     []string // full docker volume names to archive+restore
	EnvFiles    []string // resolved absolute paths on source
}

// Run executes the migration end-to-end with the following ordering:
//
//  1. Make scratch dirs on source, target, laptop.
//  2. `docker compose stop` on source (downtime begins).
//  3. Tar each named volume on source into the scratch dir.
//  4. `docker compose up -d` on source (downtime ends — source is back up).
//     We use up -d rather than start because `start` requires the containers
//     to still exist; up -d works whether they do or not.
//  5. Transfer each tarball: source → laptop → target.
//  6. Stage compose file + env files into the target's scratch dir via SFTP,
//     then mv them into the target compose dir via the (possibly sudo'd) exec.
//     Direct SFTP into root-owned target dirs would fail.
//  7. Restore each volume on target.
//  8. `docker compose up -d` on target.
//
// Source is never destroyed by Run — bringing source fully down is the user's
// call after they've verified the target.
//
// Each user-visible step is sent through the log callback so the UI can stream
// progress in real time.
func Run(ctx context.Context, src, dst *sshpkg.Connection, srcExec, dstExec ExecFn, opts RunOptions, log func(string)) error {
	if log == nil {
		log = func(string) {}
	}

	log("→ Creating scratch workspaces…")
	srcTmp, err := mktempDir(ctx, srcExec)
	if err != nil {
		return fmt.Errorf("source workspace: %w", err)
	}
	defer rmrf(ctx, srcExec, srcTmp)

	dstTmp, err := mktempDir(ctx, dstExec)
	if err != nil {
		return fmt.Errorf("target workspace: %w", err)
	}
	defer rmrf(ctx, dstExec, dstTmp)

	localTmp, err := os.MkdirTemp("", "vps-migration-*")
	if err != nil {
		return fmt.Errorf("laptop workspace: %w", err)
	}
	defer os.RemoveAll(localTmp)
	log(fmt.Sprintf("  source:%s  target:%s  laptop:%s", srcTmp, dstTmp, localTmp))

	// Scratch dirs created by sudo are root-owned. Open them so SFTP (which
	// runs as the SSH user, never elevated) can write inside.
	if err := chmodWorld(ctx, srcExec, srcTmp); err != nil {
		log("  ⚠ couldn't relax permissions on source scratch dir: " + err.Error())
	}
	if err := chmodWorld(ctx, dstExec, dstTmp); err != nil {
		log("  ⚠ couldn't relax permissions on target scratch dir: " + err.Error())
	}

	log("→ Stopping source stack (downtime begins)…")
	if err := composeStop(ctx, srcExec, opts.SourcePath); err != nil {
		return fmt.Errorf("compose stop on source: %w", err)
	}

	log(fmt.Sprintf("→ Archiving %d volume(s) on source…", len(opts.Volumes)))
	for _, vol := range opts.Volumes {
		log("  • " + vol)
		if err := archiveVolume(ctx, srcExec, vol, srcTmp); err != nil {
			// Try to bring source back up before returning so the user isn't
			// stuck with a stopped stack.
			_ = composeUp(ctx, srcExec, opts.SourcePath)
			return fmt.Errorf("archive %s: %w", vol, err)
		}
	}

	log("→ Restarting source stack (downtime ends)…")
	if err := composeUp(ctx, srcExec, opts.SourcePath); err != nil {
		log("  ⚠ source restart failed: " + err.Error())
	}

	log(fmt.Sprintf("→ Transferring %d archive(s) source → laptop → target…", len(opts.Volumes)))
	for _, vol := range opts.Volumes {
		log("  • " + vol)
		name := vol + ".tar.gz"
		if err := relayFile(src, dst, srcTmp+"/"+name, dstTmp+"/"+name, localTmp+"/"+name); err != nil {
			return fmt.Errorf("transfer %s: %w", vol, err)
		}
	}

	log("→ Creating target compose dir and copying compose file + env files…")
	if err := mkdirp(ctx, dstExec, opts.TargetPath); err != nil {
		return fmt.Errorf("create target dir: %w", err)
	}
	// Stage compose file in dstTmp, then mv (which respects sudo) into place.
	if err := relayThenMove(ctx, src, dst, dstExec,
		path.Join(opts.SourcePath, opts.ComposeFile),
		dstTmp+"/"+opts.ComposeFile,
		path.Join(opts.TargetPath, opts.ComposeFile),
		filepath.Join(localTmp, opts.ComposeFile),
	); err != nil {
		return fmt.Errorf("copy compose file: %w", err)
	}
	for _, ef := range opts.EnvFiles {
		rel, ok := relUnder(opts.SourcePath, ef)
		if !ok {
			log("  ⚠ skipping env file outside compose dir: " + ef)
			continue
		}
		dstAbs := path.Join(opts.TargetPath, rel)
		if err := mkdirp(ctx, dstExec, path.Dir(dstAbs)); err != nil {
			return fmt.Errorf("env file dir: %w", err)
		}
		stage := dstTmp + "/env-" + path.Base(ef)
		if err := relayThenMove(ctx, src, dst, dstExec, ef, stage, dstAbs,
			filepath.Join(localTmp, "env-"+path.Base(ef)),
		); err != nil {
			return fmt.Errorf("copy env file %s: %w", ef, err)
		}
	}

	log(fmt.Sprintf("→ Restoring %d volume(s) on target…", len(opts.Volumes)))
	for _, vol := range opts.Volumes {
		log("  • " + vol)
		if err := restoreVolume(ctx, dstExec, vol, dstTmp); err != nil {
			return fmt.Errorf("restore %s: %w", vol, err)
		}
	}

	log("→ Starting target stack…")
	if err := composeUp(ctx, dstExec, opts.TargetPath); err != nil {
		return fmt.Errorf("compose up on target: %w", err)
	}

	log("✓ Migration complete. Verify the target, then stop source manually when ready.")
	return nil
}

// ──────────────── helpers ────────────────

func mktempDir(ctx context.Context, exec ExecFn) (string, error) {
	res, err := exec(ctx, "mktemp -d /tmp/vps-mig.XXXXXX")
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("mktemp: %s", strings.TrimSpace(res.Stderr))
	}
	return strings.TrimSpace(res.Stdout), nil
}

func rmrf(ctx context.Context, exec ExecFn, p string) {
	if p == "" || !strings.HasPrefix(p, "/tmp/vps-mig.") {
		return // refuse to rm anything outside our own scratch dirs
	}
	_, _ = exec(ctx, "rm -rf "+shellQuote(p))
}

func mkdirp(ctx context.Context, exec ExecFn, dir string) error {
	res, err := exec(ctx, "mkdir -p "+shellQuote(dir))
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		msg := strings.TrimSpace(res.Stderr)
		if msg == "" {
			msg = fmt.Sprintf("exit %d", res.ExitCode)
		}
		return fmt.Errorf("mkdir -p %s: %s", dir, msg)
	}
	return nil
}

// chmodWorld lets the SSH user write into a scratch dir that was created with
// sudo (and is therefore root-owned). 1777 mirrors /tmp's sticky-world-writable
// permissions, so files SFTP'd in survive and nobody else can mess with them.
func chmodWorld(ctx context.Context, exec ExecFn, p string) error {
	res, err := exec(ctx, "chmod 1777 "+shellQuote(p))
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("%s", strings.TrimSpace(res.Stderr))
	}
	return nil
}

func composeStop(ctx context.Context, exec ExecFn, dir string) error {
	return composeCmd(ctx, exec, dir, "stop")
}
func composeUp(ctx context.Context, exec ExecFn, dir string) error {
	return composeCmd(ctx, exec, dir, "up -d")
}

func composeCmd(ctx context.Context, exec ExecFn, dir, sub string) error {
	cmd := fmt.Sprintf("cd %s && docker compose %s", shellQuote(dir), sub)
	res, err := exec(ctx, cmd)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		msg := strings.TrimSpace(res.Stderr)
		if msg == "" {
			msg = strings.TrimSpace(res.Stdout)
		}
		return fmt.Errorf("docker compose %s: %s", sub, msg)
	}
	return nil
}

// archiveVolume tars the contents of a docker volume into <tmp>/<vol>.tar.gz
// on the same host. Running tar inside alpine avoids needing host root on
// /var/lib/docker/volumes/*.
func archiveVolume(ctx context.Context, exec ExecFn, vol, tmp string) error {
	cmd := fmt.Sprintf(
		"docker run --rm -v %s:/src:ro -v %s:/dest alpine sh -c 'tar czf /dest/%s.tar.gz -C /src .'",
		shellQuote(vol), shellQuote(tmp), shellEsc(vol),
	)
	res, err := exec(ctx, cmd)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("%s", strings.TrimSpace(res.Stderr))
	}
	return nil
}

// restoreVolume (re)creates the docker volume on target and untars the archive
// into it. `docker volume create` is idempotent for existing volumes.
func restoreVolume(ctx context.Context, exec ExecFn, vol, tmp string) error {
	createCmd := "docker volume create " + shellQuote(vol)
	if res, err := exec(ctx, createCmd); err != nil {
		return err
	} else if res.ExitCode != 0 {
		return fmt.Errorf("volume create: %s", strings.TrimSpace(res.Stderr))
	}
	cmd := fmt.Sprintf(
		"docker run --rm -v %s:/dest -v %s:/src alpine sh -c 'tar xzf /src/%s.tar.gz -C /dest'",
		shellQuote(vol), shellQuote(tmp), shellEsc(vol),
	)
	res, err := exec(ctx, cmd)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("%s", strings.TrimSpace(res.Stderr))
	}
	return nil
}

// relayFile pulls srcRemote to localPath then pushes it to dstRemote, both via
// SFTP (no elevation possible — both ends must be user-writable). For paths
// that need root on the destination, use relayThenMove.
func relayFile(src, dst *sshpkg.Connection, srcRemote, dstRemote, localPath string) error {
	if err := os.MkdirAll(filepath.Dir(localPath), 0o755); err != nil {
		return err
	}
	if err := sftp.Download(src.Client, srcRemote, localPath); err != nil {
		return fmt.Errorf("download: %w", err)
	}
	if err := sftp.Upload(dst.Client, localPath, dstRemote); err != nil {
		return fmt.Errorf("upload: %w", err)
	}
	return nil
}

// relayThenMove SFTPs srcRemote → stagedRemote (under our /tmp scratch dir,
// so always writable) and then issues a `mv` via the destination exec — which
// is sudo'd when the user enabled that, so root-owned final destinations work.
func relayThenMove(ctx context.Context, src, dst *sshpkg.Connection, dstExec ExecFn,
	srcRemote, stagedRemote, finalRemote, localPath string) error {
	if err := relayFile(src, dst, srcRemote, stagedRemote, localPath); err != nil {
		return err
	}
	res, err := dstExec(ctx, "mv "+shellQuote(stagedRemote)+" "+shellQuote(finalRemote))
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("mv: %s", strings.TrimSpace(res.Stderr))
	}
	return nil
}

// relUnder reports whether p is under base and, if so, the path relative to it.
// Both paths are remote POSIX paths.
func relUnder(base, p string) (string, bool) {
	base = strings.TrimRight(base, "/")
	if !strings.HasPrefix(p, base+"/") {
		return "", false
	}
	return strings.TrimPrefix(p, base+"/"), true
}

// shellEsc is for embedding a name inside a *single-quoted* shell argument
// that's itself constructed inline (the tar invocations above). It only needs
// to defend against an embedded `'` inside the volume name.
func shellEsc(s string) string {
	return strings.ReplaceAll(s, "'", "'\\''")
}
