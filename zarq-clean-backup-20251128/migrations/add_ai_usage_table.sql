-- Migration: Add AI usage tracking table
-- Run this SQL on your database

CREATE TABLE IF NOT EXISTS ai_usage (
    uid VARCHAR(128) PRIMARY KEY,
    usage_count INT NOT NULL DEFAULT 0,
    usage_date DATE NOT NULL,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);

-- Index for faster lookups
CREATE INDEX IF NOT EXISTS idx_ai_usage_date ON ai_usage(usage_date);

-- Comments for documentation
COMMENT ON TABLE ai_usage IS 'Tracks daily AI feature usage per user for rate limiting';
COMMENT ON COLUMN ai_usage.uid IS 'Firebase user ID';
COMMENT ON COLUMN ai_usage.usage_count IS 'Number of AI requests made today';
COMMENT ON COLUMN ai_usage.usage_date IS 'Date of usage (resets daily)';
