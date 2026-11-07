package zynko

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"runtime/debug"
	"sync"
	"time"
)

// The events a client hands to its handlers. Register with On, or with the shortcuts
// (OnMessageCreate, OnInteraction...). Every handler gets a context that ends when the
// client is closed.

// Ready: connected (again); the caches were rebuilt from READY.
type Ready struct{ User User }

// MessageCreate: a new message.
type MessageCreate struct{ *Message }

// MessageUpdate: a message was edited (or its link preview is ready).
type MessageUpdate struct{ *Message }

// MessageDelete: a message was deleted.
type MessageDelete struct {
	ID        string `json:"id"`
	ChannelID string `json:"channel_id"`
}

// ReactionAdd: someone reacted.
type ReactionAdd struct{ Reaction }

// ReactionRemove: someone took a reaction away.
type ReactionRemove struct{ Reaction }

// TypingStart: someone is typing.
type TypingStart struct{ Typing }

// GuildCreate: the bot was added to a server, or the server came in READY.
type GuildCreate struct{ Guild *CachedGuild }

// GuildUpdate: a server's name, icon or owner changed.
type GuildUpdate struct{ Guild *CachedGuild }

// GuildDelete: the bot left the server, or it was deleted.
type GuildDelete struct{ GuildID string }

// ChannelCreate: a new channel (a new DM is a new channel).
type ChannelCreate struct{ Channel Channel }

// ChannelUpdate: a channel changed. Previous is nil when the client had not seen it.
type ChannelUpdate struct {
	Channel  Channel
	Previous *Channel
}

// ChannelDelete: a channel was deleted, or the bot can no longer see it.
type ChannelDelete struct{ Channel Channel }

// MemberAdd: someone joined a server.
type MemberAdd struct{ Member Member }

// MemberUpdate: a member's nickname or roles changed.
type MemberUpdate struct {
	Member   Member
	Previous *Member
}

// MemberRemove: someone left a server.
type MemberRemove struct {
	GuildID string `json:"guild_id"`
	UserID  string `json:"user_id"`
}

// PresenceUpdate: someone came online or went offline.
type PresenceUpdate struct {
	UserID string `json:"user_id"`
	// "online" or "offline".
	Status string `json:"status"`
}

// VoiceStateUpdate: someone joined, left or changed in voice.
type VoiceStateUpdate struct {
	State    VoiceState
	Previous *VoiceState
}

// InteractionCreate: someone used one of the bot's "/" commands, or clicked a button on its
// message.
type InteractionCreate struct{ *Interaction }

// Raw is every gateway event, as it came.
type Raw struct {
	Type string
	Data json.RawMessage
}

// Reconnecting: the link dropped; the client is connecting again after Wait.
type Reconnecting struct {
	Code   int
	Reason string
	Wait   time.Duration
}

// ErrorEvent: a handler failed (returned an error or panicked), or the token was refused.
type ErrorEvent struct{ Err error }

type handler struct {
	id int
	fn func(ctx context.Context, ev any) error
}

type waiter struct {
	match func(ev any) bool
	ch    chan any
}

type registry struct {
	mu       sync.Mutex
	next     int
	handlers map[reflect.Type][]handler
	waiters  map[reflect.Type][]*waiter
}

// On registers a handler for one event type, given by the handler's argument: *MessageCreate,
// *InteractionCreate, *Ready... It returns a function that removes the handler.
//
// A handler that returns an error, or panics, becomes an *ErrorEvent instead of a crash.
//
//	zynko.On(client, func(ctx context.Context, e *zynko.MemberAdd) error {
//		log.Println(e.Member.User.DisplayName, "entrou")
//		return nil
//	})
func On[E any](c *Client, fn func(ctx context.Context, e E) error) (remove func()) {
	t := reflect.TypeFor[E]()
	r := &c.events
	r.mu.Lock()
	r.next++
	id := r.next
	if r.handlers == nil {
		r.handlers = map[reflect.Type][]handler{}
	}
	r.handlers[t] = append(r.handlers[t], handler{id: id, fn: func(ctx context.Context, ev any) error { return fn(ctx, ev.(E)) }})
	r.mu.Unlock()
	return func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		hs := r.handlers[t]
		for i, h := range hs {
			if h.id == id {
				r.handlers[t] = append(hs[:i:i], hs[i+1:]...)
				return
			}
		}
	}
}

// Wait waits for the next event of a type that passes filter (nil takes any). Give ctx a
// deadline: it is the only time limit.
//
//	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
//	defer cancel()
//	answer, err := zynko.Wait(ctx, client, func(e *zynko.MessageCreate) bool { return e.Author.ID == userID })
func Wait[E any](ctx context.Context, c *Client, filter func(E) bool) (E, error) {
	t := reflect.TypeFor[E]()
	w := &waiter{ch: make(chan any, 1)}
	w.match = func(ev any) bool { return filter == nil || filter(ev.(E)) }
	r := &c.events
	r.mu.Lock()
	if r.waiters == nil {
		r.waiters = map[reflect.Type][]*waiter{}
	}
	r.waiters[t] = append(r.waiters[t], w)
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		ws := r.waiters[t]
		for i, x := range ws {
			if x == w {
				r.waiters[t] = append(ws[:i:i], ws[i+1:]...)
				break
			}
		}
		r.mu.Unlock()
	}()
	select {
	case ev := <-w.ch:
		return ev.(E), nil
	case <-ctx.Done():
		var zero E
		return zero, fmt.Errorf("esperando %s: %w", t, ctx.Err())
	}
}

// emit hands an event to its waiters, then to its handlers. Handlers run on their own
// goroutine (one per event, handlers in order) unless SyncEvents is set.
func (c *Client) emit(ev any) {
	t := reflect.TypeOf(ev)
	r := &c.events
	r.mu.Lock()
	hs := append([]handler(nil), r.handlers[t]...)
	var keep []*waiter
	for _, w := range r.waiters[t] {
		if w.match(ev) {
			// Each waiter takes one event, then leaves.
			select {
			case w.ch <- ev:
			default:
			}
			continue
		}
		keep = append(keep, w)
	}
	if r.waiters != nil {
		r.waiters[t] = keep
	}
	r.mu.Unlock()
	if len(hs) == 0 {
		return
	}
	run := func() {
		for _, h := range hs {
			c.call(h, ev)
		}
	}
	if c.syncEvents {
		run()
	} else {
		go run()
	}
}

func (c *Client) call(h handler, ev any) {
	_, isErr := ev.(*ErrorEvent)
	defer func() {
		if p := recover(); p != nil && !isErr {
			c.emit(&ErrorEvent{Err: fmt.Errorf("pânico num handler de %T: %v\n%s", ev, p, debug.Stack())})
		}
	}()
	if err := h.fn(c.ctx, ev); err != nil && !isErr {
		c.emit(&ErrorEvent{Err: err})
	}
}

// ---------- shortcuts ----------

// OnReady runs fn on each READY (the first connection and every reconnection).
func (c *Client) OnReady(fn func(ctx context.Context, user User) error) (remove func()) {
	return On(c, func(ctx context.Context, e *Ready) error { return fn(ctx, e.User) })
}

// OnMessageCreate runs fn for each new message (needs IntentMessages).
func (c *Client) OnMessageCreate(fn func(ctx context.Context, m *Message) error) (remove func()) {
	return On(c, func(ctx context.Context, e *MessageCreate) error { return fn(ctx, e.Message) })
}

// OnInteraction runs fn when someone uses one of the bot's commands or clicks its buttons.
func (c *Client) OnInteraction(fn func(ctx context.Context, i *Interaction) error) (remove func()) {
	return On(c, func(ctx context.Context, e *InteractionCreate) error { return fn(ctx, e.Interaction) })
}

// OnError runs fn for errors in handlers, and when the gateway gives up.
func (c *Client) OnError(fn func(err error)) (remove func()) {
	return On(c, func(_ context.Context, e *ErrorEvent) error { fn(e.Err); return nil })
}

// ErrClosed is returned by Login after Close.
var ErrClosed = errors.New("zynko: cliente fechado")
