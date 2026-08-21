// Package admin implements the control plane's web management interface:
// a password-protected dashboard for viewing backend group state, backed
// by a single local JSON file inside the container — no external
// database. On first run it generates a random password and prints it
// once to the process log (captured by `docker logs`); the password can
// be changed from the web UI afterwards.
package admin

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"golang.org/x/crypto/bcrypt"
)

// state is the on-disk shape of the admin store's JSON file.
type state struct {
	// PasswordHash is a bcrypt hash of the current admin password.
	PasswordHash string `json:"passwordHash"`
	// SessionSecret signs session cookies. Generated once and reused for
	// the container's lifetime; rotating it (e.g. on password reset)
	// invalidates all existing sessions.
	SessionSecret string `json:"sessionSecret"`
}

// Store manages the admin password and session secret, persisted to a
// single local JSON file. Safe for concurrent use.
type Store struct {
	path string
	mu   sync.RWMutex
	s    state
}

// GeneratedPassword is returned by Open when a new password was generated
// (i.e. this is the first run, or the store file didn't exist/was
// invalid). Callers should log this exactly once and never persist it in
// plaintext anywhere.
type GeneratedPassword struct {
	Password string
}

// Open loads the admin store from path, creating it with a freshly
// generated random password if it doesn't exist yet. Returns the store and,
// if a new password was generated, its plaintext value (nil otherwise).
func Open(path string) (*Store, *GeneratedPassword, error) {
	st := &Store{path: path}

	data, err := os.ReadFile(path)
	if err == nil {
		var loaded state
		if jsonErr := json.Unmarshal(data, &loaded); jsonErr == nil && loaded.PasswordHash != "" && loaded.SessionSecret != "" {
			st.s = loaded
			return st, nil, nil
		}
		// Fall through to regenerate if the file is empty/corrupt — better
		// to recover with a fresh password than to fail to start.
	} else if !os.IsNotExist(err) {
		return nil, nil, fmt.Errorf("admin: failed to read store file %s: %w", path, err)
	}

	password, err := generateRandomPassword()
	if err != nil {
		return nil, nil, fmt.Errorf("admin: failed to generate initial password: %w", err)
	}
	sessionSecret, err := generateRandomSecret(32)
	if err != nil {
		return nil, nil, fmt.Errorf("admin: failed to generate session secret: %w", err)
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return nil, nil, fmt.Errorf("admin: failed to hash generated password: %w", err)
	}

	st.s = state{PasswordHash: string(hash), SessionSecret: sessionSecret}
	if err := st.persist(); err != nil {
		return nil, nil, err
	}

	return st, &GeneratedPassword{Password: password}, nil
}

// VerifyPassword reports whether the given plaintext password matches the
// currently stored hash.
func (s *Store) VerifyPassword(password string) bool {
	s.mu.RLock()
	hash := s.s.PasswordHash
	s.mu.RUnlock()
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}

// SessionSecret returns the current session-signing secret.
func (s *Store) SessionSecret() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.s.SessionSecret
}

// ResetToRandomPassword generates a fresh random password, stores its
// hash (replacing whatever was there), rotates the session secret, and
// returns the new plaintext password. Used for the -admin-force-reset-password
// recovery flag.
func (s *Store) ResetToRandomPassword() (string, error) {
	password, err := generateRandomPassword()
	if err != nil {
		return "", fmt.Errorf("admin: failed to generate password: %w", err)
	}
	if err := s.SetPassword(password); err != nil {
		return "", err
	}
	return password, nil
}

// SetPassword replaces the stored password hash with a hash of newPassword
// and rotates the session secret, invalidating all existing sessions (so
// a stolen session cookie doesn't survive a password change).
func (s *Store) SetPassword(newPassword string) error {
	hash, err := bcrypt.GenerateFromPassword([]byte(newPassword), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("admin: failed to hash new password: %w", err)
	}
	newSecret, err := generateRandomSecret(32)
	if err != nil {
		return fmt.Errorf("admin: failed to generate new session secret: %w", err)
	}

	s.mu.Lock()
	s.s.PasswordHash = string(hash)
	s.s.SessionSecret = newSecret
	s.mu.Unlock()

	return s.persist()
}

// persist writes the current state to disk atomically (write to a temp
// file, then rename), so a crash mid-write can't corrupt the store.
func (s *Store) persist() error {
	s.mu.RLock()
	data, err := json.MarshalIndent(s.s, "", "  ")
	s.mu.RUnlock()
	if err != nil {
		return fmt.Errorf("admin: failed to marshal store state: %w", err)
	}

	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("admin: failed to create store directory %s: %w", dir, err)
	}

	tmp, err := os.CreateTemp(dir, ".admin-store-*.tmp")
	if err != nil {
		return fmt.Errorf("admin: failed to create temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() {
		// Removing the temp file after a successful rename below is
		// expected to fail (nothing left to remove) — that's fine, ignore
		// it. If the rename failed, this is a genuine best-effort cleanup.
		_ = os.Remove(tmpPath)
	}()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("admin: failed to write temp file: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("admin: failed to set permissions on temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("admin: failed to close temp file: %w", err)
	}

	if err := os.Rename(tmpPath, s.path); err != nil {
		return fmt.Errorf("admin: failed to persist store file: %w", err)
	}
	return nil
}

// generateRandomPassword produces a URL-safe, human-typeable random
// password with enough entropy for a locally-run admin credential (24
// random bytes -> 32 base64url characters, no padding).
func generateRandomPassword() (string, error) {
	return generateRandomSecret(24)
}

func generateRandomSecret(numBytes int) (string, error) {
	buf := make([]byte, numBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
