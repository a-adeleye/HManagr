// Package deploy pulls a GitHub repo onto a VPS and brings it up with docker
// compose — a minimal Coolify-style "git push → running stack" flow:
//
//  1. Verify git + docker compose exist on the VPS.
//  2. Clone the repo (or fetch + hard-reset if already cloned).
//  3. Write a .env file from the configured variables/secrets.
//  4. docker compose up -d --build, streamed live to the UI.
//
// Private repos authenticate with a GitHub token injected into the https URL
// for the one transient git command; the on-disk remote is always the clean
// URL, so the token never persists on the VPS.
package deploy

import (
	"context"
	"fmt"
	"path"
	"strings"

	"vps-manager/internal/config"
	sshpkg "vps-manager/internal/ssh"
)

// ExecFn runs a buffered remote command (possibly sudo-wrapped by the caller).
type ExecFn func(ctx context.Context, cmd string) (*sshpkg.ExecResult, error)

// StreamFn runs a remote command and streams combined output as it arrives.
// Returns the exit code.
type StreamFn func(ctx context.Context, cmd string, onOutput func(string)) (int, error)

// Options carries everything Run needs; derived from config.Deployment.
type Options struct {
	RepoURL     string
	Branch      string
	Path        string
	ComposeFile string // auto-detected when empty
	Token       string
	EnvVars     []config.EnvVar

	// WriteFile, when set, writes the .env file directly instead of shelling out
	// to `printf > file`. Local deploys supply this so .env content goes through
	// the native filesystem — Git Bash silently strips carriage returns from
	// data piped through it, which would corrupt CRLF-bearing secret values.
	WriteFile func(path, content string) error
}

var composeCandidates = []string{"compose.yaml", "compose.yml", "docker-compose.yaml", "docker-compose.yml"}

// Run executes the deploy end-to-end and returns the deployed commit (short
// hash + subject). Every log line and error is scrubbed of the token.
func Run(ctx context.Context, exec ExecFn, stream StreamFn, opts Options, log func(string)) (string, error) {
	if log == nil {
		log = func(string) {}
	}
	scrub := func(s string) string {
		if opts.Token == "" {
			return s
		}
		return strings.ReplaceAll(s, opts.Token, "***")
	}
	slog := func(s string) { log(scrub(s)) }
	fail := func(format string, args ...any) error {
		return fmt.Errorf("%s", scrub(fmt.Sprintf(format, args...)))
	}

	branch := opts.Branch
	if branch == "" {
		branch = "main"
	}
	if opts.Path == "" || opts.RepoURL == "" {
		return "", fmt.Errorf("repo URL and deploy path are required")
	}

	// 1. Preflight.
	slog("→ Checking prerequisites on target…")
	if res, err := exec(ctx, "command -v git"); err != nil {
		return "", err
	} else if res.ExitCode != 0 {
		return "", fmt.Errorf("git is not installed on the target VPS")
	}
	if res, err := exec(ctx, "docker compose version"); err != nil {
		return "", err
	} else if res.ExitCode != 0 {
		return "", fail("docker compose is not available: %s", strings.TrimSpace(res.Stderr))
	}

	authURL := injectToken(opts.RepoURL, opts.Token)
	cleanURL := cleanRepoURL(opts.RepoURL)
	dir := shellQuote(opts.Path)

	// 2. Clone or update.
	res, err := exec(ctx, "test -d "+shellQuote(opts.Path+"/.git"))
	if err != nil {
		return "", err
	}
	if res.ExitCode == 0 {
		slog(fmt.Sprintf("→ Updating existing checkout (%s @ %s)…", cleanURL, branch))
		fetch := fmt.Sprintf("git -C %s fetch --depth 1 %s %s && git -C %s reset --hard FETCH_HEAD",
			dir, shellQuote(authURL), shellQuote(branch), dir)
		if res, err := exec(ctx, fetch); err != nil {
			return "", err
		} else if res.ExitCode != 0 {
			return "", fail("git fetch/reset failed: %s", strings.TrimSpace(res.Stderr))
		}
	} else {
		slog(fmt.Sprintf("→ Cloning %s (branch %s) into %s…", cleanURL, branch, opts.Path))
		if res, err := exec(ctx, "mkdir -p "+shellQuote(path.Dir(opts.Path))); err != nil {
			return "", err
		} else if res.ExitCode != 0 {
			return "", fail("create parent dir: %s", strings.TrimSpace(res.Stderr))
		}
		clone := fmt.Sprintf("git clone --depth 1 --branch %s %s %s", shellQuote(branch), shellQuote(authURL), dir)
		if res, err := exec(ctx, clone); err != nil {
			return "", err
		} else if res.ExitCode != 0 {
			return "", fail("git clone failed: %s", strings.TrimSpace(res.Stderr))
		}
		// Never leave the token in the on-disk remote.
		if res, err := exec(ctx, fmt.Sprintf("git -C %s remote set-url origin %s", dir, shellQuote(cleanURL))); err != nil || res.ExitCode != 0 {
			slog("  ⚠ couldn't reset remote URL (continuing)")
		}
	}

	// 3. Record what we're deploying.
	commit := ""
	if res, err := exec(ctx, fmt.Sprintf("git -C %s log -1 --pretty='%%h %%s'", dir)); err == nil && res.ExitCode == 0 {
		commit = strings.TrimSpace(res.Stdout)
		slog("  deploying commit: " + commit)
	}

	// 4. Write .env from configured vars (compose reads it for substitution
	// and apps commonly env_file it).
	if len(opts.EnvVars) > 0 {
		slog(fmt.Sprintf("→ Writing .env (%d variable(s))…", len(opts.EnvVars)))
		var b strings.Builder
		for _, ev := range opts.EnvVars {
			if strings.TrimSpace(ev.Key) == "" {
				continue
			}
			b.WriteString(ev.Key)
			b.WriteString("=")
			b.WriteString(ev.Value)
			b.WriteString("\n")
		}
		envPath := path.Join(opts.Path, ".env")
		if opts.WriteFile != nil {
			// Native write (local): exact bytes, no shell normalization.
			if err := opts.WriteFile(envPath, b.String()); err != nil {
				return commit, fail("write .env: %s", err.Error())
			}
		} else {
			write := fmt.Sprintf("printf '%%s' %s > %s && chmod 600 %s",
				shellQuote(b.String()), shellQuote(envPath), shellQuote(envPath))
			if res, err := exec(ctx, write); err != nil {
				return commit, err
			} else if res.ExitCode != 0 {
				return commit, fail("write .env: %s", strings.TrimSpace(res.Stderr))
			}
		}
	}

	// 5. Resolve the compose file.
	composeFile := opts.ComposeFile
	if composeFile == "" {
		for _, cand := range composeCandidates {
			if res, err := exec(ctx, "test -f "+shellQuote(path.Join(opts.Path, cand))); err == nil && res.ExitCode == 0 {
				composeFile = cand
				break
			}
		}
		if composeFile == "" {
			return commit, fmt.Errorf("no compose file found in %s — compose-based repos are required (looked for %s)",
				opts.Path, strings.Join(composeCandidates, ", "))
		}
	}
	slog("→ Building and starting stack (docker compose -f " + composeFile + " up -d --build)…")

	// 6. Build + up, streamed.
	up := fmt.Sprintf("cd %s && docker compose -f %s up -d --build --remove-orphans", dir, shellQuote(composeFile))
	exit, err := stream(ctx, up, func(chunk string) {
		log(scrub(strings.TrimRight(chunk, "\n")))
	})
	if err != nil {
		return commit, fmt.Errorf("compose up: %w", err)
	}
	if exit != 0 {
		return commit, fmt.Errorf("docker compose up exited with code %d — see log above", exit)
	}

	// 7. Show resulting service state.
	if res, err := exec(ctx, fmt.Sprintf("cd %s && docker compose -f %s ps", dir, shellQuote(composeFile))); err == nil && res.ExitCode == 0 {
		slog("→ Stack status:")
		slog(strings.TrimRight(res.Stdout, "\n"))
	}

	slog("✓ Deploy complete.")
	return commit, nil
}

// injectToken embeds a GitHub token into an https clone URL. Non-https URLs
// (git@github.com:…) pass through untouched and rely on keys present on the
// VPS.
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
