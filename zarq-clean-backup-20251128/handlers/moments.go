package handlers

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/mux"
)

// Global database reference for moments (set in main.go)
var MomentsDB *sql.DB

// ============================================
// MOMENT CREATION
// ============================================

// CreateMoment handles moment upload (image/video)
func CreateMoment(w http.ResponseWriter, r *http.Request) {
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
	mediaType := r.FormValue("media_type")   // "image" or "video"
	visibility := r.FormValue("visibility")   // "friends" or "global"
	caption := r.FormValue("caption")

	// For friends-only: get encrypted keys for each friend
	encryptedKeysJSON := r.FormValue("encrypted_keys") // JSON array of {friend_id, encrypted_key, encrypted_iv}

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

	// Generate moment ID
	momentID := uuid.New().String()
	expiresAt := time.Now().Add(24 * time.Hour) // 24-hour expiry

	// Save file to server
	filename := fmt.Sprintf("%d_%s%s", time.Now().Unix(), momentID[:8], filepath.Ext(header.Filename))
	var uploadPath string
	var mediaURL string
	var encryptedMediaURL *string

	if visibility == "global" {
		// Global: save unencrypted
		if mediaType == "image" {
			uploadPath = filepath.Join("uploads/moments/global/images", filename)
		} else {
			uploadPath = filepath.Join("uploads/moments/global/videos", filename)
		}

		// Create directories if they don't exist
		os.MkdirAll(filepath.Dir(uploadPath), 0755)

		dst, err := os.Create(uploadPath)
		if err != nil {
			log.Printf("[CreateMoment] Failed to create file: %v", err)
			http.Error(w, "Failed to save file", http.StatusInternalServerError)
			return
		}
		defer dst.Close()

		if _, err := io.Copy(dst, file); err != nil {
			log.Printf("[CreateMoment] Failed to copy file: %v", err)
			http.Error(w, "Failed to save file", http.StatusInternalServerError)
			return
		}

		mediaURL = fmt.Sprintf("https://zarqmessenger.com/%s", uploadPath)
	} else {
		// Friends: save encrypted file
		if mediaType == "image" {
			uploadPath = filepath.Join("uploads/moments/friends/images", filename)
		} else {
			uploadPath = filepath.Join("uploads/moments/friends/videos", filename)
		}

		// Create directories if they don't exist
		os.MkdirAll(filepath.Dir(uploadPath), 0755)

		dst, err := os.Create(uploadPath)
		if err != nil {
			log.Printf("[CreateMoment] Failed to create file: %v", err)
			http.Error(w, "Failed to save file", http.StatusInternalServerError)
			return
		}
		defer dst.Close()

		if _, err := io.Copy(dst, file); err != nil {
			log.Printf("[CreateMoment] Failed to copy file: %v", err)
			http.Error(w, "Failed to save file", http.StatusInternalServerError)
			return
		}

		encMediaURL := fmt.Sprintf("https://zarqmessenger.com/%s", uploadPath)
		encryptedMediaURL = &encMediaURL
	}

	// Insert moment into database
	query := `
		INSERT INTO moments (
			id, user_id, media_type, visibility, media_url, encrypted_media_url, caption, expires_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING id, created_at
	`

	var createdAt time.Time
	err = MomentsDB.QueryRow(
		query, momentID, userID, mediaType, visibility,
		func() interface{} {
			if visibility == "global" {
				return mediaURL
			}
			return nil
		}(),
		encryptedMediaURL, caption, expiresAt,
	).Scan(&momentID, &createdAt)

	if err != nil {
		log.Printf("[CreateMoment] Database error: %v", err)
		http.Error(w, "Failed to create moment", http.StatusInternalServerError)
		return
	}

	// If friends-only, save encrypted keys for each friend
	if visibility == "friends" && encryptedKeysJSON != "" {
		var encryptedKeys []map[string]string
		if err := json.Unmarshal([]byte(encryptedKeysJSON), &encryptedKeys); err == nil {
			for _, keyData := range encryptedKeys {
				friendID := keyData["friend_id"]
				encKey := keyData["encrypted_key"]
				encIV := keyData["encrypted_iv"]

				if friendID != "" && encKey != "" && encIV != "" {
					_, err := MomentsDB.Exec(
						"INSERT INTO moment_keys (moment_id, friend_user_id, encrypted_media_key, encrypted_media_iv) VALUES ($1, $2, $3, $4)",
						momentID, friendID, encKey, encIV,
					)
					if err != nil {
						log.Printf("[CreateMoment] Failed to insert key for friend %s: %v", friendID, err)
					}
				}
			}
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":    true,
		"moment_id":  momentID,
		"created_at": createdAt,
		"expires_at": expiresAt,
		"message":    "Moment created successfully",
	})
}

// ============================================
// MOMENT FEEDS
// ============================================

// GetFriendsMoments returns active moments from friends (E2EE encrypted)
func GetFriendsMoments(w http.ResponseWriter, r *http.Request) {
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

	// Get friends' moments + user's own moments
	query := `
		SELECT
			m.id, m.user_id, m.media_type, m.encrypted_media_url, m.caption,
			m.thumbnail_url, m.created_at, m.expires_at,
			p.username, p.display_name, p.profile_avatar_url,
			COUNT(DISTINCT mv.viewer_user_id) as view_count,
			EXISTS(SELECT 1 FROM moment_views WHERE moment_id = m.id AND viewer_user_id = $1) as viewed_by_me,
			mk.encrypted_media_key, mk.encrypted_media_iv
		FROM moments m
		JOIN profiles p ON m.user_id = p.firebase_uid
		LEFT JOIN moment_views mv ON m.id = mv.moment_id
		LEFT JOIN moment_keys mk ON m.id = mk.moment_id AND mk.friend_user_id = $1
		WHERE m.visibility = 'friends'
		  AND m.is_deleted = FALSE
		  AND m.expires_at > NOW()
		  AND (
			  m.user_id = $1
			  OR m.user_id IN (
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
		GROUP BY m.id, p.username, p.display_name, p.profile_avatar_url, mk.encrypted_media_key, mk.encrypted_media_iv
		ORDER BY m.created_at DESC
		LIMIT $2 OFFSET $3
	`

	rows, err := MomentsDB.Query(query, userID, limit, offset)
	if err != nil {
		log.Printf("[GetFriendsMoments] Query error: %v", err)
		http.Error(w, "Failed to fetch moments", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var moments []map[string]interface{}
	for rows.Next() {
		var m struct {
			ID                  string
			UserID              string
			MediaType           string
			EncryptedMediaURL   sql.NullString
			Caption             sql.NullString
			ThumbnailURL        sql.NullString
			CreatedAt           time.Time
			ExpiresAt           time.Time
			Username            string
			DisplayName         sql.NullString
			ProfileAvatarURL    sql.NullString
			ViewCount           int
			ViewedByMe          bool
			EncryptedMediaKey   sql.NullString
			EncryptedMediaIV    sql.NullString
		}

		err := rows.Scan(
			&m.ID, &m.UserID, &m.MediaType, &m.EncryptedMediaURL, &m.Caption,
			&m.ThumbnailURL, &m.CreatedAt, &m.ExpiresAt,
			&m.Username, &m.DisplayName, &m.ProfileAvatarURL,
			&m.ViewCount, &m.ViewedByMe, &m.EncryptedMediaKey, &m.EncryptedMediaIV,
		)

		if err != nil {
			log.Printf("[GetFriendsMoments] Scan error: %v", err)
			continue
		}

		moment := map[string]interface{}{
			"id":                 m.ID,
			"user_id":            m.UserID,
			"username":           m.Username,
			"media_type":         m.MediaType,
			"created_at":         m.CreatedAt,
			"expires_at":         m.ExpiresAt,
			"view_count":         m.ViewCount,
			"viewed_by_me":       m.ViewedByMe,
			"visibility":         "friends",
		}

		if m.EncryptedMediaURL.Valid {
			moment["encrypted_media_url"] = m.EncryptedMediaURL.String
		}
		if m.Caption.Valid {
			moment["caption"] = m.Caption.String
		}
		if m.ThumbnailURL.Valid {
			moment["thumbnail_url"] = m.ThumbnailURL.String
		}
		if m.DisplayName.Valid {
			moment["display_name"] = m.DisplayName.String
		}
		if m.ProfileAvatarURL.Valid {
			moment["user_avatar_url"] = m.ProfileAvatarURL.String
		}
		if m.EncryptedMediaKey.Valid {
			moment["encrypted_media_key"] = m.EncryptedMediaKey.String
		}
		if m.EncryptedMediaIV.Valid {
			moment["encrypted_media_iv"] = m.EncryptedMediaIV.String
		}

		moments = append(moments, moment)
	}

	if moments == nil {
		moments = []map[string]interface{}{}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"moments":  moments,
		"has_more": len(moments) == limit,
	})
}

// GetGlobalMoments returns active global moments (public, no encryption)
func GetGlobalMoments(w http.ResponseWriter, r *http.Request) {
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
			m.id, m.user_id, m.media_type, m.media_url, m.caption,
			m.thumbnail_url, m.created_at, m.expires_at,
			p.username, p.display_name, p.profile_avatar_url,
			COUNT(DISTINCT mv.viewer_user_id) as view_count,
			EXISTS(SELECT 1 FROM moment_views WHERE moment_id = m.id AND viewer_user_id = $1) as viewed_by_me
		FROM moments m
		JOIN profiles p ON m.user_id = p.firebase_uid
		LEFT JOIN moment_views mv ON m.id = mv.moment_id
		WHERE m.visibility = 'global'
		  AND m.is_deleted = FALSE
		  AND m.expires_at > NOW()
		GROUP BY m.id, p.username, p.display_name, p.profile_avatar_url
		ORDER BY m.created_at DESC
		LIMIT $2 OFFSET $3
	`

	rows, err := MomentsDB.Query(query, userID, limit, offset)
	if err != nil {
		log.Printf("[GetGlobalMoments] Query error: %v", err)
		http.Error(w, "Failed to fetch moments", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var moments []map[string]interface{}
	for rows.Next() {
		var m struct {
			ID               string
			UserID           string
			MediaType        string
			MediaURL         sql.NullString
			Caption          sql.NullString
			ThumbnailURL     sql.NullString
			CreatedAt        time.Time
			ExpiresAt        time.Time
			Username         string
			DisplayName      sql.NullString
			ProfileAvatarURL sql.NullString
			ViewCount        int
			ViewedByMe       bool
		}

		err := rows.Scan(
			&m.ID, &m.UserID, &m.MediaType, &m.MediaURL, &m.Caption,
			&m.ThumbnailURL, &m.CreatedAt, &m.ExpiresAt,
			&m.Username, &m.DisplayName, &m.ProfileAvatarURL,
			&m.ViewCount, &m.ViewedByMe,
		)

		if err != nil {
			log.Printf("[GetGlobalMoments] Scan error: %v", err)
			continue
		}

		moment := map[string]interface{}{
			"id":           m.ID,
			"user_id":      m.UserID,
			"username":     m.Username,
			"media_type":   m.MediaType,
			"created_at":   m.CreatedAt,
			"expires_at":   m.ExpiresAt,
			"view_count":   m.ViewCount,
			"viewed_by_me": m.ViewedByMe,
			"visibility":   "global",
		}

		if m.MediaURL.Valid {
			moment["media_url"] = m.MediaURL.String
		}
		if m.Caption.Valid {
			moment["caption"] = m.Caption.String
		}
		if m.ThumbnailURL.Valid {
			moment["thumbnail_url"] = m.ThumbnailURL.String
		}
		if m.DisplayName.Valid {
			moment["display_name"] = m.DisplayName.String
		}
		if m.ProfileAvatarURL.Valid {
			moment["user_avatar_url"] = m.ProfileAvatarURL.String
		}

		moments = append(moments, moment)
	}

	if moments == nil {
		moments = []map[string]interface{}{}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"moments":  moments,
		"has_more": len(moments) == limit,
	})
}

// ============================================
// MOMENT VIEWS
// ============================================

// MarkMomentAsViewed records that a user viewed a moment
func MarkMomentAsViewed(w http.ResponseWriter, r *http.Request) {
	// Verify authentication
	token, err := getVerifiedTokenForTasks(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	userID := token.UID

	momentID := mux.Vars(r)["id"]

	// Insert view record (ignore if already exists)
	query := `
		INSERT INTO moment_views (moment_id, viewer_user_id)
		VALUES ($1, $2)
		ON CONFLICT (moment_id, viewer_user_id) DO NOTHING
	`

	_, err = MomentsDB.Exec(query, momentID, userID)
	if err != nil {
		log.Printf("[MarkMomentAsViewed] Database error: %v", err)
		http.Error(w, "Failed to mark as viewed", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"message": "Moment marked as viewed",
	})
}

// GetMomentViewers returns list of users who viewed a moment
func GetMomentViewers(w http.ResponseWriter, r *http.Request) {
	// Verify authentication
	token, err := getVerifiedTokenForTasks(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	userID := token.UID

	momentID := mux.Vars(r)["id"]

	// Verify moment belongs to user
	var ownerID string
	err = MomentsDB.QueryRow("SELECT user_id FROM moments WHERE id = $1", momentID).Scan(&ownerID)
	if err != nil {
		http.Error(w, "Moment not found", http.StatusNotFound)
		return
	}
	if ownerID != userID {
		http.Error(w, "Not authorized", http.StatusForbidden)
		return
	}

	// Get viewer list
	query := `
		SELECT
			mv.viewer_user_id, mv.viewed_at,
			p.username, p.display_name, p.profile_avatar_url
		FROM moment_views mv
		JOIN profiles p ON mv.viewer_user_id = p.firebase_uid
		WHERE mv.moment_id = $1
		ORDER BY mv.viewed_at DESC
	`

	rows, err := MomentsDB.Query(query, momentID)
	if err != nil {
		log.Printf("[GetMomentViewers] Query error: %v", err)
		http.Error(w, "Failed to fetch viewers", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var viewers []map[string]interface{}
	for rows.Next() {
		var v struct {
			ViewerUserID     string
			ViewedAt         time.Time
			Username         string
			DisplayName      sql.NullString
			ProfileAvatarURL sql.NullString
		}

		err := rows.Scan(&v.ViewerUserID, &v.ViewedAt, &v.Username, &v.DisplayName, &v.ProfileAvatarURL)
		if err != nil {
			log.Printf("[GetMomentViewers] Scan error: %v", err)
			continue
		}

		viewer := map[string]interface{}{
			"user_id":   v.ViewerUserID,
			"username":  v.Username,
			"viewed_at": v.ViewedAt,
		}

		if v.DisplayName.Valid {
			viewer["display_name"] = v.DisplayName.String
		}
		if v.ProfileAvatarURL.Valid {
			viewer["user_avatar_url"] = v.ProfileAvatarURL.String
		}

		viewers = append(viewers, viewer)
	}

	if viewers == nil {
		viewers = []map[string]interface{}{}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"viewers": viewers,
		"count":   len(viewers),
	})
}

// ============================================
// MOMENT DELETION
// ============================================

// DeleteMoment soft-deletes a moment (only by owner)
func DeleteMoment(w http.ResponseWriter, r *http.Request) {
	// Verify authentication
	token, err := getVerifiedTokenForTasks(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	userID := token.UID

	momentID := mux.Vars(r)["id"]

	// Soft delete moment (only if user owns it)
	query := `
		UPDATE moments
		SET is_deleted = TRUE
		WHERE id = $1 AND user_id = $2 AND is_deleted = FALSE
	`

	result, err := MomentsDB.Exec(query, momentID, userID)
	if err != nil {
		log.Printf("[DeleteMoment] Database error: %v", err)
		http.Error(w, "Failed to delete moment", http.StatusInternalServerError)
		return
	}

	rowsAffected, _ := result.RowsAffected()
	if rowsAffected == 0 {
		http.Error(w, "Moment not found or unauthorized", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"message": "Moment deleted successfully",
	})
}
