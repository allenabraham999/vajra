// Package models — build.go defines the Build aggregate, a record of an
// async "Dockerfile → Template" job kicked off by POST /v1/templates/build.
// The builder goroutine moves a Build through PENDING → BUILDING →
// COMPLETED|FAILED while the caller polls
// GET /v1/templates/builds/{id} for status.
package models

import "time"

// BuildStatus is the lifecycle state of a custom-image build job.
type BuildStatus string

const (
	BuildStatusPending   BuildStatus = "PENDING"
	BuildStatusBuilding  BuildStatus = "BUILDING"
	BuildStatusCompleted BuildStatus = "COMPLETED"
	BuildStatusFailed    BuildStatus = "FAILED"
)

// Valid reports whether s is a known BuildStatus.
func (s BuildStatus) Valid() bool {
	switch s {
	case BuildStatusPending, BuildStatusBuilding, BuildStatusCompleted, BuildStatusFailed:
		return true
	}
	return false
}

// Build is a single Dockerfile → Template build job. Dockerfile holds
// the raw text the caller uploaded; the builder writes its result back
// onto this row by stamping TemplateID, Status, and CompletedAt.
type Build struct {
	ID           string      `db:"id" json:"id"`
	AccountID    string      `db:"account_id" json:"account_id"`
	TemplateName string      `db:"template_name" json:"template_name"`
	TemplateVer  string      `db:"template_ver" json:"template_version"`
	Status       BuildStatus `db:"status" json:"status"`
	Dockerfile   string      `db:"dockerfile" json:"dockerfile,omitempty"`
	TemplateID   *string     `db:"template_id" json:"template_id,omitempty"`
	Error        *string     `db:"error" json:"error,omitempty"`
	CreatedAt    time.Time   `db:"created_at" json:"created_at"`
	CompletedAt  *time.Time  `db:"completed_at" json:"completed_at,omitempty"`
}
