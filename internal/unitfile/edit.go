// Package unitfile edits the systemd unit file's LoadCredential lines
// in place. The unit file is plain text, not YAML, so this is a simple
// line-level splice: every other line is preserved byte-for-byte.
package unitfile

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// secretsHeader marks the start of the unit's LoadCredential block
// (deploy/systemd/agent-taskd.service's "# --- Secrets: ..." comment).
const secretsHeader = "# --- Secrets:"

// systemctlShow runs "systemctl show -p DropInPaths <unitName>". It is a
// package-level var so tests can fake it instead of shelling out for real.
var systemctlShow = func(unitName string) ([]byte, error) {
	return exec.Command("systemctl", "show", "-p", "DropInPaths", unitName).Output()
}

// DropInText returns the concatenated contents of every systemd drop-in
// file that would be merged onto the unit at unitPath, resolved by asking
// systemd for the drop-ins of the unit named by unitPath's basename. It
// returns "" on any failure (systemctl missing, exec error, unit unknown to
// systemd, no DropInPaths output, or an unreadable listed file) — every
// failure mode degrades to "no drop-in content found," i.e. today's
// behavior.
func DropInText(unitPath string) string {
	unitName := filepath.Base(unitPath)
	out, err := systemctlShow(unitName)
	if err != nil {
		return ""
	}

	var paths []string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if after, ok := strings.CutPrefix(line, "DropInPaths="); ok {
			if after == "" {
				continue
			}
			paths = append(paths, strings.Fields(after)...)
		}
	}
	if len(paths) == 0 {
		return ""
	}

	var sb strings.Builder
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			return ""
		}
		sb.Write(data)
	}
	return sb.String()
}

// EffectiveText returns the literal content of unitPath with any systemd
// drop-in content (see DropInText) appended, approximating what systemd
// would actually merge for that unit.
func EffectiveText(unitPath string) (string, error) {
	data, err := os.ReadFile(unitPath)
	if err != nil {
		return "", fmt.Errorf("read unit file %s: %w", unitPath, err)
	}
	text := string(data)

	if dropIn := DropInText(unitPath); dropIn != "" {
		text += "\n" + dropIn
	}
	return text, nil
}

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

	effective, err := EffectiveText(path)
	if err != nil {
		return false, err
	}
	if HasLine(effective, ref) {
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
