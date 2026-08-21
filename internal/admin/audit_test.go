package admin

import (
	"path/filepath"
	"testing"
)

func TestAuditLog_RecordAndRecent(t *testing.T) {
	log := OpenAuditLog(filepath.Join(t.TempDir(), "audit.json"))

	log.Record(AuditLoginSuccess, "1.2.3.4", "signed in")
	log.Record(AuditLoginFailure, "5.6.7.8", "incorrect password")

	recent := log.Recent(10)
	if len(recent) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(recent))
	}
	// Newest first.
	if recent[0].Type != AuditLoginFailure {
		t.Errorf("expected newest entry first, got %v", recent[0].Type)
	}
	if recent[1].Type != AuditLoginSuccess {
		t.Errorf("expected oldest entry last, got %v", recent[1].Type)
	}
}

func TestAuditLog_RecentRespectsLimit(t *testing.T) {
	log := OpenAuditLog(filepath.Join(t.TempDir(), "audit.json"))
	for i := 0; i < 5; i++ {
		log.Record(AuditLoginSuccess, "1.2.3.4", "signed in")
	}

	recent := log.Recent(2)
	if len(recent) != 2 {
		t.Errorf("expected Recent(2) to return exactly 2 entries, got %d", len(recent))
	}
}

func TestAuditLog_RecentZeroOrNegativeReturnsAll(t *testing.T) {
	log := OpenAuditLog(filepath.Join(t.TempDir(), "audit.json"))
	log.Record(AuditLoginSuccess, "1.2.3.4", "signed in")
	log.Record(AuditLoginSuccess, "1.2.3.4", "signed in")

	if got := len(log.Recent(0)); got != 2 {
		t.Errorf("expected Recent(0) to return all entries, got %d", got)
	}
}

func TestAuditLog_TrimsToMaxEntries(t *testing.T) {
	log := OpenAuditLog(filepath.Join(t.TempDir(), "audit.json"))
	for i := 0; i < maxAuditEntries+50; i++ {
		log.Record(AuditLoginSuccess, "1.2.3.4", "signed in")
	}

	all := log.Recent(0)
	if len(all) != maxAuditEntries {
		t.Errorf("expected the log to be trimmed to %d entries, got %d", maxAuditEntries, len(all))
	}
}

func TestAuditLog_PersistsAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.json")
	log := OpenAuditLog(path)
	log.Record(AuditPasswordChanged, "9.9.9.9", "password changed via web UI")

	reopened := OpenAuditLog(path)
	recent := reopened.Recent(10)
	if len(recent) != 1 {
		t.Fatalf("expected 1 entry to persist across reopen, got %d", len(recent))
	}
	if recent[0].Type != AuditPasswordChanged || recent[0].IP != "9.9.9.9" {
		t.Errorf("unexpected persisted entry: %+v", recent[0])
	}
}

func TestAuditLog_EmptyPathIsInMemoryOnly(t *testing.T) {
	log := OpenAuditLog("")
	log.Record(AuditLoginSuccess, "1.2.3.4", "signed in")

	if len(log.Recent(10)) != 1 {
		t.Error("expected an empty path to still work in-memory")
	}
}

func TestAuditLog_OpenWithNoExistingFileStartsEmpty(t *testing.T) {
	log := OpenAuditLog(filepath.Join(t.TempDir(), "does-not-exist.json"))
	if len(log.Recent(10)) != 0 {
		t.Error("expected a fresh audit log to start empty")
	}
}
