package ai

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	sshpkg "vps-manager/internal/ssh"
)

func TestRenderImagesFlagsDanglingAndUnused(t *testing.T) {
	out := `{"Repository":"myapp","Tag":"v1","ID":"abc123","Size":"120MB"}
{"Repository":"<none>","Tag":"<none>","ID":"def456","Size":"50MB"}
{"Repository":"postgres","Tag":"16-alpine","ID":"ghi789","Size":"200MB"}
`
	inUse := map[string]bool{"postgres:16-alpine": true}
	var rows []imageRow
	if err := json.Unmarshal([]byte(renderImages(out, inUse)), &rows); err != nil {
		t.Fatalf("output isn't valid JSON: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("got %d rows, want 3", len(rows))
	}
	byID := map[string]imageRow{}
	for _, r := range rows {
		byID[r.ID] = r
	}
	if byID["abc123"].InUse {
		t.Error("myapp:v1 has no container — should not be flagged inUse")
	}
	if byID["abc123"].Dangling {
		t.Error("myapp:v1 is tagged — should not be flagged dangling")
	}
	if !byID["def456"].Dangling {
		t.Error("<none>:<none> should be flagged dangling")
	}
	if byID["def456"].InUse {
		t.Error("a dangling image should never be flagged inUse")
	}
	if !byID["ghi789"].InUse {
		t.Error("postgres:16-alpine is referenced by a container — should be flagged inUse")
	}
}

func TestRenderImagesEmpty(t *testing.T) {
	if got := renderImages("", nil); got != "null" {
		t.Errorf("empty input should render as JSON null, got %q", got)
	}
}

func TestImagesInUseMatchesBareAndTaggedRefs(t *testing.T) {
	fake := func(_ context.Context, cmd string) (*sshpkg.ExecResult, error) {
		if !strings.HasPrefix(cmd, "docker ps") {
			t.Fatalf("unexpected command: %s", cmd)
		}
		return &sshpkg.ExecResult{Stdout: "postgres:16-alpine\nmyimage\n", ExitCode: 0}, nil
	}
	set, err := imagesInUse(context.Background(), fake)
	if err != nil {
		t.Fatal(err)
	}
	if !set["postgres:16-alpine"] {
		t.Error("expected explicitly tagged ref present")
	}
	// "myimage" (no tag shown by `docker ps`) means the image is docker's
	// implicit myimage:latest — a later lookup against that form must match.
	if !set["myimage:latest"] {
		t.Error("expected bare ref to also register its implicit :latest form")
	}
}

func TestRenderVolumesFlagsOrphans(t *testing.T) {
	out := `{"Driver":"local","Name":"app_data","Mountpoint":"/var/lib/docker/volumes/app_data/_data"}
{"Driver":"local","Name":"old_leftover","Mountpoint":"/var/lib/docker/volumes/old_leftover/_data"}
`
	inUse := map[string]bool{"app_data": true}
	var rows []volumeRow
	if err := json.Unmarshal([]byte(renderVolumes(out, inUse)), &rows); err != nil {
		t.Fatalf("output isn't valid JSON: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
	for _, r := range rows {
		switch r.Name {
		case "app_data":
			if !r.InUse {
				t.Error("app_data should be flagged inUse")
			}
		case "old_leftover":
			if r.InUse {
				t.Error("old_leftover should be flagged orphaned (not inUse)")
			}
		}
	}
}

func TestLabelValue(t *testing.T) {
	labels := "com.docker.compose.project=batlaz,com.docker.compose.service=api,other=x"
	if got := labelValue(labels, "com.docker.compose.project"); got != "batlaz" {
		t.Errorf("got %q, want batlaz", got)
	}
	if got := labelValue(labels, "missing"); got != "" {
		t.Errorf("got %q, want empty for missing key", got)
	}
}

func TestRunReadonlyRejectsUnlisted(t *testing.T) {
	_, err := runReadonly(nil, nil, readonlyIn{Command: "rm -rf /"})
	if err == nil {
		t.Fatal("expected rejection for a command outside the allowlist")
	}
	if !strings.Contains(err.Error(), "not on the read-only allowlist") {
		t.Errorf("wrong rejection reason: %v", err)
	}
}

func TestRunReadonlyRejectsChaining(t *testing.T) {
	_, err := runReadonly(nil, nil, readonlyIn{Command: "docker ps; rm -rf /"})
	if err == nil {
		t.Fatal("expected rejection for a chained command")
	}
	if !strings.Contains(err.Error(), "shell metacharacter") {
		t.Errorf("wrong rejection reason: %v", err)
	}
}

func TestRunReadonlyRejectsRedirection(t *testing.T) {
	for _, cmd := range []string{
		"docker ps > /etc/cron.d/evil",
		"docker logs abc | sh",
		"cat $(echo /etc/shadow)",
		"docker ps `whoami`",
	} {
		if _, err := runReadonly(nil, nil, readonlyIn{Command: cmd}); err == nil {
			t.Errorf("expected rejection for %q", cmd)
		}
	}
}

func TestRunReadonlyRejectsEmpty(t *testing.T) {
	if _, err := runReadonly(nil, nil, readonlyIn{Command: "   "}); err == nil {
		t.Fatal("expected rejection for an empty command")
	}
}
