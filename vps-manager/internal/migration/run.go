package migration

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

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
	// ExternalNetworks are docker network names the compose file declares as
	// external; the migration recreates any that are missing on the target.
	ExternalNetworks []string
	// BuildImages are image tags of build services to ship prebuilt (save/load)
	// instead of rebuilding on the target.
	BuildImages []string
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
//  6. Archive the WHOLE source compose directory (compose file, .env, build
//     contexts like ./backend, configs — everything), relay it through the
//     laptop, and extract it into the target compose dir via the (possibly
//     sudo'd) exec so root-owned destinations and files (e.g. a 600 .env) work.
//  7. Restore each volume on target.
//  8. Ship each build service's prebuilt image (docker save → relay → load) so
//     the target needn't reproduce the build context, ensure external networks
//     exist, then `docker compose up -d`.
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
		if err := relayFile(src, dst, srcTmp+"/"+name, dstTmp+"/"+name, localTmp+"/"+name, log, "volume "+vol); err != nil {
			return fmt.Errorf("transfer %s: %w", vol, err)
		}
	}

	log("→ Copying the whole stack directory (compose file, .env, build contexts, …)…")
	if err := mkdirp(ctx, dstExec, opts.TargetPath); err != nil {
		return fmt.Errorf("create target dir: %w", err)
	}
	// Copy the ENTIRE source compose directory rather than cherry-picking the
	// compose file + env files: a stack dir routinely also holds build contexts
	// (./backend), Dockerfiles, configs and a .env that `docker compose config`
	// never reports. Archiving on source (via the possibly-sudo'd exec) reads
	// root-owned files like a 600 .env; extracting on target writes into
	// root-owned dirs like /opt/<stack>.
	const dirArchive = "compose-dir.tar.gz"
	if err := archiveTree(ctx, srcExec, opts.SourcePath, srcTmp, dirArchive); err != nil {
		return fmt.Errorf("archive source dir: %w", err)
	}
	if err := relayFile(src, dst, srcTmp+"/"+dirArchive, dstTmp+"/"+dirArchive, localTmp+"/"+dirArchive, log, "stack directory"); err != nil {
		return fmt.Errorf("transfer stack dir: %w", err)
	}
	if err := extractTree(ctx, dstExec, dstTmp, dirArchive, opts.TargetPath); err != nil {
		return fmt.Errorf("extract stack dir: %w", err)
	}
	// Env files docker resolved to paths *outside* the compose dir live elsewhere
	// on the source and aren't part of the directory we just copied.
	for _, ef := range opts.EnvFiles {
		if _, ok := relUnder(opts.SourcePath, ef); !ok {
			log("  ⚠ env file outside the stack directory not migrated: " + ef)
		}
	}

	log(fmt.Sprintf("→ Restoring %d volume(s) on target…", len(opts.Volumes)))
	for _, vol := range opts.Volumes {
		log("  • " + vol)
		if err := restoreVolume(ctx, dstExec, vol, dstTmp); err != nil {
			return fmt.Errorf("restore %s: %w", vol, err)
		}
	}

	if len(opts.BuildImages) > 0 {
		log(fmt.Sprintf("→ Shipping %d built image(s) source → laptop → target…", len(opts.BuildImages)))
		for i, image := range opts.BuildImages {
			log("  • " + image)
			if !imageExists(ctx, srcExec, image) {
				log("    ⚠ not found on source; the target will try to build it instead")
				continue
			}
			file := fmt.Sprintf("img-%d.tar.gz", i)
			tarPath := strings.TrimSuffix(srcTmp+"/"+file, ".gz")
			imgTotal := imageSize(ctx, srcExec, image)
			log("    saving image on source…")
			// docker save emits no progress, so poll the growing archive instead.
			if err := withHeartbeat(2*time.Second, log, func() string {
				return saveStatus(ctx, srcExec, tarPath, imgTotal)
			}, func() error {
				return saveImage(ctx, srcExec, image, srcTmp, file)
			}); err != nil {
				return fmt.Errorf("save image %s: %w", image, err)
			}
			if err := relayFile(src, dst, srcTmp+"/"+file, dstTmp+"/"+file, localTmp+"/"+file, log, "image "+image); err != nil {
				return fmt.Errorf("transfer image %s: %w", image, err)
			}
			log("    loading image on target…")
			// docker load gives nothing pollable on disk, so tick elapsed time.
			loadStart := time.Now()
			if err := withHeartbeat(2*time.Second, log, func() string {
				return fmt.Sprintf("loading image…  %ds elapsed", int(time.Since(loadStart).Seconds()))
			}, func() error {
				return loadImage(ctx, dstExec, dstTmp, file)
			}); err != nil {
				return fmt.Errorf("load image %s: %w", image, err)
			}
		}
	}

	if len(opts.ExternalNetworks) > 0 {
		log(fmt.Sprintf("→ Ensuring %d external network(s) on target…", len(opts.ExternalNetworks)))
		for _, net := range opts.ExternalNetworks {
			log("  • " + net)
			if err := ensureNetwork(ctx, dstExec, srcExec, net); err != nil {
				return fmt.Errorf("create external network %s: %w", net, err)
			}
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
// SFTP (no elevation possible — both ends must be user-writable). Paths that
// need root are handled by staging into our world-writable /tmp scratch dir and
// then moving/extracting through the (possibly sudo'd) exec. Byte progress for
// each leg is streamed through log as an in-place-updating line (see
// progressLogger), so large transfers don't look frozen.
func relayFile(src, dst *sshpkg.Connection, srcRemote, dstRemote, localPath string, log func(string), label string) error {
	if err := os.MkdirAll(filepath.Dir(localPath), 0o755); err != nil {
		return err
	}
	if err := sftp.DownloadProgress(src.Client, srcRemote, localPath, progressLogger(log, label+" ↓ source→laptop")); err != nil {
		return fmt.Errorf("download: %w", err)
	}
	if err := sftp.UploadProgress(dst.Client, localPath, dstRemote, progressLogger(log, label+" ↑ laptop→target")); err != nil {
		return fmt.Errorf("upload: %w", err)
	}
	return nil
}

// progressMark prefixes in-place progress lines. The frontend treats a log line
// starting with this (U+200B zero-width space, invisible) as a replacement for
// the previous progress line rather than an append — giving a terminal-style
// updating counter without flooding the log.
const progressMark = "​"

// progressLogger adapts the migration log callback into an sftp.ProgressFn that
// emits "<label>  47.0 MB / 210.0 MB (22%)" lines. Returns nil when log is nil
// so sftp skips progress tracking entirely.
func progressLogger(log func(string), label string) sftp.ProgressFn {
	if log == nil {
		return nil
	}
	return func(done, total int64) {
		line := progressMark + "      ↳ " + label + "  " + humanBytes(done)
		if total > 0 {
			line += " / " + humanBytes(total) + fmt.Sprintf("  (%d%%)", done*100/total)
		}
		log(line)
	}
}

func humanBytes(n int64) string {
	const u = 1024
	switch {
	case n < u:
		return fmt.Sprintf("%d B", n)
	case n < u*u:
		return fmt.Sprintf("%.1f KB", float64(n)/u)
	case n < u*u*u:
		return fmt.Sprintf("%.1f MB", float64(n)/u/u)
	default:
		return fmt.Sprintf("%.2f GB", float64(n)/u/u/u)
	}
}

// archiveTree tars the entire contents of dir into <tmp>/<name> on the same
// host. It runs through exec (sudo when enabled) so it can read root-owned
// files such as a 600 .env, and writes into our world-writable scratch dir so
// the unprivileged SFTP relay can pick the archive up. The `.` form archives
// dotfiles (e.g. .env) too, unlike a `*` glob.
func archiveTree(ctx context.Context, exec ExecFn, dir, tmp, name string) error {
	cmd := fmt.Sprintf("tar czf %s -C %s .", shellQuote(tmp+"/"+name), shellQuote(dir))
	res, err := exec(ctx, cmd)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("%s", strings.TrimSpace(res.Stderr))
	}
	return nil
}

// extractTree unpacks an archive previously relayed into <tmp>/<name> into dir
// on the target, through exec so sudo can write into root-owned destinations
// like /opt/<stack>.
func extractTree(ctx context.Context, exec ExecFn, tmp, name, dir string) error {
	cmd := fmt.Sprintf("tar xzf %s -C %s", shellQuote(tmp+"/"+name), shellQuote(dir))
	res, err := exec(ctx, cmd)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("%s", strings.TrimSpace(res.Stderr))
	}
	return nil
}

// imageExists reports whether a docker image tag is present on the host.
func imageExists(ctx context.Context, exec ExecFn, image string) bool {
	res, err := exec(ctx, "docker image inspect "+shellQuote(image))
	return err == nil && res != nil && res.ExitCode == 0
}

// saveImage exports a docker image to <tmp>/<name> (gzipped) on the source host.
// `docker save -o … && gzip` avoids a pipeline so the exit code reflects the
// save; the archive lands in our world-writable scratch dir for the SFTP relay.
func saveImage(ctx context.Context, exec ExecFn, image, tmp, name string) error {
	tarPath := strings.TrimSuffix(tmp+"/"+name, ".gz")
	cmd := fmt.Sprintf("docker save %s -o %s && gzip -f %s",
		shellQuote(image), shellQuote(tarPath), shellQuote(tarPath))
	res, err := exec(ctx, cmd)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("%s", strings.TrimSpace(res.Stderr))
	}
	return nil
}

// loadImage imports an image archive previously relayed into <tmp>/<name> on
// the target. `docker load` is the last stage of the pipe, so its exit code is
// what we check.
func loadImage(ctx context.Context, exec ExecFn, tmp, name string) error {
	cmd := fmt.Sprintf("gunzip -c %s | docker load", shellQuote(tmp+"/"+name))
	res, err := exec(ctx, cmd)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		msg := strings.TrimSpace(res.Stderr)
		if msg == "" {
			msg = strings.TrimSpace(res.Stdout)
		}
		return fmt.Errorf("%s", msg)
	}
	return nil
}

// withHeartbeat runs fn while a background goroutine emits status() as an
// in-place progress line every interval, so a long blocking op that produces no
// output of its own (docker save/load) still visibly ticks. The goroutine is
// fully stopped before withHeartbeat returns, so no stray line lands after the
// next discrete step.
func withHeartbeat(interval time.Duration, log func(string), status func() string, fn func() error) error {
	if log == nil || status == nil {
		return fn()
	}
	stop := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				if s := status(); s != "" {
					log(progressMark + "      ↳ " + s)
				}
			}
		}
	}()
	err := fn()
	close(stop)
	<-stopped
	return err
}

// saveStatus reports how far `docker save` has gotten by reading the size of the
// archive it's writing, shown against the image's total size when known.
func saveStatus(ctx context.Context, exec ExecFn, tarPath string, total int64) string {
	n := fileSize(ctx, exec, tarPath)
	if n <= 0 {
		return "saving image…"
	}
	s := "saving image  " + humanBytes(n)
	if total > 0 {
		pct := n * 100 / total
		if pct > 99 {
			pct = 99
		}
		s += fmt.Sprintf(" / ~%s  (%d%%)", humanBytes(total), pct)
	}
	return s
}

// fileSize returns the byte size of path, or 0 if it can't be stat'd (e.g. the
// file doesn't exist yet). Uses plain `stat` args so it works whether or not
// the exec is sudo-wrapped.
func fileSize(ctx context.Context, exec ExecFn, path string) int64 {
	res, err := exec(ctx, "stat -c %s "+shellQuote(path))
	if err != nil || res == nil || res.ExitCode != 0 {
		return 0
	}
	var n int64
	_, _ = fmt.Sscanf(strings.TrimSpace(res.Stdout), "%d", &n)
	return n
}

// imageSize returns the docker image's reported size in bytes (best-effort, 0 on
// failure) — used to estimate save progress.
func imageSize(ctx context.Context, exec ExecFn, image string) int64 {
	res, err := exec(ctx, "docker image inspect -f '{{.Size}}' "+shellQuote(image))
	if err != nil || res == nil || res.ExitCode != 0 {
		return 0
	}
	var n int64
	_, _ = fmt.Sscanf(strings.TrimSpace(res.Stdout), "%d", &n)
	return n
}

// ensureNetwork creates the named docker network on the target if it isn't
// already there. Compose refuses to start a stack whose external network is
// missing, and it never creates external networks itself. We mirror the source
// network's driver when we can read it (the rendered compose config omits the
// driver for external networks), defaulting to bridge otherwise.
func ensureNetwork(ctx context.Context, dstExec, srcExec ExecFn, name string) error {
	if res, err := dstExec(ctx, "docker network inspect "+shellQuote(name)); err == nil && res.ExitCode == 0 {
		return nil // already present
	}
	driver := ""
	if res, err := srcExec(ctx, "docker network inspect -f '{{.Driver}}' "+shellQuote(name)); err == nil && res.ExitCode == 0 {
		driver = strings.TrimSpace(res.Stdout)
	}
	cmd := "docker network create "
	if driver != "" {
		cmd += "--driver " + shellQuote(driver) + " "
	}
	cmd += shellQuote(name)
	res, err := dstExec(ctx, cmd)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("%s", strings.TrimSpace(res.Stderr))
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
