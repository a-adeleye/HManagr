package docker

import (
	"context"
	"fmt"
	"strings"
)

// TeardownOptions controls how much of a stack TeardownStack removes. The
// compose project is always brought down (containers + the networks compose
// created); the rest are opt-in because they destroy data or source files.
type TeardownOptions struct {
	Path          string   `json:"path"`          // compose dir on the VPS
	Project       string   `json:"project"`       // compose project name (-p)
	ComposeFiles  []string `json:"composeFiles"`  // -f files (empty = auto-detect)
	RemoveVolumes bool     `json:"removeVolumes"` // docker compose down --volumes
	RemoveImages  bool     `json:"removeImages"`  // docker compose down --rmi all
	RemoveDir     bool     `json:"removeDir"`     // rm -rf the stack directory
}

// composeFlags renders `-p <project>` and one `-f` per file (trailing space).
func composeFlags(project string, files []string) string {
	var b strings.Builder
	if p := strings.TrimSpace(project); p != "" {
		b.WriteString("-p " + shellQuote(p) + " ")
	}
	for _, f := range files {
		if f = strings.TrimSpace(f); f != "" {
			b.WriteString("-f " + shellQuote(f) + " ")
		}
	}
	return b.String()
}

// ComposeDown streams `docker compose down` for the stack at dir, optionally
// also removing named volumes (--volumes) and images (--rmi all). Live output is
// forwarded to onOutput. External volumes and external networks are never
// touched by compose down. Returns the remote exit code.
func ComposeDown(ctx context.Context, stream StreamFn, dir, project string, files []string, removeVolumes, removeImages bool, onOutput func(string)) (int, error) {
	down := "--remove-orphans"
	if removeVolumes {
		down += " --volumes"
	}
	if removeImages {
		down += " --rmi all"
	}
	cmd := fmt.Sprintf("cd %s && docker compose %sdown %s", shellQuote(dir), composeFlags(project, files), down)
	return stream(ctx, cmd, onOutput)
}

// RemoveDir deletes the stack directory. It refuses obviously dangerous targets
// (empty, root, or a path with fewer than two segments like "/opt") so a bad
// value can never turn into `rm -rf /` or wipe a top-level system dir.
func RemoveDir(ctx context.Context, exec ExecFn, dir string) error {
	clean := strings.TrimRight(strings.TrimSpace(dir), "/")
	if !safeToRemove(clean) {
		return fmt.Errorf("refusing to remove unsafe path %q", dir)
	}
	res, err := exec(ctx, "rm -rf "+shellQuote(clean))
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("rm -rf %s: %s", clean, strings.TrimSpace(res.Stderr))
	}
	return nil
}

// safeToRemove allows only absolute paths with at least two non-empty segments
// (e.g. /opt/myapp), keeping teardown away from /, /opt, /home, etc.
func safeToRemove(p string) bool {
	if p == "" || !strings.HasPrefix(p, "/") {
		return false
	}
	segments := 0
	for _, s := range strings.Split(p, "/") {
		if s != "" {
			segments++
		}
	}
	return segments >= 2
}

// StreamFn runs a remote command and streams combined output. Matches the shape
// the deploy package uses and that the App layer supplies.
type StreamFn func(ctx context.Context, cmd string, onOutput func(string)) (int, error)
