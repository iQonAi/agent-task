package github

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseIssue(t *testing.T) {
	js := `{"number":42,"title":"Login is broken","body":"Steps...","url":"https://github.com/o/r/issues/42"}`
	got, err := parseIssue(js)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.Number != 42 || got.Title != "Login is broken" || got.URL == "" {
		t.Errorf("got %+v", got)
	}
}

func TestParseIssueBadJSON(t *testing.T) {
	if _, err := parseIssue("not json"); err == nil {
		t.Error("expected error for bad JSON")
	}
}

func TestBuildPRBody(t *testing.T) {
	body := BuildPRBody(PRInfo{
		TaskID: "t123", Agent: "claude",
		IssueURL: "https://github.com/o/r/issues/7",
		Summary:  "Added subtract().",
	})
	for _, want := range []string{
		"`t123`", "`claude`",
		"https://github.com/o/r/issues/7",
		"Added subtract().",
		"human review required",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("PR body missing %q:\n%s", want, body)
		}
	}
	// The reworded marker must NOT carry the emoji AI-attribution pattern.
	if strings.Contains(body, "🤖") {
		t.Error("PR body contains the 🤖 AI-attribution emoji")
	}
}

func TestBuildPRBodyNoIssueNoSummary(t *testing.T) {
	body := BuildPRBody(PRInfo{TaskID: "t1", Agent: "mock"})
	if strings.Contains(body, "- Issue:") {
		t.Error("issue line present with no issue URL")
	}
	if !strings.Contains(body, "no summary") {
		t.Error("missing empty-summary placeholder")
	}
}

func TestBuildPRBodyTestOutput(t *testing.T) {
	body := BuildPRBody(PRInfo{TaskID: "t1", Agent: "claude", TestOutput: "ok 5 tests"})
	if !strings.Contains(body, "## Test results") || !strings.Contains(body, "ok 5 tests") {
		t.Errorf("test output not rendered:\n%s", body)
	}
}

// ghStub is a fake `gh` binary installed on PATH, exiting with $STUB_EXIT
// (0 = success). It ignores its arguments: CheckAccess's own call-shape
// (gh api repos/{owner}/{repo}) is not under test here, only that its
// result (success vs error) propagates.
const ghStub = `#!/bin/sh
exit "${STUB_EXIT:-0}"
`

// TestCheckAccess covers both outcomes of the underlying gh call: success
// (no error) and failure (the raw gh error returned, unmodified -- it is
// the caller's job, not this method's, to decide what a failure means).
func TestCheckAccess(t *testing.T) {
	cases := []struct {
		name     string
		stubExit string
		wantErr  bool
	}{
		{name: "token can see the repo", stubExit: "0", wantErr: false},
		{name: "gh reports an error (404 or otherwise)", stubExit: "1", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			binDir := t.TempDir()
			if err := os.WriteFile(filepath.Join(binDir, "gh"), []byte(ghStub), 0o755); err != nil {
				t.Fatalf("write gh stub: %v", err)
			}
			t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("STUB_EXIT", tc.stubExit)

			err := New("o", "r", "tok").CheckAccess(context.Background())
			if tc.wantErr && err == nil {
				t.Fatal("CheckAccess() = nil, want an error")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("CheckAccess() = %v, want nil", err)
			}
		})
	}
}
