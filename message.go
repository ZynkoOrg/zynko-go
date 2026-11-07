package zynko

import (
	"context"
	"errors"
)

// Message is a message, with shortcuts to answer it, react, edit and delete.
type Message struct {
	ID              string          `json:"id"`
	ChannelID       string          `json:"channel_id"`
	Author          User            `json:"author"`
	Content         string          `json:"content"`
	Timestamp       int64           `json:"timestamp"`
	EditedTimestamp int64           `json:"edited_timestamp,omitempty"`
	ReplyTo         *MessageRef     `json:"reply_to"`
	Attachments     []Attachment    `json:"attachments"`
	Reactions       []ReactionCount `json:"reactions"`
	Nonce           string          `json:"nonce,omitempty"`
	Embeds          []Embed         `json:"embeds"`
	Cards           []Card          `json:"cards,omitempty"`
	Components      Components      `json:"components,omitempty"`
	// The command or click this (bot) message answers.
	Interaction *InteractionRef `json:"interaction,omitempty"`
	// Only its reader sees it, and it is not kept.
	Ephemeral bool `json:"ephemeral,omitempty"`

	client *Client
	rest   *Rest
}

var errUnbound = errors.New("zynko: mensagem sem cliente (venha de um Client ou de um Rest)")

// Channel is the message's channel, when the client has seen it.
func (m *Message) Channel() (Channel, bool) {
	if m.client == nil {
		return Channel{}, false
	}
	return m.client.Channel(m.ChannelID)
}

// GuildID is the server the message is in; "" in a DM (or when the channel is unknown).
func (m *Message) GuildID() string {
	ch, _ := m.Channel()
	return ch.GuildID
}

// Member is the author as a member of the server (nickname, roles); false in DMs.
func (m *Message) Member() (Member, bool) {
	g := m.GuildID()
	if g == "" {
		return Member{}, false
	}
	return m.client.Member(g, m.Author.ID)
}

// Mine reports whether the author is the bot itself.
func (m *Message) Mine() bool {
	return m.client != nil && m.Author.ID == m.client.UserID()
}

// Reply answers in the same channel, as a reply to this message.
func (m *Message) Reply(ctx context.Context, content string) (*Message, error) {
	return m.ReplyWith(ctx, SendOptions{Content: content})
}

// ReplyWith answers with files, cards or buttons, as a reply to this message.
func (m *Message) ReplyWith(ctx context.Context, opts SendOptions) (*Message, error) {
	if m.rest == nil {
		return nil, errUnbound
	}
	opts.ReplyTo = m.ID
	return m.rest.Send(ctx, m.ChannelID, opts)
}

// React adds the bot's reaction.
func (m *Message) React(ctx context.Context, emoji string) error {
	if m.rest == nil {
		return errUnbound
	}
	return m.rest.React(ctx, m.ChannelID, m.ID, emoji)
}

// Unreact takes the bot's reaction away.
func (m *Message) Unreact(ctx context.Context, emoji string) error {
	if m.rest == nil {
		return errUnbound
	}
	return m.rest.Unreact(ctx, m.ChannelID, m.ID, emoji)
}

// Edit changes the text. Only the author edits: a bot edits its own messages.
func (m *Message) Edit(ctx context.Context, content string) (*Message, error) {
	return m.EditWith(ctx, EditOptions{Content: content})
}

// EditWith changes the text, cards or buttons: whatever is set.
func (m *Message) EditWith(ctx context.Context, edit EditOptions) (*Message, error) {
	if m.rest == nil {
		return nil, errUnbound
	}
	return m.rest.EditMessage(ctx, m.ChannelID, m.ID, edit)
}

// Delete deletes the message: the bot's own, or anyone's with Gerenciar mensagens.
func (m *Message) Delete(ctx context.Context) error {
	if m.rest == nil {
		return errUnbound
	}
	return m.rest.DeleteMessage(ctx, m.ChannelID, m.ID)
}
