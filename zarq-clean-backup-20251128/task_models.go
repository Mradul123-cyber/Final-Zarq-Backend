package main

import (
	"database/sql/driver"
	"encoding/json"
	"errors"
	"time"
)

// ============================================
// MODELS FOR DAILY TASKS FEATURE
// ============================================

// DailyTask represents a daily challenge/task
type DailyTask struct {
	ID                   int       `json:"id"`
	TaskDate             time.Time `json:"task_date"`
	TitleEN              string    `json:"title_en"`
	TitleHI              string    `json:"title_hi"`
	DescriptionEN        string    `json:"description_en"`
	DescriptionHI        string    `json:"description_hi"`
	Category             string    `json:"category"` // safe_fun, skill_based
	Difficulty           string    `json:"difficulty"` // easy, medium, hard
	Points               int       `json:"points"`
	AIVerificationPrompt string    `json:"ai_verification_prompt,omitempty"`
	RequireVerification  bool      `json:"require_verification"`
	IsActive             bool      `json:"is_active"`
	CreatedAt            time.Time `json:"created_at"`
	UpdatedAt            time.Time `json:"updated_at"`
}

// TaskSubmission represents a user's task submission
type TaskSubmission struct {
	ID                        string          `json:"id"`
	UserID                    string          `json:"user_id"`
	TaskID                    *int            `json:"task_id,omitempty"`
	IsTaskSubmission          bool            `json:"is_task_submission"`
	MediaType                 string          `json:"media_type"` // image, video
	MediaURL                  string          `json:"media_url"`
	ThumbnailURL              string          `json:"thumbnail_url,omitempty"`
	EncryptedMediaKey         string          `json:"encrypted_media_key,omitempty"`
	Caption                   string          `json:"caption,omitempty"`
	Visibility                string          `json:"visibility"` // friends, global
	AIVerified                bool            `json:"ai_verified"`
	AIVerificationConfidence  int             `json:"ai_verification_confidence"`
	AIVerificationDetails     json.RawMessage `json:"ai_verification_details,omitempty"`
	PointsEarned              int             `json:"points_earned"`
	IsDeleted                 bool            `json:"is_deleted"`
	ExpiresAt                 time.Time       `json:"expires_at"`
	CreatedAt                 time.Time       `json:"created_at"`
	UpdatedAt                 time.Time       `json:"updated_at"`

	// Additional fields for responses
	Username                  string          `json:"username,omitempty"`
	ProfilePictureURL         string          `json:"profile_picture_url,omitempty"`
	ReactionCount             int             `json:"reaction_count,omitempty"`
	CommentCount              int             `json:"comment_count,omitempty"`
	UserReaction              string          `json:"user_reaction,omitempty"`
	Task                      *DailyTask      `json:"task,omitempty"`
}

// TaskShareRecipient for E2EE friends-only sharing
type TaskShareRecipient struct {
	ID                 int       `json:"id"`
	SubmissionID       string    `json:"submission_id"`
	RecipientUserID    string    `json:"recipient_user_id"`
	EncryptedMediaKey  string    `json:"encrypted_media_key"`
	HasViewed          bool      `json:"has_viewed"`
	ViewedAt           *time.Time `json:"viewed_at,omitempty"`
	CreatedAt          time.Time `json:"created_at"`
}

// TaskReaction represents a reaction on a submission
type TaskReaction struct {
	ID           int       `json:"id"`
	SubmissionID string    `json:"submission_id"`
	UserID       string    `json:"user_id"`
	ReactionType string    `json:"reaction_type"` // like, love, fire, clap, wow
	CreatedAt    time.Time `json:"created_at"`

	// Additional fields for responses
	Username          string `json:"username,omitempty"`
	ProfilePictureURL string `json:"profile_picture_url,omitempty"`
}

// UserTaskStats for gamification
type UserTaskStats struct {
	UserID           string    `json:"user_id"`
	TotalPoints      int       `json:"total_points"`
	CurrentStreak    int       `json:"current_streak"`
	LongestStreak    int       `json:"longest_streak"`
	TasksCompleted   int       `json:"tasks_completed"`
	FreePosts        int       `json:"free_posts"`
	LastTaskDate     *time.Time `json:"last_task_date,omitempty"`
	Badges           BadgeList `json:"badges"`
	Rank             int       `json:"rank"`
	WeeklyPoints     int       `json:"weekly_points"`
	WeeklyRank       int       `json:"weekly_rank"`
	LastWeeklyReset  *time.Time `json:"last_weekly_reset,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

// Badge represents an achievement badge
type Badge struct {
	ID               int       `json:"id"`
	BadgeKey         string    `json:"badge_key"`
	NameEN           string    `json:"name_en"`
	NameHI           string    `json:"name_hi"`
	DescriptionEN    string    `json:"description_en,omitempty"`
	DescriptionHI    string    `json:"description_hi,omitempty"`
	IconEmoji        string    `json:"icon_emoji,omitempty"`
	IconURL          string    `json:"icon_url,omitempty"`
	RequirementType  string    `json:"requirement_type"` // streak, total_tasks, points, weekly_top
	RequirementValue int       `json:"requirement_value"`
	BadgeTier        string    `json:"badge_tier"` // bronze, silver, gold, diamond
	IsActive         bool      `json:"is_active"`
	CreatedAt        time.Time `json:"created_at"`

	// Additional fields for user context
	EarnedAt         *time.Time `json:"earned_at,omitempty"`
}

// BadgeList for JSONB array handling
type BadgeList []int

func (b *BadgeList) Scan(value interface{}) error {
	if value == nil {
		*b = []int{}
		return nil
	}
	bytes, ok := value.([]byte)
	if !ok {
		return errors.New("type assertion to []byte failed")
	}
	return json.Unmarshal(bytes, b)
}

func (b BadgeList) Value() (driver.Value, error) {
	return json.Marshal(b)
}

// TaskComment represents a comment on a submission
type TaskComment struct {
	ID           int       `json:"id"`
	SubmissionID string    `json:"submission_id"`
	UserID       string    `json:"user_id"`
	CommentText  string    `json:"comment_text"`
	IsDeleted    bool      `json:"is_deleted"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`

	// Additional fields for responses
	Username          string `json:"username"`
	ProfilePictureURL string `json:"profile_picture_url,omitempty"`
}

// TaskReport for content moderation
type TaskReport struct {
	ID             int        `json:"id"`
	SubmissionID   string     `json:"submission_id"`
	ReporterUserID string     `json:"reporter_user_id"`
	Reason         string     `json:"reason"` // inappropriate, spam, fake, violence, hate
	Details        string     `json:"details,omitempty"`
	Status         string     `json:"status"` // pending, under_review, resolved, dismissed
	AdminNotes     string     `json:"admin_notes,omitempty"`
	ReviewedBy     *string    `json:"reviewed_by,omitempty"`
	ReviewedAt     *time.Time `json:"reviewed_at,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`

	// Additional fields for admin panel
	ReporterUsername  string           `json:"reporter_username,omitempty"`
	Submission        *TaskSubmission  `json:"submission,omitempty"`
}

// UserTaskBan for user banning system
type UserTaskBan struct {
	ID              int        `json:"id"`
	UserID          string     `json:"user_id"`
	BanType         string     `json:"ban_type"` // temporary, permanent
	Reason          string     `json:"reason"`
	BannedUntil     *time.Time `json:"banned_until,omitempty"`
	BannedBy        *string    `json:"banned_by,omitempty"`
	ViolationsCount int        `json:"violations_count"`
	IsActive        bool       `json:"is_active"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
}

// ReportThreshold for tracking report counts
type ReportThreshold struct {
	ID               int       `json:"id"`
	SubmissionID     string    `json:"submission_id"`
	ReportCount      int       `json:"report_count"`
	ThresholdReached bool      `json:"threshold_reached"`
	AdminNotified    bool      `json:"admin_notified"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

// ============================================
// REQUEST/RESPONSE MODELS
// ============================================

// SubmitTaskRequest for submitting a task
type SubmitTaskRequest struct {
	TaskID           *int     `json:"task_id,omitempty"`
	IsTaskSubmission bool     `json:"is_task_submission"`
	MediaType        string   `json:"media_type"` // image, video
	Caption          string   `json:"caption,omitempty"`
	Visibility       string   `json:"visibility"` // friends, global
	FriendIDs        []string `json:"friend_ids,omitempty"` // For friends-only
}

// AIVerificationResponse from Gemini
type AIVerificationResponse struct {
	Verified   bool   `json:"verified"`
	Confidence int    `json:"confidence"`
	Detected   string `json:"detected,omitempty"`
	WordCount  int    `json:"word_count,omitempty"`
}

// CreateReactionRequest for adding a reaction
type CreateReactionRequest struct {
	ReactionType string `json:"reaction_type"` // like, love, fire, clap, wow
}

// CreateCommentRequest for adding a comment
type CreateCommentRequest struct {
	CommentText string `json:"comment_text"`
}

// CreateReportRequest for reporting content
type CreateReportRequest struct {
	Reason  string `json:"reason"` // inappropriate, spam, fake, violence, hate
	Details string `json:"details,omitempty"`
}

// FeedResponse with pagination
type FeedResponse struct {
	Submissions []TaskSubmission `json:"submissions"`
	HasMore     bool             `json:"has_more"`
	NextCursor  string           `json:"next_cursor,omitempty"`
}

// LeaderboardResponse
type LeaderboardEntry struct {
	UserID            string `json:"user_id"`
	Username          string `json:"username"`
	ProfilePictureURL string `json:"profile_picture_url,omitempty"`
	Points            int    `json:"points"`
	Rank              int    `json:"rank"`
	CurrentStreak     int    `json:"current_streak"`
	TasksCompleted    int    `json:"tasks_completed"`
}

type LeaderboardResponse struct {
	Type    string                 `json:"type"` // weekly, all_time
	Entries []LeaderboardEntry     `json:"entries"`
}

// UserStatsResponse with badges expanded
type UserStatsResponse struct {
	Stats          UserTaskStats `json:"stats"`
	BadgesEarned   []Badge       `json:"badges_earned"`
	NextBadges     []Badge       `json:"next_badges"` // Next badges to unlock
}
