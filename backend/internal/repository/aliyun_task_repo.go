package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

type aliyunTaskRepository struct {
	db *sql.DB
}

func NewAliyunTaskRepository(db *sql.DB) service.AliyunTaskRepository {
	return &aliyunTaskRepository{db: db}
}

func (r *aliyunTaskRepository) UpsertSubmitted(ctx context.Context, task *service.AliyunTaskRecord) error {
	if r == nil || r.db == nil {
		return fmt.Errorf("aliyun task repository is not configured")
	}
	if task == nil || strings.TrimSpace(task.TaskID) == "" {
		return fmt.Errorf("task_id is required")
	}
	submitPayload := nullAnyMap(task.SubmitResponse)
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO aliyun_tasks (
			task_id, user_id, api_key_id, account_id, group_id, model, status,
			meter_unit, meter_quantity, request_hash, submit_response, updated_at
		)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,NOW())
		ON CONFLICT (task_id) DO UPDATE SET
			user_id = EXCLUDED.user_id,
			api_key_id = EXCLUDED.api_key_id,
			account_id = EXCLUDED.account_id,
			group_id = EXCLUDED.group_id,
			model = EXCLUDED.model,
			status = EXCLUDED.status,
			meter_unit = EXCLUDED.meter_unit,
			meter_quantity = EXCLUDED.meter_quantity,
			request_hash = EXCLUDED.request_hash,
			submit_response = EXCLUDED.submit_response,
			updated_at = NOW()
	`, task.TaskID, task.UserID, task.APIKeyID, task.AccountID, task.GroupID, task.Model, task.Status,
		task.MeterUnit, task.MeterQuantity, task.RequestHash, submitPayload)
	if err != nil {
		return fmt.Errorf("upsert aliyun task: %w", err)
	}
	return nil
}

func (r *aliyunTaskRepository) GetByTaskID(ctx context.Context, taskID string) (*service.AliyunTaskRecord, error) {
	if r == nil || r.db == nil {
		return nil, fmt.Errorf("aliyun task repository is not configured")
	}
	var (
		task              service.AliyunTaskRecord
		groupID           sql.NullInt64
		meterUnit         sql.NullString
		meterQuantity     sql.NullFloat64
		usageLogID        sql.NullInt64
		billedAt          sql.NullTime
		requestHash       sql.NullString
		submitResponseRaw sql.NullString
		finalResponseRaw  sql.NullString
	)
	err := r.db.QueryRowContext(ctx, `
		SELECT id, task_id, user_id, api_key_id, account_id, group_id, model, status,
		       meter_unit, meter_quantity, usage_log_id, billed_at, request_hash,
		       submit_response::text, final_response::text, created_at, updated_at
		FROM aliyun_tasks
		WHERE task_id = $1
	`, strings.TrimSpace(taskID)).Scan(
		&task.ID, &task.TaskID, &task.UserID, &task.APIKeyID, &task.AccountID, &groupID,
		&task.Model, &task.Status, &meterUnit, &meterQuantity, &usageLogID, &billedAt,
		&requestHash, &submitResponseRaw, &finalResponseRaw, &task.CreatedAt, &task.UpdatedAt,
	)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, sql.ErrNoRows
		}
		return nil, fmt.Errorf("get aliyun task: %w", err)
	}
	if groupID.Valid {
		task.GroupID = &groupID.Int64
	}
	if meterUnit.Valid {
		task.MeterUnit = &meterUnit.String
	}
	if meterQuantity.Valid {
		task.MeterQuantity = &meterQuantity.Float64
	}
	if usageLogID.Valid {
		task.UsageLogID = &usageLogID.Int64
	}
	if billedAt.Valid {
		task.BilledAt = &billedAt.Time
	}
	if requestHash.Valid {
		task.RequestHash = &requestHash.String
	}
	task.SubmitResponse = anyMapFromJSONString(submitResponseRaw)
	task.FinalResponse = anyMapFromJSONString(finalResponseRaw)
	return &task, nil
}

func (r *aliyunTaskRepository) UpdateFinal(ctx context.Context, taskID, status string, meterUnit *string, meterQuantity *float64, finalResponse map[string]any) error {
	if r == nil || r.db == nil {
		return fmt.Errorf("aliyun task repository is not configured")
	}
	_, err := r.db.ExecContext(ctx, `
		UPDATE aliyun_tasks
		SET status = $2,
		    meter_unit = COALESCE($3::varchar, meter_unit),
		    meter_quantity = COALESCE($4::numeric, meter_quantity),
		    final_response = $5::jsonb,
		    updated_at = NOW()
		WHERE task_id = $1
	`, strings.TrimSpace(taskID), strings.TrimSpace(status), meterUnit, meterQuantity, nullAnyMap(finalResponse))
	if err != nil {
		return fmt.Errorf("update aliyun task final: %w", err)
	}
	return nil
}

func (r *aliyunTaskRepository) MarkBilled(ctx context.Context, taskID string, usageLogID *int64) error {
	if r == nil || r.db == nil {
		return fmt.Errorf("aliyun task repository is not configured")
	}
	_, err := r.db.ExecContext(ctx, `
		UPDATE aliyun_tasks
		SET billed_at = COALESCE(billed_at, NOW()),
		    usage_log_id = COALESCE(usage_log_id, $2::bigint),
		    updated_at = NOW()
		WHERE task_id = $1
	`, strings.TrimSpace(taskID), usageLogID)
	if err != nil {
		return fmt.Errorf("mark aliyun task billed: %w", err)
	}
	return nil
}

func nullAnyMap(v map[string]any) any {
	if len(v) == 0 {
		return nil
	}
	payload, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return string(payload)
}

func anyMapFromJSONString(v sql.NullString) map[string]any {
	if !v.Valid || strings.TrimSpace(v.String) == "" {
		return nil
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(v.String), &out); err != nil {
		return nil
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
