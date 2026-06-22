// Package backup manages scheduled, off-site backups of a docker-compose stack
// on a VPS. A backup job is materialized entirely ON the VPS — a metadata file,
// a secrets file, a runner script, and a /etc/cron.d entry — so backups keep
// running on schedule even when this desktop app is closed. The app is just a
// management UI over those on-VPS files, which makes the VPS the source of
// truth: listing jobs reads them back off the VPS.
//
// Backups are pushed to an S3-compatible bucket with restic (encrypted,
// deduplicated, with snapshot listing + retention). Each job captures logical
// database dumps (pg_dump / mysqldump), tarballs of named volumes, and the
// compose files, then `restic backup`s the lot.
package backup

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strings"

	sshpkg "vps-manager/internal/ssh"
)

// ExecFn runs a buffered remote command (sudo-wrapped by the caller when the
// SSH user needs elevation to touch /etc and docker).
type ExecFn func(ctx context.Context, cmd string) (*sshpkg.ExecResult, error)

// StreamFn runs a remote command and streams combined output as it arrives.
type StreamFn func(ctx context.Context, cmd string, onOutput func(string)) (int, error)

// SecretWriteFn writes content to a (root-owned) path at the given octal mode
// WITHOUT placing the content on a command line — the App layer implements it by
// staging over SFTP and moving into place, so S3 keys and the restic password
// never appear in `ps` output or sudo's syslog.
type SecretWriteFn func(ctx context.Context, path, content, mode string) error

// NewID returns a fresh lowercase-hex id (used for job ids and temp filenames).
func NewID() string { return newID() }

// Where job files live on the VPS. baseDir is root-owned; the .env inside it is
// chmod 600 so the S3 keys and restic password never leak to other users.
const (
	baseDir = "/etc/vps-manager/backups"
	cronDir = "/etc/cron.d"
	logDir  = "/var/log"
)

func jobJSON(id string) string   { return baseDir + "/" + id + ".json" }
func jobEnv(id string) string    { return baseDir + "/" + id + ".env" }
func jobScript(id string) string { return baseDir + "/" + id + ".sh" }
func cronFile(id string) string  { return cronDir + "/vps-manager-" + id }
func logFile(id string) string   { return logDir + "/vps-manager-backup-" + id + ".log" }

// Job is the non-secret description of a backup job, stored as <id>.json.
type Job struct {
	ID             string          `json:"id"`
	Name           string          `json:"name"`
	StackPath      string          `json:"stackPath"`      // compose dir on the VPS
	Volumes        []string        `json:"volumes"`        // named docker volumes to tar
	Databases      []DBTarget      `json:"databases"`      // logical DB dumps
	IncludeCompose bool            `json:"includeCompose"` // copy the compose dir into the backup
	Schedule       string          `json:"schedule"`       // 5-field cron expression
	Retention      RetentionPolicy `json:"retention"`
	Bucket         BucketRef       `json:"bucket"`
	Enabled        bool            `json:"enabled"`
	LastRun        string          `json:"lastRun,omitempty"`
	LastStatus     string          `json:"lastStatus,omitempty"`
}

// DBTarget identifies one database to dump. Container is the docker container
// NAME (stable across restarts, unlike the id); the password is sniffed from the
// container at backup time, never stored.
type DBTarget struct {
	Container string `json:"container"` // container name
	Engine    string `json:"engine"`    // "postgres" | "mysql"
	User      string `json:"user"`
	DB        string `json:"db"`
}

// BucketRef is the non-secret half of the destination. Endpoint may include a
// scheme for non-AWS providers (e.g. https://s3.us-west-002.backblazeb2.com).
type BucketRef struct {
	Endpoint string `json:"endpoint"`
	Region   string `json:"region"`
	Bucket   string `json:"bucket"`
	Prefix   string `json:"prefix"`
}

type RetentionPolicy struct {
	KeepDaily   int `json:"keepDaily"`
	KeepWeekly  int `json:"keepWeekly"`
	KeepMonthly int `json:"keepMonthly"`
	KeepLast    int `json:"keepLast"`
}

// Secrets are written only to <id>.env (chmod 600 root) and never returned to
// the frontend. On edit, blank fields mean "keep what's already on the VPS".
type Secrets struct {
	AccessKey      string `json:"accessKey"`
	SecretKey      string `json:"secretKey"`
	ResticPassword string `json:"resticPassword"`
}

// Snapshot is one restic snapshot as shown in the UI.
type Snapshot struct {
	ID    string   `json:"id"`
	Time  string   `json:"time"`
	Tags  []string `json:"tags"`
	Paths []string `json:"paths"`
}

// ───────── job CRUD (the VPS is the source of truth) ─────────

// SaveJob writes (or overwrites) a job's four on-VPS files. On edit, blank
// secret fields are backfilled from the existing <id>.env so the caller doesn't
// have to re-enter S3 keys. When the job is disabled the cron entry is removed
// so it stops firing while the config is retained.
func SaveJob(ctx context.Context, exec ExecFn, secretWrite SecretWriteFn, job Job, secrets Secrets) (Job, error) {
	if job.ID == "" {
		job.ID = newID()
	}
	if !validID(job.ID) {
		return job, fmt.Errorf("invalid job id")
	}
	if strings.TrimSpace(job.Schedule) != "" && !validCron(job.Schedule) {
		return job, fmt.Errorf("invalid cron schedule %q", job.Schedule)
	}

	// Merge secrets: keep existing values for any field left blank on edit.
	existing := readEnv(ctx, exec, job.ID)
	env := buildEnv(job.Bucket, secrets, existing)
	if env["RESTIC_PASSWORD"] == "" || env["AWS_ACCESS_KEY_ID"] == "" || env["AWS_SECRET_ACCESS_KEY"] == "" {
		return job, fmt.Errorf("missing bucket credentials or restic password")
	}

	if err := secretWrite(ctx, jobEnv(job.ID), renderEnv(env), "600"); err != nil {
		return job, fmt.Errorf("write env: %w", err)
	}
	if err := writeFile(ctx, exec, jobScript(job.ID), RenderScript(job), "700"); err != nil {
		return job, fmt.Errorf("write script: %w", err)
	}
	meta, _ := json.Marshal(job) // compact, single line — one job per line on read
	if err := writeFile(ctx, exec, jobJSON(job.ID), string(meta)+"\n", "644"); err != nil {
		return job, fmt.Errorf("write metadata: %w", err)
	}
	if job.Enabled {
		if err := writeFile(ctx, exec, cronFile(job.ID), RenderCron(job), "644"); err != nil {
			return job, fmt.Errorf("write cron: %w", err)
		}
	} else {
		_, _ = exec(ctx, "rm -f "+shellQuote(cronFile(job.ID)))
	}
	return job, nil
}

// ListJobs reads every job's metadata file off the VPS. Each .json is a single
// compact line, so we can cat them all and parse line by line.
func ListJobs(ctx context.Context, exec ExecFn) ([]Job, error) {
	// 2>/dev/null swallows the "no such file" the glob produces when empty.
	res, err := exec(ctx, "cat "+baseDir+"/*.json 2>/dev/null")
	if err != nil {
		return nil, err
	}
	var jobs []Job
	for _, line := range strings.Split(res.Stdout, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var j Job
		if err := json.Unmarshal([]byte(line), &j); err == nil && j.ID != "" {
			jobs = append(jobs, j)
		}
	}
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].Name < jobs[j].Name })
	return jobs, nil
}

// GetJob returns one job by id (nil if absent).
func GetJob(ctx context.Context, exec ExecFn, id string) (*Job, error) {
	if !validID(id) {
		return nil, fmt.Errorf("invalid job id")
	}
	res, err := exec(ctx, "cat "+shellQuote(jobJSON(id))+" 2>/dev/null")
	if err != nil {
		return nil, err
	}
	line := strings.TrimSpace(res.Stdout)
	if !strings.HasPrefix(line, "{") {
		return nil, nil
	}
	var j Job
	if err := json.Unmarshal([]byte(line), &j); err != nil {
		return nil, err
	}
	return &j, nil
}

// SetJobStatus updates only the LastRun/LastStatus fields in a job's metadata
// (without touching its secrets or script), used after a run finishes.
func SetJobStatus(ctx context.Context, exec ExecFn, id, lastRun, lastStatus string) error {
	j, err := GetJob(ctx, exec, id)
	if err != nil || j == nil {
		return err
	}
	j.LastRun = lastRun
	j.LastStatus = lastStatus
	meta, _ := json.Marshal(j)
	return writeFile(ctx, exec, jobJSON(id), string(meta)+"\n", "644")
}

// DeleteJob removes all of a job's files and its cron entry.
func DeleteJob(ctx context.Context, exec ExecFn, id string) error {
	if !validID(id) {
		return fmt.Errorf("invalid job id")
	}
	cmd := "rm -f " + shellQuote(jobJSON(id)) + " " + shellQuote(jobEnv(id)) + " " +
		shellQuote(jobScript(id)) + " " + shellQuote(cronFile(id))
	res, err := exec(ctx, cmd)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("delete job: %s", strings.TrimSpace(res.Stderr))
	}
	return nil
}

// RunNow executes a job's script immediately, streaming its output. It does not
// touch LastRun/LastStatus — the caller records those.
func RunNow(ctx context.Context, stream StreamFn, id string, onOutput func(string)) (int, error) {
	if !validID(id) {
		return -1, fmt.Errorf("invalid job id")
	}
	return stream(ctx, shellQuote(jobScript(id)), onOutput)
}

// ───────── helpers ─────────

// writeFile writes content to a (possibly root-owned) path with the given octal
// mode, creating parent dirs. It runs through exec, so when exec is sudo-wrapped
// the redirect happens as root and root-owned destinations like /etc work.
func writeFile(ctx context.Context, exec ExecFn, p, content, mode string) error {
	cmd := fmt.Sprintf("mkdir -p %s && printf '%%s' %s > %s && chmod %s %s",
		shellQuote(path.Dir(p)), shellQuote(content), shellQuote(p), mode, shellQuote(p))
	res, err := exec(ctx, cmd)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("%s", strings.TrimSpace(res.Stderr))
	}
	return nil
}

// readEnv reads an existing job .env into a map (empty if it doesn't exist yet),
// used to preserve secrets across edits.
func readEnv(ctx context.Context, exec ExecFn, id string) map[string]string {
	out := map[string]string{}
	res, err := exec(ctx, "cat "+shellQuote(jobEnv(id))+" 2>/dev/null")
	if err != nil || res == nil {
		return out
	}
	for _, line := range strings.Split(res.Stdout, "\n") {
		if i := strings.IndexByte(line, '='); i > 0 {
			out[strings.TrimSpace(line[:i])] = shellUnquote(strings.TrimRight(line[i+1:], "\r"))
		}
	}
	return out
}

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// validID restricts ids to lowercase hex so they're safe inside file paths and
// cron filenames.
func validID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for _, r := range id {
		if !((r >= 'a' && r <= 'f') || (r >= '0' && r <= '9')) {
			return false
		}
	}
	return true
}

// validCron does a light sanity check: five space-separated fields of the cron
// charset. We don't fully parse — cron will reject anything truly malformed.
func validCron(expr string) bool {
	fields := strings.Fields(strings.TrimSpace(expr))
	if len(fields) != 5 {
		return false
	}
	for _, f := range fields {
		for _, r := range f {
			if !((r >= '0' && r <= '9') || r == '*' || r == '/' || r == ',' || r == '-') {
				return false
			}
		}
	}
	return true
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}
