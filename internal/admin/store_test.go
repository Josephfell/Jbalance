package admin

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOpen_GeneratesPasswordOnFirstRun(t *testing.T) {
	path := filepath.Join(t.TempDir(), "admin.json")

	store, generated, err := Open(path)
	if err != nil {
		t.Fatalf("Open() error: %v", err)
	}
	if generated == nil {
		t.Fatal("expected a generated password on first run, got nil")
	}
	if generated.Password == "" {
		t.Error("expected a non-empty generated password")
	}
	if !store.VerifyPassword(generated.Password) {
		t.Error("expected the generated password to verify against the stored hash")
	}
}

func TestOpen_LoadsExistingStoreWithoutRegenerating(t *testing.T) {
	path := filepath.Join(t.TempDir(), "admin.json")

	_, generated1, err := Open(path)
	if err != nil {
		t.Fatalf("Open() error: %v", err)
	}

	store2, generated2, err := Open(path)
	if err != nil {
		t.Fatalf("second Open() error: %v", err)
	}
	if generated2 != nil {
		t.Error("expected no new password to be generated when a valid store already exists")
	}
	if !store2.VerifyPassword(generated1.Password) {
		t.Error("expected the original password to still verify after reopening the store")
	}
}

func TestOpen_RegeneratesOnCorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "admin.json")
	if err := os.WriteFile(path, []byte("not valid json"), 0o600); err != nil {
		t.Fatalf("failed to write corrupt file: %v", err)
	}

	_, generated, err := Open(path)
	if err != nil {
		t.Fatalf("Open() error: %v", err)
	}
	if generated == nil {
		t.Error("expected a new password to be generated for a corrupt store file")
	}
}

func TestSetPassword_ChangesVerification(t *testing.T) {
	path := filepath.Join(t.TempDir(), "admin.json")
	store, generated, err := Open(path)
	if err != nil {
		t.Fatalf("Open() error: %v", err)
	}

	if err := store.SetPassword("a-brand-new-password-123"); err != nil {
		t.Fatalf("SetPassword() error: %v", err)
	}

	if store.VerifyPassword(generated.Password) {
		t.Error("expected the old password to no longer verify after SetPassword")
	}
	if !store.VerifyPassword("a-brand-new-password-123") {
		t.Error("expected the new password to verify")
	}
}

func TestSetPassword_RotatesSessionSecret(t *testing.T) {
	path := filepath.Join(t.TempDir(), "admin.json")
	store, _, err := Open(path)
	if err != nil {
		t.Fatalf("Open() error: %v", err)
	}

	before := store.SessionSecret()
	if err := store.SetPassword("a-brand-new-password-123"); err != nil {
		t.Fatalf("SetPassword() error: %v", err)
	}
	after := store.SessionSecret()

	if before == after {
		t.Error("expected the session secret to rotate after SetPassword, but it did not change")
	}
}

func TestSetPassword_PersistsAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "admin.json")
	store, _, err := Open(path)
	if err != nil {
		t.Fatalf("Open() error: %v", err)
	}
	if err := store.SetPassword("a-brand-new-password-123"); err != nil {
		t.Fatalf("SetPassword() error: %v", err)
	}

	reopened, generated, err := Open(path)
	if err != nil {
		t.Fatalf("reopen Open() error: %v", err)
	}
	if generated != nil {
		t.Error("expected no regeneration on reopen after a normal SetPassword")
	}
	if !reopened.VerifyPassword("a-brand-new-password-123") {
		t.Error("expected the changed password to persist across reopening the store")
	}
}

func TestResetToRandomPassword(t *testing.T) {
	path := filepath.Join(t.TempDir(), "admin.json")
	store, generated, err := Open(path)
	if err != nil {
		t.Fatalf("Open() error: %v", err)
	}

	newPassword, err := store.ResetToRandomPassword()
	if err != nil {
		t.Fatalf("ResetToRandomPassword() error: %v", err)
	}
	if newPassword == generated.Password {
		t.Error("expected the reset password to differ from the original")
	}
	if !store.VerifyPassword(newPassword) {
		t.Error("expected the newly reset password to verify")
	}
	if store.VerifyPassword(generated.Password) {
		t.Error("expected the original password to no longer verify after reset")
	}
}

func TestVerifyPassword_RejectsWrongPassword(t *testing.T) {
	path := filepath.Join(t.TempDir(), "admin.json")
	store, _, err := Open(path)
	if err != nil {
		t.Fatalf("Open() error: %v", err)
	}
	if store.VerifyPassword("definitely-not-the-right-password") {
		t.Error("expected an incorrect password to fail verification")
	}
}
