package backup

import (
	"context"
	"fmt"
	"strings"
)

// RestoreOptions controls a restore from a restic snapshot. The same snapshot can
// be restored to its origin VPS or to a different one (the target just needs
// access to the same bucket).
type RestoreOptions struct {
	SnapshotID      string `json:"snapshotId"`
	TargetPath      string `json:"targetPath"`      // compose dir to restore files into / bring up
	RestoreVolumes  bool   `json:"restoreVolumes"`  // recreate named volumes from the tarballs
	RestoreCompose  bool   `json:"restoreCompose"`  // copy the compose files back
	ComposeUp       bool   `json:"composeUp"`       // docker compose up -d after restoring
	ImportDatabases bool   `json:"importDatabases"` // load the .sql dumps into running containers
}

// JobEnvPath is the on-VPS env file for a job (used when restoring on the origin
// VPS, where the credentials already live).
func JobEnvPath(id string) string { return jobEnv(id) }

// WriteRestoreEnv stages a throwaway env file from a bucket + secrets, for
// restoring on a VPS that doesn't already host the job. Returns its path.
func WriteRestoreEnv(ctx context.Context, secretWrite SecretWriteFn, bucket BucketRef, secrets Secrets) (string, error) {
	env := buildEnv(bucket, secrets, nil)
	if env["RESTIC_PASSWORD"] == "" || env["AWS_ACCESS_KEY_ID"] == "" || env["AWS_SECRET_ACCESS_KEY"] == "" {
		return "", fmt.Errorf("restoring on another VPS needs the bucket credentials and restic password")
	}
	p := "/tmp/vps-restore-" + newID() + ".env"
	if err := secretWrite(ctx, p, renderEnv(env), "600"); err != nil {
		return "", err
	}
	return p, nil
}

// RenderRestoreScript builds the restore runner: pull the snapshot with restic,
// then recreate volumes (stopping the stack first so they're not in use), copy
// the compose files back, bring the stack up, and optionally import DB dumps.
// envPath points at the file that exports RESTIC_REPOSITORY + credentials.
func RenderRestoreScript(job Job, opts RestoreOptions, envPath string, cleanupEnv bool) string {
	var b strings.Builder
	w := func(s string) { b.WriteString(s + "\n") }
	tp := shellQuote(strings.TrimRight(opts.TargetPath, "/"))
	hasTarget := strings.TrimSpace(opts.TargetPath) != ""

	w("set -e")
	w("export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
	w("set -a; . " + shellQuote(envPath) + "; set +a")
	w(`STAGING="$(mktemp -d /tmp/vps-restore.XXXXXX)"`)
	w(`START=$(date +%s)`)
	w(`( while true; do sleep 10; echo "  … working — $(( $(date +%s) - START ))s elapsed"; done ) & HB=$!`)
	// Clean up STAGING always; also wipe a throwaway env (cross-VPS restore) even
	// if the Go-side cleanup never runs. envPath is a generated hex path with no
	// shell metacharacters, so it's safe to embed in the trap directly.
	trapCmd := `kill "$HB" 2>/dev/null; rm -rf "$STAGING"`
	if cleanupEnv {
		trapCmd += "; rm -f " + envPath
	}
	w("trap '" + trapCmd + "' EXIT")

	w(`echo "→ downloading snapshot from the bucket (restic restore)…"`)
	w("restic restore " + shellQuote(opts.SnapshotID) + ` --target "$STAGING"`)
	w(`echo "  ✓ snapshot downloaded"`)
	// restic restores under the original absolute path, whose name is random per
	// backup — locate the content dir by its known file patterns instead.
	w(`HIT="$(find "$STAGING" -mindepth 1 \( -name 'vol_*.tar.gz' -o -name 'db_*.sql' -o -type d -name compose \) -print 2>/dev/null | head -1)"`)
	w(`if [ -n "$HIT" ]; then CONTENT="$(dirname "$HIT")"; else CONTENT="$STAGING"; echo "  ⚠ snapshot has no recognizable content"; fi`)

	if opts.RestoreCompose && hasTarget {
		w("")
		w(`if [ -d "$CONTENT/compose" ]; then mkdir -p ` + tp + `; cp -a "$CONTENT/compose/." ` + tp + `/; printf '  ✓ compose files restored to %s\n' ` + tp + `; else echo "  (snapshot has no compose files)"; fi`)
	}

	if opts.RestoreVolumes && len(job.Volumes) > 0 {
		w("")
		w(`echo "→ restoring named volumes (overwrites existing data)…"`)
		if hasTarget {
			w("cd " + tp + " && docker compose down --remove-orphans 2>/dev/null || true")
		}
		// Drive the loop from the job's declared volumes (not a glob over the
		// snapshot) so a restore never touches an unrelated same-named volume on
		// the target.
		for _, vol := range job.Volumes {
			base := "vol_" + sanitize(vol) + ".tar.gz"
			f := `"$CONTENT/` + base + `"`
			vq := shellQuote(vol)
			w("if [ -e " + f + " ]; then")
			w("  echo " + shellQuote("  • "+vol))
			// Stop any container still mounting this volume before overwriting it
			// (covers the no-target case where compose down didn't run). Strict:
			// if we can't stop it, abort rather than wipe a live volume.
			w("  cids=\"$(docker ps -q --filter volume=" + vq + " 2>/dev/null)\"")
			w(`  if [ -n "$cids" ]; then docker stop $cids >/dev/null; fi`)
			w("  docker volume create " + vq + " >/dev/null")
			// Validate the archive (tar tzf) BEFORE deleting live data, so a
			// truncated/corrupt tarball leaves the existing volume intact. Pass
			// the filename positionally ($1) so a crafted name can't be parsed by
			// the inner shell.
			w("  docker run --rm -v " + vq + `:/dest -v "$CONTENT":/src:ro alpine sh -c 'set -e; tar tzf "/src/$1" >/dev/null; find /dest -mindepth 1 -delete; tar xzf "/src/$1" -C /dest' _ ` + shellQuote(base))
			w(`  if [ -n "$cids" ]; then docker start $cids >/dev/null || true; fi`)
			w("else echo " + shellQuote("  (no archive for "+vol+")") + "; fi")
		}
	}

	if opts.ComposeUp && hasTarget {
		w("")
		w(`echo "→ starting stack (docker compose up -d)…"`)
		w("cd " + tp + " && docker compose up -d")
	}

	if opts.ImportDatabases {
		for _, d := range job.Databases {
			df := `"$CONTENT/` + dbDumpName(d.Container, d.DB) + `"`
			c := shellQuote(d.Container)
			w("")
			w("if [ -f " + df + " ]; then")
			w(`  if [ "$(docker inspect -f '{{.State.Running}}' ` + c + ` 2>/dev/null)" != "true" ]; then`)
			w("    echo " + shellQuote("  (container "+d.Container+" not running — skipping import)"))
			w("  else")
			w("    echo " + shellQuote("→ importing dump into "+d.Container+" ("+d.DB+")"))
			switch d.Engine {
			case "postgres":
				// ON_ERROR_STOP + single-transaction: a bad/duplicate import aborts
				// (and rolls back) loudly instead of silently corrupting data.
				w(`    PW="$(docker exec ` + c + ` printenv POSTGRES_PASSWORD 2>/dev/null || true)"`)
				w(`    docker exec -i -e PGPASSWORD="$PW" ` + c + ` psql -v ON_ERROR_STOP=1 --single-transaction -U ` + shellQuote(d.User) + ` -d ` + shellQuote(d.DB) + ` < ` + df)
			case "mysql":
				w(`    PW="$(docker exec ` + c + ` printenv MYSQL_ROOT_PASSWORD 2>/dev/null || true)"`)
				w(`    [ -n "$PW" ] || PW="$(docker exec ` + c + ` printenv MYSQL_PASSWORD 2>/dev/null || true)"`)
				w(`    docker exec -i -e MYSQL_PWD="$PW" ` + c + ` mysql -u ` + shellQuote(d.User) + ` ` + shellQuote(d.DB) + ` < ` + df)
			}
			w("  fi")
			w("else echo " + shellQuote("  (no dump found for "+d.Container+")") + "; fi")
		}
	}

	w("")
	w(`echo "✓ restore complete"`)
	return b.String()
}
