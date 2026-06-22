// Package provision stands up a managed database on a VPS as a small,
// self-contained docker-compose stack — the "add a managed database" flow,
// borrowed for an SSH client. The generated database is deliberately shaped so
// the rest of the app discovers it for free:
//
//   - It runs under a STABLE container_name. The existing DB browser and the
//     restic backup feature both key off the container name (they `docker exec`
//     into it rather than connecting over a host port), so a predictable name is
//     what wires those features up automatically.
//   - By default NO host port is published — the database is reachable only on
//     the docker network, matching how the DB manager already talks to it. A
//     host port is added only when the caller explicitly asks for one.
//
// The approach mirrors internal/deploy's "exec and parse" + streaming compose-up
// pattern: we render a deterministic compose.yaml, write it plus a 0600 .env
// (secrets staged via SecretWriteFn so they never touch a command line), then
// `docker compose up -d`, streamed live to the UI. A strong password is
// generated with crypto/rand when none is supplied, and returned ONCE so the UI
// can show it.
package provision

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"path"
	"sort"
	"strings"

	sshpkg "vps-manager/internal/ssh"
)

// ExecFn runs a buffered remote command (sudo-wrapped by the caller when the
// SSH user needs elevation to touch the chosen directory and docker).
type ExecFn func(ctx context.Context, cmd string) (*sshpkg.ExecResult, error)

// StreamFn runs a remote command and streams combined output as it arrives.
// Returns the exit code.
type StreamFn func(ctx context.Context, cmd string, onOutput func(string)) (int, error)

// SecretWriteFn writes content to a path at the given octal mode WITHOUT placing
// the content on a command line — the App layer implements it by staging over
// SFTP and moving into place, so the generated DB password never appears in `ps`
// output or sudo's syslog.
type SecretWriteFn func(ctx context.Context, path, content, mode string) error

// EngineKind identifies a supported database engine.
type EngineKind string

const (
	Postgres EngineKind = "postgres"
	MySQL    EngineKind = "mysql"
	Redis    EngineKind = "redis"
	MongoDB  EngineKind = "mongodb"
)

// Engine describes one provisionable database engine: its default image, the
// port it listens on inside the container, where its data lives, and the env
// keys that set credentials. The catalog is static (Engines()) and drives the
// provisioning form on the frontend.
type Engine struct {
	Kind         EngineKind `json:"kind"`
	Label        string     `json:"label"`        // human name for the form
	Image        string     `json:"image"`        // default image (no tag)
	Tags         []string   `json:"tags"`         // selectable tags, Tags[0] is the default
	DefaultPort  int        `json:"defaultPort"`  // container port the engine listens on
	MountPath    string     `json:"mountPath"`    // where the named volume mounts inside the container
	NeedsUser    bool       `json:"needsUser"`    // engine has a username credential
	NeedsDB      bool       `json:"needsDB"`      // engine creates an initial database
	NeedsAuth    bool       `json:"needsAuth"`    // engine requires/accepts a password
	UserEnvKey   string     `json:"userEnvKey"`   // env key for the username (empty if N/A)
	PassEnvKey   string     `json:"passEnvKey"`   // env key for the password
	DBEnvKey     string     `json:"dbEnvKey"`     // env key for the initial DB name (empty if N/A)
	DefaultUser  string     `json:"defaultUser"`  // suggested username
	BackupEngine string     `json:"backupEngine"` // engine string the backup feature understands ("postgres"|"mysql"|"")
}

// catalog is the single source of truth for engine defaults. Engines() returns a
// copy so callers can't mutate it.
var catalog = []Engine{
	{
		Kind:         Postgres,
		Label:        "PostgreSQL",
		Image:        "postgres",
		Tags:         []string{"16", "15", "14", "13"},
		DefaultPort:  5432,
		MountPath:    "/var/lib/postgresql/data",
		NeedsUser:    true,
		NeedsDB:      true,
		NeedsAuth:    true,
		UserEnvKey:   "POSTGRES_USER",
		PassEnvKey:   "POSTGRES_PASSWORD",
		DBEnvKey:     "POSTGRES_DB",
		DefaultUser:  "postgres",
		BackupEngine: "postgres",
	},
	{
		Kind:         MySQL,
		Label:        "MySQL / MariaDB",
		Image:        "mariadb",
		Tags:         []string{"11", "10.11", "mysql:8.0", "mysql:8.4"},
		DefaultPort:  3306,
		MountPath:    "/var/lib/mysql",
		NeedsUser:    false, // root is implied; MYSQL_ROOT_PASSWORD is the credential
		NeedsDB:      true,
		NeedsAuth:    true,
		UserEnvKey:   "", // backups dump as root
		PassEnvKey:   "MYSQL_ROOT_PASSWORD",
		DBEnvKey:     "MYSQL_DATABASE",
		DefaultUser:  "root",
		BackupEngine: "mysql",
	},
	{
		Kind:        Redis,
		Label:       "Redis",
		Image:       "redis",
		Tags:        []string{"7", "7.2", "6"},
		DefaultPort: 6379,
		MountPath:   "/data",
		NeedsUser:   false,
		NeedsDB:     false,
		NeedsAuth:   true,
		PassEnvKey:  "REDIS_PASSWORD", // consumed by our command override, see renderService
	},
	{
		Kind:        MongoDB,
		Label:       "MongoDB",
		Image:       "mongo",
		Tags:        []string{"7", "6", "5"},
		DefaultPort: 27017,
		MountPath:   "/data/db",
		NeedsUser:   true,
		NeedsDB:     false,
		NeedsAuth:   true,
		UserEnvKey:  "MONGO_INITDB_ROOT_USERNAME",
		PassEnvKey:  "MONGO_INITDB_ROOT_PASSWORD",
		DefaultUser: "root",
	},
}

// Engines returns the static engine catalog for the provisioning form.
func Engines() []Engine {
	out := make([]Engine, len(catalog))
	copy(out, catalog)
	return out
}

// engineFor looks up an engine by kind.
func engineFor(kind EngineKind) (Engine, bool) {
	for _, e := range catalog {
		if e.Kind == kind {
			return e, true
		}
	}
	return Engine{}, false
}

// Spec describes a database to provision.
type Spec struct {
	Engine     EngineKind `json:"engine"`
	Dir        string     `json:"dir"`        // compose dir on the VPS, e.g. /opt/databases/app-pg
	Name       string     `json:"name"`       // service + container base name (validated)
	Tag        string     `json:"tag"`        // image tag (defaults to engine's first tag)
	User       string     `json:"user"`       // username (engines that take one)
	Database   string     `json:"database"`   // initial database (engines that take one)
	Password   string     `json:"password"`   // optional; generated when blank
	ExposePort int        `json:"exposePort"` // publish this host port; 0 = internal only
	VolumeName string     `json:"volumeName"` // optional override; defaults to <name>_data
}

// Result is returned after a successful Provision. Password is the resolved
// password (generated or supplied) so the UI can show it ONCE.
type Result struct {
	ContainerName string `json:"containerName"`
	Service       string `json:"service"`
	VolumeName    string `json:"volumeName"`
	Image         string `json:"image"`
	Password      string `json:"password"`
	ComposeFile   string `json:"composeFile"`
	BackupEngine  string `json:"backupEngine"`
}

// resolved is a Spec with all defaults filled in and validated, ready to render.
type resolved struct {
	engine     Engine
	name       string
	image      string
	user       string
	database   string
	password   string
	exposePort int
	volume     string
	dir        string
}

// resolveSpec validates the spec and fills defaults: engine lookup, tag, image,
// username, volume name, and a generated password when none is supplied.
func resolveSpec(spec Spec) (resolved, error) {
	eng, ok := engineFor(spec.Engine)
	if !ok {
		return resolved{}, fmt.Errorf("unknown engine %q", spec.Engine)
	}
	name := strings.TrimSpace(spec.Name)
	if !ValidName(name) {
		return resolved{}, fmt.Errorf("invalid database name %q: use 1-63 chars of a-z, 0-9, '-' or '_', starting with a letter", spec.Name)
	}
	if strings.TrimSpace(spec.Dir) == "" {
		return resolved{}, fmt.Errorf("a target directory is required")
	}

	image := resolveImage(eng, spec.Tag)

	user := strings.TrimSpace(spec.User)
	if eng.NeedsUser {
		if user == "" {
			user = eng.DefaultUser
		}
		if !ValidName(user) {
			return resolved{}, fmt.Errorf("invalid username %q", spec.User)
		}
	} else {
		user = ""
	}

	database := strings.TrimSpace(spec.Database)
	if eng.NeedsDB {
		if database == "" {
			database = name
		}
		if !ValidName(database) {
			return resolved{}, fmt.Errorf("invalid database name %q", spec.Database)
		}
	} else {
		database = ""
	}

	if spec.ExposePort < 0 || spec.ExposePort > 65535 {
		return resolved{}, fmt.Errorf("invalid host port %d", spec.ExposePort)
	}

	volume := strings.TrimSpace(spec.VolumeName)
	if volume == "" {
		volume = name + "_data"
	}
	if !ValidName(volume) {
		return resolved{}, fmt.Errorf("invalid volume name %q", spec.VolumeName)
	}

	password := spec.Password
	if eng.NeedsAuth && strings.TrimSpace(password) == "" {
		pw, err := GeneratePassword()
		if err != nil {
			return resolved{}, fmt.Errorf("generate password: %w", err)
		}
		password = pw
	}

	return resolved{
		engine:     eng,
		name:       name,
		image:      image,
		user:       user,
		database:   database,
		password:   password,
		exposePort: spec.ExposePort,
		volume:     volume,
		dir:        strings.TrimRight(strings.TrimSpace(spec.Dir), "/"),
	}, nil
}

// resolveImage builds the "image:tag" string. A tag may itself name an
// alternative image (the catalog encodes MySQL's options as "mysql:8.0"); such
// a tag containing a ':' is treated as a full image reference.
func resolveImage(eng Engine, tag string) string {
	tag = strings.TrimSpace(tag)
	if tag == "" {
		if len(eng.Tags) > 0 {
			tag = eng.Tags[0]
		} else {
			tag = "latest"
		}
	}
	if strings.Contains(tag, ":") {
		return tag // full image:tag reference (e.g. "mysql:8.0")
	}
	return eng.Image + ":" + tag
}

// ───────── compose rendering ─────────

// RenderCompose renders a deterministic compose.yaml for the resolved spec: a
// single service with a stable container_name, a named volume mounted at the
// engine's data path, restart: unless-stopped, the credential env (referencing
// the .env file written alongside), and a published port ONLY when ExposePort is
// set. The secret values themselves are NOT written here — the env keys
// reference variables provided by the 0600 .env, so the compose file is safe to
// store world-readable.
func RenderCompose(spec Spec) (string, error) {
	r, err := resolveSpec(spec)
	if err != nil {
		return "", err
	}
	return renderCompose(r), nil
}

func renderCompose(r resolved) string {
	var b strings.Builder
	w := func(s string) { b.WriteString(s + "\n") }

	w("# Generated by vps-manager — managed database (" + string(r.engine.Kind) + ").")
	w("# Do not edit by hand; re-provision to change.")
	w("services:")
	w("  " + r.name + ":")
	w("    image: " + r.image)
	w("    container_name: " + r.name) // STABLE name the DB browser + backup key off
	w("    restart: unless-stopped")

	env := composeEnv(r)
	if len(env) > 0 {
		w("    environment:")
		for _, kv := range env {
			// Value is a ${VAR} reference into the .env, so no quoting hazard.
			w("      " + kv[0] + ": " + kv[1])
		}
	}

	if cmd := serviceCommand(r); cmd != "" {
		w("    command: " + cmd)
	}

	// Publish a host port ONLY when explicitly requested. Default: internal
	// network only, matching the existing DB manager (which uses docker exec).
	if r.exposePort > 0 {
		w("    ports:")
		w(fmt.Sprintf("      - \"%d:%d\"", r.exposePort, r.engine.DefaultPort))
	}

	w("    volumes:")
	w("      - " + r.volume + ":" + r.engine.MountPath)
	w("")
	w("volumes:")
	w("  " + r.volume + ":")

	return b.String()
}

// composeEnv returns the ordered [key, value] env pairs for the service. Values
// are ${VAR} references resolved from the .env file (renderEnvFile writes the
// matching assignments), so no secret is ever inlined into the compose file.
func composeEnv(r resolved) [][2]string {
	var out [][2]string
	add := func(k string) {
		if k != "" {
			out = append(out, [2]string{k, "${" + k + "}"})
		}
	}
	eng := r.engine
	if eng.NeedsUser {
		add(eng.UserEnvKey)
	}
	if eng.NeedsAuth {
		add(eng.PassEnvKey)
	}
	if eng.NeedsDB {
		add(eng.DBEnvKey)
	}
	return out
}

// serviceCommand returns a compose `command:` override for engines that don't
// read a password from an env var directly. Redis needs `--requirepass`, fed
// from the REDIS_PASSWORD variable in the .env.
func serviceCommand(r resolved) string {
	if r.engine.Kind == Redis && r.engine.NeedsAuth && r.password != "" {
		// ${REDIS_PASSWORD} is supplied via the .env; compose substitutes it.
		return `["redis-server", "--requirepass", "${REDIS_PASSWORD}", "--appendonly", "yes"]`
	}
	return ""
}

// renderEnvFile produces the 0600 .env content: one KEY=value per line. These
// are the actual secret values; they are written via SecretWriteFn (never on a
// command line) and referenced by ${KEY} in the compose file.
func renderEnvFile(r resolved) string {
	pairs := map[string]string{}
	eng := r.engine
	if eng.NeedsUser && eng.UserEnvKey != "" {
		pairs[eng.UserEnvKey] = r.user
	}
	if eng.NeedsAuth && eng.PassEnvKey != "" {
		pairs[eng.PassEnvKey] = r.password
	}
	if eng.NeedsDB && eng.DBEnvKey != "" {
		pairs[eng.DBEnvKey] = r.database
	}

	keys := make([]string, 0, len(pairs))
	for k := range pairs {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	for _, k := range keys {
		// Single-quote so any character in a generated password survives compose's
		// .env parsing. compose treats the quotes as literal-ish for env_file, but
		// for variable substitution in the compose file it strips matching quotes.
		b.WriteString(k + "=" + envQuote(pairs[k]) + "\n")
	}
	return b.String()
}

// envQuote wraps a value in single quotes for a compose .env line, escaping any
// embedded single quote. GeneratePassword only emits hex, so this is belt-and-
// suspenders for user-supplied passwords.
func envQuote(s string) string {
	if s == "" {
		return ""
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// ───────── provisioning ─────────

// Provision creates the directory on the VPS, writes compose.yaml and a 0600
// .env (secrets via secretWrite), then `docker compose up -d`, streamed to
// onOutput. It returns the resolved container name + the generated/used password
// so the UI can display the password ONCE.
func Provision(ctx context.Context, exec ExecFn, stream StreamFn, secretWrite SecretWriteFn, spec Spec, onOutput func(string)) (*Result, error) {
	if onOutput == nil {
		onOutput = func(string) {}
	}
	r, err := resolveSpec(spec)
	if err != nil {
		return nil, err
	}

	// 1. Preflight: docker compose must exist.
	onOutput("→ Checking docker compose on target…")
	if res, err := exec(ctx, "docker compose version"); err != nil {
		return nil, err
	} else if res.ExitCode != 0 {
		return nil, fmt.Errorf("docker compose is not available: %s", strings.TrimSpace(res.Stderr))
	}

	dir := shellQuote(r.dir)
	composePath := path.Join(r.dir, "compose.yaml")
	envPath := path.Join(r.dir, ".env")

	// 2. Make the directory.
	onOutput("→ Creating " + r.dir + " …")
	if res, err := exec(ctx, "mkdir -p "+dir); err != nil {
		return nil, err
	} else if res.ExitCode != 0 {
		return nil, fmt.Errorf("create dir: %s", strings.TrimSpace(res.Stderr))
	}

	// 3. Write compose.yaml (non-secret) and the 0600 .env (secret values via
	//    secretWrite, so the password never touches a command line).
	onOutput("→ Writing compose.yaml …")
	if res, err := exec(ctx, fmt.Sprintf("printf '%%s' %s > %s && chmod 644 %s",
		shellQuote(renderCompose(r)), shellQuote(composePath), shellQuote(composePath))); err != nil {
		return nil, err
	} else if res.ExitCode != 0 {
		return nil, fmt.Errorf("write compose.yaml: %s", strings.TrimSpace(res.Stderr))
	}

	onOutput("→ Writing .env (0600) …")
	if env := renderEnvFile(r); env != "" {
		if err := secretWrite(ctx, envPath, env, "600"); err != nil {
			return nil, fmt.Errorf("write .env: %w", err)
		}
	}

	// 4. Bring it up, streamed.
	onOutput("→ Starting database (docker compose up -d) …")
	up := fmt.Sprintf("cd %s && docker compose up -d", dir)
	exit, err := stream(ctx, up, func(chunk string) {
		onOutput(strings.TrimRight(chunk, "\n"))
	})
	if err != nil {
		return nil, fmt.Errorf("compose up: %w", err)
	}
	if exit != 0 {
		return nil, fmt.Errorf("docker compose up exited with code %d — see log above", exit)
	}

	onOutput("✓ Database provisioned as container " + r.name + ".")
	return &Result{
		ContainerName: r.name,
		Service:       r.name,
		VolumeName:    r.volume,
		Image:         r.image,
		Password:      r.password,
		ComposeFile:   "compose.yaml",
		BackupEngine:  r.engine.BackupEngine,
	}, nil
}

// ───────── validation & secrets ─────────

// ValidName restricts a name to a strict allowlist safe for docker
// service/container/volume names and database identifiers: 1-63 chars, starting
// with an ASCII letter, then letters, digits, '-' or '_'. This rejects anything
// that could break out of a YAML scalar or shell word.
func ValidName(s string) bool {
	if len(s) == 0 || len(s) > 63 {
		return false
	}
	first := rune(s[0])
	if !((first >= 'a' && first <= 'z') || (first >= 'A' && first <= 'Z')) {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}

// GeneratePassword returns a 32-hex-char (16-byte) cryptographically strong
// password. Hex keeps the value safe in env files, URLs, and shells with no
// quoting surprises.
func GeneratePassword() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}
