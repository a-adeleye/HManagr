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
//
// RepoURL is optional: leaving it blank skips steps 1's git check and all of
// step 2, so this can also drive a directory some other system already
// populates and owns (e.g. a CI pipeline that scp's its own compose.yaml and
// keeps a hand-managed .env there) — vps-manager then just logs into the
// registry (if configured) and runs compose against whatever's already on
// disk, without cloning on top of it or requiring an empty directory.
package deploy

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"strings"
	"time"

	"vps-manager/internal/config"
	sshpkg "vps-manager/internal/ssh"
)

// envRefRe matches a ${NAME} reference to another variable in the same set.
// Only the braced form is expanded so values that legitimately contain a bare
// '$' (bcrypt hashes like $2a$…, regexes) are never touched.
var envRefRe = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// resolveEnvVars expands ${KEY} references in env values against the other
// variables in the same deployment, so users can compose values like
// DATABASE_URL=postgres://app:${DB_PASSWORD}@db/app without repeating a secret.
// docker compose does this for the compose file but NOT for env_file values,
// which is the gap this fills. Resolution iterates (a value may reference one
// that itself references another) with a fixed cap to break cycles; a reference
// to an undefined key is left verbatim so downstream tools can still use it.
func resolveEnvVars(vars []config.EnvVar) []config.EnvVar {
	values := make(map[string]string, len(vars))
	for _, v := range vars {
		values[v.Key] = v.Value
	}
	expand := func(s string) string {
		return envRefRe.ReplaceAllStringFunc(s, func(m string) string {
			key := m[2 : len(m)-1] // strip ${ and }
			if val, ok := values[key]; ok {
				return val
			}
			return m
		})
	}
	out := make([]config.EnvVar, len(vars))
	copy(out, vars)
	for pass := 0; pass < 10; pass++ {
		changed := false
		for i := range out {
			expanded := expand(out[i].Value)
			if expanded != out[i].Value {
				out[i].Value = expanded
				values[out[i].Key] = expanded
				changed = true
			}
		}
		if !changed {
			break
		}
	}
	return out
}

// ExecFn runs a buffered remote command (possibly sudo-wrapped by the caller).
type ExecFn func(ctx context.Context, cmd string) (*sshpkg.ExecResult, error)

// StreamFn runs a remote command and streams combined output as it arrives.
// Returns the exit code.
type StreamFn func(ctx context.Context, cmd string, onOutput func(string)) (int, error)

// ExecStdinFn runs a buffered remote command with extraStdin piped to its
// stdin, e.g. `docker login --password-stdin` — so a registry token never
// touches the command line, shell history, or process list.
type ExecStdinFn func(ctx context.Context, cmd, extraStdin string) (*sshpkg.ExecResult, error)

// Options carries everything Run needs; derived from config.Deployment.
type Options struct {
	// RepoURL is optional. Blank means "the directory at Path is already
	// populated by something else" — Run skips git entirely and just brings
	// up whatever compose file is already there. Branch/Token are ignored
	// when RepoURL is blank.
	RepoURL     string
	Branch      string
	Path        string
	ComposeFile string // auto-detected when empty
	Token       string
	EnvVars     []config.EnvVar

	// Registry* log the target into a container registry (e.g. ghcr.io)
	// before `docker compose up`, so compose can pull a private pre-built
	// image. All three empty skips the login step entirely.
	RegistryHost     string
	RegistryUsername string
	RegistryToken    string
	// ExecStdin is required when RegistryHost is set; unused otherwise.
	ExecStdin ExecStdinFn

	// ImageOverride, when set, is passed as an inline env-var override (named
	// ImageEnvVar, default "API_IMAGE") to `docker compose up` on the
	// target — e.g. API_IMAGE=ghcr.io/owner/name:sha — so the target pulls
	// exactly this image without it ever being written into its .env. Set by
	// the app layer after a local build-and-publish phase (see
	// internal/publish); never set for a plain clone-and-build-on-target or
	// pull-existing-directory deploy.
	ImageOverride string
	ImageEnvVar   string

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

	if opts.Path == "" {
		return "", fmt.Errorf("deploy path is required")
	}

	// 1. Preflight. Git is only needed when we're actually cloning/pulling.
	slog("→ Checking prerequisites on target…")
	if opts.RepoURL != "" {
		if res, err := exec(ctx, "command -v git"); err != nil {
			return "", err
		} else if res.ExitCode != 0 {
			return "", fmt.Errorf("git is not installed on the target VPS")
		}
	}
	if res, err := exec(ctx, "docker compose version"); err != nil {
		return "", err
	} else if res.ExitCode != 0 {
		return "", fail("docker compose is not available: %s", strings.TrimSpace(res.Stderr))
	}

	dir := shellQuote(opts.Path)
	commit := ""

	// 2. Clone or update — skipped entirely when RepoURL is blank, so
	// vps-manager can also drive a directory it doesn't own (e.g. one a CI
	// pipeline already populates and keeps its own .env in) without trying to
	// git-clone on top of it.
	if opts.RepoURL == "" {
		if res, err := exec(ctx, "test -d "+dir); err != nil {
			return "", err
		} else if res.ExitCode != 0 {
			return "", fmt.Errorf("%s does not exist on the target — either create it first (e.g. via your existing deploy pipeline) or set a repo URL for vps-manager to clone", opts.Path)
		}
		slog("→ No repo URL set — using the existing directory as-is (no git, .env left untouched unless variables are configured below).")
	} else {
		branch := opts.Branch
		if branch == "" {
			branch = "main"
		}
		authURL := injectToken(opts.RepoURL, opts.Token)
		cleanURL := cleanRepoURL(opts.RepoURL)

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
		if res, err := exec(ctx, fmt.Sprintf("git -C %s log -1 --pretty='%%h %%s'", dir)); err == nil && res.ExitCode == 0 {
			commit = strings.TrimSpace(res.Stdout)
			slog("  deploying commit: " + commit)
		}
	}

	// 4. Write .env from configured vars (compose reads it for substitution
	// and apps commonly env_file it).
	if len(opts.EnvVars) > 0 {
		slog(fmt.Sprintf("→ Writing .env (%d variable(s))…", len(opts.EnvVars)))
		resolved := resolveEnvVars(opts.EnvVars)
		var b strings.Builder
		for _, ev := range resolved {
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
	// 5b. Registry login, so compose can pull a private pre-built image
	// instead of needing a build context on the target. Best-effort logout
	// once the deploy finishes so the credential doesn't sit in the target's
	// docker config any longer than the deploy itself.
	if opts.RegistryHost != "" {
		if opts.RegistryUsername == "" || opts.RegistryToken == "" {
			return commit, fmt.Errorf("registry host is set but username or token is missing")
		}
		if opts.ExecStdin == nil {
			return commit, fmt.Errorf("registry login requires stdin support (internal error: ExecStdin not wired)")
		}
		slog("→ Logging into " + opts.RegistryHost + "…")
		login := fmt.Sprintf("docker login %s -u %s --password-stdin", shellQuote(opts.RegistryHost), shellQuote(opts.RegistryUsername))
		if res, err := opts.ExecStdin(ctx, login, opts.RegistryToken+"\n"); err != nil {
			return commit, err
		} else if res.ExitCode != 0 {
			return commit, fail("docker login failed: %s", strings.TrimSpace(firstNonEmpty(res.Stderr, res.Stdout)))
		}
		defer func() {
			logoutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_, _ = exec(logoutCtx, "docker logout "+shellQuote(opts.RegistryHost))
		}()
	}

	// An image override is passed as an inline env-var assignment ahead of
	// the compose command — e.g. `API_IMAGE=ghcr.io/owner/name:sha docker
	// compose ...` — exactly like the CI pipeline does it, so the pulled
	// image is pinned to precisely what the publish phase just pushed
	// without writing anything into the target's .env.
	envPrefix := ""
	if opts.ImageOverride != "" {
		envVar := opts.ImageEnvVar
		if envVar == "" {
			envVar = "API_IMAGE"
		}
		envPrefix = envVar + "=" + shellQuote(opts.ImageOverride) + " "
		slog(fmt.Sprintf("→ Pinning %s=%s for this deploy…", envVar, opts.ImageOverride))
	}

	slog("→ Building and starting stack (docker compose -f " + composeFile + " up -d --build --pull always)…")

	// 6. Build + up, streamed. --pull always forces a fresh pull of every
	// image-only service (registry-backed ones especially) even when a local
	// image already exists under the same tag — otherwise redeploying a
	// mutable tag like :latest would silently keep running the stale cached
	// image. Services with their own build: section are unaffected (nothing
	// to pull yet — it only refreshes their Dockerfile FROM base layers).
	up := fmt.Sprintf("cd %s && %sdocker compose -f %s up -d --build --pull always --remove-orphans", dir, envPrefix, shellQuote(composeFile))
	exit, err := stream(ctx, up, func(chunk string) {
		log(scrub(strings.TrimRight(chunk, "\n")))
	})
	if err != nil {
		return commit, fmt.Errorf("compose up: %w", err)
	}
	if exit != 0 {
		return commit, fmt.Errorf("docker compose up exited with code %d — see log above", exit)
	}

	// 7. Health-gate. `up -d` returns as soon as containers START, so a service
	// that crashes on boot or fails its healthcheck still looks like a success.
	// Probe the stack and fail the deploy (with logs) if anything is broken.
	if err := probeStack(ctx, exec, dir, composeFile, slog); err != nil {
		return commit, err
	}

	slog("✓ Deploy complete.")
	return commit, nil
}

// composePS is the subset of `docker compose ps --format json` we read.
type composePS struct {
	Name     string `json:"Name"`
	Service  string `json:"Service"`
	State    string `json:"State"`
	Health   string `json:"Health"`
	ExitCode int    `json:"ExitCode"`
}

// probeStack waits for the just-started services to settle, then reports their
// health. It fails the deploy only when a service is DEFINITIVELY broken (exited
// non-zero, restarting, dead, or unhealthy); a service still running its
// healthcheck is reported as a warning, not a failure. A probe that can't run at
// all (old compose, unparseable output) degrades to a plain status dump and
// never blocks the deploy on its own shortcomings.
func probeStack(ctx context.Context, exec ExecFn, dir, composeFile string, slog func(string)) error {
	slog("→ Checking service health…")
	psCmd := fmt.Sprintf("cd %s && docker compose -f %s ps --format json", dir, shellQuote(composeFile))

	var services []composePS
	var parsed bool
	// Poll up to ~24s so containers with healthchecks have time to pass.
	for attempt := 0; attempt < 12; attempt++ {
		res, err := exec(ctx, psCmd)
		if err != nil {
			return err // transport / context cancellation
		}
		if res.ExitCode != 0 {
			break // `ps --format json` unsupported → degrade below
		}
		svc, ok := parseComposePS(res.Stdout)
		if !ok {
			break
		}
		parsed = true
		services = svc
		if !anyPending(services) {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}

	if !parsed {
		// Couldn't introspect health — show a plain status and don't block.
		if res, err := exec(ctx, fmt.Sprintf("cd %s && docker compose -f %s ps", dir, shellQuote(composeFile))); err == nil && res.ExitCode == 0 {
			slog("→ Stack status:")
			slog(strings.TrimRight(res.Stdout, "\n"))
		}
		return nil
	}

	var broken []composePS
	for _, s := range services {
		state := strings.ToLower(s.State)
		health := strings.ToLower(s.Health)
		switch {
		case health == "unhealthy", state == "restarting", state == "dead",
			state == "exited" && s.ExitCode != 0:
			broken = append(broken, s)
			slog(fmt.Sprintf("  ✗ %s — %s", svcLabel(s), describe(s)))
		case health == "starting", state == "created":
			slog(fmt.Sprintf("  … %s — %s (still starting)", svcLabel(s), describe(s)))
		default:
			slog(fmt.Sprintf("  ✓ %s — %s", svcLabel(s), describe(s)))
		}
	}

	if len(broken) > 0 {
		names := make([]string, len(broken))
		for i, s := range broken {
			names[i] = svcLabel(s)
			slog(fmt.Sprintf("→ Recent logs for %s:", svcLabel(s)))
			logsCmd := fmt.Sprintf("cd %s && docker compose -f %s logs --no-color --tail=40 %s",
				dir, shellQuote(composeFile), shellQuote(s.Service))
			if res, err := exec(ctx, logsCmd); err == nil {
				slog(strings.TrimRight(res.Stdout, "\n"))
			}
		}
		return fmt.Errorf("deploy started but %d service(s) failed to come up healthy: %s",
			len(broken), strings.Join(names, ", "))
	}

	slog(fmt.Sprintf("✓ %d service(s) healthy.", len(services)))
	return nil
}

func svcLabel(s composePS) string {
	if s.Service != "" {
		return s.Service
	}
	return s.Name
}

func describe(s composePS) string {
	d := s.State
	if s.Health != "" {
		d += " / " + s.Health
	}
	if strings.EqualFold(s.State, "exited") {
		d += fmt.Sprintf(" (code %d)", s.ExitCode)
	}
	return d
}

// anyPending reports whether any service is still settling, so the poll loop
// keeps waiting rather than judging a healthcheck that hasn't finished.
func anyPending(svcs []composePS) bool {
	for _, s := range svcs {
		state := strings.ToLower(s.State)
		health := strings.ToLower(s.Health)
		if health == "starting" || state == "created" || state == "restarting" {
			return true
		}
	}
	return false
}

// parseComposePS decodes `docker compose ps --format json`, tolerating both the
// NDJSON (one object per line) and JSON-array shapes different compose versions
// emit. The bool is false only when the output looks like JSON we couldn't
// decode (so the caller degrades instead of trusting a half-read result).
func parseComposePS(out string) ([]composePS, bool) {
	out = strings.TrimSpace(out)
	if out == "" {
		return nil, true
	}
	if strings.HasPrefix(out, "[") {
		var arr []composePS
		if err := json.Unmarshal([]byte(out), &arr); err != nil {
			return nil, false
		}
		return arr, true
	}
	var svcs []composePS
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var s composePS
		if err := json.Unmarshal([]byte(line), &s); err != nil {
			return nil, false
		}
		svcs = append(svcs, s)
	}
	return svcs, true
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
