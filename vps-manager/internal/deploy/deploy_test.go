package deploy

import (
	"testing"

	"vps-manager/internal/config"
)

func TestResolveEnvVars(t *testing.T) {
	in := []config.EnvVar{
		{Key: "DB_PASSWORD", Value: "s3cret"},
		{Key: "DATABASE_URL", Value: "postgres://app:${DB_PASSWORD}@db/app"},
		{Key: "CHAIN", Value: "${DATABASE_URL}?sslmode=disable"}, // references a resolved value
		{Key: "BCRYPT", Value: "$2a$10$abcdEFGH"},                 // bare $ must be untouched
		{Key: "UNKNOWN", Value: "${NOPE}"},                       // undefined ref kept verbatim
	}
	got := map[string]string{}
	for _, v := range resolveEnvVars(in) {
		got[v.Key] = v.Value
	}
	if got["DATABASE_URL"] != "postgres://app:s3cret@db/app" {
		t.Errorf("DATABASE_URL = %q", got["DATABASE_URL"])
	}
	if got["CHAIN"] != "postgres://app:s3cret@db/app?sslmode=disable" {
		t.Errorf("CHAIN (transitive) = %q", got["CHAIN"])
	}
	if got["BCRYPT"] != "$2a$10$abcdEFGH" {
		t.Errorf("BCRYPT mangled = %q", got["BCRYPT"])
	}
	if got["UNKNOWN"] != "${NOPE}" {
		t.Errorf("UNKNOWN should stay verbatim, got %q", got["UNKNOWN"])
	}
}

func TestInjectToken(t *testing.T) {
	got := injectToken("https://github.com/owner/repo.git", "tok123")
	want := "https://x-access-token:tok123@github.com/owner/repo.git"
	if got != want {
		t.Errorf("got %s want %s", got, want)
	}
	// ssh URLs pass through untouched
	ssh := "git@github.com:owner/repo.git"
	if got := injectToken(ssh, "tok123"); got != ssh {
		t.Errorf("ssh url modified: %s", got)
	}
	// no token → unchanged
	if got := injectToken("https://github.com/o/r", ""); got != "https://github.com/o/r" {
		t.Errorf("unexpected change: %s", got)
	}
}

func TestCleanRepoURL(t *testing.T) {
	if got := cleanRepoURL("https://user:pw@github.com/o/r.git"); got != "https://github.com/o/r.git" {
		t.Errorf("credentials not stripped: %s", got)
	}
	if got := cleanRepoURL("https://github.com/o/r.git"); got != "https://github.com/o/r.git" {
		t.Errorf("clean url changed: %s", got)
	}
}

func TestParseComposePS(t *testing.T) {
	// NDJSON (one object per line) — the common Compose v2 shape.
	ndjson := `{"Name":"app-web-1","Service":"web","State":"running","Health":"healthy","ExitCode":0}
{"Name":"app-db-1","Service":"db","State":"exited","Health":"","ExitCode":1}`
	svcs, ok := parseComposePS(ndjson)
	if !ok || len(svcs) != 2 {
		t.Fatalf("ndjson: ok=%v n=%d", ok, len(svcs))
	}
	if svcs[0].Service != "web" || svcs[1].ExitCode != 1 {
		t.Fatalf("ndjson decoded wrong: %+v", svcs)
	}

	// JSON array — the shape some versions emit.
	arr := `[{"Name":"x","Service":"x","State":"running","Health":"","ExitCode":0}]`
	if svcs, ok := parseComposePS(arr); !ok || len(svcs) != 1 {
		t.Fatalf("array: ok=%v n=%d", ok, len(svcs))
	}

	// Empty output is parseable-empty (no services), not a failure.
	if svcs, ok := parseComposePS("  \n "); !ok || len(svcs) != 0 {
		t.Fatalf("empty: ok=%v n=%d", ok, len(svcs))
	}

	// Malformed JSON object → not parseable, so the caller degrades gracefully.
	if _, ok := parseComposePS(`{not json}`); ok {
		t.Fatal("malformed JSON should report ok=false")
	}
}

func TestAnyPending(t *testing.T) {
	if !anyPending([]composePS{{State: "running", Health: "starting"}}) {
		t.Error("health=starting should be pending")
	}
	if !anyPending([]composePS{{State: "restarting"}}) {
		t.Error("state=restarting should be pending")
	}
	if anyPending([]composePS{{State: "running", Health: "healthy"}}) {
		t.Error("healthy running should not be pending")
	}
	if anyPending([]composePS{{State: "exited", ExitCode: 0}}) {
		t.Error("exited should not be pending")
	}
}
