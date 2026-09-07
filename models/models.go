// Package models holds row types and the Postgres store.
package models

import (
	"encoding/json"
	"time"
)

type Room struct {
	ID   string `json:"id"`
	Slug string `json:"slug"`
	Name string `json:"name"`
	// CreatedByUserID is the workspace creator; NULL for legacy and agent-only rooms.
	CreatedByUserID *string   `json:"created_by_user_id,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
	// AvatarAttachmentID is the uploaded workspace image; nil means initials on Color.
	AvatarAttachmentID *string `json:"avatar_attachment_id,omitempty"`
	AvatarURL          string  `json:"avatar_url,omitempty"`
	// Color is a hue slot 0-11 picked at create time, stable for the room's life.
	Color int16 `json:"color"`
	// Delivery policy: an unacked receipt dead-letters after this many days, and
	// a receipt handed out more than MaxAttempts times fails as retries_exhausted.
	DeliveryDeadLetterDays int `json:"delivery_dead_letter_days"`
	DeliveryMaxAttempts    int `json:"delivery_max_attempts"`
}

// AvatarPath is where a workspace image is fetched from (with the room's
// participant credentials), or "" when the room shows initials.
func AvatarPath(attachmentID *string) string {
	if attachmentID == nil {
		return ""
	}
	return "/api/v1/attachments/" + *attachmentID
}

// RoomColorSlots is how many hues the initials fallback cycles through.
const RoomColorSlots = 12

type Tag struct {
	Tag      string  `json:"tag"`
	TaggedBy *string `json:"tagged_by,omitempty"`
}

type Participant struct {
	ID     string `json:"id"`
	RoomID string `json:"room_id"`
	Name   string `json:"name"`
	// Avatar is the legacy emoji column. Nothing renders it any more: a member
	// shows their uploaded image, or the shared seedling. Kept off the wire so
	// no client can start drawing it again; the column goes next release.
	Avatar             string  `json:"-"`
	AvatarAttachmentID *string `json:"avatar_attachment_id,omitempty"`
	Description        string  `json:"description"`
	IsHuman            bool    `json:"is_human"`
	Role               string  `json:"role"`
	// server-verified owning principal (set by owner-scoped invites); the
	// trust anchor for "whose agent is this" — never trust in-message claims
	OwnerID   *string `json:"owner_id,omitempty"`
	OwnerName *string `json:"owner_name,omitempty"`
	// the owner's account, when the owner row is linked to one
	OwnerUserID   *string `json:"owner_user_id,omitempty"`
	OwnerUsername *string `json:"owner_username,omitempty"`
	// UserID links a human's row to their account; agents and cli humans have none.
	UserID   *string `json:"user_id,omitempty"`
	Username *string `json:"username,omitempty"`
	Revoked  bool    `json:"revoked,omitempty"`
	Online   bool    `json:"online"`
	// Presence spells Online out ("online"/"offline"); DeclaredOffline is the
	// sticky task-21 state an agent set on itself, cleared only by DeclareOnline.
	Presence        string    `json:"presence"`
	DeclaredOffline bool      `json:"declared_offline,omitempty"`
	LastSeenAt      time.Time `json:"last_seen_at"`
	CreatedAt       time.Time `json:"created_at"`
	Tags            []Tag     `json:"tags"`
}

// NotifyPrefs are a participant's own notification settings (web client).
type NotifyPrefs struct {
	Enabled bool `json:"enabled"`
	Sound   bool `json:"sound"`
	// ArchiveAfterSecs hides an inactive thread from the sidebar; 0 = never.
	ArchiveAfterSecs int `json:"archive_after_secs"`
}

type Channel struct {
	ID        string  `json:"id"`
	RoomID    string  `json:"room_id"`
	Name      string  `json:"name"`
	Topic     string  `json:"topic"`
	CreatedBy *string `json:"created_by,omitempty"`
	Archived  bool    `json:"archived"`
	// Private channels are invite-only: never listed in browse, joinable only by
	// being added by an existing member.
	Private   bool      `json:"private"`
	CreatedAt time.Time `json:"created_at"`
	// per-viewer read state, populated only by ListChannelsUnread
	UnreadCount int64 `json:"unread_count"`
	// UnreadMentions counts the unread top-level messages that @mention the
	// viewer (directly or via @channel/@here/@everyone). The badge shows this;
	// a plain unread with no mention just glows the channel name.
	UnreadMentions int64      `json:"unread_mentions"`
	LastReadAt     *time.Time `json:"last_read_at,omitempty"`
	// Muted is the viewer's per-channel notification mute (ListChannelsUnread
	// only). Unread state still accumulates; the web client just stays quiet.
	Muted bool `json:"muted"`
	// MemberCount is populated only by BrowsableChannels (the browse view).
	MemberCount *int64 `json:"member_count,omitempty"`
	// Member is populated only by BrowsableChannels: browse shows the whole
	// public map, with the viewer's own channels marked instead of hidden.
	Member *bool `json:"member,omitempty"`
}

// ChannelLayout is one participant's whole sidebar order: the named sections
// and the default section ("Channels") that holds everything else.
type ChannelLayout struct {
	Groups           []ChannelGroup `json:"groups"`
	Ungrouped        []string       `json:"ungrouped"`
	DefaultCollapsed bool           `json:"default_collapsed"`
}

// ChannelGroup is one participant's sidebar section. Groups are purely personal
// (Slack-style sections): they hold no room state and emit no events. ChannelIDs
// lists the channels placed in this group for this participant, in order.
type ChannelGroup struct {
	ID            string    `json:"id"`
	ParticipantID string    `json:"participant_id"`
	Name          string    `json:"name"`
	Position      int       `json:"position"`
	Collapsed     bool      `json:"collapsed"`
	CreatedAt     time.Time `json:"created_at"`
	ChannelIDs    []string  `json:"channel_ids"`
}

type AttachmentMeta struct {
	ID          string    `json:"id"`
	Filename    string    `json:"filename"`
	ContentType string    `json:"content_type"`
	SizeBytes   int64     `json:"size_bytes"`
	CreatedAt   time.Time `json:"created_at"`
}

type Attachment struct {
	AttachmentMeta
	RoomID     string `json:"room_id"`
	UploaderID string `json:"uploader_id"`
	Data       []byte `json:"-"`
}

type Message struct {
	ID           string  `json:"id"`
	RoomID       string  `json:"room_id"`
	ChannelID    string  `json:"channel_id"`
	ThreadRootID *string `json:"thread_root_id,omitempty"`
	AuthorID     string  `json:"author_id"`
	AuthorName   string  `json:"author_name"`
	Body         string  `json:"body"`
	IsBroadcast  bool    `json:"is_broadcast"`
	// Kind is "message" for normal posts, "system" for membership timeline
	// entries ("joined #x"); system rows skip unread counts, search, threads.
	Kind        string           `json:"kind"`
	CreatedAt   time.Time        `json:"created_at"`
	EditedAt    *time.Time       `json:"edited_at,omitempty"`
	ReplyCount  int              `json:"reply_count"`
	LastReplyAt *time.Time       `json:"last_reply_at,omitempty"`
	ReplierIDs  []string         `json:"replier_ids"` // distinct, most recent first, capped
	Attachments []AttachmentMeta `json:"attachments"`
	Mentions    []string         `json:"mentions"`
	// Reactions groups every emoji on the message with who added it, in the
	// order the emoji first appeared (Slack keeps that order too).
	Reactions []Reaction `json:"reactions"`
	// AckedBy is every recipient who explicitly took the ask on, oldest first.
	// Empty is the normal case: only an ask is ever acked.
	AckedBy []Ack `json:"acked_by"`
	// ReplyToID is what to pass as thread_root_id (or to `ac reply`) to continue
	// this message's thread. Agents kept deriving it wrong from thread_root_id
	// being null on a root, so every scanned message states it outright.
	ReplyToID string `json:"reply_to"`
}

// ReplyTo is the thread root to reply under: the root itself for a reply,
// the message's own id for a root.
func (m Message) ReplyTo() string {
	if m.ThreadRootID != nil {
		return *m.ThreadRootID
	}
	return m.ID
}

// Ack is one recipient's explicit acknowledgement of an ask. It is a receipt,
// not a reaction: the watcher nags an agent until it has one on every ask
// addressed to it, and 👀 / ✅ keep their own separate meanings.
type Ack struct {
	ParticipantID string    `json:"participant_id"`
	Name          string    `json:"name"`
	AckedAt       time.Time `json:"acked_at"`
}

// Reaction is one emoji on a message and everybody who added it, oldest first.
type Reaction struct {
	Emoji          string   `json:"emoji"`
	Count          int      `json:"count"`
	ParticipantIDs []string `json:"participant_ids"`
	Names          []string `json:"names"`
}

// ReactionEvent is the message.reaction payload: who did what to which
// message, plus the message's full reaction list after the change so a client
// repaints without a fetch. AuthorID is the MESSAGE author, so the relevance
// filter can hand you reactions to your own posts.
type ReactionEvent struct {
	MessageID       string     `json:"message_id"`
	ChannelID       string     `json:"channel_id"`
	ThreadRootID    *string    `json:"thread_root_id"`
	AuthorID        string     `json:"author_id"`
	AuthorName      string     `json:"author_name"`
	Emoji           string     `json:"emoji"`
	ParticipantID   string     `json:"participant_id"`
	ParticipantName string     `json:"participant_name"`
	Added           bool       `json:"added"`
	Reactions       []Reaction `json:"reactions"`
}

// AckEvent is the payload of a message.ack event: who acked what, plus the
// message's full ack list so a client can repaint without a refetch.
type AckEvent struct {
	MessageID       string  `json:"message_id"`
	ChannelID       string  `json:"channel_id"`
	ThreadRootID    *string `json:"thread_root_id"`
	AuthorID        string  `json:"author_id"`
	AuthorName      string  `json:"author_name"`
	ParticipantID   string  `json:"participant_id"`
	ParticipantName string  `json:"participant_name"`
	AckedBy         []Ack   `json:"acked_by"`
}

// PendingAck is one ask addressed to a participant that it has not acked yet.
// Reason says why it is an ask: "mention" or "thread".
type PendingAck struct {
	MessageID   string    `json:"message_id"`
	ChannelID   string    `json:"channel_id"`
	ChannelName string    `json:"channel_name"`
	AuthorID    string    `json:"author_id"`
	AuthorName  string    `json:"author_name"`
	CreatedAt   time.Time `json:"created_at"`
	Excerpt     string    `json:"excerpt"`
	Reason      string    `json:"reason"`
}

type Event struct {
	Seq       int64           `json:"seq"`
	RoomID    string          `json:"room_id"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
	CreatedAt time.Time       `json:"created_at"`
}

// SearchResult is a message plus a relevance score.
type SearchResult struct {
	Message
	Score float64 `json:"score"`
	// Via is set by hybrid search: "text" when the text leg found it (alone or
	// with the semantic leg), "semantic" when only the embedding leg did.
	Via string `json:"via,omitempty"`
}

// Search kinds for SearchFilters.Kinds.
const (
	SearchKindMessage    = "message"    // a top-level post
	SearchKindThread     = "thread"     // a thread root with replies, or a reply
	SearchKindAttachment = "attachment" // carries at least one attachment
)

type SearchFilters struct {
	ChannelID *string
	// AuthorIDs ORs within the field: any of these authors, human or agent.
	AuthorIDs    []string
	ThreadRootID *string
	// Kinds ORs within the field (SearchKind*); empty means every kind.
	Kinds         []string
	Since         *time.Time
	Until         *time.Time
	HasAttachment *bool
	Limit         int
	// MemberID, when set, restricts results to channels this participant is a
	// member of. Handlers always set it so search never leaks a channel you are
	// not in.
	MemberID *string
}

// User is a person's account; a login through any provider lands on one of these.
type User struct {
	ID                 string  `json:"id"`
	Username           string  `json:"username"`
	DisplayName        string  `json:"display_name"`
	Email              *string `json:"email,omitempty"`
	MustChangePassword bool    `json:"must_change_password"`
	// LastActiveWorkspaceID is the users.last_active_room_id hint, re-validated per request.
	LastActiveWorkspaceID *string   `json:"last_active_workspace_id,omitempty"`
	CreatedAt             time.Time `json:"created_at"`
}

// Session is one ses_ login. Only the token's sha256 is stored.
type Session struct {
	ID         string    `json:"id"`
	UserID     string    `json:"user_id"`
	Provider   string    `json:"provider"`
	CreatedAt  time.Time `json:"created_at"`
	LastUsedAt time.Time `json:"last_used_at"`
	ExpiresAt  time.Time `json:"expires_at"`
}

// SessionMaxAge caps a session at 90 days from creation, whatever the sliding TTL says.
const SessionMaxAge = 90 * 24 * time.Hour

// SessionTouchEvery bounds how often a request refreshes last_used_at and expires_at.
const SessionTouchEvery = 5 * time.Minute

// OnlineWindow is how recently a participant must have been seen to count as online.
const OnlineWindow = 90 * time.Second

// AgentExpireAfter is how long an agent (is_human=false) may stay unseen before
// it drops off every roster. The row stays, so its messages keep their author
// and the next request from its token puts it back. Humans never expire.
const AgentExpireAfter = 24 * time.Hour
