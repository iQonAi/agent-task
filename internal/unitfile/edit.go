// Package unitfile edits the systemd unit file's LoadCredential lines
// in place. The unit file is plain text, not YAML, so this is a simple
// line-level splice: every other line is preserved byte-for-byte.
package unitfile

import (
	"fmt"
	"os"
	"strings"
)

// secretsHeader marks the start of the unit's LoadCredential block
// (deploy/systemd/agent-taskd.service's "# --- Secrets: ..." comment).
const secretsHeader = "# --- Secrets:"

// HasLine reports whether unitText contains an active (non-commented)
// LoadCredential= line for ref. A commented-out placeholder line (for a repo
// that has not joined yet) does not count.
func HasLine(unitText, ref string) bool {
	prefix := "LoadCredential=" + ref + ":"
	for _, line := range strings.Split(unitText, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), prefix) {
			return true
		}
	}
	return false
}

// EnsureLoadCredential idempotently adds a "LoadCredential=<ref>:<credPath>"
// line under the unit file's "# --- Secrets:" block, if no active line for
// ref exists yet. It leaves every other line byte-for-byte untouched and
// reports whether it wrote a change.
func EnsureLoadCredential(path, ref, credPath string) (bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return false, fmt.Errorf("read unit file %s: %w", path, err)
	}
	raw := string(data)

	if HasLine(raw, ref) {
		return false, nil
	}

	lines := strings.Split(raw, "\n")
	secretsIdx := -1
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), secretsHeader) {
			secretsIdx = i
			break
		}
	}
	if secretsIdx == -1 {
		return false, fmt.Errorf("unit file %s: no %q block found", path, secretsHeader)
	}

	// Insert at the end of the contiguous (non-blank) block that starts at
	// the header: the last comment or LoadCredential line before the next
	// blank line.
	insertAt := secretsIdx
	for i := secretsIdx + 1; i < len(lines) && strings.TrimSpace(lines[i]) != ""; i++ {
		insertAt = i
	}

	newLine := fmt.Sprintf("LoadCredential=%s:%s", ref, credPath)
	out := make([]string, 0, len(lines)+1)
	out = append(out, lines[:insertAt+1]...)
	out = append(out, newLine)
	out = append(out, lines[insertAt+1:]...)

	if err := os.WriteFile(path, []byte(strings.Join(out, "\n")), 0o644); err != nil {
		return false, fmt.Errorf("write unit file %s: %w", path, err)
	}
	return true, nil
}
