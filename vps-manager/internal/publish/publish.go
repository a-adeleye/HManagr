// Package publish clones a repo, builds a Docker image from it, and pushes
// the image to a container registry — always on the machine running
// vps-manager, never on the deploy target. Run returns the pushed image
// reference (repo:tag) and the built commit, for the caller to hand the
// image reference to deploy.Run as an inline override so the target only
// ever receives a pulled image, never the source checkout.
package publish

import (
	"context"
	"fmt"
	"path"
	"strings"

	sshpkg "vps-manager/internal/ssh"
)

// ExecFn runs a buffered command (on the local machine, in this package's
// case) — same shape as deploy.ExecFn.
type ExecFn func(ctx context.Context, cmd string) (*sshpkg.ExecResult, error)

// StreamFn runs a command and streams combined output as it arrives.
type StreamFn func(ctx context.Context, cmd string, onOutput func(string)) (int, error)

// ExecStdinFn runs a command with extraStdin piped to its stdin, e.g.
// `docker login --password-stdin`.
type ExecStdinFn func(ctx context.Context, cmd, extraStdin string) (*sshpkg.ExecResult, error)

// Options carries everything Run needs; derived from config.Deployment's
// Publish* fields (plus the repo fields it shares with a target-side deploy).
type Options struct {
	RepoURL string
	Branch  string
	Token   string // GitHub token for private-repo clone

	// LocalPath is fully owned by this package and hard-reset every run —
	// never point it at a working checkout you edit by hand.
	LocalPath    string
	BuildContext string // relative to LocalPath; "." if blank
	ImageRepo    string // e.g. ghcr.io/owner/name — no tag

	RegistryHost     string
	RegistryUsername string
	RegistryToken    string
	ExecStdin        ExecStdinFn
}

// Run clones/updates the repo locally, builds an image tagged with the
// commit's short hash, and pushes it. Returns (imageRef, commit, err).
func Run(ctx context.Context, exec ExecFn, stream StreamFn, opts Options, log func(string)) (string, string, error) {
	if log == nil {
		log = func(string) {}
	}
	scrub := func(s string) string {
		if opts.Token != "" {
			s = strings.ReplaceAll(s, opts.Token, "***")
		}
		if opts.RegistryToken != "" {
			s = strings.ReplaceAll(s, opts.RegistryToken, "***")
		}
		return s
	}
	slog := func(s string) { log(scrub(s)) }
	fail := func(format string, args ...any) error {
		return fmt.Errorf("%s", scrub(fmt.Sprintf(format, args...)))
	}

	if opts.RepoURL == "" || opts.LocalPath == "" || opts.ImageRepo == "" {
		return "", "", fmt.Errorf("repo URL, local build path and image repo are all required to build and publish")
	}
	if opts.RegistryHost == "" || opts.RegistryUsername == "" || opts.RegistryToken == "" {
		return "", "", fmt.Errorf("registry host, username and token are required to publish an image")
	}
	if opts.ExecStdin == nil {
		return "", "", fmt.Errorf("publish requires stdin support (internal error: ExecStdin not wired)")
	}

	branch := opts.Branch
	if branch == "" {
		branch = "main"
	}

	// 1. Preflight.
	slog("→ Checking local build prerequisites…")
	if res, err := exec(ctx, "command -v git"); err != nil {
		return "", "", err
	} else if res.ExitCode != 0 {
		return "", "", fmt.Errorf("git is not available where vps-manager builds images")
	}
	if res, err := exec(ctx, "docker version"); err != nil {
		return "", "", err
	} else if res.ExitCode != 0 {
		return "", "", fail("docker is not available where vps-manager builds images: %s", strings.TrimSpace(res.Stderr))
	}

	dir := shellQuote(opts.LocalPath)
	authURL := injectToken(opts.RepoURL, opts.Token)
	cleanURL := cleanRepoURL(opts.RepoURL)

	// 2. Clone or update the LOCAL checkout.
	res, err := exec(ctx, "test -d "+shellQuote(opts.LocalPath+"/.git"))
	if err != nil {
		return "", "", err
	}
	if res.ExitCode == 0 {
		slog(fmt.Sprintf("→ Updating local build checkout (%s @ %s)…", cleanURL, branch))
		fetch := fmt.Sprintf("git -C %s fetch --depth 1 %s %s && git -C %s reset --hard FETCH_HEAD",
			dir, shellQuote(authURL), shellQuote(branch), dir)
		if res, err := exec(ctx, fetch); err != nil {
			return "", "", err
		} else if res.ExitCode != 0 {
			return "", "", fail("git fetch/reset failed: %s", strings.TrimSpace(res.Stderr))
		}
	} else {
		slog(fmt.Sprintf("→ Cloning %s (branch %s) into %s…", cleanURL, branch, opts.LocalPath))
		if res, err := exec(ctx, "mkdir -p "+shellQuote(path.Dir(opts.LocalPath))); err != nil {
			return "", "", err
		} else if res.ExitCode != 0 {
			return "", "", fail("create parent dir: %s", strings.TrimSpace(res.Stderr))
		}
		clone := fmt.Sprintf("git clone --depth 1 --branch %s %s %s", shellQuote(branch), shellQuote(authURL), dir)
		if res, err := exec(ctx, clone); err != nil {
			return "", "", err
		} else if res.ExitCode != 0 {
			return "", "", fail("git clone failed: %s", strings.TrimSpace(res.Stderr))
		}
		// Never leave the token in the on-disk remote.
		if res, err := exec(ctx, fmt.Sprintf("git -C %s remote set-url origin %s", dir, shellQuote(cleanURL))); err != nil || res.ExitCode != 0 {
			slog("  ⚠ couldn't reset remote URL (continuing)")
		}
	}

	commit := ""
	if res, err := exec(ctx, fmt.Sprintf("git -C %s log -1 --pretty='%%h %%s'", dir)); err == nil && res.ExitCode == 0 {
		commit = strings.TrimSpace(res.Stdout)
		slog("  building commit: " + commit)
	}
	shortSha := ""
	if res, err := exec(ctx, fmt.Sprintf("git -C %s rev-parse --short HEAD", dir)); err == nil && res.ExitCode == 0 {
		shortSha = strings.TrimSpace(res.Stdout)
	}
	if shortSha == "" {
		return commit, "", fmt.Errorf("couldn't resolve the built commit's short hash")
	}

	buildCtx := opts.BuildContext
	if buildCtx == "" {
		buildCtx = "."
	}
	imageRef := fmt.Sprintf("%s:%s", opts.ImageRepo, shortSha)

	// 3. Build, streamed.
	slog(fmt.Sprintf("→ Building %s (context %s)…", imageRef, buildCtx))
	buildCmd := fmt.Sprintf("docker build -t %s %s", shellQuote(imageRef), shellQuote(path.Join(opts.LocalPath, buildCtx)))
	exit, err := stream(ctx, buildCmd, func(chunk string) { log(scrub(strings.TrimRight(chunk, "\n"))) })
	if err != nil {
		return commit, "", fmt.Errorf("docker build: %w", err)
	}
	if exit != 0 {
		return commit, "", fmt.Errorf("docker build exited with code %d — see log above", exit)
	}

	// 4. Login + push. Best-effort logout afterward so the credential doesn't
	// sit in the local docker config any longer than this run.
	slog("→ Logging into " + opts.RegistryHost + "…")
	login := fmt.Sprintf("docker login %s -u %s --password-stdin", shellQuote(opts.RegistryHost), shellQuote(opts.RegistryUsername))
	if res, err := opts.ExecStdin(ctx, login, opts.RegistryToken+"\n"); err != nil {
		return commit, "", err
	} else if res.ExitCode != 0 {
		return commit, "", fail("docker login failed: %s", strings.TrimSpace(firstNonEmpty(res.Stderr, res.Stdout)))
	}
	defer func() { _, _ = exec(ctx, "docker logout "+shellQuote(opts.RegistryHost)) }()

	slog("→ Pushing " + imageRef + "…")
	exit, err = stream(ctx, "docker push "+shellQuote(imageRef), func(chunk string) { log(scrub(strings.TrimRight(chunk, "\n"))) })
	if err != nil {
		return commit, "", fmt.Errorf("docker push: %w", err)
	}
	if exit != 0 {
		return commit, "", fmt.Errorf("docker push exited with code %d — see log above", exit)
	}

	slog("✓ Published " + imageRef)
	return imageRef, commit, nil
}

// firstNonEmpty returns the first non-blank string, for picking whichever of
// stderr/stdout actually carries a CLI's error message.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// injectToken embeds a GitHub token into an https clone URL. Non-https URLs
// (git@github.com:…) pass through untouched and rely on keys present locally.
func injectToken(repo, token string) string {
	if token == "" || !strings.HasPrefix(repo, "https://") {
		return repo
	}
	return "https://x-access-token:" + token + "@" + strings.TrimPrefix(repo, "https://")
}

// cleanRepoURL strips any credentials a user might have pasted into the URL.
func cleanRepoURL(repo string) string {
	if !strings.HasPrefix(repo, "https://") {
		return repo
	}
	rest := strings.TrimPrefix(repo, "https://")
	if at := strings.LastIndex(rest, "@"); at >= 0 {
		rest = rest[at+1:]
	}
	return "https://" + rest
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}
