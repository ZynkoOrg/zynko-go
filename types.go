package zynko

import "encoding/json"

// The Zynko API's objects, as the server sends them (docs.zynko.org/api/objetos). Ids are
// strings; times are milliseconds since 1970. A field the server sends as null arrives as
// the zero value ("" for ids and text).

// User is an account, person or bot.
type User struct {
	ID          string `json:"id"`
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
	Avatar      string `json:"avatar,omitempty"`
	Bot         bool   `json:"bot"`
}

// Profile is a user with their bio, banner and color.
type Profile struct {
	User   User   `json:"user"`
	Bio    string `json:"bio,omitempty"`
	Banner string `json:"banner,omitempty"`
	Accent string `json:"accent,omitempty"`
}

// Guild is a server: what GUILD_UPDATE carries.
type Guild struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Icon    string `json:"icon,omitempty"`
	OwnerID string `json:"owner_id"`
}

// GuildFull is a server as READY and GUILD_CREATE bring it.
type GuildFull struct {
	Guild
	Channels []Channel `json:"channels"`
	Members  []Member  `json:"members"`
	// Ids of the members online now.
	Presences []string `json:"presences"`
	Roles     []Role   `json:"roles"`
	Emojis    []Emoji  `json:"emojis"`
}

// Overwrite is a channel's permission change for a role (Type 0) or a member (Type 1).
type Overwrite struct {
	ID    string     `json:"id"`
	Type  int        `json:"type"`
	Allow Permission `json:"allow"`
	Deny  Permission `json:"deny"`
}

// ForumTag is a tag a forum's posts can carry.
type ForumTag struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Emoji     string `json:"emoji,omitempty"`
	Moderated bool   `json:"moderated,omitempty"`
}

// Channel is a text channel, voice room, category, forum, post or DM.
type Channel struct {
	ID               string   `json:"id"`
	GuildID          string   `json:"guild_id,omitempty"`
	Type             int      `json:"type"`
	Name             string   `json:"name"`
	Topic            string   `json:"topic,omitempty"`
	Position         int      `json:"position"`
	ParentID         string   `json:"parent_id,omitempty"`
	LastMessageID    string   `json:"last_message_id,omitempty"`
	RateLimitPerUser int      `json:"rate_limit_per_user,omitempty"`
	OwnerID          string   `json:"owner_id,omitempty"`
	Pinned           bool     `json:"pinned,omitempty"`
	Locked           bool     `json:"locked,omitempty"`
	MessageCount     int      `json:"message_count,omitempty"`
	AppliedTags      []string `json:"applied_tags,omitempty"`
	// The voice server a room runs on, when someone picked one.
	VoiceServer   string     `json:"voice_server,omitempty"`
	AvailableTags []ForumTag `json:"available_tags,omitempty"`
	// In a DM: the other person.
	Recipients           []User      `json:"recipients,omitempty"`
	PermissionOverwrites []Overwrite `json:"permission_overwrites,omitempty"`
}

// Member is someone in a server.
type Member struct {
	GuildID  string `json:"guild_id"`
	User     User   `json:"user"`
	Nick     string `json:"nick,omitempty"`
	IsAdmin  bool   `json:"is_admin"`
	JoinedAt int64  `json:"joined_at"`
	// Not including @everyone, which everybody has.
	Roles []string `json:"roles"`
}

// Name is the nickname, or the display name when there is none.
func (m Member) Name() string {
	if m.Nick != "" {
		return m.Nick
	}
	return m.User.DisplayName
}

// Role is a server role. The @everyone role has the server's id.
type Role struct {
	ID          string     `json:"id"`
	GuildID     string     `json:"guild_id"`
	Name        string     `json:"name"`
	Color       *int       `json:"color"`
	Position    int        `json:"position"`
	Permissions Permission `json:"permissions"`
	Hoist       bool       `json:"hoist"`
}

// Emoji is a server's custom emoji.
type Emoji struct {
	ID       string `json:"id"`
	GuildID  string `json:"guild_id"`
	Name     string `json:"name"`
	URL      string `json:"url"`
	Animated bool   `json:"animated"`
}

// Attachment is a file sent with a message.
type Attachment struct {
	ID          string `json:"id"`
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	Size        int64  `json:"size"`
	// /api/attachments/{id}: needs the token to download.
	URL       string `json:"url"`
	Thumbnail string `json:"thumbnail,omitempty"`
	Width     int    `json:"width,omitempty"`
	Height    int    `json:"height,omitempty"`
}

// ReactionCount is one emoji's reactions on a message.
type ReactionCount struct {
	// A Unicode emoji, or name:id for a server emoji.
	Emoji string `json:"emoji"`
	Count int    `json:"count"`
	Me    bool   `json:"me"`
}

// MessageRef is the message another one answers.
type MessageRef struct {
	ID      string `json:"id"`
	Author  *User  `json:"author"`
	Content string `json:"content,omitempty"`
}

// Embed is a link preview.
type Embed struct {
	URL         string      `json:"url"`
	SiteName    string      `json:"site_name,omitempty"`
	Title       string      `json:"title,omitempty"`
	Description string      `json:"description,omitempty"`
	Author      string      `json:"author,omitempty"`
	Color       string      `json:"color,omitempty"`
	Image       *EmbedImage `json:"image,omitempty"`
	Video       *EmbedVideo `json:"video,omitempty"`
}

type EmbedImage struct {
	URL    string `json:"url"`
	Width  int    `json:"width,omitempty"`
	Height int    `json:"height,omitempty"`
	Large  bool   `json:"large"`
}

type EmbedVideo struct {
	Provider string `json:"provider"`
	ID       string `json:"id"`
}

// Card is a bot's card: a title, text, fields and pictures with a colored edge
// (docs.zynko.org/bots/comandos#cartoes).
type Card struct {
	Title string `json:"title,omitempty"`
	// Markdown, like a message.
	Description string `json:"description,omitempty"`
	// Where the title points.
	URL string `json:"url,omitempty"`
	// The edge's color, 0xRRGGBB.
	Color  int         `json:"color,omitempty"`
	Author *CardAuthor `json:"author,omitempty"`
	Fields []CardField `json:"fields,omitempty"`
	// Wide, under the text.
	Image *CardImage `json:"image,omitempty"`
	// Small, beside the text.
	Thumbnail *CardImage  `json:"thumbnail,omitempty"`
	Footer    *CardFooter `json:"footer,omitempty"`
	// Shown in the footer, in ms.
	Timestamp int64 `json:"timestamp,omitempty"`
}

type CardAuthor struct {
	Name    string `json:"name"`
	URL     string `json:"url,omitempty"`
	IconURL string `json:"icon_url,omitempty"`
}

type CardField struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Inline bool   `json:"inline,omitempty"`
}

type CardImage struct {
	URL string `json:"url"`
}

type CardFooter struct {
	Text    string `json:"text"`
	IconURL string `json:"icon_url,omitempty"`
}

// ButtonStyle is how a button looks.
type ButtonStyle string

const (
	ButtonPrimary   ButtonStyle = "primary"
	ButtonSecondary ButtonStyle = "secondary"
	ButtonSuccess   ButtonStyle = "success"
	ButtonDanger    ButtonStyle = "danger"
	ButtonLink      ButtonStyle = "link"
)

// Button is a button on a bot's message.
type Button struct {
	Style ButtonStyle `json:"style"`
	Label string      `json:"label,omitempty"`
	Emoji string      `json:"emoji,omitempty"`
	// What the bot gets back when it is clicked (all but links).
	CustomID string `json:"custom_id,omitempty"`
	// Link buttons only.
	URL      string `json:"url,omitempty"`
	Disabled bool   `json:"disabled,omitempty"`
}

// Components are rows of buttons, top to bottom: up to 5 rows of 5.
type Components [][]Button

// InteractionRef is the command or click a bot's message answers.
type InteractionRef struct {
	ID string `json:"id"`
	// The command; empty for a button.
	Name string `json:"name,omitempty"`
	User User   `json:"user"`
}

// OptionType is the type of a command option.
type OptionType string

const (
	OptionString  OptionType = "string"
	OptionInteger OptionType = "integer"
	OptionNumber  OptionType = "number"
	OptionBoolean OptionType = "boolean"
	OptionUser    OptionType = "user"
	OptionChannel OptionType = "channel"
	OptionRole    OptionType = "role"
)

// CommandOption is a field of a "/" command.
type CommandOption struct {
	Name        string     `json:"name"`
	Description string     `json:"description"`
	Type        OptionType `json:"type"`
	Required    bool       `json:"required,omitempty"`
	// The only values allowed.
	Choices  []Choice `json:"choices,omitempty"`
	MinValue *float64 `json:"min_value,omitempty"`
	MaxValue *float64 `json:"max_value,omitempty"`
}

// Choice is one of an option's allowed values. Value is a string or a number.
type Choice struct {
	Name  string `json:"name"`
	Value any    `json:"value"`
}

// Command is a "/" command (docs.zynko.org/bots/comandos).
type Command struct {
	// 1 to 32 lowercase letters, digits, _ or -.
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Options     []CommandOption `json:"options,omitempty"`
}

// Limit is a helper for CommandOption.MinValue and MaxValue.
func Limit(v float64) *float64 { return &v }

// InteractionData is INTERACTION_CREATE as it comes.
type InteractionData struct {
	ID        string              `json:"id"`
	Token     string              `json:"token"`
	Type      string              `json:"type"` // "command" or "button"
	ChannelID string              `json:"channel_id"`
	GuildID   string              `json:"guild_id,omitempty"`
	User      User                `json:"user"`
	Member    *Member             `json:"member"`
	Command   *InteractionCommand `json:"command,omitempty"`
	CustomID  string              `json:"custom_id,omitempty"`
	Message   *Message            `json:"message,omitempty"`
}

// InteractionCommand is the command someone used, with what they filled in.
type InteractionCommand struct {
	Name    string                   `json:"name"`
	Options []InteractionOptionValue `json:"options"`
}

// InteractionOptionValue is one option someone filled in. User, channel and role options
// come as ids.
type InteractionOptionValue struct {
	Name  string          `json:"name"`
	Type  OptionType      `json:"type"`
	Value json.RawMessage `json:"value"`
}

// Invite is an invite to a server.
type Invite struct {
	Code      string `json:"code"`
	GuildID   string `json:"guild_id"`
	CreatorID string `json:"creator_id"`
	Uses      int    `json:"uses"`
	MaxUses   *int   `json:"max_uses"`
	ExpiresAt *int64 `json:"expires_at"`
}

// VoiceState says where someone is in voice.
type VoiceState struct {
	UserID  string `json:"user_id"`
	GuildID string `json:"guild_id,omitempty"`
	// Empty when the account left voice.
	ChannelID  string `json:"channel_id,omitempty"`
	SelfMute   bool   `json:"self_mute"`
	SelfDeaf   bool   `json:"self_deaf"`
	SelfStream bool   `json:"self_stream"`
	SelfVideo  bool   `json:"self_video"`
	SessionID  string `json:"session_id"`
}

// ReadyData is the READY event: the account's whole state.
type ReadyData struct {
	User        User         `json:"user"`
	Guilds      []GuildFull  `json:"guilds"`
	DMChannels  []Channel    `json:"dm_channels"`
	Presences   []string     `json:"presences"`
	VoiceStates []VoiceState `json:"voice_states"`
	SessionID   string       `json:"session_id"`
}

// Reaction is MESSAGE_REACTION_ADD and MESSAGE_REACTION_REMOVE.
type Reaction struct {
	MessageID string `json:"message_id"`
	ChannelID string `json:"channel_id"`
	UserID    string `json:"user_id"`
	Emoji     string `json:"emoji"`
}

// Typing is TYPING_START.
type Typing struct {
	ChannelID string `json:"channel_id"`
	UserID    string `json:"user_id"`
	Timestamp int64  `json:"timestamp"`
}

// VoiceJoin is the answer to joining a voice room (POST /channels/{id}/voice).
type VoiceJoin struct {
	URL string `json:"url"`
	// The voice server's id (Brasil 1, Brasil 2...).
	Server    string `json:"server,omitempty"`
	Token     string `json:"token"`
	Room      string `json:"room"`
	CanSpeak  bool   `json:"can_speak"`
	CanStream bool   `json:"can_stream"`
}
