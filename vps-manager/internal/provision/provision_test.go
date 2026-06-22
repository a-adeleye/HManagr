package provision

import (
	"strings"
	"testing"
)

func TestRenderComposePostgres(t *testing.T) {
	out, err := RenderCompose(Spec{
		Engine:   Postgres,
		Dir:      "/opt/databases/app-pg",
		Name:     "app_pg",
		User:     "appuser",
		Database: "appdb",
		Password: "supplied-secret",
	})
	if err != nil {
		t.Fatalf("RenderCompose: %v", err)
	}

	// Stable container_name keyed off by the DB browser + backup.
	if !strings.Contains(out, "container_name: app_pg") {
		t.Errorf("missing stable container_name:\n%s", out)
	}
	if !strings.Contains(out, "image: postgres:16") {
		t.Errorf("expected default postgres:16 tag:\n%s", out)
	}
	if !strings.Contains(out, "restart: unless-stopped") {
		t.Errorf("missing restart policy:\n%s", out)
	}
	// Named volume declared and mounted at the engine data path.
	if !strings.Contains(out, "app_pg_data:/var/lib/postgresql/data") {
		t.Errorf("missing volume mount:\n%s", out)
	}
	if !strings.Contains(out, "\nvolumes:\n  app_pg_data:") {
		t.Errorf("missing top-level volume declaration:\n%s", out)
	}
	// Correct credential env keys, by reference (not inlined secrets).
	for _, key := range []string{"POSTGRES_USER", "POSTGRES_PASSWORD", "POSTGRES_DB"} {
		if !strings.Contains(out, key+": ${"+key+"}") {
			t.Errorf("missing env key %s by reference:\n%s", key, out)
		}
	}
	// The secret must never be inlined into the compose file.
	if strings.Contains(out, "supplied-secret") {
		t.Errorf("password leaked into compose.yaml:\n%s", out)
	}
	// No host port unless requested.
	if strings.Contains(out, "ports:") {
		t.Errorf("unexpected ports line (internal-only by default):\n%s", out)
	}
}

func TestRenderComposeExposePort(t *testing.T) {
	out, err := RenderCompose(Spec{
		Engine:     Postgres,
		Dir:        "/opt/db",
		Name:       "pg",
		ExposePort: 6543,
	})
	if err != nil {
		t.Fatalf("RenderCompose: %v", err)
	}
	if !strings.Contains(out, "ports:") {
		t.Errorf("expected ports line when ExposePort set:\n%s", out)
	}
	if !strings.Contains(out, `"6543:5432"`) {
		t.Errorf("expected host:container port mapping:\n%s", out)
	}
}

func TestRenderComposeMySQL(t *testing.T) {
	out, err := RenderCompose(Spec{Engine: MySQL, Dir: "/opt/db", Name: "mydb", Database: "shop"})
	if err != nil {
		t.Fatalf("RenderCompose: %v", err)
	}
	for _, key := range []string{"MYSQL_ROOT_PASSWORD", "MYSQL_DATABASE"} {
		if !strings.Contains(out, key+": ${"+key+"}") {
			t.Errorf("missing env key %s:\n%s", key, out)
		}
	}
	// MySQL/MariaDB has no username env var (root is implied).
	if strings.Contains(out, "MYSQL_USER") {
		t.Errorf("unexpected MYSQL_USER env:\n%s", out)
	}
	if !strings.Contains(out, "/var/lib/mysql") {
		t.Errorf("wrong mount path:\n%s", out)
	}
}

func TestRenderComposeRedis(t *testing.T) {
	out, err := RenderCompose(Spec{Engine: Redis, Dir: "/opt/db", Name: "cache"})
	if err != nil {
		t.Fatalf("RenderCompose: %v", err)
	}
	// Redis sets its password via a command override, fed from the .env.
	if !strings.Contains(out, "--requirepass") || !strings.Contains(out, "${REDIS_PASSWORD}") {
		t.Errorf("expected requirepass command override:\n%s", out)
	}
	if !strings.Contains(out, "/data") {
		t.Errorf("wrong mount path:\n%s", out)
	}
	// No initial database for redis.
	if strings.Contains(out, "_DB:") {
		t.Errorf("redis should have no DB env:\n%s", out)
	}
}

func TestRenderComposeMongo(t *testing.T) {
	out, err := RenderCompose(Spec{Engine: MongoDB, Dir: "/opt/db", Name: "mongo"})
	if err != nil {
		t.Fatalf("RenderCompose: %v", err)
	}
	for _, key := range []string{"MONGO_INITDB_ROOT_USERNAME", "MONGO_INITDB_ROOT_PASSWORD"} {
		if !strings.Contains(out, key+": ${"+key+"}") {
			t.Errorf("missing env key %s:\n%s", key, out)
		}
	}
	if !strings.Contains(out, "/data/db") {
		t.Errorf("wrong mount path:\n%s", out)
	}
}

func TestRenderComposeMySQLAltTag(t *testing.T) {
	// A tag containing a colon names a full alternative image (mysql instead of mariadb).
	out, err := RenderCompose(Spec{Engine: MySQL, Dir: "/opt/db", Name: "mydb", Tag: "mysql:8.0"})
	if err != nil {
		t.Fatalf("RenderCompose: %v", err)
	}
	if !strings.Contains(out, "image: mysql:8.0") {
		t.Errorf("expected full image reference for colon tag:\n%s", out)
	}
}

func TestRenderComposeRejectsBadNames(t *testing.T) {
	bad := []Spec{
		{Engine: Postgres, Dir: "/opt/db", Name: "bad name"},
		{Engine: Postgres, Dir: "/opt/db", Name: "9starts-with-digit"},
		{Engine: Postgres, Dir: "/opt/db", Name: "back`tick"},
		{Engine: Postgres, Dir: "/opt/db", Name: "semi;colon"},
		{Engine: Postgres, Dir: "", Name: "ok"},      // missing dir
		{Engine: "nope", Dir: "/opt/db", Name: "ok"}, // unknown engine
	}
	for _, s := range bad {
		if _, err := RenderCompose(s); err == nil {
			t.Errorf("expected error for spec %+v", s)
		}
	}
}

func TestRenderEnvFileContainsSecret(t *testing.T) {
	r, err := resolveSpec(Spec{Engine: Postgres, Dir: "/opt/db", Name: "pg", Password: "hunter2", User: "u", Database: "d"})
	if err != nil {
		t.Fatal(err)
	}
	env := renderEnvFile(r)
	if !strings.Contains(env, "POSTGRES_PASSWORD='hunter2'") {
		t.Errorf("env file missing quoted secret:\n%s", env)
	}
	if !strings.Contains(env, "POSTGRES_USER='u'") || !strings.Contains(env, "POSTGRES_DB='d'") {
		t.Errorf("env file missing user/db:\n%s", env)
	}
}

func TestValidName(t *testing.T) {
	good := []string{"a", "app", "app_pg", "App-1", "abc123", strings.Repeat("a", 63)}
	for _, s := range good {
		if !ValidName(s) {
			t.Errorf("ValidName(%q) = false, want true", s)
		}
	}
	bad := []string{"", "1abc", "-abc", "_abc", "ab c", "ab/c", "a;b", "a$b", "a'b",
		"a`b", strings.Repeat("a", 64), "café"}
	for _, s := range bad {
		if ValidName(s) {
			t.Errorf("ValidName(%q) = true, want false", s)
		}
	}
}

func TestGeneratePassword(t *testing.T) {
	pw, err := GeneratePassword()
	if err != nil {
		t.Fatal(err)
	}
	if len(pw) != 32 {
		t.Errorf("password length = %d, want 32", len(pw))
	}
	for _, r := range pw {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			t.Errorf("password has non-hex char %q: %s", r, pw)
		}
	}
	// Two calls should not collide.
	pw2, _ := GeneratePassword()
	if pw == pw2 {
		t.Error("two generated passwords collided")
	}
}

func TestResolveSpecGeneratesPassword(t *testing.T) {
	r, err := resolveSpec(Spec{Engine: Postgres, Dir: "/opt/db", Name: "pg"})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.password) != 32 {
		t.Errorf("expected generated 32-char password, got %q", r.password)
	}
	// Defaults: user falls back to the engine default, db falls back to the name.
	if r.user != "postgres" {
		t.Errorf("user default = %q, want postgres", r.user)
	}
	if r.database != "pg" {
		t.Errorf("database default = %q, want pg (the name)", r.database)
	}
	if r.volume != "pg_data" {
		t.Errorf("volume default = %q, want pg_data", r.volume)
	}
}

func TestEnginesCatalog(t *testing.T) {
	engs := Engines()
	if len(engs) != 4 {
		t.Fatalf("expected 4 engines, got %d", len(engs))
	}
	// Mutating the returned slice must not affect the catalog.
	engs[0].Label = "MUTATED"
	if Engines()[0].Label == "MUTATED" {
		t.Error("Engines() leaked a mutable reference to the catalog")
	}
}
