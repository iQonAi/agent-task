package unitfile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixture is a minimal stand-in for deploy/systemd/agent-taskd.service's
// relevant section: enough context before and after the Secrets block to
// prove the splice doesn't disturb anything else.
const fixture = `[Service]
Type=notify
User=agent-taskd

# --- Secrets: systemd LoadCredential (root-owned 0600 source files) ---
# Each secret is delivered read-only into $CREDENTIALS_DIRECTORY.
LoadCredential=gh-token-agent-task:/etc/agent-task/credentials/gh-token-agent-task
#LoadCredential=gh-token-case-tracker-fc:/etc/agent-task/credentials/gh-token-case-tracker-fc

Restart=on-failure
`

func write(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "agent-taskd.service")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

func read(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func TestEnsureLoadCredential_AddsMissingLine(t *testing.T) {
	path := write(t, fixture)

	changed, err := EnsureLoadCredential(path, "claude-oauth-token", "/etc/agent-task/credentials/claude-oauth-token")
	if err != nil {
		t.Fatalf("EnsureLoadCredential: %v", err)
	}
	if !changed {
		t.Fatal("changed = false, want true for a new ref")
	}

	got := read(t, path)
	if !HasLine(got, "claude-oauth-token") {
		t.Fatalf("new LoadCredential line not found in:\n%s", got)
	}
	// Everything that existed before must still be present untouched.
	if !containsLine(got, "LoadCredential=gh-token-agent-task:/etc/agent-task/credentials/gh-token-agent-task") {
		t.Fatalf("existing active line was disturbed:\n%s", got)
	}
	if !containsLine(got, "#LoadCredential=gh-token-case-tracker-fc:/etc/agent-task/credentials/gh-token-case-tracker-fc") {
		t.Fatalf("existing commented line was disturbed:\n%s", got)
	}
	if !containsLine(got, "Restart=on-failure") {
		t.Fatalf("content after the block was disturbed:\n%s", got)
	}
}

func TestEnsureLoadCredential_IdempotentOnRepeat(t *testing.T) {
	path := write(t, fixture)

	if _, err := EnsureLoadCredential(path, "claude-oauth-token", "/etc/agent-task/credentials/claude-oauth-token"); err != nil {
		t.Fatalf("first EnsureLoadCredential: %v", err)
	}
	once := read(t, path)

	changed, err := EnsureLoadCredential(path, "claude-oauth-token", "/etc/agent-task/credentials/claude-oauth-token")
	if err != nil {
		t.Fatalf("second EnsureLoadCredential: %v", err)
	}
	if changed {
		t.Fatal("changed = true on second call, want false (no-op)")
	}

	twice := read(t, path)
	if once != twice {
		t.Fatalf("second call modified the file.\nafter first:\n%s\nafter second:\n%s", once, twice)
	}
	if n := strings.Count(twice, "LoadCredential=claude-oauth-token:"); n != 1 {
		t.Fatalf("LoadCredential line for claude-oauth-token appears %d times, want 1", n)
	}
}

func TestEnsureLoadCredential_ExistingLineForOtherRefUntouched(t *testing.T) {
	path := write(t, fixture)
	before := read(t, path)

	changed, err := EnsureLoadCredential(path, "some-other-ref", "/etc/agent-task/credentials/some-other-ref")
	if err != nil {
		t.Fatalf("EnsureLoadCredential: %v", err)
	}
	if !changed {
		t.Fatal("changed = false, want true for a new ref")
	}

	got := read(t, path)
	if !containsLine(got, "LoadCredential=gh-token-agent-task:/etc/agent-task/credentials/gh-token-agent-task") {
		t.Fatalf("unrelated existing ref's line was disturbed:\n%s", got)
	}
	if got == before {
		t.Fatal("file was not changed despite reporting changed=true")
	}
}

func TestHasLine(t *testing.T) {
	cases := []struct {
		name string
		ref  string
		want bool
	}{
		{name: "active line present", ref: "gh-token-agent-task", want: true},
		{name: "commented line does not count", ref: "gh-token-case-tracker-fc", want: false},
		{name: "ref not present at all", ref: "nope", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := HasLine(fixture, tc.ref); got != tc.want {
				t.Errorf("HasLine(fixture, %q) = %v, want %v", tc.ref, got, tc.want)
			}
		})
	}
}

func containsLine(text, line string) bool {
	for _, l := range strings.Split(text, "\n") {
		if l == line {
			return true
		}
	}
	return false
}
