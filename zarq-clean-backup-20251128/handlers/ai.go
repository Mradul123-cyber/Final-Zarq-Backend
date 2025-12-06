// handlers/ai.go
package handlers

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"time"

	"firebase.google.com/go/v4/auth"
)

// AI request/response structures
type AIRequest struct {
	Action         string `json:"action"`          // translate, summarize, explain, enhance
	Text           string `json:"text"`            // Message content
	TargetLanguage string `json:"targetLanguage"`  // For translate
	Style          string `json:"style"`           // For enhance: formal, casual, concise, fix
}

type AIResponse struct {
	Content   string `json:"content"`
	Success   bool   `json:"success"`
	IsOnDevice bool  `json:"isOnDevice"`
	Error     string `json:"error,omitempty"`
}

type GeminiRequest struct {
	Contents         []GeminiContent   `json:"contents"`
	GenerationConfig *GenerationConfig `json:"generationConfig,omitempty"`
}

type GeminiContent struct {
	Role  string         `json:"role,omitempty"`
	Parts []GeminiPart `json:"parts"`
}

type GeminiPart struct {
	Text string `json:"text"`
}

type GenerationConfig struct {
	MaxOutputTokens int     `json:"maxOutputTokens"`
	Temperature     float64 `json:"temperature"`
}

type GeminiResponse struct {
	Candidates []struct {
		Content struct {
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"content"`
		FinishReason string `json:"finishReason"`
	} `json:"candidates"`
	UsageMetadata struct {
		PromptTokenCount     int `json:"promptTokenCount"`
		CandidatesTokenCount int `json:"candidatesTokenCount"`
		TotalTokenCount      int `json:"totalTokenCount"`
	} `json:"usageMetadata"`
}

// Daily usage tracking
const dailyMessageLimit = 5            // Max requests per user per day (FREE TIER)
const dailyTokenBudget = 5000          // Max tokens per user per day (FREE TIER)
const globalDailyLimit = 1000          // Google Gemini free tier: 1,000 requests/day
const globalTokenLimit = 1000000       // Google Gemini free tier: 1M tokens/day (input + output)
const globalRPMLimit = 15              // Google Gemini free tier: 15 requests/minute

// User subscription information
type UserSubscriptionInfo struct {
	IsPremium         bool
	PlanID            string
	DailyRequestLimit int
	DailyTokenLimit   int
	EndDate           time.Time
}

// Check if user has active premium subscription and get their limits
func getUserSubscriptionInfo(db *sql.DB, uid string) (*UserSubscriptionInfo, error) {
	var planID string
	var dailyRequestLimit int
	var dailyTokenLimit int
	var endDate time.Time

	err := db.QueryRow(`
		SELECT us.plan_id, sp.daily_request_limit, sp.daily_token_limit, us.end_date
		FROM user_subscriptions us
		JOIN subscription_plans sp ON us.plan_id = sp.plan_id
		WHERE us.uid = $1
		  AND us.status = 'active'
		  AND us.end_date > NOW()
		ORDER BY us.end_date DESC
		LIMIT 1
	`, uid).Scan(&planID, &dailyRequestLimit, &dailyTokenLimit, &endDate)

	if err == sql.ErrNoRows {
		// User has no active premium subscription - return free tier limits
		return &UserSubscriptionInfo{
			IsPremium:         false,
			PlanID:            "free",
			DailyRequestLimit: dailyMessageLimit,
			DailyTokenLimit:   dailyTokenBudget,
		}, nil
	} else if err != nil {
		return nil, err
	}

	// User has active premium subscription
	return &UserSubscriptionInfo{
		IsPremium:         true,
		PlanID:            planID,
		DailyRequestLimit: dailyRequestLimit,
		DailyTokenLimit:   dailyTokenLimit,
		EndDate:           endDate,
	}, nil
}

// Chat history message structure
type ChatMessage struct {
	Role    string
	Content string
}

// Save user message and AI response to chat history
func saveChatHistory(db *sql.DB, uid string, userMessage string, aiResponse string, tokensUsed int) error {
	// Save user message
	_, err := db.Exec(`
		INSERT INTO ai_chat_history (uid, role, content, tokens_used)
		VALUES ($1, 'user', $2, 0)
	`, uid, userMessage)
	if err != nil {
		return err
	}

	// Save AI response
	_, err = db.Exec(`
		INSERT INTO ai_chat_history (uid, role, content, tokens_used)
		VALUES ($1, 'assistant', $2, $3)
	`, uid, aiResponse, tokensUsed)

	return err
}

// Get chat history for user with limit based on subscription tier
// Free tier: last 3 messages, Premium tier: last 7 messages
func getChatHistory(db *sql.DB, uid string, isPremium bool) ([]ChatMessage, error) {
	// Determine history limit based on subscription
	historyLimit := 3  // Free tier: last 3 messages (1-2 exchanges)
	if isPremium {
		historyLimit = 7  // Premium tier: last 7 messages (3-4 exchanges)
	}

	rows, err := db.Query(`
		SELECT role, content
		FROM ai_chat_history
		WHERE uid = $1
		ORDER BY timestamp DESC
		LIMIT $2
	`, uid, historyLimit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var messages []ChatMessage
	for rows.Next() {
		var msg ChatMessage
		if err := rows.Scan(&msg.Role, &msg.Content); err != nil {
			return nil, err
		}
		messages = append(messages, msg)
	}

	// Reverse the array to get chronological order (oldest first)
	for i := len(messages)/2 - 1; i >= 0; i-- {
		opp := len(messages) - 1 - i
		messages[i], messages[opp] = messages[opp], messages[i]
	}

	return messages, nil
}

// Clear old chat history (older than 1 hour of inactivity)
func clearOldChatHistory(db *sql.DB) error {
	oneHourAgo := time.Now().Add(-1 * time.Hour)

	_, err := db.Exec(`
		DELETE FROM ai_chat_history
		WHERE timestamp < $1
	`, oneHourAgo)

	return err
}

func checkAndIncrementGlobalUsage(db *sql.DB) (bool, int, error) {
	today := time.Now().Format("2006-01-02")

	// Start transaction for atomic operation
	tx, err := db.Begin()
	if err != nil {
		return false, 0, err
	}
	defer tx.Rollback()

	// Check current global usage (both requests and tokens)
	var count int
	var totalTokens int64
	err = tx.QueryRow(`
		SELECT request_count, total_tokens_used
		FROM global_ai_usage
		WHERE usage_date = $1
		FOR UPDATE
	`, today).Scan(&count, &totalTokens)

	if err == sql.ErrNoRows {
		// First request of the day - create entry
		_, err = tx.Exec(`
			INSERT INTO global_ai_usage (usage_date, request_count, total_tokens_used)
			VALUES ($1, 1, 0)
		`, today)
		if err != nil {
			return false, 0, err
		}
		if err = tx.Commit(); err != nil {
			return false, 0, err
		}
		return true, globalDailyLimit - 1, nil
	} else if err != nil {
		return false, 0, err
	}

	// CRITICAL: Check if global request limit reached
	if count >= globalDailyLimit {
		return false, 0, nil
	}

	// CRITICAL: Check if global token limit reached (1M tokens/day)
	// Assume worst case: 200 input + 300 output = 500 tokens per request
	if totalTokens >= globalTokenLimit-500 {
		return false, 0, nil
	}

	// Increment global request counter
	_, err = tx.Exec(`
		UPDATE global_ai_usage
		SET request_count = request_count + 1, updated_at = CURRENT_TIMESTAMP
		WHERE usage_date = $1
	`, today)
	if err != nil {
		return false, 0, err
	}

	if err = tx.Commit(); err != nil {
		return false, 0, err
	}

	return true, globalDailyLimit - count - 1, nil
}

// Update global token usage after successful API call
// CRITICAL: totalTokens includes thinking tokens from Gemini (charged by Google)
func updateTokenUsage(db *sql.DB, totalTokens, inputTokens, outputTokens int) error {
	today := time.Now().Format("2006-01-02")

	_, err := db.Exec(`
		UPDATE global_ai_usage
		SET total_tokens_used = total_tokens_used + $1,
		    input_tokens = input_tokens + $2,
		    output_tokens = output_tokens + $3,
		    updated_at = CURRENT_TIMESTAMP
		WHERE usage_date = $4
	`, totalTokens, inputTokens, outputTokens, today)

	return err
}

// Update per-user token usage after successful API call
func updateUserTokenUsage(db *sql.DB, uid string, totalTokens int) error {
	_, err := db.Exec(`
		UPDATE ai_usage
		SET tokens_used = tokens_used + $1, updated_at = CURRENT_TIMESTAMP
		WHERE uid = $2
	`, totalTokens, uid)

	return err
}

// Check and enforce per-minute rate limit (15 RPM)
func checkRPMLimit(db *sql.DB) (bool, int, error) {
	// Get current minute timestamp (truncated to minute)
	now := time.Now()
	currentMinute := time.Date(now.Year(), now.Month(), now.Day(), now.Hour(), now.Minute(), 0, 0, now.Location())

	// Start transaction for atomic operation
	tx, err := db.Begin()
	if err != nil {
		return false, 0, err
	}
	defer tx.Rollback()

	// Check current minute's request count
	var count int
	err = tx.QueryRow(`
		SELECT request_count
		FROM ai_rate_limit
		WHERE minute_timestamp = $1
		FOR UPDATE
	`, currentMinute).Scan(&count)

	if err == sql.ErrNoRows {
		// First request this minute - create entry
		_, err = tx.Exec(`
			INSERT INTO ai_rate_limit (minute_timestamp, request_count)
			VALUES ($1, 1)
		`, currentMinute)
		if err != nil {
			return false, 0, err
		}

		// Clean up old rate limit records (older than 5 minutes)
		fiveMinutesAgo := currentMinute.Add(-5 * time.Minute)
		_, _ = tx.Exec(`
			DELETE FROM ai_rate_limit
			WHERE minute_timestamp < $1
		`, fiveMinutesAgo)

		if err = tx.Commit(); err != nil {
			return false, 0, err
		}
		return true, globalRPMLimit - 1, nil
	} else if err != nil {
		return false, 0, err
	}

	// CRITICAL: Check if RPM limit reached (15 requests/minute)
	if count >= globalRPMLimit {
		return false, 0, nil
	}

	// Increment minute counter
	_, err = tx.Exec(`
		UPDATE ai_rate_limit
		SET request_count = request_count + 1
		WHERE minute_timestamp = $1
	`, currentMinute)
	if err != nil {
		return false, 0, err
	}

	if err = tx.Commit(); err != nil {
		return false, 0, err
	}

	return true, globalRPMLimit - count - 1, nil
}

func checkAndIncrementUsage(db *sql.DB, uid string, requestLimit int, tokenLimit int) (bool, int, error) {
	today := time.Now().Format("2006-01-02")

	// Check current usage (both requests and tokens)
	var count int
	var tokensUsed int64
	var lastDate string
	err := db.QueryRow(`
		SELECT usage_count, tokens_used, usage_date::text
		FROM ai_usage
		WHERE uid = $1
	`, uid).Scan(&count, &tokensUsed, &lastDate)

	if err == sql.ErrNoRows {
		// First time user
		_, err = db.Exec(`
			INSERT INTO ai_usage (uid, usage_count, tokens_used, usage_date)
			VALUES ($1, 1, 0, $2)
		`, uid, today)
		return true, requestLimit - 1, err
	} else if err != nil {
		return false, 0, err
	}

	// Reset counter if new day
	if lastDate != today {
		_, err = db.Exec(`
			UPDATE ai_usage
			SET usage_count = 1, tokens_used = 0, usage_date = $1, updated_at = CURRENT_TIMESTAMP
			WHERE uid = $2
		`, today, uid)
		return true, requestLimit - 1, err
	}

	// CRITICAL: Check BOTH limits (requests AND tokens) using subscription limits
	if count >= requestLimit {
		return false, 0, nil  // Request limit reached
	}

	if tokensUsed >= int64(tokenLimit) {
		return false, -1, nil  // Token budget exhausted (use -1 to differentiate)
	}

	// Increment request counter (tokens will be updated after API call)
	_, err = db.Exec(`
		UPDATE ai_usage
		SET usage_count = usage_count + 1, updated_at = CURRENT_TIMESTAMP
		WHERE uid = $1
	`, uid)

	return true, requestLimit - count - 1, err
}

type TokenUsage struct {
	InputTokens  int
	OutputTokens int
	TotalTokens  int
}

func callGeminiAPI(prompt string, isPremium bool, history []ChatMessage) (string, *TokenUsage, error) {
	var apiKey string
	if isPremium {
		apiKey = os.Getenv("GEMINI_API_KEY_PAID")  // With billing, for premium users
		if apiKey == "" {
			return "", nil, fmt.Errorf("GEMINI_API_KEY_PAID not configured")
		}
		log.Printf("[AI] Using PAID API key for premium user")
	} else {
		apiKey = os.Getenv("GEMINI_API_KEY_FREE")  // No billing, for free users
		if apiKey == "" {
			return "", nil, fmt.Errorf("GEMINI_API_KEY_FREE not configured")
		}
		log.Printf("[AI] Using FREE API key for free tier user")
	}

	url := fmt.Sprintf("https://generativelanguage.googleapis.com/v1beta/models/gemini-2.5-flash:generateContent?key=%s", apiKey)

	// Build conversation contents from history + current prompt
	var contents []GeminiContent

	// Add historical messages to build context
	for _, msg := range history {
		role := "user"
		if msg.Role == "assistant" {
			role = "model"  // Gemini uses "model" instead of "assistant"
		}

		contents = append(contents, GeminiContent{
			Role:  role,
			Parts: []GeminiPart{{Text: msg.Content}},
		})
	}

	// Add current user prompt
	contents = append(contents, GeminiContent{
		Role:  "user",
		Parts: []GeminiPart{{Text: prompt}},
	})

	reqBody := GeminiRequest{
		Contents: contents,
		GenerationConfig: &GenerationConfig{
			MaxOutputTokens: 8192,  // Gemini's maximum - allows complete responses
			Temperature:     0.7,
		},
	}

	jsonData, err := json.Marshal(reqBody)
	if err != nil {
		return "", nil, err
	}

	resp, err := http.Post(url, "application/json", bytes.NewBuffer(jsonData))
	if err != nil {
		return "", nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", nil, err
	}

	if resp.StatusCode != http.StatusOK {
		return "", nil, fmt.Errorf("gemini API error: %s", string(body))
	}

	var geminiResp GeminiResponse
	if err := json.Unmarshal(body, &geminiResp); err != nil {
		return "", nil, err
	}

	if len(geminiResp.Candidates) == 0 {
		log.Printf("Gemini returned empty candidates. Full response: %s", string(body))
		return "", nil, fmt.Errorf("no content in response - possibly blocked by safety filters")
	}

	if len(geminiResp.Candidates[0].Content.Parts) == 0 {
		log.Printf("Gemini returned empty parts. Full response: %s", string(body))
		return "", nil, fmt.Errorf("no content in response - empty parts")
	}

	// Extract token usage from response
	tokenUsage := &TokenUsage{
		InputTokens:  geminiResp.UsageMetadata.PromptTokenCount,
		OutputTokens: geminiResp.UsageMetadata.CandidatesTokenCount,
		TotalTokens:  geminiResp.UsageMetadata.TotalTokenCount,
	}

	// Check if response was incomplete due to token limit
	if geminiResp.Candidates[0].FinishReason == "MAX_TOKENS" {
		return "", tokenUsage, fmt.Errorf("INCOMPLETE_RESPONSE")
	}

	return geminiResp.Candidates[0].Content.Parts[0].Text, tokenUsage, nil
}

func ProcessAI(db *sql.DB, firebaseAuth *auth.Client) http.HandlerFunc {
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
		var req AIRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request", http.StatusBadRequest)
			return
		}

		// STEP 1: Check user subscription status and get their limits
		subscriptionInfo, err := getUserSubscriptionInfo(db, token.UID)
		if err != nil {
			log.Printf("Subscription check error: %v", err)
			http.Error(w, "Internal error", http.StatusInternalServerError)
			return
		}

		// Log subscription info for monitoring
		if subscriptionInfo.IsPremium {
			log.Printf("[AI] Premium user %s (Plan: %s) - Limits: %d requests/day, %d tokens/day",
				token.UID, subscriptionInfo.PlanID, subscriptionInfo.DailyRequestLimit, subscriptionInfo.DailyTokenLimit)
		} else {
			log.Printf("[AI] Free user %s - Limits: %d requests/day, %d tokens/day",
				token.UID, subscriptionInfo.DailyRequestLimit, subscriptionInfo.DailyTokenLimit)
		}

		// CRITICAL: Check global limit first (Google Gemini free tier protection)
		globalAllowed, _, err := checkAndIncrementGlobalUsage(db)
		if err != nil {
			log.Printf("Global usage check error: %v", err)
			http.Error(w, "Internal error", http.StatusInternalServerError)
			return
		}

		if !globalAllowed {
			resp := AIResponse{
				Content: "🔒 AI Feature Temporarily Unavailable\n\nOur free AI quota for today has been reached. This helps us keep Zarq free for everyone!\n\n✨ Want unlimited AI access? Upgrade to Premium!\n\nFree quota resets at midnight.",
				Success: false,
				Error:   "Global daily limit reached",
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(resp)
			return
		}

		// CRITICAL: Check per-minute rate limit (15 RPM)
		rpmAllowed, _, err := checkRPMLimit(db)
		if err != nil {
			log.Printf("RPM check error: %v", err)
			http.Error(w, "Internal error", http.StatusInternalServerError)
			return
		}

		if !rpmAllowed {
			resp := AIResponse{
				Content: "⏱️ Too Many Requests\n\nPlease wait a moment. Our AI is processing other requests right now.\n\nTry again in a few seconds!",
				Success: false,
				Error:   "Rate limit exceeded (15 RPM)",
			}
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Retry-After", "60") // Tell client to retry after 60 seconds
			json.NewEncoder(w).Encode(resp)
			return
		}

		// Check per-user usage limit (both requests and tokens) using subscription limits
		allowed, remaining, err := checkAndIncrementUsage(db, token.UID, subscriptionInfo.DailyRequestLimit, subscriptionInfo.DailyTokenLimit)
		if err != nil {
			log.Printf("Usage check error: %v", err)
			http.Error(w, "Internal error", http.StatusInternalServerError)
			return
		}

		if !allowed {
			// Check if it's token budget exhaustion (remaining = -1) or request limit (remaining = 0)
			if remaining == -1 {
				// Token budget exhausted
				resp := AIResponse{
					Content: fmt.Sprintf("🔋 Daily AI Token Budget Exhausted\n\nYou've used all your AI tokens for today (%d tokens).\n\n✨ Upgrade to Premium for:\n• 10x more tokens per day (50,000 tokens)\n• Complete responses guaranteed\n• Priority processing\n\nYour budget resets tomorrow!", subscriptionInfo.DailyTokenLimit),
					Success: false,
					Error:   "Token budget exhausted",
				}
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(resp)
				return
			} else {
				// Request limit reached
				resp := AIResponse{
					Content: fmt.Sprintf("📊 Daily Request Limit Reached\n\nYou've used all %d AI requests for today.\n\n✨ Upgrade to Premium for:\n• 50 requests per day\n• Higher token budget\n• Priority support\n\nYour limit resets tomorrow!", subscriptionInfo.DailyRequestLimit),
					Success: false,
					Error:   "Request limit reached",
				}
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(resp)
				return
			}
		}

		// Build prompt based on action
		var prompt string
		switch req.Action {
	case "chat":
		// General conversational AI for AI Chat Screen
		prompt = req.Text
		case "translate":
			prompt = fmt.Sprintf("Translate the following text to %s. Only provide the translation, nothing else:\n\n%s", req.TargetLanguage, req.Text)
		case "summarize":
			// Language-specific prompts for natural language response
			if req.TargetLanguage != "" && req.TargetLanguage != "English" {
				summarizeInstructions := map[string]string{
					"Spanish":    "Proporciona un breve resumen del siguiente mensaje en 1-2 oraciones:",
					"French":     "Fournissez un bref résumé du message suivant en 1-2 phrases:",
					"German":     "Geben Sie eine kurze Zusammenfassung der folgenden Nachricht in 1-2 Sätzen:",
					"Italian":    "Fornisci un breve riassunto del seguente messaggio in 1-2 frasi:",
					"Portuguese": "Fornisci un breve riassunto del seguente messaggio in 1-2 frasi:",
					"Russian":    "Предоставьте краткое изложение следующего сообщения в 1-2 предложениях:",
					"Chinese":    "请用1-2句话简要概括以下消息:",
					"Japanese":   "次のメッセージを1〜2文で要約してください:",
					"Korean":     "다음 메시지를 1-2문장으로 요약해주세요:",
					"Arabic":     "قدم ملخصًا موجزًا للرسالة التالية في جملة أو جملتين:",
					"Hindi":      "निम्नलिखित संदेश का 1-2 वाक्यों में संक्षिप्त सारांश प्रदान करें:",
					"Bengali":    "নিম্নলিখিত বার্তার একটি সংক্ষিপ্ত সারাংশ 1-2 বাক্যে প্রদান করুন:",
					"Tamil":      "பின்வரும் செய்தியின் சுருக்கமான சுருக்கத்தை 1-2 வாக்கியங்களில் வழங்கவும்:",
					"Telugu":     "కింది సందేశం యొక్క సంక్షిప్త సారాంశాన్ని 1-2 వాక్యాలలో అందించండి:",
					"Marathi":    "खालील संदेशाचा 1-2 वाक्यांमध्ये थोडक्यात सारांश द्या:",
				}
				if instruction, ok := summarizeInstructions[req.TargetLanguage]; ok {
					prompt = fmt.Sprintf("%s\n\n%s", instruction, req.Text)
				} else {
					prompt = fmt.Sprintf("Provide a brief summary of the following message in 1-2 sentences:\n\n%s", req.Text)
				}
			} else {
				prompt = fmt.Sprintf("Provide a brief summary of the following message in 1-2 sentences:\n\n%s", req.Text)
			}
		case "explain":
			// Language-specific prompts for natural language response
			if req.TargetLanguage != "" && req.TargetLanguage != "English" {
				explainInstructions := map[string]string{
					"Spanish":    "Explica el siguiente mensaje en términos simples:",
					"French":     "Expliquez le message suivant en termes simples:",
					"German":     "Erklären Sie die folgende Nachricht in einfachen Worten:",
					"Italian":    "Spiega il seguente messaggio in termini semplici:",
					"Portuguese": "Explique a seguinte mensagem em termos simples:",
					"Russian":    "Объясните следующее сообщение простыми словами:",
					"Chinese":    "用简单的话解释以下消息:",
					"Japanese":   "次のメッセージを簡単な言葉で説明してください:",
					"Korean":     "다음 메시지를 간단한 용어로 설명해주세요:",
					"Arabic":     "اشرح الرسالة التالية بعبارات بسيطة:",
					"Hindi":      "निम्नलिखित संदेश को सरल शब्दों में समझाएं:",
					"Bengali":    "নিম্নলিখিত বার্তাটি সহজ ভাষায় ব্যাখ্যা করুন:",
					"Tamil":      "பின்வரும் செய்தியை எளிய சொற்களில் விளக்கவும்:",
					"Telugu":     "కింది సందేశాన్ని సరళమైన పదాలలో వివరించండి:",
					"Marathi":    "खालील संदेश सोप्या शब्दात समजावून सांगा:",
				}
				if instruction, ok := explainInstructions[req.TargetLanguage]; ok {
					prompt = fmt.Sprintf("%s\n\n%s", instruction, req.Text)
				} else {
					prompt = fmt.Sprintf("Explain the following message in simple terms:\n\n%s", req.Text)
				}
			} else {
				prompt = fmt.Sprintf("Explain the following message in simple terms:\n\n%s", req.Text)
			}
		case "enhance":
			// Language-specific prompts for natural language response
			if req.TargetLanguage != "" && req.TargetLanguage != "English" {
				var instruction string
				switch req.Style {
				case "formal":
					formalInstructions := map[string]string{
						"Spanish":    "Reescribe el siguiente mensaje en un tono profesional y formal:",
						"French":     "Réécrivez le message suivant dans un ton professionnel et formel:",
						"German":     "Schreiben Sie die folgende Nachricht in einem professionellen, formellen Ton um:",
						"Italian":    "Riscrivi il seguente messaggio in un tono professionale e formale:",
						"Portuguese": "Reescreva a seguinte mensagem em um tom profissional e formal:",
						"Russian":    "Перепишите следующее сообщение в профессиональном, формальном тоне:",
						"Chinese":    "以专业、正式的语气重写以下消息:",
						"Japanese":   "次のメッセージをプロフェッショナルでフォーマルなトーンで書き直してください:",
						"Korean":     "다음 메시지를 전문적이고 공식적인 어조로 다시 작성하세요:",
						"Arabic":     "أعد كتابة الرسالة التالية بنبرة احترافية ورسمية:",
						"Hindi":      "निम्नलिखित संदेश को पेशेवर, औपचारिक स्वर में फिर से लिखें:",
						"Bengali":    "নিম্নলিখিত বার্তাটি একটি পেশাদার, আনুষ্ঠানিক স্বরে পুনরায় লিখুন:",
						"Tamil":      "பின்வரும் செய்தியை தொழில்முறை, முறையான தொனியில் மீண்டும் எழுதவும்:",
						"Telugu":     "కింది సందేశాన్ని వృత్తిపరమైన, అధికారిక స్వరంలో తిరిగి వ్రాయండి:",
						"Marathi":    "खालील संदेश व्यावसायिक, औपचारिक स्वरात पुन्हा लिहा:",
					}
					if inst, ok := formalInstructions[req.TargetLanguage]; ok {
						instruction = inst
					}
				case "casual":
					casualInstructions := map[string]string{
						"Spanish":    "Reescribe el siguiente mensaje en un tono casual y amistoso:",
						"French":     "Réécrivez le message suivant dans un ton décontracté et amical:",
						"German":     "Schreiben Sie die folgende Nachricht in einem lockeren, freundlichen Ton um:",
						"Italian":    "Riscrivi il seguente messaggio in un tono casual e amichevole:",
						"Portuguese": "Reescreva a seguinte mensagem em um tom casual e amigável:",
						"Russian":    "Перепишите следующее сообщение в непринужденном, дружелюбном тоне:",
						"Chinese":    "以随意、友好的语气重写以下消息:",
						"Japanese":   "次のメッセージをカジュアルでフレンドリーなトーンで書き直してください:",
						"Korean":     "다음 메시지를 캐주얼하고 친근한 어조로 다시 작성하세요:",
						"Arabic":     "أعد كتابة الرسالة التالية بنبرة عادية وودية:",
						"Hindi":      "निम्नलिखित संदेश को अनौपचारिक, मैत्रीपूर्ण स्वर में फिर से लिखें:",
						"Bengali":    "নিম্নলিখিত বার্তাটি একটি নৈমিত্তিক, বন্ধুত্বপূর্ণ স্বরে পুনরায় লিখুন:",
						"Tamil":      "பின்வரும் செய்தியை சாதாரண, நட்பான தொனியில் மீண்டும் எழுதவும்:",
						"Telugu":     "కింది సందేశాన్ని సాధారణ, స్నేహపూర్వక స్వరంలో తిరిగి వ్రాయండి:",
						"Marathi":    "खालील संदेश अनौपचारिक, मैत्रीपूर्ण स्वरात पुन्हा लिहा:",
					}
					if inst, ok := casualInstructions[req.TargetLanguage]; ok {
						instruction = inst
					}
				case "concise":
					conciseInstructions := map[string]string{
						"Spanish":    "Reescribe el siguiente mensaje para que sea más conciso y claro:",
						"French":     "Réécrivez le message suivant pour qu'il soit plus concis et clair:",
						"German":     "Schreiben Sie die folgende Nachricht prägnanter und klarer um:",
						"Italian":    "Riscrivi il seguente messaggio per renderlo più conciso e chiaro:",
						"Portuguese": "Reescreva a seguinte mensagem para ser mais concisa e clara:",
						"Russian":    "Перепишите следующее сообщение, чтобы оно было более кратким и ясным:",
						"Chinese":    "重写以下消息，使其更简洁明了:",
						"Japanese":   "次のメッセージをより簡潔で明確に書き直してください:",
						"Korean":     "다음 메시지를 더 간결하고 명확하게 다시 작성하세요:",
						"Arabic":     "أعد كتابة الرسالة التالية لتكون أكثر إيجازًا ووضوحًا:",
						"Hindi":      "निम्नलिखित संदेश को अधिक संक्षिप्त और स्पष्ट बनाने के लिए फिर से लिखें:",
						"Bengali":    "নিম্নলিখিত বার্তাটি আরও সংক্ষিপ্ত এবং পরিষ্কার করতে পুনরায় লিখুন:",
						"Tamil":      "பின்வரும் செய்தியை மேலும் சுருக்கமாகவும் தெளிவாகவும் மீண்டும் எழுதவும்:",
						"Telugu":     "కింది సందేశాన్ని మరింత సంక్షిప్తంగా మరియు స్పష్టంగా తిరిగి వ్రాయండి:",
						"Marathi":    "खालील संदेश अधिक संक्षिप्त आणि स्पष्ट करण्यासाठी पुन्हा लिहा:",
					}
					if inst, ok := conciseInstructions[req.TargetLanguage]; ok {
						instruction = inst
					}
				case "fix":
					fixInstructions := map[string]string{
						"Spanish":    "Corrige la gramática y la ortografía en el siguiente mensaje manteniendo el mismo significado:",
						"French":     "Corrigez la grammaire et l'orthographe du message suivant en conservant le même sens:",
						"German":     "Korrigieren Sie Grammatik und Rechtschreibung der folgenden Nachricht, während Sie die gleiche Bedeutung beibehalten:",
						"Italian":    "Correggi la grammatica e l'ortografia nel seguente messaggio mantenendo lo stesso significato:",
						"Portuguese": "Corrija a gramática e a ortografia na seguinte mensagem mantendo o mesmo significado:",
						"Russian":    "Исправьте грамматику и орфографию в следующем сообщении, сохраняя тот же смысл:",
						"Chinese":    "修正以下消息中的语法和拼写错误，同时保持相同的含义:",
						"Japanese":   "次のメッセージの文法とスペルを修正し、同じ意味を保ってください:",
						"Korean":     "다음 메시지의 문법과 맞춤법을 수정하되 동일한 의미를 유지하세요:",
						"Arabic":     "صحح القواعد والإملاء في الرسالة التالية مع الحفاظ على نفس المعنى:",
						"Hindi":      "निम्नलिखित संदेश में व्याकरण और वर्तनी को ठीक करें जबकि अर्थ वही रखें:",
						"Bengali":    "নিম্নলিখিত বার্তায় ব্যাকরণ এবং বানান ঠিক করুন একই অর্থ রেখে:",
						"Tamil":      "பின்வரும் செய்தியில் இலக்கணம் மற்றும் எழுத்துப்பிழைகளை சரிசெய்யவும், அதே பொருளை வைத்துக்கொண்டு:",
						"Telugu":     "కింది సందేశంలో వ్యాకరణం మరియు స్పెల్లింగ్‌ను సరిచేయండి, అదే అర్థాన్ని ఉంచుతూ:",
						"Marathi":    "खालील संदेशातील व्याकरण आणि शब्दलेखन सुधारा, समान अर्थ राखून:",
					}
					if inst, ok := fixInstructions[req.TargetLanguage]; ok {
						instruction = inst
					}
				}

				if instruction != "" {
					prompt = fmt.Sprintf("%s\n\n%s", instruction, req.Text)
				} else {
					// Fallback to English if language not found
					switch req.Style {
					case "formal":
						prompt = fmt.Sprintf("Rewrite the following message in a professional, formal tone:\n\n%s", req.Text)
					case "casual":
						prompt = fmt.Sprintf("Rewrite the following message in a casual, friendly tone:\n\n%s", req.Text)
					case "concise":
						prompt = fmt.Sprintf("Rewrite the following message to be more concise and clear:\n\n%s", req.Text)
					case "fix":
						prompt = fmt.Sprintf("Fix grammar and spelling in the following message while keeping the same meaning:\n\n%s", req.Text)
					default:
						prompt = fmt.Sprintf("Improve the following message:\n\n%s", req.Text)
					}
				}
			} else {
				// English or no target language specified
				switch req.Style {
				case "formal":
					prompt = fmt.Sprintf("Rewrite the following message in a professional, formal tone:\n\n%s", req.Text)
				case "casual":
					prompt = fmt.Sprintf("Rewrite the following message in a casual, friendly tone:\n\n%s", req.Text)
				case "concise":
					prompt = fmt.Sprintf("Rewrite the following message to be more concise and clear:\n\n%s", req.Text)
				case "fix":
					prompt = fmt.Sprintf("Fix grammar and spelling in the following message while keeping the same meaning:\n\n%s", req.Text)
				default:
					prompt = fmt.Sprintf("Improve the following message:\n\n%s", req.Text)
				}
			}
		default:
			http.Error(w, "Invalid action", http.StatusBadRequest)
			return
		}

		// Get chat history for context (only for "chat" action)
		var chatHistory []ChatMessage
		if req.Action == "chat" {
			chatHistory, err = getChatHistory(db, token.UID, subscriptionInfo.IsPremium)
			if err != nil {
				log.Printf("Failed to get chat history: %v", err)
				// Continue without history
				chatHistory = []ChatMessage{}
			}
			log.Printf("[AI] Retrieved %d historical messages for context", len(chatHistory))
		}

		// Call Gemini API with appropriate API key and history
		result, tokenUsage, err := callGeminiAPI(prompt, subscriptionInfo.IsPremium, chatHistory)
		if err != nil {
			log.Printf("Gemini API error: %v", err)

			// Check if response was incomplete due to token limits
			if err.Error() == "INCOMPLETE_RESPONSE" {
				resp := AIResponse{
					Content: "⚠️ Response Incomplete\n\nYour request was too complex for the free tier token limit.\n\n✨ Upgrade to Premium for:\n• Complete AI responses\n• Higher token limits\n• Priority processing\n\nTry simplifying your request or upgrade now!",
					Success: false,
					Error:   "Response incomplete - token limit reached",
				}
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(resp)
				return
			}

			resp := AIResponse{
				Content: "Sorry, I encountered an error processing your request. Please try again.",
				Success: false,
				Error:   err.Error(),
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(resp)
			return
		}

		// CRITICAL: Track token usage in database (including thinking tokens!)
		// Update global token usage
		if err := updateTokenUsage(db, tokenUsage.TotalTokens, tokenUsage.InputTokens, tokenUsage.OutputTokens); err != nil {
			log.Printf("Warning: Failed to update global token usage: %v", err)
			// Don't fail the request, just log the error
		}

		// Update per-user token usage
		if err := updateUserTokenUsage(db, token.UID, tokenUsage.TotalTokens); err != nil {
			log.Printf("Warning: Failed to update user token usage: %v", err)
			// Don't fail the request, just log the error
		}

		// Log token usage for monitoring
		log.Printf("[AI] Token usage - Input: %d, Output: %d, Total: %d",
			tokenUsage.InputTokens, tokenUsage.OutputTokens, tokenUsage.TotalTokens)

		// Save chat history (only for "chat" action)
		if req.Action == "chat" {
			if err := saveChatHistory(db, token.UID, req.Text, result, tokenUsage.TotalTokens); err != nil {
				log.Printf("Warning: Failed to save chat history: %v", err)
				// Don't fail the request, just log the error
			}
		}

		// Success response
		resp := AIResponse{
			Content:   result,
			Success:   true,
			IsOnDevice: false,
		}

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Remaining-Messages", fmt.Sprintf("%d", remaining))
		json.NewEncoder(w).Encode(resp)
	}
}
