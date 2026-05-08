package master

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/allenabraham999/vajra/internal/models"
	"github.com/allenabraham999/vajra/internal/store"
)

// errorMessageMaxBytes caps how much of an error message we persist on a
// failed operation. Operations table errors are surfaced to API clients,
// so unbounded blobs are both a UX and a storage concern.
const errorMessageMaxBytes = 1024

// OperationTracker writes operation rows for audit + status polling. Every
// mutating dispatcher call should be wrappable in a Start/Complete pair.
type OperationTracker struct {
	s store.Store
}

// NewOperationTracker constructs a tracker bound to the given store.
func NewOperationTracker(s store.Store) *OperationTracker {
	return &OperationTracker{s: s}
}

// Start creates an operation row in IN_PROGRESS state and returns its ID.
// The ID is generated from 16 bytes of OS randomness, hex-encoded — we
// don't pull in a UUID dependency just for this.
func (t *OperationTracker) Start(ctx context.Context, accountID, sandboxID string, opType models.OperationType) (string, error) {
	if t == nil || t.s == nil {
		return "", fmt.Errorf("operation tracker: store is nil")
	}
	id, err := randomID()
	if err != nil {
		return "", fmt.Errorf("operation tracker: id: %w", err)
	}
	op := &models.Operation{
		ID:        id,
		AccountID: accountID,
		SandboxID: sandboxID,
		Type:      opType,
		Status:    models.OperationStatusInProgress,
		StartedAt: time.Now().UTC(),
	}
	if err := t.s.Operations().Create(ctx, op); err != nil {
		return "", fmt.Errorf("operation tracker: create: %w", err)
	}
	return id, nil
}

// Complete marks the operation COMPLETED if err == nil, else FAILED with
// the truncated err.Error() (cap at errorMessageMaxBytes).
func (t *OperationTracker) Complete(ctx context.Context, opID string, err error) error {
	if t == nil || t.s == nil {
		return fmt.Errorf("operation tracker: store is nil")
	}
	now := time.Now().UTC()
	status := models.OperationStatusCompleted
	var errMsg *string
	if err != nil {
		status = models.OperationStatusFailed
		s := truncateError(err.Error(), errorMessageMaxBytes)
		errMsg = &s
	}
	if updateErr := t.s.Operations().UpdateStatus(ctx, opID, status, errMsg, &now); updateErr != nil {
		return fmt.Errorf("operation tracker: update %s: %w", opID, updateErr)
	}
	return nil
}

// truncateError returns s shortened to at most n bytes. We trim on a byte
// boundary because operation rows are stored as a CHAR/TEXT and the cap is
// purely a storage guardrail; if the truncation lands inside a multibyte
// rune, the operation history just shows a slightly garbled tail, which
// is acceptable for an error string.
func truncateError(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// randomID returns 16 bytes of crypto-grade randomness as a 32-character
// hex string. Used as the operation primary key.
func randomID() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf[:]), nil
}
