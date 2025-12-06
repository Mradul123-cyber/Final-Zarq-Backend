package handlers

import (
	"encoding/base64"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"path/filepath"
	"strconv"
	"time"

	"firebase.google.com/go/v4/auth"
	"github.com/google/uuid"
	"github.com/gorilla/mux"
)

// Global database reference - set this in your main.go
var TaskDB *sql.DB

// Global Firebase Auth client - set this in your main.go
var FirebaseAuthClient *auth.Client

// ============================================
// AUTH HELPER
// ============================================

// getVerifiedTokenForTasks verifies Firebase token and returns user ID
func getVerifiedTokenForTasks(r *http.Request) (*auth.Token, error) {
	if FirebaseAuthClient == nil {
		return nil, fmt.Errorf("firebase auth not initialized")
	}

	authHeader := r.Header.Get("Authorization")
	if authHeader == "" {
		return nil, fmt.Errorf("authorization header required")
	}

	tokenStr := strings.TrimPrefix(authHeader, "Bearer ")
	token, err := FirebaseAuthClient.VerifyIDToken(context.Background(), tokenStr)
	if err != nil {
		return nil, fmt.Errorf("invalid token: %v", err)
	}

	return token, nil
}

// ============================================
// TASK MANAGEMENT ENDPOINTS
// ============================================

// GetTodayTask returns today's daily tasks (3 tasks: easy, medium, hard)
func GetTodayTask(w http.ResponseWriter, r *http.Request) {
	// Verify authentication
	token, err := getVerifiedTokenForTasks(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	userID := token.UID

	// Get today's 3 tasks
	query := `
		SELECT id, task_date, title_en, title_hi, description_en, description_hi,
		       category, difficulty, points, require_verification, created_at
		FROM daily_tasks
		WHERE task_date = CURRENT_DATE AND is_active = TRUE
		ORDER BY
			CASE difficulty
				WHEN 'easy' THEN 1
				WHEN 'medium' THEN 2
				WHEN 'hard' THEN 3
			END
		LIMIT 3
	`

	rows, err := TaskDB.Query(query)
	if err != nil {
		log.Printf("[GetTodayTask] Database error: %v", err)
		http.Error(w, "Failed to fetch tasks", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var tasks []map[string]interface{}
	for rows.Next() {
		var task DailyTask
		err := rows.Scan(
			&task.ID, &task.TaskDate, &task.TitleEN, &task.TitleHI,
			&task.DescriptionEN, &task.DescriptionHI, &task.Category,
			&task.Difficulty, &task.Points, &task.RequireVerification,
			&task.CreatedAt,
		)
		if err != nil {
			log.Printf("[GetTodayTask] Scan error: %v", err)
			continue
		}

		// Check if user completed this specific task today
		var completed bool
		checkQuery := `
			SELECT EXISTS(
				SELECT 1 FROM task_submissions
				WHERE user_id = $1
				  AND task_id = $2
				  AND ai_verified = TRUE
				  AND DATE(created_at) = CURRENT_DATE
			)
		`
		TaskDB.QueryRow(checkQuery, userID, task.ID).Scan(&completed)

		tasks = append(tasks, map[string]interface{}{
			"id":                   task.ID,
			"task_date":            task.TaskDate,
			"title_en":             task.TitleEN,
			"title_hi":             task.TitleHI,
			"description_en":       task.DescriptionEN,
			"description_hi":       task.DescriptionHI,
			"category":             task.Category,
			"difficulty":           task.Difficulty,
			"points":               task.Points,
			"require_verification": task.RequireVerification,
			"created_at":           task.CreatedAt,
			"completed":            completed,
		})
	}

	if len(tasks) == 0 {
		http.Error(w, "No tasks available for today", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"tasks": tasks,
	})
}

// SubmitTask handles task submission with media upload
func SubmitTask(w http.ResponseWriter, r *http.Request) {
	// Verify authentication
	token, err := getVerifiedTokenForTasks(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	userID := token.UID

	// Parse multipart form (max 100MB)
	err = r.ParseMultipartForm(100 << 20)
	if err != nil {
		http.Error(w, "Failed to parse form", http.StatusBadRequest)
		return
	}

	// Get form values
	taskIDStr := r.FormValue("task_id")
	isTaskSubmission := r.FormValue("is_task_submission") == "true"
	mediaType := r.FormValue("media_type") // "image" or "video"
	caption := r.FormValue("caption")
	visibility := r.FormValue("visibility") // "friends" or "global"

	// Validate required fields
	if mediaType != "image" && mediaType != "video" {
		http.Error(w, "Invalid media_type", http.StatusBadRequest)
		return
	}
	if visibility != "friends" && visibility != "global" {
		http.Error(w, "Invalid visibility", http.StatusBadRequest)
		return
	}

	// Get uploaded file
	file, header, err := r.FormFile("media")
	if err != nil {
		http.Error(w, "Media file is required", http.StatusBadRequest)
		return
	}
	defer file.Close()

	// Validate file size (max 50MB for images, 100MB for videos)
	maxSize := int64(50 << 20) // 50MB
	if mediaType == "video" {
		maxSize = 100 << 20 // 100MB
	}
	if header.Size > maxSize {
		http.Error(w, "File too large", http.StatusBadRequest)
		return
	}

	// Save file to server
	filename := fmt.Sprintf("%d_%s%s", time.Now().Unix(), uuid.New().String()[:8], filepath.Ext(header.Filename))
	var uploadPath string
	if mediaType == "image" {
		uploadPath = filepath.Join("uploads/tasks/images", filename)
	} else {
		uploadPath = filepath.Join("uploads/tasks/videos", filename)
	}

	// Create file on disk
	dst, err := os.Create(uploadPath)
	if err != nil {
		log.Printf("[SubmitTask] Failed to create file: %v", err)
		http.Error(w, "Failed to save file", http.StatusInternalServerError)
		return
	}
	defer dst.Close()

	// Copy uploaded file to destination
	if _, err := io.Copy(dst, file); err != nil {
		log.Printf("[SubmitTask] Failed to copy file: %v", err)
		http.Error(w, "Failed to save file", http.StatusInternalServerError)
		return
	}

	// Generate media URL (accessible via your domain)
	mediaURL := fmt.Sprintf("https://zarqmessenger.com/%s", uploadPath)

	// Create submission in database
	submissionID := uuid.New().String()
	expiresAt := time.Now().Add(24 * time.Hour) // 24-hour expiry

	var taskID *int
	if isTaskSubmission && taskIDStr != "" {
		tid, _ := strconv.Atoi(taskIDStr)
		taskID = &tid
	}

	query := `
		INSERT INTO task_submissions (
			id, user_id, task_id, is_task_submission, media_type,
			media_url, caption, visibility, expires_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		RETURNING id, created_at
	`

	var submission struct {
		ID        string    `json:"id"`
		CreatedAt time.Time `json:"created_at"`
	}

	err = TaskDB.QueryRow(
		query, submissionID, userID, taskID, isTaskSubmission,
		mediaType, mediaURL, caption, visibility, expiresAt,
	).Scan(&submission.ID, &submission.CreatedAt)

	if err != nil {
		log.Printf("[SubmitTask] Database error: %v", err)
		http.Error(w, "Failed to create submission", http.StatusInternalServerError)
		return
	}

	// If it's a task submission and requires verification, verify with AI
	if isTaskSubmission && taskID != nil {
		go verifyTaskWithAI(submissionID, *taskID, mediaURL, mediaType)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":      true,
		"submission_id": submission.ID,
		"created_at":    submission.CreatedAt,
		"message":       "Submission created successfully",
	})
}

// verifyTaskWithAI uses Gemini Vision API to verify task completion
func verifyTaskWithAI(submissionID string, taskID int, mediaURL string, mediaType string) {
	// Get task verification prompt
	var prompt string
	var requiredVerification bool
	err := TaskDB.QueryRow(
		"SELECT ai_verification_prompt, require_verification FROM daily_tasks WHERE id = $1",
		taskID,
	).Scan(&prompt, &requiredVerification)

	if err != nil {
		log.Printf("[verifyTaskWithAI] Error fetching task: %v", err)
		return
	}

	// If verification not required, auto-approve
	if !requiredVerification || prompt == "" {
		log.Printf("[verifyTaskWithAI] Task %d does not require verification, auto-approving", taskID)
		updateQuery := `
			UPDATE task_submissions
			SET ai_verified = TRUE,
			    ai_verification_confidence = 100,
			    points_earned = (SELECT points FROM daily_tasks WHERE id = $2)
			WHERE id = $1
		`
		_, err = TaskDB.Exec(updateQuery, submissionID, taskID)
		if err != nil {
			log.Printf("[verifyTaskWithAI] Failed to update submission: %v", err)
		}
		return
	}

	// Get API key
	apiKey := os.Getenv("GEMINI_API_KEY")
	if apiKey == "" {
		log.Println("[verifyTaskWithAI] GEMINI_API_KEY not set")
		return
	}

	// Extract file path from URL
	// Example: "http://64.227.191.148/uploads/tasks/images/file.jpg" -> "uploads/tasks/images/file.jpg"
	filePath := extractFilePath(mediaURL)
	if filePath == "" {
		log.Printf("[verifyTaskWithAI] Could not extract file path from URL: %s", mediaURL)
		return
	}

	// Read file and convert to base64
	fileData, err := os.ReadFile(filePath)
	if err != nil {
		log.Printf("[verifyTaskWithAI] Failed to read file %s: %v", filePath, err)
		return
	}

	base64Data := base64.StdEncoding.EncodeToString(fileData)

	// Determine MIME type
	var mimeType string
	if mediaType == "image" {
		// Try to detect exact image type
		ext := filepath.Ext(filePath)
		switch ext {
		case ".jpg", ".jpeg":
			mimeType = "image/jpeg"
		case ".png":
			mimeType = "image/png"
		case ".gif":
			mimeType = "image/gif"
		case ".webp":
			mimeType = "image/webp"
		default:
			mimeType = "image/jpeg"
		}
	} else if mediaType == "video" {
		ext := filepath.Ext(filePath)
		switch ext {
		case ".mp4":
			mimeType = "video/mp4"
		case ".mov":
			mimeType = "video/quicktime"
		case ".avi":
			mimeType = "video/x-msvideo"
		default:
			mimeType = "video/mp4"
		}
	}

	// Prepare request to Gemini 2.5 Flash
	geminiURL := "https://generativelanguage.googleapis.com/v1beta/models/gemini-2.5-flash:generateContent?key=" + apiKey

	// Create strict verification prompt
	strictPrompt := fmt.Sprintf(`You are a strict task verifier. Analyze this image carefully.

Task requirement: %s

Instructions:
1. Look ONLY for what the task specifically asks for
2. Be STRICT - if anything is missing or incorrect, return NO
3. Ignore unrelated objects in the image
4. Respond with ONLY one word: either YES or NO

Does this image EXACTLY match the task requirement?`, prompt)

	requestBody := map[string]interface{}{
		"contents": []map[string]interface{}{
			{
				"parts": []map[string]interface{}{
					{"text": strictPrompt},
					{
						"inline_data": map[string]string{
							"mime_type": mimeType,
							"data":      base64Data,
						},
					},
				},
			},
		},
	}

	jsonData, err := json.Marshal(requestBody)
	if err != nil {
		log.Printf("[verifyTaskWithAI] Failed to marshal request: %v", err)
		return
	}

	// Send request to Gemini
	resp, err := http.Post(geminiURL, "application/json", bytes.NewBuffer(jsonData))
	if err != nil {
		log.Printf("[verifyTaskWithAI] Gemini API request error: %v", err)
		return
	}
	defer resp.Body.Close()

	// Read response
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("[verifyTaskWithAI] Failed to read response: %v", err)
		return
	}

	// Parse response
	var geminiResp map[string]interface{}
	if err := json.Unmarshal(body, &geminiResp); err != nil {
		log.Printf("[verifyTaskWithAI] Failed to parse response: %v", err)
		log.Printf("[verifyTaskWithAI] Response body: %s", string(body))
		return
	}

	// Extract text from response
	// Response structure: {"candidates": [{"content": {"parts": [{"text": "..."}]}}]}
	verified := false
	confidence := 0

	if candidates, ok := geminiResp["candidates"].([]interface{}); ok && len(candidates) > 0 {
		if candidate, ok := candidates[0].(map[string]interface{}); ok {
			if content, ok := candidate["content"].(map[string]interface{}); ok {
				if parts, ok := content["parts"].([]interface{}); ok && len(parts) > 0 {
					if part, ok := parts[0].(map[string]interface{}); ok {
						if text, ok := part["text"].(string); ok {
							text = strings.ToUpper(strings.TrimSpace(text))
							log.Printf("[verifyTaskWithAI] Gemini response: %s", text)

							if strings.Contains(text, "YES") {
								verified = true
								confidence = 85
							} else if strings.Contains(text, "NO") {
								verified = false
								confidence = 80
							} else {
								// Ambiguous response
								verified = false
								confidence = 50
							}
						}
					}
				}
			}
		}
	}

	// Update submission with verification result
	updateQuery := `
		UPDATE task_submissions
		SET ai_verified = $2,
		    ai_verification_confidence = $3,
		    points_earned = CASE WHEN $2 = TRUE THEN (SELECT points FROM daily_tasks WHERE id = $4) ELSE 0 END
		WHERE id = $1
	`

	_, err = TaskDB.Exec(updateQuery, submissionID, verified, confidence, taskID)
	if err != nil {
		log.Printf("[verifyTaskWithAI] Failed to update submission: %v", err)
	} else {
		log.Printf("[verifyTaskWithAI] Submission %s verified: %v (confidence: %d%%)", submissionID, verified, confidence)
	}
}

// extractFilePath extracts local filepath from URL
// Example: "http://64.227.191.148/uploads/tasks/images/file.jpg" -> "uploads/tasks/images/file.jpg"
func extractFilePath(url string) string {
	// Find "uploads/" in the URL
	idx := -1
	for i := 0; i < len(url)-7; i++ {
		if url[i:i+8] == "uploads/" {
			idx = i
			break
		}
	}

	if idx == -1 {
		return ""
	}

	return url[idx:]
}

func GetFriendsFeed(w http.ResponseWriter, r *http.Request) {
	// Verify authentication
	token, err := getVerifiedTokenForTasks(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	userID := token.UID

	// Get pagination params
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 50 {
		limit = 20
	}
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))

	// Get friends + user's own submissions
	query := `
		SELECT
			s.id, s.user_id, s.task_id, s.media_type, s.media_url,
			s.thumbnail_url, s.caption, s.points_earned, s.created_at,
			p.username, p.display_name, p.profile_avatar_url,
			COUNT(DISTINCT r.id) as reaction_count,
			COUNT(DISTINCT c.id) as comment_count,
			ur.reaction_type as user_reaction
		FROM task_submissions s
		JOIN profiles p ON s.user_id = p.firebase_uid
		LEFT JOIN task_reactions r ON s.id = r.submission_id
		LEFT JOIN task_comments c ON s.id = c.submission_id AND c.is_deleted = FALSE
		LEFT JOIN task_reactions ur ON s.id = ur.submission_id AND ur.user_id = $1
		WHERE s.visibility = 'friends'
		  AND s.is_deleted = FALSE
		  AND s.expires_at > NOW()
		  AND s.ai_verified = TRUE
		  AND (
			  s.user_id = $1
			  OR s.user_id IN (
				  SELECT
					  CASE
						  WHEN user_a_uid = $1 THEN user_b_uid
						  ELSE user_a_uid
					  END
				  FROM friendships
				  WHERE (user_a_uid = $1 OR user_b_uid = $1)
					AND status = 'accepted'
			  )
		  )
		GROUP BY s.id, p.username, p.display_name, p.profile_avatar_url, ur.reaction_type
		ORDER BY s.created_at DESC
		LIMIT $2 OFFSET $3
	`

	rows, err := TaskDB.Query(query, userID, limit, offset)
	if err != nil {
		log.Printf("[GetFriendsFeed] Query error: %v", err)
		http.Error(w, "Failed to fetch feed", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var submissions []map[string]interface{}
	for rows.Next() {
		var s struct {
			ID                string
			UserID            string
			TaskID            *int
			MediaType         string
			MediaURL          string
			ThumbnailURL      sql.NullString
			Caption           sql.NullString
			PointsEarned      int
			CreatedAt         time.Time
			Username          string
			DisplayName       sql.NullString
			ProfilePictureURL sql.NullString
			ReactionCount     int
			CommentCount      int
			UserReaction      sql.NullString
		}

		err := rows.Scan(
			&s.ID, &s.UserID, &s.TaskID, &s.MediaType, &s.MediaURL,
			&s.ThumbnailURL, &s.Caption, &s.PointsEarned, &s.CreatedAt,
			&s.Username, &s.DisplayName, &s.ProfilePictureURL, &s.ReactionCount,
			&s.CommentCount, &s.UserReaction,
		)

		if err != nil {
			log.Printf("[GetFriendsFeed] Scan error: %v", err)
			continue
		}

		// Get reaction breakdown by type
		reactionBreakdown := make(map[string]int)
		breakdownQuery := `
			SELECT reaction_type, COUNT(*) as count
			FROM task_reactions
			WHERE submission_id = $1
			GROUP BY reaction_type
		`
		breakdownRows, err := TaskDB.Query(breakdownQuery, s.ID)
		if err == nil {
			defer breakdownRows.Close()
			for breakdownRows.Next() {
				var reactionType string
				var count int
				if err := breakdownRows.Scan(&reactionType, &count); err == nil {
					reactionBreakdown[reactionType] = count
				}
			}
		}

		submission := map[string]interface{}{
			"id":             s.ID,
			"user_id":        s.UserID,
			"username":       s.Username,
			"media_type":     s.MediaType,
			"media_url":      s.MediaURL,
			"caption":        s.Caption.String,
			"points_earned":  s.PointsEarned,
			"ai_verified":    true,
			"task_title":     "Daily Task",
			"created_at":     s.CreatedAt,
			"reaction_count": s.ReactionCount,
			"comment_count":  s.CommentCount,
			"reactions":      reactionBreakdown,
		}

		if s.DisplayName.Valid && s.DisplayName.String != "" {
			submission["display_name"] = s.DisplayName.String
		}
		if s.ProfilePictureURL.Valid {
			submission["user_avatar_url"] = s.ProfilePictureURL.String
		}
		if s.UserReaction.Valid {
			submission["user_reaction"] = s.UserReaction.String
		}

		submissions = append(submissions, submission)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"submissions": submissions,
		"has_more":    len(submissions) == limit,
	})
}

func GetGlobalFeed(w http.ResponseWriter, r *http.Request) {
	// Verify authentication
	token, err := getVerifiedTokenForTasks(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	userID := token.UID

	// Get pagination params
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 50 {
		limit = 20
	}
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))

	query := `
		SELECT
			s.id, s.user_id, s.task_id, s.media_type, s.media_url,
			s.thumbnail_url, s.caption, s.points_earned, s.created_at,
			p.username, p.display_name, p.profile_avatar_url,
			COUNT(DISTINCT r.id) as reaction_count,
			COUNT(DISTINCT c.id) as comment_count,
			ur.reaction_type as user_reaction
		FROM task_submissions s
		JOIN profiles p ON s.user_id = p.firebase_uid
		LEFT JOIN task_reactions r ON s.id = r.submission_id
		LEFT JOIN task_comments c ON s.id = c.submission_id AND c.is_deleted = FALSE
		LEFT JOIN task_reactions ur ON s.id = ur.submission_id AND ur.user_id = $1
		WHERE s.visibility = 'global'
		  AND s.is_deleted = FALSE
		  AND s.expires_at > NOW()
		  AND s.ai_verified = TRUE
		GROUP BY s.id, p.username, p.display_name, p.profile_avatar_url, ur.reaction_type
		ORDER BY s.created_at DESC
		LIMIT $2 OFFSET $3
	`

	rows, err := TaskDB.Query(query, userID, limit, offset)
	if err != nil {
		log.Printf("[GetGlobalFeed] Query error: %v", err)
		http.Error(w, "Failed to fetch feed", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var submissions []map[string]interface{}
	for rows.Next() {
		var s struct {
			ID                string
			UserID            string
			TaskID            *int
			MediaType         string
			MediaURL          string
			ThumbnailURL      sql.NullString
			Caption           sql.NullString
			PointsEarned      int
			CreatedAt         time.Time
			Username          string
			DisplayName       sql.NullString
			ProfilePictureURL sql.NullString
			ReactionCount     int
			CommentCount      int
			UserReaction      sql.NullString
		}

		err := rows.Scan(
			&s.ID, &s.UserID, &s.TaskID, &s.MediaType, &s.MediaURL,
			&s.ThumbnailURL, &s.Caption, &s.PointsEarned, &s.CreatedAt,
			&s.Username, &s.DisplayName, &s.ProfilePictureURL, &s.ReactionCount,
			&s.CommentCount, &s.UserReaction,
		)

		if err != nil {
			log.Printf("[GetGlobalFeed] Scan error: %v", err)
			continue
		}

		// Get reaction breakdown by type
		reactionBreakdown := make(map[string]int)
		breakdownQuery := `
			SELECT reaction_type, COUNT(*) as count
			FROM task_reactions
			WHERE submission_id = $1
			GROUP BY reaction_type
		`
		breakdownRows, err := TaskDB.Query(breakdownQuery, s.ID)
		if err == nil {
			defer breakdownRows.Close()
			for breakdownRows.Next() {
				var reactionType string
				var count int
				if err := breakdownRows.Scan(&reactionType, &count); err == nil {
					reactionBreakdown[reactionType] = count
				}
			}
		}

		submission := map[string]interface{}{
			"id":             s.ID,
			"user_id":        s.UserID,
			"username":       s.Username,
			"media_type":     s.MediaType,
			"media_url":      s.MediaURL,
			"caption":        s.Caption.String,
			"points_earned":  s.PointsEarned,
			"ai_verified":    true,
			"task_title":     "Daily Task",
			"created_at":     s.CreatedAt,
			"reaction_count": s.ReactionCount,
			"comment_count":  s.CommentCount,
			"reactions":      reactionBreakdown,
		}

		if s.DisplayName.Valid && s.DisplayName.String != "" {
			submission["display_name"] = s.DisplayName.String
		}
		if s.ProfilePictureURL.Valid {
			submission["user_avatar_url"] = s.ProfilePictureURL.String
		}
		if s.UserReaction.Valid {
			submission["user_reaction"] = s.UserReaction.String
		}

		submissions = append(submissions, submission)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"submissions": submissions,
		"has_more":    len(submissions) == limit,
	})
}

// GetUserStats returns user's gamification stats
func GetUserStats(w http.ResponseWriter, r *http.Request) {
	// Verify authentication
	token, err := getVerifiedTokenForTasks(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	userID := token.UID

	var stats struct {
		TotalPoints   int       `json:"total_points"`
		CurrentStreak int       `json:"current_streak"`
		LongestStreak int       `json:"longest_streak"`
		TasksCompleted int      `json:"tasks_completed"`
		Rank          int       `json:"rank"`
		WeeklyPoints  int       `json:"weekly_points"`
		WeeklyRank    int       `json:"weekly_rank"`
	}

	query := `
		SELECT total_points, current_streak, longest_streak,
		       tasks_completed, rank, weekly_points, weekly_rank
		FROM user_task_stats
		WHERE user_id = $1
	`

	err = TaskDB.QueryRow(query, userID).Scan(
		&stats.TotalPoints, &stats.CurrentStreak, &stats.LongestStreak,
		&stats.TasksCompleted, &stats.Rank, &stats.WeeklyPoints,
		&stats.WeeklyRank,
	)

	if err == sql.ErrNoRows {
		// User hasn't completed any tasks yet - create initial stats record
		insertQuery := `
			INSERT INTO user_task_stats (user_id, total_points, current_streak, longest_streak, tasks_completed, rank, weekly_points, weekly_rank)
			VALUES ($1, 0, 0, 0, 0, 0, 0, 0)
			ON CONFLICT (user_id) DO NOTHING
		`
		_, insertErr := TaskDB.Exec(insertQuery, userID)
		if insertErr != nil {
			log.Printf("[GetUserStats] Failed to create initial stats: %v", insertErr)
		}

		// Return zeros
		stats = struct {
			TotalPoints   int `json:"total_points"`
			CurrentStreak int `json:"current_streak"`
			LongestStreak int `json:"longest_streak"`
			TasksCompleted int `json:"tasks_completed"`
			Rank          int `json:"rank"`
			WeeklyPoints  int `json:"weekly_points"`
			WeeklyRank    int `json:"weekly_rank"`
		}{0, 0, 0, 0, 0, 0, 0}
	} else if err != nil {
		log.Printf("[GetUserStats] Database error: %v", err)
		http.Error(w, "Failed to fetch stats", http.StatusInternalServerError)
		return
	}

	// Get task breakdown by difficulty
	breakdown := map[string]int{
		"easy":   0,
		"medium": 0,
		"hard":   0,
	}

	breakdownQuery := `
		SELECT dt.difficulty, COUNT(*) as count
		FROM task_submissions ts
		JOIN daily_tasks dt ON ts.task_id = dt.id
		WHERE ts.user_id = $1
		  AND ts.is_task_submission = TRUE
		  AND ts.ai_verified = TRUE
		GROUP BY dt.difficulty
	`

	rows, err := TaskDB.Query(breakdownQuery, userID)
	if err != nil {
		log.Printf("[GetUserStats] Breakdown query error: %v", err)
	} else {
		defer rows.Close()

		for rows.Next() {
			var difficulty string
			var count int
			if err := rows.Scan(&difficulty, &count); err == nil {
				breakdown[strings.ToLower(difficulty)] = count
				log.Printf("[GetUserStats] Breakdown: %s = %d", difficulty, count)
			}
		}
	}

	log.Printf("[GetUserStats] Final breakdown: %v", breakdown)

	// Create response with breakdown
	response := map[string]interface{}{
		"total_points":        stats.TotalPoints,
		"current_streak":      stats.CurrentStreak,
		"longest_streak":      stats.LongestStreak,
		"tasks_completed":     stats.TasksCompleted,
		"rank":                stats.Rank,
		"weekly_points":       stats.WeeklyPoints,
		"weekly_rank":         stats.WeeklyRank,
		"tasks_by_difficulty": breakdown,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

// ReactToSubmission adds/removes a reaction
func ReactToSubmission(w http.ResponseWriter, r *http.Request) {
	// Verify authentication
	token, err := getVerifiedTokenForTasks(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	userID := token.UID

	submissionID := mux.Vars(r)["id"]

	var req struct {
		ReactionType string `json:"reaction_type"` // like, love, fire, clap, wow
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}

	// Validate reaction type
	validReactions := map[string]bool{
		"like": true, "love": true, "fire": true, "clap": true, "wow": true,
	}
	if !validReactions[req.ReactionType] {
		http.Error(w, "Invalid reaction type", http.StatusBadRequest)
		return
	}

	// Insert or update reaction
	query := `
		INSERT INTO task_reactions (submission_id, user_id, reaction_type)
		VALUES ($1, $2, $3)
		ON CONFLICT (submission_id, user_id)
		DO UPDATE SET reaction_type = $3
	`

	_, err = TaskDB.Exec(query, submissionID, userID, req.ReactionType)
	if err != nil {
		log.Printf("[ReactToSubmission] Database error: %v", err)
		http.Error(w, "Failed to add reaction", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"message": "Reaction added successfully",
	})
}

// RemoveReaction removes a reaction
func RemoveReaction(w http.ResponseWriter, r *http.Request) {
	// Verify authentication
	token, err := getVerifiedTokenForTasks(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	userID := token.UID

	submissionID := mux.Vars(r)["id"]

	query := "DELETE FROM task_reactions WHERE submission_id = $1 AND user_id = $2"

	_, err = TaskDB.Exec(query, submissionID, userID)
	if err != nil {
		log.Printf("[RemoveReaction] Database error: %v", err)
		http.Error(w, "Failed to remove reaction", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"message": "Reaction removed successfully",
	})
}

// GetSubmissionStatus returns verification status of a submission
func GetSubmissionStatus(w http.ResponseWriter, r *http.Request) {
	// Verify authentication
	token, err := getVerifiedTokenForTasks(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	userID := token.UID

	submissionID := mux.Vars(r)["id"]

	var result struct {
		ID                       string `json:"id"`
		AIVerified               bool   `json:"ai_verified"`
		AIVerificationConfidence int    `json:"ai_verification_confidence"`
		PointsEarned             int    `json:"points_earned"`
		IsTaskSubmission         bool   `json:"is_task_submission"`
	}

	query := `
		SELECT id, ai_verified, ai_verification_confidence, points_earned, is_task_submission
		FROM task_submissions
		WHERE id = $1 AND user_id = $2
	`

	err = TaskDB.QueryRow(query, submissionID, userID).Scan(
		&result.ID, &result.AIVerified, &result.AIVerificationConfidence,
		&result.PointsEarned, &result.IsTaskSubmission,
	)

	if err == sql.ErrNoRows {
		http.Error(w, "Submission not found", http.StatusNotFound)
		return
	}
	if err != nil {
		log.Printf("[GetSubmissionStatus] Database error: %v", err)
		http.Error(w, "Failed to fetch submission", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

// ReportSubmission creates a report
func ReportSubmission(w http.ResponseWriter, r *http.Request) {
	// Verify authentication
	token, err := getVerifiedTokenForTasks(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	userID := token.UID

	submissionID := mux.Vars(r)["id"]

	var req struct {
		Reason  string `json:"reason"`  // inappropriate, spam, fake, violence, hate
		Details string `json:"details"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}

	query := `
		INSERT INTO task_reports (submission_id, reporter_user_id, reason, details)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (submission_id, reporter_user_id) DO NOTHING
	`

	_, err = TaskDB.Exec(query, submissionID, userID, req.Reason, req.Details)
	if err != nil {
		log.Printf("[ReportSubmission] Database error: %v", err)
		http.Error(w, "Failed to submit report", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"message": "Report submitted successfully",
	})
}

// Helper types (you'll need to move DailyTask to your main models or import it)
type DailyTask struct {
	ID                   int       `json:"id"`
	TaskDate             time.Time `json:"task_date"`
	TitleEN              string    `json:"title_en"`
	TitleHI              string    `json:"title_hi"`
	DescriptionEN        string    `json:"description_en"`
	DescriptionHI        string    `json:"description_hi"`
	Category             string    `json:"category"`
	Difficulty           string    `json:"difficulty"`
	Points               int       `json:"points"`
	RequireVerification  bool      `json:"require_verification"`
	CreatedAt            time.Time `json:"created_at"`
}
