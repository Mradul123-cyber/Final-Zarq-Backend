// handlers/payment.go
package handlers

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"firebase.google.com/go/v4/auth"
)

// Razorpay API structures
type CreateOrderRequest struct {
	PlanID string `json:"plan_id"` // monthly_basic, monthly_pro, yearly_basic, yearly_pro
}

type CreateOrderResponse struct {
	OrderID   string `json:"order_id"`
	Amount    int    `json:"amount"`       // in paise
	Currency  string `json:"currency"`
	KeyID     string `json:"key_id"`       // Razorpay key_id for client
	Success   bool   `json:"success"`
	Error     string `json:"error,omitempty"`
}

type VerifyPaymentRequest struct {
	OrderID         string `json:"order_id"`
	PaymentID       string `json:"payment_id"`
	Signature       string `json:"signature"`
	PlanID          string `json:"plan_id"`
}

type VerifyPaymentResponse struct {
	Success       bool   `json:"success"`
	Message       string `json:"message"`
	SubscriptionID int   `json:"subscription_id,omitempty"`
	Error         string `json:"error,omitempty"`
}

type RazorpayOrderRequest struct {
	Amount   int               `json:"amount"`   // in paise
	Currency string            `json:"currency"`
	Receipt  string            `json:"receipt"`
	Notes    map[string]string `json:"notes,omitempty"`
}

type RazorpayOrderResponse struct {
	ID       string            `json:"id"`
	Entity   string            `json:"entity"`
	Amount   int               `json:"amount"`
	Currency string            `json:"currency"`
	Receipt  string            `json:"receipt"`
	Status   string            `json:"status"`
	Notes    map[string]string `json:"notes"`
	CreatedAt int64            `json:"created_at"`
}

// CreatePaymentOrder creates a new Razorpay order for subscription purchase
func CreatePaymentOrder(db *sql.DB, firebaseAuth *auth.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Verify Firebase token
		authHeader := r.Header.Get("Authorization")
		if authHeader == "" {
			http.Error(w, "Authorization required", http.StatusUnauthorized)
			return
		}

		tokenStr := authHeader[len("Bearer "):]
		token, err := firebaseAuth.VerifyIDToken(context.Background(), tokenStr)
		if err != nil {
			http.Error(w, "Invalid token", http.StatusUnauthorized)
			return
		}

		// Parse request
		var req CreateOrderRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request", http.StatusBadRequest)
			return
		}

		// Check if user already has an active subscription
		var existingPlanID string
		var existingEndDate time.Time
		err = db.QueryRow(`
			SELECT plan_id, end_date
			FROM user_subscriptions
			WHERE uid = $1 AND status = 'active' AND end_date > NOW()
		`, token.UID).Scan(&existingPlanID, &existingEndDate)

		if err == nil {
			// User has active subscription - prevent purchase
			resp := CreateOrderResponse{
				Success: false,
				Error:   "You already have an active subscription. Please wait for it to expire before purchasing a new plan.",
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(resp)
			return
		} else if err != sql.ErrNoRows {
			// Database error
			log.Printf("Error checking existing subscription: %v", err)
			http.Error(w, "Internal error", http.StatusInternalServerError)
			return
		}

		// Get plan details from database
		var amountRupees float64
		var planName string
		err = db.QueryRow(`
			SELECT price_inr, plan_name
			FROM subscription_plans
			WHERE plan_id = $1 AND is_active = TRUE
		`, req.PlanID).Scan(&amountRupees, &planName)

		if err == sql.ErrNoRows {
			resp := CreateOrderResponse{
				Success: false,
				Error:   "Invalid plan selected",
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(resp)
			return
		} else if err != nil {
			log.Printf("Database error getting plan: %v", err)
			http.Error(w, "Internal error", http.StatusInternalServerError)
			return
		}

		// Convert rupees to paise (Razorpay uses paise)
		amountInPaise := int(amountRupees * 100)

		// Generate unique receipt number
		receipt := fmt.Sprintf("ZARQ_%s_%d", token.UID[:8], time.Now().Unix())

		// Create Razorpay order via API
		razorpayKeyID := os.Getenv("RAZORPAY_KEY_ID")
		razorpaySecret := os.Getenv("RAZORPAY_SECRET")

		if razorpayKeyID == "" || razorpaySecret == "" {
			log.Printf("Razorpay credentials not configured")
			http.Error(w, "Payment system not configured", http.StatusInternalServerError)
			return
		}

		// Prepare Razorpay order request
		orderReq := RazorpayOrderRequest{
			Amount:   amountInPaise,
			Currency: "INR",
			Receipt:  receipt,
			Notes: map[string]string{
				"uid":     token.UID,
				"plan_id": req.PlanID,
				"email":   token.Claims["email"].(string),
			},
		}

		orderJSON, _ := json.Marshal(orderReq)

		// Call Razorpay API to create order
		httpReq, err := http.NewRequest("POST", "https://api.razorpay.com/v1/orders", bytes.NewBuffer(orderJSON))
		if err != nil {
			log.Printf("Error creating Razorpay request: %v", err)
			http.Error(w, "Internal error", http.StatusInternalServerError)
			return
		}

		httpReq.SetBasicAuth(razorpayKeyID, razorpaySecret)
		httpReq.Header.Set("Content-Type", "application/json")

		client := &http.Client{Timeout: 10 * time.Second}
		httpResp, err := client.Do(httpReq)
		if err != nil {
			log.Printf("Error calling Razorpay API: %v", err)
			http.Error(w, "Payment gateway error", http.StatusInternalServerError)
			return
		}
		defer httpResp.Body.Close()

		body, _ := io.ReadAll(httpResp.Body)

		if httpResp.StatusCode != http.StatusOK {
			log.Printf("Razorpay API error: %s", string(body))
			http.Error(w, "Payment gateway error", http.StatusInternalServerError)
			return
		}

		var razorpayOrder RazorpayOrderResponse
		if err := json.Unmarshal(body, &razorpayOrder); err != nil {
			log.Printf("Error parsing Razorpay response: %v", err)
			http.Error(w, "Internal error", http.StatusInternalServerError)
			return
		}

		// Save order to database
		_, err = db.Exec(`
			INSERT INTO payment_orders (order_id, uid, plan_id, amount, currency, status, receipt, notes)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		`, razorpayOrder.ID, token.UID, req.PlanID, amountInPaise, "INR", "created", receipt, orderJSON)

		if err != nil {
			log.Printf("Error saving order to database: %v", err)
			// Continue anyway, order was created in Razorpay
		}

		// Return order details to client
		resp := CreateOrderResponse{
			OrderID:  razorpayOrder.ID,
			Amount:   amountInPaise,
			Currency: "INR",
			KeyID:    razorpayKeyID,
			Success:  true,
		}

		log.Printf("[Payment] Created order %s for user %s, plan %s, amount ₹%.2f",
			razorpayOrder.ID, token.UID, req.PlanID, amountRupees)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}
}

// VerifyPayment verifies the Razorpay payment signature and activates subscription
func VerifyPayment(db *sql.DB, firebaseAuth *auth.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Verify Firebase token
		authHeader := r.Header.Get("Authorization")
		if authHeader == "" {
			http.Error(w, "Authorization required", http.StatusUnauthorized)
			return
		}

		tokenStr := authHeader[len("Bearer "):]
		token, err := firebaseAuth.VerifyIDToken(context.Background(), tokenStr)
		if err != nil {
			http.Error(w, "Invalid token", http.StatusUnauthorized)
			return
		}

		// Parse request
		var req VerifyPaymentRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request", http.StatusBadRequest)
			return
		}

		// Verify payment signature (if provided) or fetch from Razorpay API
		razorpayKeyID := os.Getenv("RAZORPAY_KEY_ID")
		razorpaySecret := os.Getenv("RAZORPAY_SECRET")
		if razorpaySecret == "" {
			log.Printf("Razorpay secret not configured")
			http.Error(w, "Payment system not configured", http.StatusInternalServerError)
			return
		}

		if req.Signature != "" {
			// Signature provided - verify it using HMAC
			message := req.OrderID + "|" + req.PaymentID
			h := hmac.New(sha256.New, []byte(razorpaySecret))
			h.Write([]byte(message))
			expectedSignature := hex.EncodeToString(h.Sum(nil))

			if expectedSignature != req.Signature {
				log.Printf("[Payment] Invalid signature for payment %s", req.PaymentID)
				resp := VerifyPaymentResponse{
					Success: false,
					Error:   "Payment verification failed - invalid signature",
				}
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(resp)
				return
			}
			log.Printf("[Payment] Signature verified for payment %s", req.PaymentID)
		} else {
			// No signature - verify by fetching payment from Razorpay API
			log.Printf("[Payment] No signature, verifying via Razorpay API for payment %s", req.PaymentID)

			apiURL := fmt.Sprintf("https://api.razorpay.com/v1/payments/%s", req.PaymentID)
			httpReq, err := http.NewRequest("GET", apiURL, nil)
			if err != nil {
				log.Printf("Error creating Razorpay request: %v", err)
				http.Error(w, "Internal error", http.StatusInternalServerError)
				return
			}

			httpReq.SetBasicAuth(razorpayKeyID, razorpaySecret)
			client := &http.Client{Timeout: 10 * time.Second}
			httpResp, err := client.Do(httpReq)
			if err != nil {
				log.Printf("Error calling Razorpay API: %v", err)
				http.Error(w, "Payment verification failed", http.StatusInternalServerError)
				return
			}
			defer httpResp.Body.Close()

			body, _ := io.ReadAll(httpResp.Body)
			if httpResp.StatusCode != http.StatusOK {
				log.Printf("Razorpay API error: %s", string(body))
				resp := VerifyPaymentResponse{
					Success: false,
					Error:   "Payment not found or verification failed",
				}
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(resp)
				return
			}

			var paymentDetails map[string]interface{}
			if err := json.Unmarshal(body, &paymentDetails); err != nil {
				log.Printf("Error parsing Razorpay response: %v", err)
				http.Error(w, "Internal error", http.StatusInternalServerError)
				return
			}

			// Verify payment status and order_id match
			status, _ := paymentDetails["status"].(string)
			paymentOrderID, _ := paymentDetails["order_id"].(string)

			if status != "captured" && status != "authorized" {
				log.Printf("[Payment] Payment %s has invalid status: %s", req.PaymentID, status)
				resp := VerifyPaymentResponse{
					Success: false,
					Error:   fmt.Sprintf("Payment status is %s, not captured", status),
				}
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(resp)
				return
			}

			if paymentOrderID != req.OrderID {
				log.Printf("[Payment] Order ID mismatch: expected %s, got %s", req.OrderID, paymentOrderID)
				resp := VerifyPaymentResponse{
					Success: false,
					Error:   "Order ID mismatch",
				}
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(resp)
				return
			}

			log.Printf("[Payment] Payment verified via Razorpay API - Status: %s, OrderID: %s", status, paymentOrderID)
		}

		// Payment is valid - start transaction
		tx, err := db.Begin()
		if err != nil {
			log.Printf("Error starting transaction: %v", err)
			http.Error(w, "Internal error", http.StatusInternalServerError)
			return
		}
		defer tx.Rollback()

		// Get order details
		var orderAmount int
		var orderUID string
		err = tx.QueryRow(`
			SELECT amount, uid
			FROM payment_orders
			WHERE order_id = $1
		`, req.OrderID).Scan(&orderAmount, &orderUID)

		if err != nil {
			log.Printf("Error getting order: %v", err)
			http.Error(w, "Order not found", http.StatusNotFound)
			return
		}

		// Verify UID matches
		if orderUID != token.UID {
			log.Printf("[Payment] UID mismatch for order %s", req.OrderID)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		// Update order status
		_, err = tx.Exec(`
			UPDATE payment_orders
			SET status = 'paid', updated_at = NOW()
			WHERE order_id = $1
		`, req.OrderID)

		if err != nil {
			log.Printf("Error updating order: %v", err)
			http.Error(w, "Internal error", http.StatusInternalServerError)
			return
		}

		// Save payment transaction
		_, err = tx.Exec(`
			INSERT INTO payment_transactions
			(razorpay_payment_id, razorpay_order_id, razorpay_signature, uid, amount, currency, status)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
		`, req.PaymentID, req.OrderID, req.Signature, token.UID, orderAmount, "INR", "success")

		if err != nil {
			log.Printf("Error saving transaction: %v", err)
			http.Error(w, "Internal error", http.StatusInternalServerError)
			return
		}

		// Get plan duration
		var durationDays int
		err = tx.QueryRow(`
			SELECT duration_days FROM subscription_plans WHERE plan_id = $1
		`, req.PlanID).Scan(&durationDays)

		if err != nil {
			log.Printf("Error getting plan duration: %v", err)
			http.Error(w, "Internal error", http.StatusInternalServerError)
			return
		}

		// Calculate subscription end date
		endDate := time.Now().AddDate(0, 0, durationDays)

		// Create or update user subscription
		var subscriptionID int
		err = tx.QueryRow(`
			INSERT INTO user_subscriptions (uid, plan_id, status, start_date, end_date)
			VALUES ($1, $2, 'active', NOW(), $3)
			ON CONFLICT (uid)
			DO UPDATE SET
				plan_id = $2,
				status = 'active',
				start_date = NOW(),
				end_date = $3,
				updated_at = NOW()
			RETURNING subscription_id
		`, token.UID, req.PlanID, endDate).Scan(&subscriptionID)

		if err != nil {
			log.Printf("Error creating subscription: %v", err)
			http.Error(w, "Internal error", http.StatusInternalServerError)
			return
		}

		// Commit transaction
		if err = tx.Commit(); err != nil {
			log.Printf("Error committing transaction: %v", err)
			http.Error(w, "Internal error", http.StatusInternalServerError)
			return
		}

		log.Printf("[Payment] Successfully verified payment %s for user %s, plan %s, subscription ID %d",
			req.PaymentID, token.UID, req.PlanID, subscriptionID)

		// Return success
		resp := VerifyPaymentResponse{
			Success:        true,
			Message:        "Payment verified successfully! Premium subscription activated.",
			SubscriptionID: subscriptionID,
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}
}

// RazorpayWebhook handles webhook events from Razorpay
func RazorpayWebhook(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Read webhook payload
		body, err := io.ReadAll(r.Body)
		if err != nil {
			log.Printf("Error reading webhook body: %v", err)
			http.Error(w, "Invalid request", http.StatusBadRequest)
			return
		}

		// Verify webhook signature
		razorpayWebhookSecret := os.Getenv("RAZORPAY_WEBHOOK_SECRET")
		if razorpayWebhookSecret != "" {
			signature := r.Header.Get("X-Razorpay-Signature")
			if signature != "" {
				h := hmac.New(sha256.New, []byte(razorpayWebhookSecret))
				h.Write(body)
				expectedSignature := hex.EncodeToString(h.Sum(nil))

				if signature != expectedSignature {
					log.Printf("[Webhook] Invalid signature")
					http.Error(w, "Invalid signature", http.StatusUnauthorized)
					return
				}
			}
		}

		// Parse webhook event
		var event map[string]interface{}
		if err := json.Unmarshal(body, &event); err != nil {
			log.Printf("Error parsing webhook: %v", err)
			http.Error(w, "Invalid JSON", http.StatusBadRequest)
			return
		}

		eventType := event["event"].(string)
		eventID, _ := event["event_id"].(string)

		// Log webhook event
		log.Printf("[Webhook] Received event: %s (ID: %s)", eventType, eventID)

		// Save webhook event to database
		payload, _ := json.Marshal(event)
		_, err = db.Exec(`
			INSERT INTO razorpay_webhook_events (event_id, event_type, payload, processed)
			VALUES ($1, $2, $3, FALSE)
			ON CONFLICT (event_id) DO NOTHING
		`, eventID, eventType, payload)

		if err != nil {
			log.Printf("Error saving webhook event: %v", err)
			// Continue processing anyway
		}

		// Process specific events
		switch eventType {
		case "payment.captured":
			// Payment was captured successfully
			log.Printf("[Webhook] Payment captured event")
			// Additional processing can be done here if needed

		case "payment.failed":
			// Payment failed
			log.Printf("[Webhook] Payment failed event")
			// Update order status if needed

		case "order.paid":
			// Order was paid
			log.Printf("[Webhook] Order paid event")

		default:
			log.Printf("[Webhook] Unhandled event type: %s", eventType)
		}

		// Return 200 OK to acknowledge receipt
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	}
}

// GetSubscriptionStatus returns the current user's subscription status
type SubscriptionStatusResponse struct {
	HasActiveSubscription bool      `json:"has_active_subscription"`
	PlanID                string    `json:"plan_id,omitempty"`
	PlanName              string    `json:"plan_name,omitempty"`
	Status                string    `json:"status,omitempty"`
	StartDate             time.Time `json:"start_date,omitempty"`
	EndDate               time.Time `json:"end_date,omitempty"`
	DaysRemaining         int       `json:"days_remaining,omitempty"`
	HoursRemaining        int       `json:"hours_remaining,omitempty"`
	MinutesRemaining      int       `json:"minutes_remaining,omitempty"`
	DailyRequestLimit     int       `json:"daily_request_limit"`
	DailyTokenLimit       int       `json:"daily_token_limit"`
}

func GetSubscriptionStatus(db *sql.DB, firebaseAuth *auth.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Verify Firebase token
		authHeader := r.Header.Get("Authorization")
		if authHeader == "" {
			http.Error(w, "Authorization required", http.StatusUnauthorized)
			return
		}

		tokenStr := authHeader[len("Bearer "):]
		token, err := firebaseAuth.VerifyIDToken(context.Background(), tokenStr)
		if err != nil {
			http.Error(w, "Invalid token", http.StatusUnauthorized)
			return
		}

		// Get subscription info
		var planID, planName, status string
		var startDate, endDate time.Time
		var dailyRequestLimit, dailyTokenLimit int

		err = db.QueryRow(`
			SELECT us.plan_id, sp.plan_name, us.status, us.start_date, us.end_date,
			       sp.daily_request_limit, sp.daily_token_limit
			FROM user_subscriptions us
			JOIN subscription_plans sp ON us.plan_id = sp.plan_id
			WHERE us.uid = $1 AND us.status = 'active' AND us.end_date > NOW()
		`, token.UID).Scan(&planID, &planName, &status, &startDate, &endDate, &dailyRequestLimit, &dailyTokenLimit)

		if err == sql.ErrNoRows {
			// No active subscription - return free tier info
			resp := SubscriptionStatusResponse{
				HasActiveSubscription: false,
				DailyRequestLimit:     5,
				DailyTokenLimit:       5000,
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(resp)
			return
		} else if err != nil {
			log.Printf("Error getting subscription: %v", err)
			http.Error(w, "Internal error", http.StatusInternalServerError)
			return
		}

		// Calculate time remaining with precision
		timeRemaining := time.Until(endDate)
		totalHours := int(timeRemaining.Hours())
		totalMinutes := int(timeRemaining.Minutes())

		daysRemaining := totalHours / 24
		hoursRemaining := totalHours % 24
		minutesRemaining := totalMinutes % 60

		resp := SubscriptionStatusResponse{
			HasActiveSubscription: true,
			PlanID:                planID,
			PlanName:              planName,
			Status:                status,
			StartDate:             startDate,
			EndDate:               endDate,
			DaysRemaining:         daysRemaining,
			HoursRemaining:        hoursRemaining,
			MinutesRemaining:      minutesRemaining,
			DailyRequestLimit:     dailyRequestLimit,
			DailyTokenLimit:       dailyTokenLimit,
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}
}

// SubscriptionPlan represents a subscription plan with all details
type SubscriptionPlan struct {
	PlanID            string   `json:"plan_id"`
	Name              string   `json:"name"`
	Price             float64  `json:"price"`
	Duration          string   `json:"duration"`
	DurationDays      int      `json:"duration_days"`
	Features          []string `json:"features"`
	DailyRequestLimit int      `json:"daily_request_limit"`
	DailyTokenLimit   int      `json:"daily_token_limit"`
	Color             int64    `json:"color"`
	IsPopular         bool     `json:"popular,omitempty"`
	AlreadyUsed       bool     `json:"already_used,omitempty"`
}

// GetSubscriptionPlans returns all active subscription plans
func GetSubscriptionPlans(db *sql.DB, firebaseAuth *auth.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Try to get user ID from auth header (optional)
		var userID string
		authHeader := r.Header.Get("Authorization")
		if authHeader != "" && firebaseAuth != nil {
			tokenStr := strings.TrimPrefix(authHeader, "Bearer ")
			token, err := firebaseAuth.VerifyIDToken(context.Background(), tokenStr)
			if err == nil {
				userID = token.UID
			}
		}

		// Get user's purchase history if authenticated
		usedPlanTypes := make(map[string]bool)
		if userID != "" {
			rows, err := db.Query(`
				SELECT DISTINCT sp.plan_type
				FROM user_subscriptions us
				JOIN subscription_plans sp ON us.plan_id = sp.plan_id
				WHERE us.uid = $1 AND sp.plan_type = 'trial'
			`, userID)
			if err == nil {
				defer rows.Close()
				for rows.Next() {
					var planType string
					if err := rows.Scan(&planType); err == nil {
						usedPlanTypes[planType] = true
					}
				}
			}
		}

		// Query all active plans
		rows, err := db.Query(`
			SELECT plan_id, plan_name, plan_type, duration_days, price_inr,
			       daily_request_limit, daily_token_limit, features
			FROM subscription_plans
			WHERE is_active = TRUE
			ORDER BY price_inr ASC
		`)
		if err != nil {
			log.Printf("Error fetching subscription plans: %v", err)
			http.Error(w, "Internal error", http.StatusInternalServerError)
			return
		}
		defer rows.Close()

		var plans []SubscriptionPlan
		for rows.Next() {
			var plan SubscriptionPlan
			var planType string
			var durationDays int
			var featuresJSON []byte

			err := rows.Scan(
				&plan.PlanID,
				&plan.Name,
				&planType,
				&durationDays,
				&plan.Price,
				&plan.DailyRequestLimit,
				&plan.DailyTokenLimit,
				&featuresJSON,
			)
			if err != nil {
				log.Printf("Error scanning plan row: %v", err)
				continue
			}

			// Set duration days
			plan.DurationDays = durationDays

			// Convert duration_days to human-readable duration
			if durationDays == 1 {
				plan.Duration = "1 Day"
			} else if durationDays == 7 {
				plan.Duration = "7 Days"
			} else if durationDays == 30 {
				plan.Duration = "1 Month"
			} else if durationDays == 365 {
				plan.Duration = "12 Months"
			} else {
				plan.Duration = fmt.Sprintf("%d Days", durationDays)
			}

			// Parse features from JSONB (convert object to array of strings)
			var featuresMap map[string]string
			if len(featuresJSON) > 0 {
				if err := json.Unmarshal(featuresJSON, &featuresMap); err == nil {
					// Convert map to ordered array
					plan.Features = []string{}

					// Add requests first if exists
					if val, ok := featuresMap["requests"]; ok {
						plan.Features = append(plan.Features, val)
					}
					// Add tokens second if exists
					if val, ok := featuresMap["tokens"]; ok {
						plan.Features = append(plan.Features, val)
					}
					// Add complete responses if exists
					if val, ok := featuresMap["complete_responses"]; ok {
						plan.Features = append(plan.Features, val)
					}
					// Add support if exists
					if val, ok := featuresMap["support"]; ok {
						plan.Features = append(plan.Features, val)
					}
					// Add discount/renewable if exists
					if val, ok := featuresMap["discount"]; ok {
						plan.Features = append(plan.Features, val)
					} else if val, ok := featuresMap["renewable"]; ok {
						plan.Features = append(plan.Features, val)
					}
					// Add one_time if exists
					if val, ok := featuresMap["one_time"]; ok {
						plan.Features = append(plan.Features, val)
					}
				}
			}

			// Set color based on plan type
			switch planType {
			case "trial":
				if durationDays == 1 {
					plan.Color = 0xFF10B981 // Green for 1-day trial
				} else {
					plan.Color = 0xFF6366F1 // Indigo for 1-week trial
				}
				// Mark trial as already used if user has used any trial before
				if usedPlanTypes["trial"] {
					plan.AlreadyUsed = true
				}
			case "monthly":
				plan.Color = 0xFF8B5CF6 // Purple for monthly
				plan.IsPopular = true
			case "yearly":
				plan.Color = 0xFFF59E0B // Amber/Gold for yearly
				plan.IsPopular = true
			default:
				plan.Color = 0xFF6366F1 // Default indigo
			}

			plans = append(plans, plan)
		}

		if err = rows.Err(); err != nil {
			log.Printf("Error iterating plan rows: %v", err)
			http.Error(w, "Internal error", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(plans)
	}
}
