package backup

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	sshpkg "vps-manager/internal/ssh"
)

// installScript installs restic if it isn't already present: package manager
// first, then a pinned static binary from GitHub. restic ships a single static
// Go binary, so the download works across distros.
const installScript = `if command -v restic >/dev/null 2>&1; then restic version; exit 0; fi
if command -v apt-get >/dev/null 2>&1; then apt-get update -y >/dev/null 2>&1 && apt-get install -y restic >/dev/null 2>&1 && restic version && exit 0; fi
if command -v dnf >/dev/null 2>&1; then dnf install -y restic >/dev/null 2>&1 && restic version && exit 0; fi
if command -v yum >/dev/null 2>&1; then yum install -y restic >/dev/null 2>&1 && restic version && exit 0; fi
if command -v apk >/dev/null 2>&1; then apk add --no-cache restic >/dev/null 2>&1 && restic version && exit 0; fi
arch=$(uname -m)
case "$arch" in
  x86_64|amd64) a=amd64 ;;
  aarch64|arm64) a=arm64 ;;
  armv7l) a=arm ;;
  *) echo "unsupported architecture: $arch" >&2; exit 1 ;;
esac
v=0.16.5
url="https://github.com/restic/restic/releases/download/v${v}/restic_${v}_linux_${a}.bz2"
tmp="$(mktemp)"
if command -v curl >/dev/null 2>&1; then curl -fsSL "$url" -o "$tmp.bz2"; else wget -qO "$tmp.bz2" "$url"; fi || { echo "download failed" >&2; exit 1; }
command -v bunzip2 >/dev/null 2>&1 || { echo "bunzip2 (bzip2) is required to install restic" >&2; exit 1; }
bunzip2 -f "$tmp.bz2" && install -m 0755 "$tmp" /usr/local/bin/restic && rm -f "$tmp" && restic version`

// EnsureRestic makes sure restic is on the VPS, installing it if needed. Returns
// the version line. Give it a generous context — a cold install downloads.
func EnsureRestic(ctx context.Context, exec ExecFn) (string, error) {
	res, err := exec(ctx, installScript)
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		msg := strings.TrimSpace(res.Stderr)
		if msg == "" {
			msg = strings.TrimSpace(res.Stdout)
		}
		return "", fmt.Errorf("could not install restic: %s", msg)
	}
	return strings.TrimSpace(strings.SplitN(res.Stdout, "\n", 2)[0]), nil
}

// InitRepo initializes the job's restic repository if it isn't already. Reading
// the repo config is a cheap existence probe; we only init when it's absent.
func InitRepo(ctx context.Context, exec ExecFn, id string) error {
	if !validID(id) {
		return fmt.Errorf("invalid job id")
	}
	cmd := sourceEnv(id) + "; restic cat config >/dev/null 2>&1 || restic init"
	res, err := exec(ctx, cmd)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("restic repo init failed: %s", resticErr(res))
	}
	return nil
}

// TestTarget verifies the bucket credentials by initializing/opening the repo
// using a throwaway env file (so it works before the job is saved).
func TestTarget(ctx context.Context, exec ExecFn, secretWrite SecretWriteFn, bucket BucketRef, secrets Secrets) error {
	env := buildEnv(bucket, secrets, nil)
	if env["RESTIC_PASSWORD"] == "" || env["AWS_ACCESS_KEY_ID"] == "" || env["AWS_SECRET_ACCESS_KEY"] == "" {
		return fmt.Errorf("bucket credentials and a restic password are required")
	}
	tmp := "/tmp/vps-bak-test-" + newID() + ".env"
	if err := secretWrite(ctx, tmp, renderEnv(env), "600"); err != nil {
		return fmt.Errorf("stage test env: %w", err)
	}
	cmd := fmt.Sprintf("set -a; . %s; set +a; restic cat config >/dev/null 2>&1 || restic init; rc=$?; rm -f %s; exit $rc",
		shellQuote(tmp), shellQuote(tmp))
	res, err := exec(ctx, cmd)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("could not reach the bucket: %s", resticErr(res))
	}
	return nil
}

// resticSnapshot is the subset of `restic snapshots --json` we surface.
type resticSnapshot struct {
	ShortID string   `json:"short_id"`
	Time    string   `json:"time"`
	Tags    []string `json:"tags"`
	Paths   []string `json:"paths"`
}

// ListSnapshots returns the job's restic snapshots, newest first.
func ListSnapshots(ctx context.Context, exec ExecFn, id string) ([]Snapshot, error) {
	if !validID(id) {
		return nil, fmt.Errorf("invalid job id")
	}
	res, err := exec(ctx, sourceEnv(id)+"; restic snapshots --json")
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("restic snapshots failed: %s", resticErr(res))
	}
	var raw []resticSnapshot
	out := strings.TrimSpace(res.Stdout)
	if out == "" {
		return nil, nil
	}
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		return nil, fmt.Errorf("parse snapshots: %w", err)
	}
	snaps := make([]Snapshot, 0, len(raw))
	for _, s := range raw {
		snaps = append(snaps, Snapshot{ID: s.ShortID, Time: s.Time, Tags: s.Tags, Paths: s.Paths})
	}
	// restic returns oldest-first; show newest-first.
	for i, j := 0, len(snaps)-1; i < j; i, j = i+1, j-1 {
		snaps[i], snaps[j] = snaps[j], snaps[i]
	}
	return snaps, nil
}

// ForgetSnapshot deletes one snapshot and prunes the repo.
func ForgetSnapshot(ctx context.Context, exec ExecFn, id, snapshotID string) error {
	if !validID(id) {
		return fmt.Errorf("invalid job id")
	}
	if !validHex(snapshotID) {
		return fmt.Errorf("invalid snapshot id")
	}
	cmd := sourceEnv(id) + "; restic forget " + shellQuote(snapshotID) + " --prune"
	res, err := exec(ctx, cmd)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("restic forget failed: %s", resticErr(res))
	}
	return nil
}

func validHex(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for _, r := range s {
		if !((r >= 'a' && r <= 'f') || (r >= '0' && r <= '9')) {
			return false
		}
	}
	return true
}

func resticErr(res *sshpkg.ExecResult) string {
	msg := strings.TrimSpace(res.Stderr)
	if msg == "" {
		msg = strings.TrimSpace(res.Stdout)
	}
	if msg == "" {
		msg = fmt.Sprintf("exit %d", res.ExitCode)
	}
	return msg
}
