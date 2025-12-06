package handlers

// ============================================================================
// DEPRECATED FILE - NO LONGER IN USE
// ============================================================================
// This file contained customization endpoints (theme, colors, bubble styles)
// These features are now handled client-side using local storage (SharedPreferences)
// to eliminate server load and improve performance.
//
// Removed endpoints:
// - GET  /v1/user/settings
// - POST /v1/user/settings
// - GET  /v1/customization/options/bubble
//
// This file can be safely deleted.
// Kept for reference only.
// ============================================================================

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"

	// Import your middleware package to access the GetUIDFromContext helper
	// Use the correct module name from your go.mod:
	"zarq-messenger/middleware"
)

// Structs for data mapping
type BubbleOption struct {
	ID          int    `json:"id"`
	Name        string `json:"name"`
	ResourceKey string `json:"resource_key"`
}

// NOTE: UpdateSettingsRequest is redundant as UserSettingsRequest handles updates.
// Leaving it here as it was in your input, but focusing on UserSettingsRequest.
type UpdateSettingsRequest struct {
	BubbleStyle string `json:"bubble_style"`
}

type UserSettingsRequest struct {
	// Fields are pointers for optional updates (nil if not provided in JSON)
	BubbleStyle         *string `json:"bubble_style,omitempty"`
	MyBubbleColorStart  *string `json:"my_bubble_color_start,omitempty"`
	MyBubbleColorEnd    *string `json:"my_bubble_color_end,omitempty"`
	HomeScreenStyle     *string `json:"home_screen_style,omitempty"`
}

type UserSettingsResponse struct {
	BubbleStyle        string `json:"bubble_style"`
	MyBubbleColorStart string `json:"my_bubble_color_start"`
	MyBubbleColorEnd   string `json:"my_bubble_color_end"`
	HomeScreenStyle    string `json:"home_screen_style"`
}

// Handler for GET /v1/user/settings - Reads the user's current settings
func GetUserSettingsHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		firebaseUID, ok := middleware.GetUIDFromContext(r)
		if !ok {
			http.Error(w, "Unauthorized: User context missing.", http.StatusUnauthorized)
			return
		}
		
		// --- UPDATED QUERY ---
		var bubbleStyle, colorStart, colorEnd, homeScreenStyle string
		query := `
			SELECT message_bubble_style, my_bubble_color_start, my_bubble_color_end, home_screen_style
			FROM profiles
			WHERE firebase_uid = $1;
		`
		row := db.QueryRow(query, firebaseUID)

		// --- UPDATED SCAN ---
		err := row.Scan(&bubbleStyle, &colorStart, &colorEnd, &homeScreenStyle)

		if err != nil {
			if err == sql.ErrNoRows {
				// If profile is missing, use safe defaults
				bubbleStyle = "default_rounded"
				colorStart = "667EEA"
				colorEnd = "764BA2"
				homeScreenStyle = "current"
			} else {
				log.Printf("DB Query Error fetching settings for %s: %v", firebaseUID, err)
				http.Error(w, "Error fetching user settings.", http.StatusInternalServerError)
				return
			}
		}

		// --- UPDATED RESPONSE ---
		w.Header().Set("Content-Type", "application/json")
		response := UserSettingsResponse{
			BubbleStyle: bubbleStyle,
			MyBubbleColorStart: colorStart,
			MyBubbleColorEnd: colorEnd,
			HomeScreenStyle: homeScreenStyle,
		}
		
		if err := json.NewEncoder(w).Encode(response); err != nil {
			log.Printf("JSON Encode Error: %v", err)
			http.Error(w, "Failed to encode response.", http.StatusInternalServerError)
		}
	}
}

// 1. Handler for GET /v1/customization/options/bubble
func GetBubbleOptionsHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		query := `
			SELECT id, name, resource_key 
			FROM customization_options 
			WHERE type = 'bubble_style' 
			ORDER BY id;
		`
		
		rows, err := db.Query(query)
		if err != nil {
			log.Printf("DB Error: %v", err)
			http.Error(w, "Could not fetch customization options.", http.StatusInternalServerError)
			return
		}
		defer rows.Close() 

		var options []BubbleOption
		for rows.Next() {
			var option BubbleOption
			if err := rows.Scan(&option.ID, &option.Name, &option.ResourceKey); err != nil {
				log.Printf("DB Scan Error: %v", err)
				http.Error(w, "Error processing options.", http.StatusInternalServerError)
				return
			}
			options = append(options, option)
		}

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(options); err != nil {
			log.Printf("JSON Encode Error: %v", err)
			http.Error(w, "Failed to encode response.", http.StatusInternalServerError)
		}
	}
}

// 2. Handler for POST /v1/user/settings (FIXED PARAMETER INDEXING)
func UpdateUserSettingsHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		firebaseUID, ok := middleware.GetUIDFromContext(r)
		if !ok {
			http.Error(w, "Unauthorized: User context missing.", http.StatusUnauthorized)
			return
		}

		var req UserSettingsRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request body.", http.StatusBadRequest)
			return
		}

		var updates []string
		var args []interface{}
		paramIndex := 1 // Parameter index starts at $1

		// 1. Bubble Style Update
		if req.BubbleStyle != nil {
			updates = append(updates, fmt.Sprintf("message_bubble_style = $%d", paramIndex))
			args = append(args, *req.BubbleStyle)
			paramIndex++
		}

		// 2. Color Start Update
		if req.MyBubbleColorStart != nil {
			updates = append(updates, fmt.Sprintf("my_bubble_color_start = $%d", paramIndex))
			args = append(args, *req.MyBubbleColorStart)
			paramIndex++
		}

		// 3. Color End Update
		if req.MyBubbleColorEnd != nil {
			updates = append(updates, fmt.Sprintf("my_bubble_color_end = $%d", paramIndex))
			args = append(args, *req.MyBubbleColorEnd)
			paramIndex++
		}

		// 4. Home Screen Style Update
		if req.HomeScreenStyle != nil {
			updates = append(updates, fmt.Sprintf("home_screen_style = $%d", paramIndex))
			args = append(args, *req.HomeScreenStyle)
			paramIndex++
		}

		if len(updates) == 0 {
			http.Error(w, "No fields provided for update.", http.StatusBadRequest)
			return
		}

		// Build the final query
		setClause := strings.Join(updates, ", ")
		
		// The WHERE clause uses the next available parameter index (which is current paramIndex)
		whereParam := paramIndex 
		
		finalQuery := fmt.Sprintf(
			"UPDATE profiles SET %s WHERE firebase_uid = $%d;",
			setClause,
			whereParam,
		)

		// Add firebase_uid as the final argument
		args = append(args, firebaseUID)

		// --- EXECUTE THE TRANSACTION ---
		_, err := db.Exec(finalQuery, args...)
		if err != nil {
			// Log the specific database error for debugging
			log.Printf("DB Update Error for %s: %v", firebaseUID, err)
			http.Error(w, "Failed to update user settings.", http.StatusInternalServerError)
			return
		}

		// Success: 204 No Content
		w.WriteHeader(http.StatusNoContent)
	}
}
