package service

import (
	"context"
	"time"
)

type AliyunTaskRecord struct {
	ID             int64
	TaskID         string
	UserID         int64
	APIKeyID       int64
	AccountID      int64
	GroupID        *int64
	Model          string
	Status         string
	MeterUnit      *string
	MeterQuantity  *float64
	UsageLogID     *int64
	BilledAt       *time.Time
	RequestHash    *string
	SubmitResponse map[string]any
	FinalResponse  map[string]any
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

type AliyunTaskRepository interface {
	UpsertSubmitted(ctx context.Context, task *AliyunTaskRecord) error
	GetByTaskID(ctx context.Context, taskID string) (*AliyunTaskRecord, error)
	UpdateFinal(ctx context.Context, taskID, status string, meterUnit *string, meterQuantity *float64, finalResponse map[string]any) error
	MarkBilled(ctx context.Context, taskID string, usageLogID *int64) error
}
