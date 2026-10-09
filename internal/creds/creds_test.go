package creds

import (
	"os"
	"path/filepath"
	"testing"
)

// write drops a credential file into a temp dir and points
// CREDENTIALS_DIRECTORY at it for the duration of the test
func write(t *testing.T, name, content string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o400); err != nil {
		t.Fatalf("write credential: %v", err)
	}
	t.Setenv("CREDENTIALS_DIRECTORY", dir)
}

func TestGetReadsCredential(t *testing.T) {
	write(t, "gh-token-agent-task", "github_pat_example\n")

	token, ok, err := Get("gh-token-agent-task")
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	if !ok {
		t.Fatal("credentials not found")
	}

	if token != "github_pat_example" {
		t.Fatalf("token = %q (trailing new line not trimmed?)", token)
	}
}

// A missing credential is the public-repo case: not found, not an error.
func TestMissingCredentialIsNotAnError(t *testing.T) {
	write(t, "other", "x")

	_, ok, err := Get("gh-token-agent-task")
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	if ok {
		t.Error("reported a credential that does not exist")
	}
}

// Outside systemd there is no credentials directory at all.
func TestNoCredentialsDirectory(t *testing.T) {
	t.Setenv("CREDENTIALS_DIRECTORY", "")

	_, ok, err := Get("gh-token-agent-task")
	if err != nil || ok {
		t.Errorf("got (ok=%v, err=%v), want (false, nil)", ok, err)
	}
}

// An empty file is a file a real misconfiguration and must not look like "no token".
func TestEmptyCredentialIsAnError(t *testing.T) {
	write(t, "gh-token-agent-task", "\n")

	if _, _, err := Get("gh-token-agent-task"); err == nil {
		t.Fatal("expected an error for an empty credential, got nil")
	}
}

func TestRefCannotEscapeDirectory(t *testing.T) {
	write(t, "gh-token-agent-task", "x")

	for _, ref := range []string{"../../etc/passwd", "sub/token", ""} {
		if _, _, err := Get(ref); err == nil {
			t.Errorf("ref %q was accepted, want rejection", ref)
		}
	}
}

// TestValidateRef covers the ref-validation helper shared by Get and
// `creds set`: valid bare file names, and the invalid forms that must be
// rejected (empty, separators, parent-directory escape).
func TestValidateRef(t *testing.T) {
	cases := []struct {
		ref     string
		wantErr bool
	}{
		{ref: "gh-token-agent-task", wantErr: false},
		{ref: "claude-oauth-token", wantErr: false},
		{ref: "", wantErr: true},
		{ref: "..", wantErr: true},
		{ref: "../etc/passwd", wantErr: true},
		{ref: "sub/token", wantErr: true},
		{ref: `sub\token`, wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.ref, func(t *testing.T) {
			err := ValidateRef(tc.ref)
			if tc.wantErr && err == nil {
				t.Fatalf("ValidateRef(%q) = nil, want error", tc.ref)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("ValidateRef(%q) = %v, want nil", tc.ref, err)
			}
		})
	}
}
