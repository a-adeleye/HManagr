package deploy

import "testing"

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
