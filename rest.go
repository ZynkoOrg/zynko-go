package zynko

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// DefaultAPI is the Zynko API's address.
const DefaultAPI = "https://zynko.org/api"

// Error is an answer the API refused (docs.zynko.org/api/#erros).
type Error struct {
	// HTTP status.
	Status int
	// bad_request, forbidden, not_found, rate_limited...
	Code    string
	Message string
}

func (e *Error) Error() string {
	return fmt.Sprintf("zynko: %d %s: %s", e.Status, e.Code, e.Message)
}

// RestOptions configures a Rest.
type RestOptions struct {
	Token string
	// https://zynko.org/api unless you run your own server.
	API string
	// Waits out a 429 and tries again, up to this many times. 0 means the default, 3;
	// -1 never retries.
	Retries int
	// The HTTP client; http.DefaultClient when nil.
	HTTPClient *http.Client
}

// Rest is the HTTP API with a bot's token. Every route is reachable through Request; the
// common ones have their own method.
type Rest struct {
	api     string
	token   string
	retries int
	http    *http.Client
	// The client messages are bound to, so their methods know the caches; nil for a bare Rest.
	client *Client
}

// NewRest makes a Rest on its own, without a gateway connection.
func NewRest(opts RestOptions) *Rest {
	api := strings.TrimRight(opts.API, "/")
	if api == "" {
		api = DefaultAPI
	}
	retries := opts.Retries
	if retries == 0 {
		retries = 3
	} else if retries < 0 {
		retries = 0
	}
	h := opts.HTTPClient
	if h == nil {
		h = http.DefaultClient
	}
	return &Rest{api: api, token: opts.Token, retries: retries, http: h}
}

// API is the API's address, without a trailing slash.
func (r *Rest) API() string { return r.api }

// Base is the server's address without /api, to turn /api/media/... paths into links.
func (r *Rest) Base() string { return strings.TrimSuffix(r.api, "/api") }

// Link turns an avatar, emoji or other /api/... path the API hands out into a full link.
func (r *Rest) Link(path string) string {
	if path == "" || strings.HasPrefix(path, "http:") || strings.HasPrefix(path, "https:") {
		return path
	}
	return r.Base() + path
}

// Request calls any route. body is sent as JSON (unless it is a *Form); the answer is decoded
// into out when out is not nil.
func (r *Rest) Request(ctx context.Context, method, path string, body, out any) error {
	var payload []byte
	contentType := ""
	switch b := body.(type) {
	case nil:
	case *Form:
		payload, contentType = b.body, b.contentType
	default:
		var err error
		if payload, err = json.Marshal(b); err != nil {
			return err
		}
		contentType = "application/json"
	}
	for attempt := 0; ; attempt++ {
		var reader io.Reader
		if payload != nil {
			reader = bytes.NewReader(payload)
		}
		req, err := http.NewRequestWithContext(ctx, method, r.api+path, reader)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bot "+r.token)
		req.Header.Set("User-Agent", "zynko-go")
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		res, err := r.http.Do(req)
		if err != nil {
			return err
		}
		data, err := io.ReadAll(res.Body)
		res.Body.Close()
		if err != nil {
			return err
		}
		if res.StatusCode >= 200 && res.StatusCode < 300 {
			if out == nil || len(bytes.TrimSpace(data)) == 0 {
				return nil
			}
			return json.Unmarshal(data, out)
		}
		var e struct {
			Code         string   `json:"code"`
			Message      string   `json:"message"`
			RetryAfterMS *float64 `json:"retry_after_ms"`
		}
		_ = json.Unmarshal(data, &e)
		if res.StatusCode == http.StatusTooManyRequests && attempt < r.retries && e.Code == "rate_limited" {
			wait := time.Second
			if e.RetryAfterMS != nil {
				wait = time.Duration(*e.RetryAfterMS * float64(time.Millisecond))
			} else if s, err := strconv.ParseFloat(res.Header.Get("Retry-After"), 64); err == nil {
				wait = time.Duration(s * float64(time.Second))
			}
			select {
			case <-time.After(wait + 50*time.Millisecond):
				continue
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		if e.Code == "" {
			e.Code = "http_" + strconv.Itoa(res.StatusCode)
		}
		if e.Message == "" {
			e.Message = http.StatusText(res.StatusCode)
		}
		return &Error{Status: res.StatusCode, Code: e.Code, Message: e.Message}
	}
}

// Form is a multipart body for Request (an upload).
type Form struct {
	body        []byte
	contentType string
}

// File is a file to send with a message.
type File struct {
	Name string
	Data []byte
	// Default: worked out from the name.
	ContentType string
}

// FileFromPath reads a file from disk, to send.
func FileFromPath(path string) (File, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return File{}, err
	}
	return File{Name: filepath.Base(path), Data: data}, nil
}

// SendOptions is a message to send.
type SendOptions struct {
	Content string
	// Answer this message (from the same channel).
	ReplyTo string
	// Files to send with it, uploaded first (up to 10).
	Files []File
	Nonce string
	// Cards (up to 5).
	Cards []Card
	// Rows of buttons.
	Components Components
}

// EditOptions changes a message: only what is set. A nil field stays as it was; an empty,
// non-nil Components (Components{}) takes the buttons away.
type EditOptions struct {
	// Empty leaves the text as it was.
	Content    string
	Cards      []Card
	Components Components
}

func (e EditOptions) body() map[string]any {
	b := map[string]any{}
	if e.Content != "" {
		b["content"] = e.Content
	}
	if e.Cards != nil {
		b["cards"] = e.Cards
	}
	if e.Components != nil {
		b["components"] = e.Components
	}
	return b
}

// ReplyOptions is what an interaction's answer carries.
type ReplyOptions struct {
	Content    string
	Cards      []Card
	Components Components
	// Only the person who used the command sees it (and it is not kept). No buttons.
	Ephemeral bool
}

func (r *Rest) bind(m *Message) *Message {
	if m != nil {
		m.client, m.rest = r.client, r
	}
	return m
}

// ---------- messages ----------

// Send sends a message; files are uploaded first.
func (r *Rest) Send(ctx context.Context, channelID string, opts SendOptions) (*Message, error) {
	body := map[string]any{"content": opts.Content}
	if opts.ReplyTo != "" {
		body["reply_to"] = opts.ReplyTo
	}
	if opts.Nonce != "" {
		body["nonce"] = opts.Nonce
	}
	if len(opts.Files) > 0 {
		ids := make([]string, 0, len(opts.Files))
		for _, f := range opts.Files {
			a, err := r.Upload(ctx, channelID, f)
			if err != nil {
				return nil, err
			}
			ids = append(ids, a.ID)
		}
		body["attachment_ids"] = ids
	}
	if len(opts.Cards) > 0 {
		body["cards"] = opts.Cards
	}
	if len(opts.Components) > 0 {
		body["components"] = opts.Components
	}
	var m Message
	if err := r.Request(ctx, "POST", "/channels/"+channelID+"/messages", body, &m); err != nil {
		return nil, err
	}
	return r.bind(&m), nil
}

// Upload uploads a file to a channel, to be sent in a message (see Send).
func (r *Rest) Upload(ctx context.Context, channelID string, f File) (*Attachment, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename="%s"`, strings.NewReplacer(`"`, "_", "\\", "_", "\r", "", "\n", "").Replace(f.Name)))
	ct := f.ContentType
	if ct == "" {
		ct = http.DetectContentType(f.Data)
		if byExt := mimeByExt(f.Name); byExt != "" {
			ct = byExt
		}
	}
	h.Set("Content-Type", ct)
	part, err := w.CreatePart(h)
	if err != nil {
		return nil, err
	}
	if _, err := part.Write(f.Data); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	var a Attachment
	err = r.Request(ctx, "POST", "/channels/"+channelID+"/attachments", &Form{body: buf.Bytes(), contentType: w.FormDataContentType()}, &a)
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// EditMessage changes the bot's own message.
func (r *Rest) EditMessage(ctx context.Context, channelID, messageID string, edit EditOptions) (*Message, error) {
	var m Message
	if err := r.Request(ctx, "PATCH", "/channels/"+channelID+"/messages/"+messageID, edit.body(), &m); err != nil {
		return nil, err
	}
	return r.bind(&m), nil
}

// DeleteMessage deletes a message: the bot's own, or anyone's with Gerenciar mensagens.
func (r *Rest) DeleteMessage(ctx context.Context, channelID, messageID string) error {
	return r.Request(ctx, "DELETE", "/channels/"+channelID+"/messages/"+messageID, nil, nil)
}

// MessagesQuery picks which messages to fetch.
type MessagesQuery struct {
	Before string
	After  string
	// Up to 100.
	Limit int
}

// Messages fetches a channel's messages, newest first.
func (r *Rest) Messages(ctx context.Context, channelID string, q MessagesQuery) ([]*Message, error) {
	v := url.Values{}
	if q.Before != "" {
		v.Set("before", q.Before)
	}
	if q.After != "" {
		v.Set("after", q.After)
	}
	if q.Limit > 0 {
		v.Set("limit", strconv.Itoa(q.Limit))
	}
	path := "/channels/" + channelID + "/messages"
	if len(v) > 0 {
		path += "?" + v.Encode()
	}
	var ms []*Message
	if err := r.Request(ctx, "GET", path, nil, &ms); err != nil {
		return nil, err
	}
	for _, m := range ms {
		r.bind(m)
	}
	return ms, nil
}

// React adds the bot's reaction: a Unicode emoji, or name:id for a server emoji.
func (r *Rest) React(ctx context.Context, channelID, messageID, emoji string) error {
	return r.Request(ctx, "PUT", "/channels/"+channelID+"/messages/"+messageID+"/reactions/"+url.PathEscape(emoji), nil, nil)
}

// Unreact takes the bot's reaction away.
func (r *Rest) Unreact(ctx context.Context, channelID, messageID, emoji string) error {
	return r.Request(ctx, "DELETE", "/channels/"+channelID+"/messages/"+messageID+"/reactions/"+url.PathEscape(emoji), nil, nil)
}

// Typing shows "digitando..." for a few seconds.
func (r *Rest) Typing(ctx context.Context, channelID string) error {
	return r.Request(ctx, "POST", "/channels/"+channelID+"/typing", map[string]any{}, nil)
}

// ---------- people, servers, channels ----------

// Me is the bot's own account.
func (r *Rest) Me(ctx context.Context) (*User, error) {
	var u User
	return &u, r.Request(ctx, "GET", "/users/@me", nil, &u)
}

// Profile is someone's profile.
func (r *Rest) Profile(ctx context.Context, userID string) (*Profile, error) {
	var p Profile
	return &p, r.Request(ctx, "GET", "/users/"+userID+"/profile", nil, &p)
}

// DM is the DM channel with someone, opened if needed.
func (r *Rest) DM(ctx context.Context, userID string) (*Channel, error) {
	var c Channel
	return &c, r.Request(ctx, "POST", "/users/@me/channels", map[string]any{"recipient_id": userID}, &c)
}

// Members lists a server's members.
func (r *Rest) Members(ctx context.Context, guildID string) ([]Member, error) {
	var ms []Member
	return ms, r.Request(ctx, "GET", "/guilds/"+guildID+"/members", nil, &ms)
}

// Kick removes someone from a server.
func (r *Rest) Kick(ctx context.Context, guildID, userID string) error {
	return r.Request(ctx, "DELETE", "/guilds/"+guildID+"/members/"+userID, nil, nil)
}

// Ban bans someone from a server.
func (r *Rest) Ban(ctx context.Context, guildID, userID string) error {
	return r.Request(ctx, "PUT", "/guilds/"+guildID+"/bans/"+userID, nil, nil)
}

// Unban lifts a ban.
func (r *Rest) Unban(ctx context.Context, guildID, userID string) error {
	return r.Request(ctx, "DELETE", "/guilds/"+guildID+"/bans/"+userID, nil, nil)
}

// AddRole gives someone a role.
func (r *Rest) AddRole(ctx context.Context, guildID, userID, roleID string) error {
	return r.Request(ctx, "PUT", "/guilds/"+guildID+"/members/"+userID+"/roles/"+roleID, nil, nil)
}

// RemoveRole takes a role away from someone.
func (r *Rest) RemoveRole(ctx context.Context, guildID, userID, roleID string) error {
	return r.Request(ctx, "DELETE", "/guilds/"+guildID+"/members/"+userID+"/roles/"+roleID, nil, nil)
}

// NewRole is a role to create.
type NewRole struct {
	Name        string      `json:"name"`
	Color       *int        `json:"color,omitempty"`
	Permissions *Permission `json:"permissions,omitempty"`
	Hoist       bool        `json:"hoist,omitempty"`
}

// CreateRole creates a role.
func (r *Rest) CreateRole(ctx context.Context, guildID string, role NewRole) (*Role, error) {
	var out Role
	return &out, r.Request(ctx, "POST", "/guilds/"+guildID+"/roles", role, &out)
}

// NewChannel is a channel to create.
type NewChannel struct {
	Name string `json:"name"`
	// ChannelText (the default), ChannelVoice, ChannelCategory or ChannelForum.
	Type     int    `json:"type"`
	ParentID string `json:"parent_id,omitempty"`
	Topic    string `json:"topic,omitempty"`
}

// CreateChannel creates a channel.
func (r *Rest) CreateChannel(ctx context.Context, guildID string, ch NewChannel) (*Channel, error) {
	var out Channel
	return &out, r.Request(ctx, "POST", "/guilds/"+guildID+"/channels", ch, &out)
}

// ChannelEdit changes a channel: only the fields that are set.
type ChannelEdit struct {
	Name             *string `json:"name,omitempty"`
	Topic            *string `json:"topic,omitempty"`
	Position         *int    `json:"position,omitempty"`
	ParentID         *string `json:"parent_id,omitempty"`
	RateLimitPerUser *int    `json:"rate_limit_per_user,omitempty"`
}

// EditChannel changes a channel.
func (r *Rest) EditChannel(ctx context.Context, channelID string, edit ChannelEdit) (*Channel, error) {
	var out Channel
	return &out, r.Request(ctx, "PATCH", "/channels/"+channelID, edit, &out)
}

// DeleteChannel deletes a channel.
func (r *Rest) DeleteChannel(ctx context.Context, channelID string) error {
	return r.Request(ctx, "DELETE", "/channels/"+channelID, nil, nil)
}

// InviteOptions are an invite's limits; zero means none.
type InviteOptions struct {
	MaxAgeSecs int `json:"max_age_secs,omitempty"`
	MaxUses    int `json:"max_uses,omitempty"`
}

// CreateInvite makes an invite to a server.
func (r *Rest) CreateInvite(ctx context.Context, guildID string, opts InviteOptions) (*Invite, error) {
	var out Invite
	return &out, r.Request(ctx, "POST", "/guilds/"+guildID+"/invites", opts, &out)
}

// ---------- commands ----------

// SetCommands replaces all of the bot's "/" commands.
func (r *Rest) SetCommands(ctx context.Context, commands []Command) ([]Command, error) {
	if commands == nil {
		commands = []Command{}
	}
	var out []Command
	return out, r.Request(ctx, "PUT", "/users/@me/commands", commands, &out)
}

// Commands lists the bot's "/" commands.
func (r *Rest) Commands(ctx context.Context) ([]Command, error) {
	var out []Command
	return out, r.Request(ctx, "GET", "/users/@me/commands", nil, &out)
}

// InteractionCallback answers an interaction (see Interaction). kind is "message" or "update".
func (r *Rest) InteractionCallback(ctx context.Context, id, token, kind string, body map[string]any) (*Message, error) {
	b := map[string]any{"type": kind}
	for k, v := range body {
		b[k] = v
	}
	var m Message
	if err := r.Request(ctx, "POST", "/interactions/"+id+"/"+token+"/callback", b, &m); err != nil {
		return nil, err
	}
	return r.bind(&m), nil
}

// ---------- voice ----------

// VoiceToken is a media token for a voice room (the voice package uses it).
func (r *Rest) VoiceToken(ctx context.Context, channelID string) (*VoiceJoin, error) {
	var out VoiceJoin
	return &out, r.Request(ctx, "POST", "/channels/"+channelID+"/voice", map[string]any{}, &out)
}

func mimeByExt(name string) string {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".mp3":
		return "audio/mpeg"
	case ".ogg":
		return "audio/ogg"
	case ".mp4":
		return "video/mp4"
	case ".pdf":
		return "application/pdf"
	case ".txt":
		return "text/plain; charset=utf-8"
	case ".json":
		return "application/json"
	}
	return ""
}
