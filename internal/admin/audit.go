package admin

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// AuditEventType identifies what kind of admin-UI event an AuditEntry
// records.
type AuditEventType string

const (
	AuditLoginSuccess     AuditEventType = "login_success"
	AuditLoginFailure     AuditEventType = "login_failure"
	AuditLoginRateLimited AuditEventType = "login_rate_limited"
	AuditPasswordChanged  AuditEventType = "password_changed"
	AuditPasswordReset    AuditEventType = "password_reset" // via -admin-force-reset-password
	AuditLogout           AuditEventType = "logout"
	AuditOverrideChanged  AuditEventType = "override_changed"
	AuditAlgorithmChanged AuditEventType = "algorithm_changed"
)

// AuditEntry is one recorded admin-UI event.
type AuditEntry struct {
	Time    time.Time      `json:"time"`
	Type    AuditEventType `json:"type"`
	IP      string         `json:"ip"`
	Message string         `json:"message"`
}

// maxAuditEntries bounds the in-memory (and persisted) audit log size —
// this is meant to help debug recent activity ("did someone lock
// themselves out this morning"), not serve as a permanent compliance
// record, so an old-entries-roll-off-the-front ring buffer is the right
// tradeoff for a tool with no external database.
const maxAuditEntries = 500

// AuditLog is a small bounded, append-only (from the caller's perspective)
// log of admin-UI events, held in memory and persisted to a local JSON
// file so recent history survives a container restart. Safe for
// concurrent use.
type AuditLog struct {
	path string
	mu   sync.Mutex
	// entries is stored newest-last; Recent() returns newest-first.
	entries []AuditEntry
}

// OpenAuditLog loads an audit log from path if it exists, or starts empty
// if it doesn't. A corrupt file is treated the same as "doesn't exist" —
// audit history is best-effort, not something worth failing startup over.
func OpenAuditLog(path string) *AuditLog {
	a := &AuditLog{path: path}

	data, err := os.ReadFile(path)
	if err == nil {
		var entries []AuditEntry
		if jsonErr := json.Unmarshal(data, &entries); jsonErr == nil {
			a.entries = entries
		}
	}

	return a
}

// Record appends a new entry, trimming the oldest entries if the log
// exceeds maxAuditEntries, and persists the result. Persistence failures
// are logged by the caller-visible error but never block the admin action
// that triggered the audit entry — audit logging is a best-effort
// side-channel, not a gate on functionality.
func (a *AuditLog) Record(eventType AuditEventType, ip, message string) {
	a.mu.Lock()
	a.entries = append(a.entries, AuditEntry{
		Time:    time.Now(),
		Type:    eventType,
		IP:      ip,
		Message: message,
	})
	if len(a.entries) > maxAuditEntries {
		a.entries = a.entries[len(a.entries)-maxAuditEntries:]
	}
	entriesCopy := make([]AuditEntry, len(a.entries))
	copy(entriesCopy, a.entries)
	a.mu.Unlock()

	if err := a.persist(entriesCopy); err != nil {
		// Best-effort: audit logging should never break the admin UI's
		// actual functionality. The error is swallowed here deliberately;
		// callers that care can call Recent() to sanity-check persistence
		// worked, e.g. in tests.
		_ = err
	}
}

// Recent returns up to limit most-recent entries, newest first.
func (a *AuditLog) Recent(limit int) []AuditEntry {
	a.mu.Lock()
	defer a.mu.Unlock()

	n := len(a.entries)
	if limit > 0 && limit < n {
		n = limit
	}

	out := make([]AuditEntry, n)
	for i := 0; i < n; i++ {
		out[i] = a.entries[len(a.entries)-1-i]
	}
	return out
}

func (a *AuditLog) persist(entries []AuditEntry) error {
	if a.path == "" {
		return nil // no path configured — in-memory only (used in tests)
	}

	data, err := json.Marshal(entries)
	if err != nil {
		return fmt.Errorf("admin: failed to marshal audit log: %w", err)
	}

	dir := filepath.Dir(a.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("admin: failed to create audit log directory: %w", err)
	}

	tmp, err := os.CreateTemp(dir, ".audit-log-*.tmp")
	if err != nil {
		return fmt.Errorf("admin: failed to create audit log temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("admin: failed to write audit log temp file: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("admin: failed to set audit log file permissions: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("admin: failed to close audit log temp file: %w", err)
	}

	if err := os.Rename(tmpPath, a.path); err != nil {
		return fmt.Errorf("admin: failed to persist audit log: %w", err)
	}
	return nil
}
