package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestParseGitHubRef_RejectsMalformedURL covers repos add --url: a handful
// of valid refs (sanity) and the malformed inputs that must be rejected.
func TestParseGitHubRef_RejectsMalformedURL(t *testing.T) {
	cases := []struct {
		name      string
		url       string
		wantOwner string
		wantRepo  string
		wantErr   bool
	}{
		{name: "https", url: "https://github.com/iQonAi/agent-task", wantOwner: "iQonAi", wantRepo: "agent-task"},
		{name: "https with .git", url: "https://github.com/iQonAi/agent-task.git", wantOwner: "iQonAi", wantRepo: "agent-task"},
		{name: "ssh", url: "git@github.com:iQonAi/agent-task.git", wantOwner: "iQonAi", wantRepo: "agent-task"},
		{name: "not a URL", url: "not-a-url", wantErr: true},
		{name: "wrong host", url: "https://gitlab.com/iQonAi/agent-task", wantErr: true},
		{name: "missing repo segment", url: "https://github.com/iQonAi", wantErr: true},
		{name: "empty", url: "", wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			owner, repo, err := parseGitHubRef(tc.url)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error for %q, got nil", tc.url)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error for %q: %v", tc.url, err)
			}
			if owner != tc.wantOwner || repo != tc.wantRepo {
				t.Fatalf("parseGitHubRef(%q) = (%q, %q), want (%q, %q)",
					tc.url, owner, repo, tc.wantOwner, tc.wantRepo)
			}
		})
	}
}

// TestRunReposAdd_DryRunWritesNothing covers repos add --dry-run: the
// config file's content and mtime must be unchanged, and no backup file
// must be written.
func TestRunReposAdd_DryRunWritesNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	const content = "repos:\n  - {name: a, owner: o, repo: r, token_ref: t}\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat config: %v", err)
	}

	if err := runReposAdd([]string{
		"--config", path, "--name", "devbox", "--owner", "acme", "--repo", "devbox", "--dry-run",
	}); err != nil {
		t.Fatalf("runReposAdd: %v", err)
	}

	after, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat config: %v", err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Fatalf("dry-run modified the config file's mtime: before=%v after=%v", before.ModTime(), after.ModTime())
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if string(got) != content {
		t.Fatalf("dry-run modified the config file's content.\ngot:\n%s\nwant:\n%s", got, content)
	}

	if _, err := os.Stat(path + ".bak"); err == nil {
		t.Fatalf("dry-run wrote a backup file")
	}
}
