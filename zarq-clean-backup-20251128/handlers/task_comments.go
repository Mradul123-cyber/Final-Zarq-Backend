package handlers

import (
	"database/sql"
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/gorilla/mux"
)

// ============================================
// COMMENT ENDPOINTS
// ============================================

// GetComments returns all comments for a submission
func GetComments(w http.ResponseWriter, r *http.Request) {
	// Verify authentication
	_, err := getVerifiedTokenForTasks(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}

	submissionID := mux.Vars(r)["id"]

	// Get pagination params
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))

	query := `
		SELECT
			c.id, c.user_id, c.comment_text, c.created_at,
			p.username, p.display_name, p.profile_avatar_url
		FROM task_comments c
		JOIN profiles p ON c.user_id = p.firebase_uid
		WHERE c.submission_id = $1 AND c.is_deleted = FALSE
		ORDER BY c.created_at ASC
		LIMIT $2 OFFSET $3
	`

	rows, err := TaskDB.Query(query, submissionID, limit, offset)
	if err != nil {
		log.Printf("[GetComments] Query error: %v", err)
		http.Error(w, "Failed to fetch comments", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var comments []map[string]interface{}
	for rows.Next() {
		var c struct {
			ID                int
			UserID            string
			CommentText       string
			CreatedAt         time.Time
			Username          string
			DisplayName       sql.NullString
			ProfilePictureURL sql.NullString
		}

		err := rows.Scan(
			&c.ID, &c.UserID, &c.CommentText, &c.CreatedAt,
			&c.Username, &c.DisplayName, &c.ProfilePictureURL,
		)

		if err != nil {
			log.Printf("[GetComments] Scan error: %v", err)
			continue
		}

		comment := map[string]interface{}{
			"id":           c.ID,
			"user_id":      c.UserID,
			"username":     c.Username,
			"comment_text": c.CommentText,
			"created_at":   c.CreatedAt,
		}

		if c.DisplayName.Valid && c.DisplayName.String != "" {
			comment["display_name"] = c.DisplayName.String
		}

		if c.ProfilePictureURL.Valid {
			comment["user_avatar_url"] = c.ProfilePictureURL.String
		}

		comments = append(comments, comment)
	}

	if comments == nil {
		comments = []map[string]interface{}{}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"comments": comments,
		"has_more": len(comments) == limit,
	})
}

// PostComment adds a new comment to a submission
func PostComment(w http.ResponseWriter, r *http.Request) {
	// Verify authentication
	token, err := getVerifiedTokenForTasks(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	userID := token.UID

	submissionID := mux.Vars(r)["id"]

	var req struct {
		CommentText string `json:"comment_text"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}

	// Validate comment text
	if len(req.CommentText) == 0 {
		http.Error(w, "Comment text is required", http.StatusBadRequest)
		return
	}
	if len(req.CommentText) > 500 {
		http.Error(w, "Comment too long (max 500 characters)", http.StatusBadRequest)
		return
	}

	// Insert comment
	query := `
		INSERT INTO task_comments (submission_id, user_id, comment_text)
		VALUES ($1, $2, $3)
		RETURNING id, created_at
	`

	var commentID int
	var createdAt time.Time
	err = TaskDB.QueryRow(query, submissionID, userID, req.CommentText).Scan(&commentID, &createdAt)
	if err != nil {
		log.Printf("[PostComment] Database error: %v", err)
		http.Error(w, "Failed to post comment", http.StatusInternalServerError)
		return
	}

	// Get user info for response
	var username string
	var displayName sql.NullString
	var avatarURL sql.NullString
	userQuery := "SELECT username, display_name, profile_avatar_url FROM profiles WHERE firebase_uid = $1"
	TaskDB.QueryRow(userQuery, userID).Scan(&username, &displayName, &avatarURL)

	response := map[string]interface{}{
		"id":           commentID,
		"user_id":      userID,
		"username":     username,
		"comment_text": req.CommentText,
		"created_at":   createdAt,
	}

	if displayName.Valid && displayName.String != "" {
		response["display_name"] = displayName.String
	}

	if avatarURL.Valid {
		response["user_avatar_url"] = avatarURL.String
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

// DeleteComment soft-deletes a comment (only by comment owner)
func DeleteComment(w http.ResponseWriter, r *http.Request) {
	// Verify authentication
	token, err := getVerifiedTokenForTasks(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	userID := token.UID

	commentIDStr := mux.Vars(r)["id"]
	commentID, err := strconv.Atoi(commentIDStr)
	if err != nil {
		http.Error(w, "Invalid comment ID", http.StatusBadRequest)
		return
	}

	// Soft delete comment (only if user owns it)
	query := `
		UPDATE task_comments
		SET is_deleted = TRUE, updated_at = CURRENT_TIMESTAMP
		WHERE id = $1 AND user_id = $2 AND is_deleted = FALSE
	`

	result, err := TaskDB.Exec(query, commentID, userID)
	if err != nil {
		log.Printf("[DeleteComment] Database error: %v", err)
		http.Error(w, "Failed to delete comment", http.StatusInternalServerError)
		return
	}

	rowsAffected, _ := result.RowsAffected()
	if rowsAffected == 0 {
		http.Error(w, "Comment not found or unauthorized", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"message": "Comment deleted successfully",
	})
}
