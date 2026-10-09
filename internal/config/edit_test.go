package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixtureConfig mirrors config.example.yaml's comment style (top-of-file
// comments, a mid-file comment, a blank line between every section, and an
// inline comment on a scalar), so the round-trip tests prove AddRepo and
// RemoveRepo preserve comments and formatting byte-for-byte.
const fixtureConfig = `# Example config for the agent-task daemon. Copy to config.yaml and edit
# NO real secrets here - token_ref is the LoadCredential secret name only

socket_path: /run/agent-task/agent-task.sock
data_dir: /var/lib/agent-task

limits:
  max_concurrent: 2
  task_timeout: 30m

repos:
  - name: agent-task
    owner: iQonAi
    repo: agent-task
    default_branch: main
    token_ref: gh-token-agent-task

# Agent base image and the podman command (the cross-user wrapper on the VM).
image: localhost/agent-task-base:dev
podman: "sudo -u agentbox /usr/local/sbin/agentbox-podman"

# Per-agent auth policy. token_ref names a LoadCredential secret (never the key).
agents:
  claude:
    auth: subscription
    token_ref: claude-oauth-token
  pi:
    auth: api_key # pi has no non-interactive subscription auth; key is ANTHROPIC_API_KEY
    token_ref: anthropic-api-key
`

func writeFixture(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(fixtureConfig), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// TestAddRepo_PreservesCommentsAndFormatting proves AddRepo's diff against
// the original file is exactly the five inserted lines: every comment,
// blank line, and other entry is untouched.
func TestAddRepo_PreservesCommentsAndFormatting(t *testing.T) {
	path := writeFixture(t)

	if err := AddRepo(path, Repo{
		Name: "devbox", Owner: "acme", Repo: "devbox",
		DefaultBranch: "main", TokenRef: "gh-token-devbox",
	}); err != nil {
		t.Fatalf("AddRepo: %v", err)
	}

	want := strings.Replace(fixtureConfig,
		"    token_ref: gh-token-agent-task\n\n",
		"    token_ref: gh-token-agent-task\n"+
			"  - name: devbox\n"+
			"    owner: acme\n"+
			"    repo: devbox\n"+
			"    default_branch: main\n"+
			"    token_ref: gh-token-devbox\n\n",
		1)

	got := readFile(t, path)
	if got != want {
		t.Fatalf("AddRepo did not produce the expected diff.\ngot:\n%s\nwant:\n%s", got, want)
	}

	bak := readFile(t, path+".bak")
	if bak != fixtureConfig {
		t.Fatalf("backup file does not match the pre-edit content.\ngot:\n%s\nwant:\n%s", bak, fixtureConfig)
	}
}

// TestRemoveRepo_PreservesCommentsAndFormatting proves RemoveRepo's diff
// against the original file is exactly the removed entry's five lines (plus
// the blank line that followed it): everything else is untouched.
func TestRemoveRepo_PreservesCommentsAndFormatting(t *testing.T) {
	path := writeFixture(t)

	found, err := RemoveRepo(path, "agent-task")
	if err != nil {
		t.Fatalf("RemoveRepo: %v", err)
	}
	if !found {
		t.Fatalf("RemoveRepo: expected found=true")
	}

	want := strings.Replace(fixtureConfig,
		"repos:\n"+
			"  - name: agent-task\n"+
			"    owner: iQonAi\n"+
			"    repo: agent-task\n"+
			"    default_branch: main\n"+
			"    token_ref: gh-token-agent-task\n\n",
		"repos:\n\n",
		1)

	got := readFile(t, path)
	if got != want {
		t.Fatalf("RemoveRepo did not produce the expected diff.\ngot:\n%s\nwant:\n%s", got, want)
	}

	bak := readFile(t, path+".bak")
	if bak != fixtureConfig {
		t.Fatalf("backup file does not match the pre-edit content.\ngot:\n%s\nwant:\n%s", bak, fixtureConfig)
	}
}

func TestAddRepo_RejectsDuplicateName(t *testing.T) {
	path := writeFixture(t)

	err := AddRepo(path, Repo{
		Name: "agent-task", Owner: "someone-else", Repo: "other",
		DefaultBranch: "main", TokenRef: "gh-token-other",
	})
	if err == nil {
		t.Fatalf("expected an error for a duplicate name, got nil")
	}

	if got := readFile(t, path); got != fixtureConfig {
		t.Fatalf("config file was modified despite the rejected add.\ngot:\n%s", got)
	}
	if _, err := os.Stat(path + ".bak"); err == nil {
		t.Fatalf("backup file was written despite the rejected add")
	}
}

func TestAddRepo_RejectsMissingOwnerOrRepo(t *testing.T) {
	cases := map[string]Repo{
		"missing owner": {Name: "devbox", Owner: "", Repo: "devbox", DefaultBranch: "main", TokenRef: "t"},
		"missing repo":  {Name: "devbox", Owner: "acme", Repo: "", DefaultBranch: "main", TokenRef: "t"},
		"missing name":  {Name: "", Owner: "acme", Repo: "devbox", DefaultBranch: "main", TokenRef: "t"},
	}

	for name, r := range cases {
		t.Run(name, func(t *testing.T) {
			path := writeFixture(t)

			if err := AddRepo(path, r); err == nil {
				t.Fatalf("expected an error for %q, got nil", name)
			}

			if got := readFile(t, path); got != fixtureConfig {
				t.Fatalf("config file was modified despite the rejected add.\ngot:\n%s", got)
			}
		})
	}
}

func TestRemoveRepo_UnknownNameErrorsCleanly(t *testing.T) {
	path := writeFixture(t)

	found, err := RemoveRepo(path, "does-not-exist")
	if err != nil {
		t.Fatalf("RemoveRepo: unexpected error: %v", err)
	}
	if found {
		t.Fatalf("RemoveRepo: expected found=false for an unknown name")
	}

	if got := readFile(t, path); got != fixtureConfig {
		t.Fatalf("config file was modified despite removing an unknown name.\ngot:\n%s", got)
	}
	if _, err := os.Stat(path + ".bak"); err == nil {
		t.Fatalf("backup file was written despite removing an unknown name")
	}
}
