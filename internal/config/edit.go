package config

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// AddRepo appends a new repo entry to the repos: sequence in the config file
// at path. It locates the insertion point via the yaml.Node parse tree, then
// splices the new entry's lines into the original text -- it never
// re-encodes the rest of the document, so every other line (including
// comments and blank lines) is preserved byte-for-byte.
func AddRepo(path string, r Repo) error {
	if r.Name == "" {
		return fmt.Errorf("repo name is required")
	}
	if r.Owner == "" || r.Repo == "" {
		return fmt.Errorf("repo %q: owner and repo are required", r.Name)
	}

	raw, root, err := readConfig(path)
	if err != nil {
		return err
	}

	reposNode, err := findMappingValue(root, "repos")
	if err != nil {
		return fmt.Errorf("config %s: %w", path, err)
	}
	if reposNode.Kind != yaml.SequenceNode {
		return fmt.Errorf("config %s: repos is not a sequence", path)
	}
	for _, item := range reposNode.Content {
		if item.Kind == yaml.MappingNode && repoNameOf(item) == r.Name {
			return fmt.Errorf("repo %q already exists in %s", r.Name, path)
		}
	}

	entry, err := repoEntryLines(r)
	if err != nil {
		return err
	}

	insertAt := reposNode.Line // fallback: right after "repos:" (empty sequence)
	if n := len(reposNode.Content); n > 0 {
		last := reposNode.Content[n-1]
		insertAt = last.Content[len(last.Content)-1].Line
	}

	if err := backupRaw(path, raw); err != nil {
		return err
	}

	lines := strings.Split(raw, "\n")
	lines = spliceInsert(lines, insertAt, entry)
	return os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o600)
}

// RemoveRepo removes the repo entry named name from the repos: sequence in
// the config file at path. Like AddRepo, it splices the matching entry's
// lines out of the original text rather than re-encoding the document, so
// everything else is preserved byte-for-byte. It reports whether a matching
// entry was found and removed.
func RemoveRepo(path string, name string) (bool, error) {
	raw, root, err := readConfig(path)
	if err != nil {
		return false, err
	}

	reposNode, err := findMappingValue(root, "repos")
	if err != nil {
		return false, fmt.Errorf("config %s: %w", path, err)
	}
	if reposNode.Kind != yaml.SequenceNode {
		return false, fmt.Errorf("config %s: repos is not a sequence", path)
	}

	var match *yaml.Node
	for _, item := range reposNode.Content {
		if item.Kind == yaml.MappingNode && repoNameOf(item) == name {
			match = item
			break
		}
	}
	if match == nil {
		return false, nil
	}

	start := match.Content[0].Line
	end := match.Content[len(match.Content)-1].Line

	if err := backupRaw(path, raw); err != nil {
		return false, err
	}

	lines := strings.Split(raw, "\n")
	lines = spliceDelete(lines, start, end)
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o600); err != nil {
		return false, err
	}
	return true, nil
}

// readConfig reads path and parses it into a yaml.Node tree, returning the
// raw text (for splicing) and the root mapping node (for locating keys).
func readConfig(path string) (raw string, root *yaml.Node, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", nil, fmt.Errorf("read config %s: %w", path, err)
	}

	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return "", nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	if len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return "", nil, fmt.Errorf("config %s: expected a top-level mapping", path)
	}
	return string(data), doc.Content[0], nil
}

// backupRaw writes raw to path+".bak" (overwriting any existing backup),
// immediately before AddRepo/RemoveRepo performs its real write.
func backupRaw(path, raw string) error {
	if err := os.WriteFile(path+".bak", []byte(raw), 0o600); err != nil {
		return fmt.Errorf("write backup %s.bak: %w", path, err)
	}
	return nil
}

// findMappingValue returns the value node for key in a mapping node's
// Content, which alternates key, value, key, value, ...
func findMappingValue(mapping *yaml.Node, key string) (*yaml.Node, error) {
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			return mapping.Content[i+1], nil
		}
	}
	return nil, fmt.Errorf("key %q not found", key)
}

// repoNameOf returns the "name" field's scalar value from a repo mapping
// node, or "" if it has none.
func repoNameOf(mapping *yaml.Node) string {
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == "name" {
			return mapping.Content[i+1].Value
		}
	}
	return ""
}

// repoEntryLines renders r as the lines of a `- name: ...` block sequence
// item, in config.example.yaml's 2-space/4-space indent style.
func repoEntryLines(r Repo) ([]string, error) {
	fields := []struct{ k, v string }{
		{"name", r.Name},
		{"owner", r.Owner},
		{"repo", r.Repo},
		{"default_branch", r.DefaultBranch},
		{"token_ref", r.TokenRef},
	}
	lines := make([]string, 0, len(fields))
	for i, f := range fields {
		val, err := scalarize(f.v)
		if err != nil {
			return nil, err
		}
		prefix := "    "
		if i == 0 {
			prefix = "  - "
		}
		lines = append(lines, fmt.Sprintf("%s%s: %s", prefix, f.k, val))
	}
	return lines, nil
}

// scalarize renders v the way yaml.v3 would render it as a plain mapping
// value, quoting it only if needed.
func scalarize(v string) (string, error) {
	b, err := yaml.Marshal(v)
	if err != nil {
		return "", err
	}
	return strings.TrimSuffix(string(b), "\n"), nil
}

// spliceInsert inserts entry as new lines immediately after line n (1-indexed,
// per yaml.Node's Line field) of lines.
func spliceInsert(lines []string, n int, entry []string) []string {
	out := make([]string, 0, len(lines)+len(entry))
	out = append(out, lines[:n]...)
	out = append(out, entry...)
	out = append(out, lines[n:]...)
	return out
}

// spliceDelete removes lines start..end inclusive (1-indexed, per
// yaml.Node's Line field) from lines.
func spliceDelete(lines []string, start, end int) []string {
	out := make([]string, 0, len(lines)-(end-start+1))
	out = append(out, lines[:start-1]...)
	out = append(out, lines[end:]...)
	return out
}
