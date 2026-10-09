package main

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iQonAi/agent-task/internal/config"
	"github.com/iQonAi/agent-task/internal/store"
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

// unitFixture is a minimal stand-in for deploy/systemd/agent-taskd.service's
// Secrets block, used by the creds set/list tests below.
const unitFixture = `[Service]
Type=notify

# --- Secrets: systemd LoadCredential (root-owned 0600 source files) ---
LoadCredential=gh-token-agent-task:/etc/agent-task/credentials/gh-token-agent-task

Restart=on-failure
`

// withCapturedStdout redirects os.Stdout to a pipe for the duration of fn and
// returns everything written to it. Used to assert, grep-style, that command
// output never contains a secret value.
func withCapturedStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stdout = w
	fn()
	os.Stdout = old
	w.Close()
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read captured stdout: %v", err)
	}
	return string(out)
}

// TestRunCredsSet_RejectsBadRef covers the hard security requirement: ref
// must be a bare file name, and the secret can only come from stdin or
// --from-file (there is no value/positional flag to reject in the first
// place -- the flag set simply has none).
func TestRunCredsSet_RejectsBadRef(t *testing.T) {
	for _, ref := range []string{"../escape", "sub/token", `sub\token`, ".."} {
		t.Run(ref, func(t *testing.T) {
			err := runCredsSet([]string{
				"--credentials-dir", t.TempDir(),
				"--unit-file", writeUnitFixture(t),
				ref,
			})
			if err == nil {
				t.Fatalf("runCredsSet(%q) = nil, want an error", ref)
			}
		})
	}
}

// writeUnitFixture drops unitFixture into a temp file and returns its path.
func writeUnitFixture(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "agent-taskd.service")
	if err := os.WriteFile(path, []byte(unitFixture), 0o644); err != nil {
		t.Fatalf("write unit fixture: %v", err)
	}
	return path
}

// TestRunCredsSet_WritesFileAndUnitLine covers the happy path end to end:
// the secret lands in the credentials dir at 0600 with the trailing newline
// trimmed, and the unit file gains a LoadCredential line for the ref. The
// daemon socket points nowhere, so the in-flight check treats it as
// unreachable and the command proceeds to (fail to) restart without
// returning an error -- the credential write already succeeded.
func TestRunCredsSet_WritesFileAndUnitLine(t *testing.T) {
	credDir := t.TempDir()
	unitPath := writeUnitFixture(t)
	const secret = "s3cr3t-value\n"

	stdin := os.Stdin
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	if _, err := w.WriteString(secret); err != nil {
		t.Fatalf("write to stdin pipe: %v", err)
	}
	w.Close()
	os.Stdin = r
	defer func() { os.Stdin = stdin }()

	out := withCapturedStdout(t, func() {
		err = runCredsSet([]string{
			"--credentials-dir", credDir,
			"--unit-file", unitPath,
			"--socket", filepath.Join(t.TempDir(), "no-such.sock"),
			"new-ref",
		})
	})
	if err != nil {
		t.Fatalf("runCredsSet: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(credDir, "new-ref"))
	if err != nil {
		t.Fatalf("read written credential: %v", err)
	}
	if string(got) != "s3cr3t-value" {
		t.Fatalf("credential content = %q, want %q (trailing newline not trimmed?)", got, "s3cr3t-value")
	}
	info, err := os.Stat(filepath.Join(credDir, "new-ref"))
	if err != nil {
		t.Fatalf("stat written credential: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("credential file mode = %o, want 0600", mode)
	}

	unitData, err := os.ReadFile(unitPath)
	if err != nil {
		t.Fatalf("read unit file: %v", err)
	}
	if !strings.Contains(string(unitData), "LoadCredential=new-ref:"+filepath.Join(credDir, "new-ref")) {
		t.Fatalf("unit file missing new LoadCredential line:\n%s", unitData)
	}

	if strings.Contains(out, "s3cr3t-value") {
		t.Fatalf("command output contains the literal secret value:\n%s", out)
	}
}

// TestRunCredsSet_IdempotentSecondRun covers the no-op case: running creds
// set again with the same secret must not rewrite the credential file or
// duplicate the unit line.
func TestRunCredsSet_IdempotentSecondRun(t *testing.T) {
	credDir := t.TempDir()
	unitPath := writeUnitFixture(t)
	const secret = "same-secret\n"

	run := func() error {
		stdin := os.Stdin
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatalf("os.Pipe: %v", err)
		}
		if _, err := w.WriteString(secret); err != nil {
			t.Fatalf("write to stdin pipe: %v", err)
		}
		w.Close()
		os.Stdin = r
		defer func() { os.Stdin = stdin }()

		return runCredsSet([]string{
			"--credentials-dir", credDir,
			"--unit-file", unitPath,
			"--socket", filepath.Join(t.TempDir(), "no-such.sock"),
			"idempotent-ref",
		})
	}

	withCapturedStdout(t, func() {
		if err := run(); err != nil {
			t.Fatalf("first runCredsSet: %v", err)
		}
	})
	afterFirst, err := os.ReadFile(unitPath)
	if err != nil {
		t.Fatalf("read unit file: %v", err)
	}

	out := withCapturedStdout(t, func() {
		if err := run(); err != nil {
			t.Fatalf("second runCredsSet: %v", err)
		}
	})
	afterSecond, err := os.ReadFile(unitPath)
	if err != nil {
		t.Fatalf("read unit file: %v", err)
	}
	if string(afterFirst) != string(afterSecond) {
		t.Fatalf("second run changed the unit file.\nfirst:\n%s\nsecond:\n%s", afterFirst, afterSecond)
	}
	if !strings.Contains(out, "no change") {
		t.Fatalf("second run output = %q, want a no-change message", out)
	}
}

// TestRunCredsSet_EnforcesModeOnRewrite covers finding 2: os.WriteFile only
// applies a mode when it creates a file, so a credential file that already
// existed at a looser mode (e.g. from before this command existed) must be
// brought back to 0600 explicitly, regardless of whether its content also
// changed.
func TestRunCredsSet_EnforcesModeOnRewrite(t *testing.T) {
	credDir := t.TempDir()
	unitPath := writeUnitFixture(t)
	const secret = "same-secret\n"

	if err := os.WriteFile(filepath.Join(credDir, "loose-mode-ref"), []byte("same-secret"), 0o644); err != nil {
		t.Fatalf("pre-create credential at 0644: %v", err)
	}

	stdin := os.Stdin
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	if _, err := w.WriteString(secret); err != nil {
		t.Fatalf("write to stdin pipe: %v", err)
	}
	w.Close()
	os.Stdin = r
	defer func() { os.Stdin = stdin }()

	withCapturedStdout(t, func() {
		if err := runCredsSet([]string{
			"--credentials-dir", credDir,
			"--unit-file", unitPath,
			"--socket", filepath.Join(t.TempDir(), "no-such.sock"),
			"loose-mode-ref",
		}); err != nil {
			t.Fatalf("runCredsSet: %v", err)
		}
	})

	info, err := os.Stat(filepath.Join(credDir, "loose-mode-ref"))
	if err != nil {
		t.Fatalf("stat credential: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("credential file mode = %o, want 0600 (pre-existing 0644 not corrected)", mode)
	}
}

// fakeTaskLister is a stub taskLister for exercising the in-flight-task
// check without a real daemon socket.
type fakeTaskLister struct {
	tasks []store.Task
	err   error
}

func (f fakeTaskLister) Tasks() ([]store.Task, error) {
	return f.tasks, f.err
}

// TestInFlightTaskIDs covers the design note's safety check: non-terminal
// tasks are reported as in flight, terminal ones are not, and a Tasks()
// error is returned to the caller rather than swallowed -- it is
// restartBlockReason's job, not this function's, to decide what an error
// means.
func TestInFlightTaskIDs(t *testing.T) {
	cases := []struct {
		name    string
		client  fakeTaskLister
		want    []string
		wantErr bool
	}{
		{
			name:    "a Tasks() error is returned, not swallowed",
			client:  fakeTaskLister{err: fmt.Errorf("decode /v1/tasks response: boom")},
			wantErr: true,
		},
		{
			name: "running and created tasks are in flight",
			client: fakeTaskLister{tasks: []store.Task{
				{ID: "t1", State: store.StateRunning},
				{ID: "t2", State: store.StateCreated},
				{ID: "t3", State: store.StateCompleted},
			}},
			want: []string{"t1", "t2"},
		},
		{
			name: "only terminal tasks is nothing in flight",
			client: fakeTaskLister{tasks: []store.Task{
				{ID: "t1", State: store.StateCompleted},
				{ID: "t2", State: store.StateFailed},
				{ID: "t3", State: store.StateCancelled},
			}},
			want: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := inFlightTaskIDs(tc.client)
			if tc.wantErr && err == nil {
				t.Fatalf("inFlightTaskIDs() error = nil, want an error")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("inFlightTaskIDs() error = %v, want nil", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("inFlightTaskIDs() = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("inFlightTaskIDs() = %v, want %v", got, tc.want)
				}
			}
		})
	}
}

// fakeTimeoutErr mimics an http.Client timeout error: it implements
// net.Error with Timeout() true, but is not a *net.OpError, so it must not
// be mistaken for "the daemon isn't listening at all."
type fakeTimeoutErr struct{}

func (fakeTimeoutErr) Error() string   { return "context deadline exceeded" }
func (fakeTimeoutErr) Timeout() bool   { return true }
func (fakeTimeoutErr) Temporary() bool { return true }

// TestDaemonUnreachable covers the critical distinction from finding 3: a
// dial failure (connection refused / no such socket) means nothing is
// listening, which is safe; a timeout or any other error does not, because
// it can mean the daemon is up but hung.
func TestDaemonUnreachable(t *testing.T) {
	dialErr := &net.OpError{Op: "dial", Net: "unix", Err: fmt.Errorf("connection refused")}
	readErr := &net.OpError{Op: "read", Net: "unix", Err: fmt.Errorf("broken pipe")}

	cases := []struct {
		name string
		err  error
		want bool
	}{
		{name: "dial failure is unreachable", err: dialErr, want: true},
		{name: "dial failure wrapped by fmt.Errorf is still unreachable", err: fmt.Errorf("connect to daemon: %w", dialErr), want: true},
		{name: "a non-dial OpError is not unreachable", err: readErr, want: false},
		{name: "a timeout is not unreachable", err: fakeTimeoutErr{}, want: false},
		{name: "a generic error is not unreachable", err: fmt.Errorf("decode response: boom"), want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := daemonUnreachable(tc.err); got != tc.want {
				t.Fatalf("daemonUnreachable(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestRestartBlockReason covers the --force gating end to end: running
// tasks block a restart unless --force is set; a daemon that isn't
// listening at all never blocks; but an inconclusive check (e.g. a
// timeout) blocks too, same as running tasks, per finding 3.
func TestRestartBlockReason(t *testing.T) {
	dialErr := &net.OpError{Op: "dial", Net: "unix", Err: fmt.Errorf("connection refused")}

	cases := []struct {
		name        string
		running     []string
		tasksErr    error
		wantBlocked bool
	}{
		{name: "nothing running, no error: proceed", running: nil, tasksErr: nil, wantBlocked: false},
		{name: "tasks running: blocked", running: []string{"t1"}, tasksErr: nil, wantBlocked: true},
		{name: "daemon not listening at all: proceed", running: nil, tasksErr: dialErr, wantBlocked: false},
		{name: "inconclusive error (e.g. timeout): blocked", running: nil, tasksErr: fakeTimeoutErr{}, wantBlocked: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reason := restartBlockReason(tc.running, tc.tasksErr)
			if blocked := reason != ""; blocked != tc.wantBlocked {
				t.Fatalf("restartBlockReason(%v, %v) = %q (blocked=%v), want blocked=%v",
					tc.running, tc.tasksErr, reason, blocked, tc.wantBlocked)
			}
		})
	}
}

// TestDefaultUnitPath guards against finding 1's regression: this command
// must edit the unit file systemd actually loads, not the repo's versioned
// reference copy at deploy/systemd/agent-taskd.service.
func TestDefaultUnitPath(t *testing.T) {
	const want = "/etc/systemd/system/agent-taskd.service"
	if defaultUnitPath != want {
		t.Fatalf("defaultUnitPath = %q, want %q (the installed unit, not the repo's reference copy)", defaultUnitPath, want)
	}
}

// TestCredStatus covers creds list's cross-reference logic: a ref can be
// fully wired, missing its file, or missing its unit line.
func TestCredStatus(t *testing.T) {
	credDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(credDir, "has-file-only"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write fixture credential: %v", err)
	}
	if err := os.WriteFile(filepath.Join(credDir, "fully-wired"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write fixture credential: %v", err)
	}
	unitText := "LoadCredential=fully-wired:/etc/agent-task/credentials/fully-wired\n" +
		"LoadCredential=has-line-only:/etc/agent-task/credentials/has-line-only\n"

	cases := []struct {
		ref  string
		want string
	}{
		{ref: "fully-wired", want: "ok"},
		{ref: "has-file-only", want: "missing unit line"},
		{ref: "has-line-only", want: "missing file"},
		{ref: "neither", want: "missing file"},
	}
	for _, tc := range cases {
		t.Run(tc.ref, func(t *testing.T) {
			if got := credStatus(credDir, unitText, tc.ref); got != tc.want {
				t.Fatalf("credStatus(%q) = %q, want %q", tc.ref, got, tc.want)
			}
		})
	}
}

// TestConfigTokenRefs covers ref collection from config.yaml: repo refs
// first (in config order), then agent refs (sorted by name), deduplicated.
func TestConfigTokenRefs(t *testing.T) {
	cfg := &config.Config{
		Repos: []config.Repo{
			{Name: "a", TokenRef: "gh-token-a"},
			{Name: "b", TokenRef: "gh-token-b"},
		},
		Agents: map[string]config.AgentConfig{
			"zeta":  {TokenRef: "gh-token-a"}, // duplicate of a repo ref
			"alpha": {TokenRef: "claude-oauth-token"},
		},
	}
	want := []string{"gh-token-a", "gh-token-b", "claude-oauth-token"}
	got := configTokenRefs(cfg)
	if len(got) != len(want) {
		t.Fatalf("configTokenRefs() = %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("configTokenRefs() = %v, want %v", got, want)
		}
	}
}

// TestRunCredsList_CrossReference runs the full command against a config
// file, a credentials dir, and a unit file, and asserts the printed output
// reflects each ref's status with no secret value involved anywhere.
func TestRunCredsList_CrossReference(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	const configContent = "repos:\n" +
		"  - {name: a, owner: o, repo: r, token_ref: fully-wired}\n" +
		"  - {name: b, owner: o, repo: r2, token_ref: has-file-only}\n"
	if err := os.WriteFile(configPath, []byte(configContent), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	credDir := t.TempDir()
	for _, ref := range []string{"fully-wired", "has-file-only"} {
		if err := os.WriteFile(filepath.Join(credDir, ref), []byte("x"), 0o600); err != nil {
			t.Fatalf("write credential %q: %v", ref, err)
		}
	}

	unitPath := filepath.Join(t.TempDir(), "agent-taskd.service")
	const unitContent = "LoadCredential=fully-wired:/etc/agent-task/credentials/fully-wired\n"
	if err := os.WriteFile(unitPath, []byte(unitContent), 0o644); err != nil {
		t.Fatalf("write unit file: %v", err)
	}

	out := withCapturedStdout(t, func() {
		if err := runCredsList([]string{
			"--config", configPath,
			"--credentials-dir", credDir,
			"--unit-file", unitPath,
		}); err != nil {
			t.Fatalf("runCredsList: %v", err)
		}
	})

	if !strings.Contains(out, "fully-wired") || !strings.Contains(out, "ok") {
		t.Fatalf("output missing fully-wired/ok:\n%s", out)
	}
	if !strings.Contains(out, "has-file-only") || !strings.Contains(out, "missing unit line") {
		t.Fatalf("output missing has-file-only/missing unit line:\n%s", out)
	}
}

// TestNoSecretLeakage is a grep-style assertion that no command output
// anywhere contains a literal secret value, across every creds set path
// exercised in this file.
func TestNoSecretLeakage(t *testing.T) {
	const secret = "do-not-leak-this-98213"
	credDir := t.TempDir()
	unitPath := writeUnitFixture(t)

	stdin := os.Stdin
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	if _, err := w.WriteString(secret + "\n"); err != nil {
		t.Fatalf("write to stdin pipe: %v", err)
	}
	w.Close()
	os.Stdin = r
	defer func() { os.Stdin = stdin }()

	var runErr error
	out := withCapturedStdout(t, func() {
		runErr = runCredsSet([]string{
			"--credentials-dir", credDir,
			"--unit-file", unitPath,
			"--socket", filepath.Join(t.TempDir(), "no-such.sock"),
			"leak-check-ref",
		})
	})
	if runErr != nil {
		if strings.Contains(runErr.Error(), secret) {
			t.Fatalf("error message contains the literal secret: %v", runErr)
		}
		t.Fatalf("runCredsSet: %v", runErr)
	}
	if strings.Contains(out, secret) {
		t.Fatalf("stdout contains the literal secret:\n%s", out)
	}

	data, err := os.ReadFile(filepath.Join(credDir, "leak-check-ref"))
	if err != nil {
		t.Fatalf("read written credential: %v", err)
	}
	if !bytes.Equal(data, []byte(secret)) {
		t.Fatalf("written credential = %q, want %q", data, secret)
	}

	unitData, err := os.ReadFile(unitPath)
	if err != nil {
		t.Fatalf("read unit file: %v", err)
	}
	if strings.Contains(string(unitData), secret) {
		t.Fatalf("unit file contains the literal secret:\n%s", unitData)
	}
}
