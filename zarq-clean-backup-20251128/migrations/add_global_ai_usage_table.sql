-- Migration: Add global AI usage tracking table
-- This table tracks total AI requests and token usage across all users per day
-- to enforce the Google Gemini free tier limits:
--   - 1,500 requests/day
--   - 1,000,000 tokens/day (input + output combined)
--   - 15 requests/minute (RPM)

CREATE TABLE IF NOT EXISTS global_ai_usage (
    usage_date DATE PRIMARY KEY,
    request_count INT NOT NULL DEFAULT 0,
    total_tokens_used BIGINT NOT NULL DEFAULT 0,
    input_tokens BIGINT NOT NULL DEFAULT 0,
    output_tokens BIGINT NOT NULL DEFAULT 0,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);

-- Per-minute rate limiting table (for 15 RPM limit)
CREATE TABLE IF NOT EXISTS ai_rate_limit (
    minute_timestamp TIMESTAMP PRIMARY KEY,
    request_count INT NOT NULL DEFAULT 0,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);

-- Create index for faster cleanup of old rate limit records
CREATE INDEX IF NOT EXISTS idx_ai_rate_limit_timestamp ON ai_rate_limit(minute_timestamp);

-- Comments for documentation
COMMENT ON TABLE global_ai_usage IS 'Tracks total daily AI requests and token usage across all users to enforce free tier limits';
COMMENT ON COLUMN global_ai_usage.usage_date IS 'Date of usage (one row per day)';
COMMENT ON COLUMN global_ai_usage.request_count IS 'Total number of AI requests made globally today';
COMMENT ON COLUMN global_ai_usage.total_tokens_used IS 'Total tokens consumed (input + output) today';
COMMENT ON COLUMN global_ai_usage.input_tokens IS 'Total input tokens (prompts) used today';
COMMENT ON COLUMN global_ai_usage.output_tokens IS 'Total output tokens (responses) used today';

COMMENT ON TABLE ai_rate_limit IS 'Tracks per-minute request counts to enforce 15 RPM limit';
COMMENT ON COLUMN ai_rate_limit.minute_timestamp IS 'Timestamp truncated to minute (e.g., 2025-01-15 14:30:00)';
COMMENT ON COLUMN ai_rate_limit.request_count IS 'Number of requests made in this minute';
