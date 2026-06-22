package backup

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

func sampleJob() Job {
	return Job{
		ID:        "abcd1234ef567890",
		Name:      "batlaz nightly",
		StackPath: "/opt/batlaz",
		Volumes:   []string{"caddy_caddy_data", "redis_data"},
		Databases: []DBTarget{
			{Container: "batlaz-postgres-1", Engine: "postgres", User: "postgres", DB: "batlaz"},
			{Container: "app-mysql-1", Engine: "mysql", User: "root", DB: "app"},
		},
		IncludeCompose: true,
		Schedule:       "0 2 * * *",
		Retention:      RetentionPolicy{KeepDaily: 7, KeepWeekly: 4, KeepMonthly: 6},
	}
}

func TestRenderScriptIsValidShell(t *testing.T) {
	script := RenderScript(sampleJob())
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not found; skipping shell syntax check")
	}
	// On Windows, LookPath may resolve a broken WSL relay rather than a real
	// bash; only run the check if bash actually executes.
	if err := exec.Command(bash, "-c", "exit 0").Run(); err != nil {
		t.Skipf("bash at %s not runnable (%v); skipping shell syntax check", bash, err)
	}
	f, err := os.CreateTemp("", "bak-*.sh")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if out, err := exec.Command(bash, "-n", f.Name()).CombinedOutput(); err != nil {
		t.Fatalf("bash -n failed: %v\n%s\n--- script ---\n%s", err, out, script)
	}
}

func TestRenderRestoreScriptIsValidShell(t *testing.T) {
	opts := RestoreOptions{
		SnapshotID: "a1b2c3d4", TargetPath: "/opt/batlaz",
		RestoreVolumes: true, RestoreCompose: true, ComposeUp: true, ImportDatabases: true,
	}
	script := RenderRestoreScript(sampleJob(), opts, "/tmp/vps-restore-deadbeef.env", true)
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not found")
	}
	if err := exec.Command(bash, "-c", "exit 0").Run(); err != nil {
		t.Skipf("bash at %s not runnable; skipping", bash)
	}
	f, err := os.CreateTemp("", "restore-*.sh")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())
	f.WriteString(script)
	f.Close()
	if out, err := exec.Command(bash, "-n", f.Name()).CombinedOutput(); err != nil {
		t.Fatalf("bash -n failed: %v\n%s\n--- script ---\n%s", err, out, script)
	}
}

func TestRenderCronIsValidShell(t *testing.T) {
	cron := RenderCron(sampleJob())
	if !strings.Contains(cron, "0 2 * * *  root  /etc/vps-manager/backups/abcd1234ef567890.sh") {
		t.Fatalf("unexpected cron line: %q", cron)
	}
	if !strings.HasSuffix(cron, "\n") {
		t.Fatal("cron line must end with a newline")
	}
}

func TestEnvQuotingRoundTrip(t *testing.T) {
	// Values with quotes/specials must survive render → read.
	secrets := Secrets{
		AccessKey:      "AKIA/EXAMPLE+key",
		SecretKey:      "s3cr3t with spaces and 'quotes'",
		ResticPassword: "p@ss$word`x",
	}
	bucket := BucketRef{Endpoint: "https://s3.example.com", Bucket: "mybucket", Prefix: "batlaz", Region: "us-east-1"}
	rendered := renderEnv(buildEnv(bucket, secrets, nil))

	parsed := map[string]string{}
	for _, line := range strings.Split(rendered, "\n") {
		if i := strings.IndexByte(line, '='); i > 0 {
			parsed[line[:i]] = shellUnquote(line[i+1:])
		}
	}
	if parsed["AWS_SECRET_ACCESS_KEY"] != secrets.SecretKey {
		t.Fatalf("secret key mangled: %q", parsed["AWS_SECRET_ACCESS_KEY"])
	}
	if parsed["RESTIC_PASSWORD"] != secrets.ResticPassword {
		t.Fatalf("restic password mangled: %q", parsed["RESTIC_PASSWORD"])
	}
	if parsed["RESTIC_REPOSITORY"] != "s3:https://s3.example.com/mybucket/batlaz" {
		t.Fatalf("repo url wrong: %q", parsed["RESTIC_REPOSITORY"])
	}
}

func TestCloudflareR2OutOfTheBox(t *testing.T) {
	// User pastes the R2 endpoint without a scheme and leaves region blank.
	bucket := BucketRef{Endpoint: "abc123.r2.cloudflarestorage.com", Bucket: "my-backups", Prefix: "batlaz"}
	if got := resticRepo(bucket); got != "s3:https://abc123.r2.cloudflarestorage.com/my-backups/batlaz" {
		t.Fatalf("R2 repo url wrong: %q", got)
	}
	env := buildEnv(bucket, Secrets{AccessKey: "a", SecretKey: "b", ResticPassword: "p"}, nil)
	if env["AWS_DEFAULT_REGION"] != "auto" {
		t.Fatalf("R2 should default region to auto, got %q", env["AWS_DEFAULT_REGION"])
	}
	// A scheme already present is preserved; a non-R2 endpoint gets no auto region.
	if got := resticRepo(BucketRef{Endpoint: "https://s3.amazonaws.com", Bucket: "b"}); got != "s3:https://s3.amazonaws.com/b" {
		t.Fatalf("aws repo url wrong: %q", got)
	}
	awsEnv := buildEnv(BucketRef{Endpoint: "s3.amazonaws.com", Bucket: "b"}, Secrets{AccessKey: "a", SecretKey: "b", ResticPassword: "p"}, nil)
	if _, ok := awsEnv["AWS_DEFAULT_REGION"]; ok {
		t.Fatalf("non-R2 blank region should stay unset, got %q", awsEnv["AWS_DEFAULT_REGION"])
	}
}

func TestForgetFlags(t *testing.T) {
	if got := forgetFlags(RetentionPolicy{}); got != "" {
		t.Fatalf("empty policy should yield no flags, got %q", got)
	}
	got := forgetFlags(RetentionPolicy{KeepDaily: 7, KeepLast: 3})
	if !strings.Contains(got, "--keep-daily 7") || !strings.Contains(got, "--keep-last 3") {
		t.Fatalf("unexpected flags: %q", got)
	}
}
