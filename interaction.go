package zynko

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"sync/atomic"
)

// Interaction is someone using one of the bot's "/" commands, or clicking a button on one of
// its messages. Answer within 15 minutes: Reply posts a message (or one only they see),
// Update changes the message whose button was clicked.
//
//	client.OnInteraction(func(ctx context.Context, i *zynko.Interaction) error {
//		if i.Command == "dado" {
//			lados, ok := i.Int("lados")
//			if !ok {
//				lados = 6
//			}
//			_, err := i.Reply(ctx, fmt.Sprintf("🎲 %d", 1+rand.Int64N(lados)))
//			return err
//		}
//		return nil
//	})
type Interaction struct {
	ID    string
	Token string
	// "command" or "button".
	Type      string
	ChannelID string
	// "" in a DM.
	GuildID string
	// Who used it.
	User User
	// Who used it, as a member of the server; nil in a DM.
	Member *Member
	// The command's name, for commands.
	Command string
	// What was typed for each option, by name, as the JSON value it came as.
	Options map[string]json.RawMessage
	// The clicked button's custom_id, for buttons.
	CustomID string
	// The message whose button was clicked.
	Message *Message
	// The event as it came.
	Data InteractionData

	client  *Client
	replied atomic.Bool
}

func newInteraction(c *Client, d InteractionData) *Interaction {
	i := &Interaction{
		ID:        d.ID,
		Token:     d.Token,
		Type:      d.Type,
		ChannelID: d.ChannelID,
		GuildID:   d.GuildID,
		User:      d.User,
		Member:    d.Member,
		CustomID:  d.CustomID,
		Options:   map[string]json.RawMessage{},
		Data:      d,
		client:    c,
	}
	if d.Command != nil {
		i.Command = d.Command.Name
		for _, o := range d.Command.Options {
			i.Options[o.Name] = o.Value
		}
	}
	if d.Message != nil {
		i.Message = c.Rest.bind(d.Message)
	}
	return i
}

// IsCommand reports whether it is a "/" command.
func (i *Interaction) IsCommand() bool { return i.Type == "command" }

// IsButton reports whether it is a button click.
func (i *Interaction) IsButton() bool { return i.Type == "button" }

// Replied reports whether it was answered already.
func (i *Interaction) Replied() bool { return i.replied.Load() }

// Has reports whether an option was filled in.
func (i *Interaction) Has(name string) bool {
	_, ok := i.Options[name]
	return ok
}

// String is an option's value as text (numbers in their JSON form). User, channel and role
// options are ids.
func (i *Interaction) String(name string) (string, bool) {
	raw, ok := i.Options[name]
	if !ok {
		return "", false
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, true
	}
	return string(raw), true
}

// Int is an integer option's value (a number typed as text counts too).
func (i *Interaction) Int(name string) (int64, bool) {
	f, ok := i.Float(name)
	if !ok || f != float64(int64(f)) {
		return 0, false
	}
	return int64(f), true
}

// Float is a number option's value (a number typed as text counts too).
func (i *Interaction) Float(name string) (float64, bool) {
	raw, ok := i.Options[name]
	if !ok {
		return 0, false
	}
	var f float64
	if json.Unmarshal(raw, &f) == nil {
		return f, true
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			return f, true
		}
	}
	return 0, false
}

// Bool is a boolean option's value.
func (i *Interaction) Bool(name string) (value, ok bool) {
	raw, ok := i.Options[name]
	if !ok {
		return false, false
	}
	if json.Unmarshal(raw, &value) != nil {
		return false, false
	}
	return value, true
}

// Channel is the channel it was used in, when the client has seen it.
func (i *Interaction) Channel() (Channel, bool) { return i.client.Channel(i.ChannelID) }

// Reply answers with a message in the channel. May be called again for follow-ups.
func (i *Interaction) Reply(ctx context.Context, content string) (*Message, error) {
	return i.ReplyWith(ctx, ReplyOptions{Content: content})
}

// ReplyEphemeral answers with a message only that person sees (and that is not kept).
func (i *Interaction) ReplyEphemeral(ctx context.Context, content string) (*Message, error) {
	return i.ReplyWith(ctx, ReplyOptions{Content: content, Ephemeral: true})
}

// ReplyWith answers with text, cards and buttons; Ephemeral for only that person.
func (i *Interaction) ReplyWith(ctx context.Context, r ReplyOptions) (*Message, error) {
	body := map[string]any{}
	if r.Content != "" {
		body["content"] = r.Content
	}
	if len(r.Cards) > 0 {
		body["cards"] = r.Cards
	}
	if len(r.Components) > 0 {
		body["components"] = r.Components
	}
	if r.Ephemeral {
		body["ephemeral"] = true
	}
	m, err := i.client.Rest.InteractionCallback(ctx, i.ID, i.Token, "message", body)
	if err == nil {
		i.replied.Store(true)
	}
	return m, err
}

// Update changes the text of the message whose button was clicked.
func (i *Interaction) Update(ctx context.Context, content string) (*Message, error) {
	return i.UpdateWith(ctx, EditOptions{Content: content})
}

// UpdateWith changes the clicked message: whatever is set (text, cards, buttons). To take
// the buttons away, set Components to Components{}.
func (i *Interaction) UpdateWith(ctx context.Context, edit EditOptions) (*Message, error) {
	if i.Type != "button" {
		return nil, errors.New("zynko: Update é só para cliques em botões")
	}
	m, err := i.client.Rest.InteractionCallback(ctx, i.ID, i.Token, "update", edit.body())
	if err == nil {
		i.replied.Store(true)
	}
	return m, err
}
