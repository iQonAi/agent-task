// Package creds resolves secrets delivered by systemd LoadCredential.
//
// Secrets exist only as 0400 files in $CREDENTIALS_DIRECTORY, owned by the
// service user, for the lifetime of the process (D3: the token is host-only and
// never enters a container). Nothing here is ever logged: a token that reaches
// the journal is a leaked token.
package creds

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ValidateRef checks that ref is safe to use as a bare file name within a
// credentials directory. ref comes from config.yaml or an operator-typed CLI
// argument, so it is operator-controlled rather than hostile -- but it names
// a file, and a name containing a separator would reach outside the
// credentials directory. Refuse rather than resolve.
func ValidateRef(ref string) error {
	if ref == "" || strings.ContainsAny(ref, `/\`) || ref == ".." {
		return fmt.Errorf("invalid credentials ref %q: must be a bare file name", ref)
	}
	return nil
}

// Get returns the credential name ref. The boolean reports whether one was
// found; a missing credeintal is not an error, because a public repo needs no
// token and the daemon also runs outside systemd during development.
func Get(ref string) (string, bool, error) {
	dir := os.Getenv("CREDENTIALS_DIRECTORY")
	if dir == "" {
		return "", false, nil // not running under systemd
	}

	if err := ValidateRef(ref); err != nil {
		return "", false, err
	}

	data, err := os.ReadFile(filepath.Join(dir, ref))
	if err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		// Note the path is safe to report; the contents never are.
		return "", false, fmt.Errorf("read credentials %q: %w", ref, err)
	}

	// Trailing newlines are near-universal in secret files and break auth in
	// ways that are miserable to debug.
	token := strings.TrimSpace(string(data))
	if token == "" {
		return "", false, fmt.Errorf("credential %q is empty", ref)
	}
	return token, true, nil
}
