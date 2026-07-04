-- Add supplier-native unit billing fields and Aliyun platform support.

SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '10min';

ALTER TABLE channel_model_pricing
    ADD COLUMN IF NOT EXISTS meter_unit VARCHAR(50),
    ADD COLUMN IF NOT EXISTS meter_unit_price NUMERIC(20,12);

COMMENT ON COLUMN channel_model_pricing.meter_unit IS 'Unit billing meter name, for example character/audio_second/request/video_token';
COMMENT ON COLUMN channel_model_pricing.meter_unit_price IS 'Unit billing price in USD per meter_unit';

ALTER TABLE usage_logs
    ADD COLUMN IF NOT EXISTS meter_cost NUMERIC(20,10) NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS meter_unit VARCHAR(50),
    ADD COLUMN IF NOT EXISTS meter_quantity NUMERIC(20,6),
    ADD COLUMN IF NOT EXISTS meter_unit_price NUMERIC(20,12),
    ADD COLUMN IF NOT EXISTS meter_detail JSONB;

COMMENT ON COLUMN usage_logs.meter_cost IS 'Raw unit-mode cost before user rate multiplier';
COMMENT ON COLUMN usage_logs.meter_unit IS 'Unit-mode meter name used for billing';
COMMENT ON COLUMN usage_logs.meter_quantity IS 'Unit-mode metered quantity';
COMMENT ON COLUMN usage_logs.meter_unit_price IS 'Unit-mode price snapshot in USD';
COMMENT ON COLUMN usage_logs.meter_detail IS 'Provider-native metering details used for audit';

CREATE TABLE IF NOT EXISTS aliyun_tasks (
    id                 BIGSERIAL PRIMARY KEY,
    task_id            TEXT NOT NULL UNIQUE,
    user_id            BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    api_key_id         BIGINT NOT NULL REFERENCES api_keys(id) ON DELETE CASCADE,
    account_id         BIGINT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    group_id           BIGINT REFERENCES groups(id) ON DELETE SET NULL,
    model              VARCHAR(100) NOT NULL,
    status             VARCHAR(32) NOT NULL DEFAULT 'pending',
    meter_unit         VARCHAR(50),
    meter_quantity     NUMERIC(20,6),
    usage_log_id       BIGINT REFERENCES usage_logs(id) ON DELETE SET NULL,
    billed_at          TIMESTAMPTZ,
    request_hash       TEXT,
    submit_response    JSONB,
    final_response     JSONB,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_aliyun_tasks_user_id ON aliyun_tasks(user_id);
CREATE INDEX IF NOT EXISTS idx_aliyun_tasks_api_key_id ON aliyun_tasks(api_key_id);
CREATE INDEX IF NOT EXISTS idx_aliyun_tasks_billed_at ON aliyun_tasks(billed_at);

COMMENT ON TABLE aliyun_tasks IS 'Aliyun async task ownership and billing idempotency records';
