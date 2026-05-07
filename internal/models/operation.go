package models

import "time"

// OperationType is the kind of asynchronous action being tracked.
type OperationType string

const (
	OperationTypeCreate   OperationType = "CREATE"
	OperationTypeStop     OperationType = "STOP"
	OperationTypeStart    OperationType = "START"
	OperationTypeDestroy  OperationType = "DESTROY"
	OperationTypeSnapshot OperationType = "SNAPSHOT"
	OperationTypeRestore  OperationType = "RESTORE"
	OperationTypeClone    OperationType = "CLONE"
	OperationTypeMigrate  OperationType = "MIGRATE"
	OperationTypeArchive  OperationType = "ARCHIVE"
)

// Valid reports whether t is a known OperationType constant.
func (t OperationType) Valid() bool {
	switch t {
	case OperationTypeCreate, OperationTypeStop, OperationTypeStart,
		OperationTypeDestroy, OperationTypeSnapshot, OperationTypeRestore,
		OperationTypeClone, OperationTypeMigrate, OperationTypeArchive:
		return true
	}
	return false
}

// OperationStatus is the lifecycle status of an operation.
type OperationStatus string

const (
	OperationStatusPending    OperationStatus = "PENDING"
	OperationStatusInProgress OperationStatus = "IN_PROGRESS"
	OperationStatusCompleted  OperationStatus = "COMPLETED"
	OperationStatusFailed     OperationStatus = "FAILED"
)

// Valid reports whether s is a known OperationStatus constant.
func (s OperationStatus) Valid() bool {
	switch s {
	case OperationStatusPending, OperationStatusInProgress,
		OperationStatusCompleted, OperationStatusFailed:
		return true
	}
	return false
}

// Operation tracks an asynchronous action against a sandbox so callers can
// poll progress. CompletedAt and Error are nil while the operation is
// still running or completed without error.
type Operation struct {
	ID          string          `db:"id" json:"id"`
	AccountID   string          `db:"account_id" json:"account_id"`
	SandboxID   string          `db:"sandbox_id" json:"sandbox_id"`
	Type        OperationType   `db:"type" json:"type"`
	Status      OperationStatus `db:"status" json:"status"`
	StartedAt   time.Time       `db:"started_at" json:"started_at"`
	CompletedAt *time.Time      `db:"completed_at" json:"completed_at,omitempty"`
	Error       *string         `db:"error" json:"error,omitempty"`
}
