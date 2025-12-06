-- Migration: Premium Subscription System
-- Date: 2025-10-17
-- Trial plans are PAID one-time offers

-- Table 1: Subscription Plans
CREATE TABLE IF NOT EXISTS subscription_plans (
    plan_id VARCHAR(50) PRIMARY KEY,
    plan_name VARCHAR(100) NOT NULL,
    plan_type VARCHAR(20) NOT NULL CHECK (plan_type IN ('trial', 'monthly', 'yearly')),
    duration_days INT NOT NULL,
    price_inr DECIMAL(10, 2) NOT NULL DEFAULT 0,
    
    -- AI limits for this plan
    daily_request_limit INT NOT NULL DEFAULT 50,
    daily_token_limit INT NOT NULL DEFAULT 50000,
    
    -- Trial restrictions
    is_one_time_only BOOLEAN DEFAULT false,
    
    -- Plan details
    description TEXT,
    features JSONB,
    is_active BOOLEAN DEFAULT true,
    
    -- Metadata
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);

-- Table 2: User Subscriptions
CREATE TABLE IF NOT EXISTS user_subscriptions (
    subscription_id SERIAL PRIMARY KEY,
    uid VARCHAR(128) NOT NULL,
    plan_id VARCHAR(50) NOT NULL REFERENCES subscription_plans(plan_id),
    
    -- Subscription status
    status VARCHAR(20) NOT NULL CHECK (status IN ('active', 'expired', 'cancelled', 'pending')) DEFAULT 'pending',
    
    -- Dates
    start_date TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    end_date TIMESTAMP NOT NULL,
    cancelled_at TIMESTAMP,
    
    -- Payment info
    razorpay_order_id VARCHAR(100),
    razorpay_payment_id VARCHAR(100),
    razorpay_subscription_id VARCHAR(100),
    amount_paid DECIMAL(10, 2),
    
    -- Auto-renewal
    auto_renew BOOLEAN DEFAULT false,
    
    -- Metadata
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);

-- Table 3: User Trial History
CREATE TABLE IF NOT EXISTS user_trial_history (
    uid VARCHAR(128) PRIMARY KEY,
    has_used_trial BOOLEAN DEFAULT false,
    trial_plan_used VARCHAR(50),
    trial_used_at TIMESTAMP,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);

-- Table 4: Payment Transactions
CREATE TABLE IF NOT EXISTS payment_transactions (
    transaction_id SERIAL PRIMARY KEY,
    uid VARCHAR(128) NOT NULL,
    subscription_id INT REFERENCES user_subscriptions(subscription_id),
    
    -- Razorpay details
    razorpay_order_id VARCHAR(100) UNIQUE,
    razorpay_payment_id VARCHAR(100) UNIQUE,
    razorpay_signature VARCHAR(500),
    
    -- Transaction info
    amount DECIMAL(10, 2) NOT NULL,
    currency VARCHAR(3) DEFAULT 'INR',
    status VARCHAR(20) NOT NULL CHECK (status IN ('pending', 'success', 'failed', 'refunded')) DEFAULT 'pending',
    
    -- Payment method
    payment_method VARCHAR(50),
    
    -- Error handling
    error_code VARCHAR(50),
    error_description TEXT,
    
    -- Metadata
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);

-- Indexes
CREATE INDEX IF NOT EXISTS idx_user_subscriptions_uid ON user_subscriptions(uid);
CREATE INDEX IF NOT EXISTS idx_user_subscriptions_status ON user_subscriptions(status);
CREATE INDEX IF NOT EXISTS idx_user_subscriptions_end_date ON user_subscriptions(end_date);
CREATE INDEX IF NOT EXISTS idx_payment_transactions_uid ON payment_transactions(uid);
CREATE INDEX IF NOT EXISTS idx_payment_transactions_razorpay_order ON payment_transactions(razorpay_order_id);

-- Insert subscription plans with FINAL pricing
INSERT INTO subscription_plans (plan_id, plan_name, plan_type, duration_days, price_inr, daily_request_limit, daily_token_limit, is_one_time_only, description, features) VALUES

-- TRIAL PLANS (One-time only, PAID)
('trial_1day', '1-Day Premium Trial', 'trial', 1, 9.00, 20, 20000, true, 
 'Try premium for 1 day - Special introductory price!', 
 '{"requests": "20 requests/day", "tokens": "20,000 tokens/day", "support": "Priority support", "one_time": "One time offer"}'),

('trial_1week', '1-Week Premium Trial', 'trial', 7, 39.00, 30, 30000, true, 
 'Try premium for 1 week - Best trial value!', 
 '{"requests": "30 requests/day", "tokens": "30,000 tokens/day", "support": "Priority support", "one_time": "One time offer"}'),

-- REGULAR PLANS (Recurring)
('premium_monthly', 'Premium Monthly', 'monthly', 30, 99.00, 50, 50000, false, 
 'Full premium access - Billed monthly', 
 '{"requests": "50 requests/day", "tokens": "50,000 tokens/day", "support": "Priority support", "complete_responses": "Guaranteed", "renewable": "Auto-renews monthly"}'),

('premium_yearly', 'Premium Yearly', 'yearly', 365, 999.00, 50, 50000, false, 
 'Full premium access - Save 15%!', 
 '{"requests": "50 requests/day", "tokens": "50,000 tokens/day", "support": "Priority support", "complete_responses": "Guaranteed", "renewable": "Auto-renews yearly", "discount": "17% off monthly price"}')

ON CONFLICT (plan_id) DO NOTHING;

-- Comments
COMMENT ON TABLE subscription_plans IS 'Available premium subscription plans';
COMMENT ON TABLE user_subscriptions IS 'User active subscriptions and history';
COMMENT ON TABLE user_trial_history IS 'Tracks if user has already used one-time trial offer';
COMMENT ON TABLE payment_transactions IS 'Payment transaction logs for Razorpay';
COMMENT ON COLUMN subscription_plans.is_one_time_only IS 'If true, user can only buy this plan once ever';
COMMENT ON COLUMN user_subscriptions.status IS 'active, expired, cancelled, or pending';

