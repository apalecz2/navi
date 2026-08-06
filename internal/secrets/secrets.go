// Package secrets generates and persists the credentials this deployment owns
// rather than borrows.
//
// The distinction is the whole point. The Telegram bot token and the webhook
// secret are issued by someone else and arrive through the environment; they
// are config. A calendar path token is minted here, has no issuer, and would
// otherwise be tempting to give a default value — which is the failure that
// survives being copied to a second machine, because a default signing key is
// indistinguishable from no signing key at all (D10, D9).
//
// Nothing calls Ensure yet. The first caller is the calendar path token in P6,
// and this exists now because a rule about how secrets come into existence,
// adopted after the first secret has shipped, is a rule with an exception
// written for the first secret.
package secrets

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Bytes of entropy per generated secret. 32 bytes is 256 bits, which is beyond
// argument for a value that appears in a URL path and is never rotated on a
// schedule.
const entropyBytes = 32

// Ensure returns the secret stored at dir/name, generating and persisting one
// on first run when the file is absent.
//
// The generated value is URL-safe and unpadded, because the known use is a
// path segment. The file is written at 0600 through a temporary file and a
// rename, so a crash between create and write cannot leave a half-written
// secret that reads as a valid short one.
func Ensure(dir, name string) (string, error) {
	if name == "" || strings.ContainsAny(name, `/\`) {
		return "", fmt.Errorf("secrets: %q is not a valid secret name", name)
	}
	path := filepath.Join(dir, name)

	existing, err := os.ReadFile(path)
	switch {
	case err == nil:
		value := strings.TrimSpace(string(existing))
		if value == "" {
			return "", fmt.Errorf("secrets: %s is empty; delete it to have one generated", path)
		}
		return value, nil
	case !errors.Is(err, fs.ErrNotExist):
		return "", fmt.Errorf("secrets: read %s: %w", path, err)
	}

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("secrets: create %s: %w", dir, err)
	}

	buf := make([]byte, entropyBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("secrets: generate %s: %w", name, err)
	}
	value := base64.RawURLEncoding.EncodeToString(buf)

	if err := writeAtomic(path, value); err != nil {
		return "", err
	}
	return value, nil
}

// writeAtomic creates the file in its final directory and renames it into
// place, so a reader never sees a partial secret.
func writeAtomic(path, value string) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*")
	if err != nil {
		return fmt.Errorf("secrets: create temporary file for %s: %w", path, err)
	}
	tmpName := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName) // no-op once the rename has succeeded
	}()

	if err := tmp.Chmod(0o600); err != nil {
		return fmt.Errorf("secrets: chmod %s: %w", tmpName, err)
	}
	if _, err := tmp.WriteString(value + "\n"); err != nil {
		return fmt.Errorf("secrets: write %s: %w", tmpName, err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("secrets: sync %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("secrets: close %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("secrets: install %s: %w", path, err)
	}
	return nil
}
