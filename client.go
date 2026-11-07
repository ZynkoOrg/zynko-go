package zynko

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Options configures a Client.
type Options struct {
	// The bot's token (dev.zynko.org). Keep it out of the code: an environment variable.
	Token string
	// What to receive. Default: IntentGuilds | IntentMessages.
	Intents Intents
	// https://zynko.org/api unless you run your own server.
	API string
	// The gateway; worked out from API unless given.
	Gateway string
	// Times a 429 is waited out and retried (default 3; -1 never).
	Retries int
	// The HTTP client for the API; http.DefaultClient when nil.
	HTTPClient *http.Client
	// Run handlers on the gateway's goroutine, one event after the other, instead of each
	// event on its own goroutine. A slow handler then holds every event back.
	SyncEvents bool
}

// VoiceHandler is what zynko.org/go/voice plugs into a client, so the client can tell the
// voice connection about the gateway. Bots don't implement it.
type VoiceHandler interface {
	ChannelID() string
	// The room's channel changed (maybe its voice server).
	HandleChannelUpdate(Channel)
	// The server says where the bot is in voice.
	HandleOwnVoiceState(VoiceState)
	// The gateway reconnected: say again where the bot is.
	HandleResync()
	Leave(ctx context.Context) error
}

// Client is a Zynko bot: it connects to the gateway, keeps servers, channels and members up
// to date, and hands events to handlers. All its methods are safe for concurrent use.
//
//	client, _ := zynko.New(zynko.Options{Token: os.Getenv("ZYNKO_TOKEN")})
//	client.OnMessageCreate(func(ctx context.Context, m *zynko.Message) error {
//		if m.Content == "!ping" {
//			_, err := m.Reply(ctx, "pong")
//			return err
//		}
//		return nil
//	})
//	me, err := client.Login(ctx)
type Client struct {
	// Rest is the whole API (see Rest).
	Rest    *Rest
	intents Intents

	gw         *gateway
	events     registry
	syncEvents bool

	// ctx lives until Close; handlers get it.
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}

	loginMu   sync.Mutex
	started   bool
	loginWait atomic.Bool
	first     chan error // READY or the first fatal error, for a waiting Login

	mu          sync.RWMutex
	user        *User
	sessionID   string
	guilds      map[string]*CachedGuild
	channels    map[string]Channel
	users       map[string]User
	voiceStates map[string]VoiceState
	voice       VoiceHandler
}

// New makes a client. It connects on Login.
func New(opts Options) (*Client, error) {
	if opts.Token == "" {
		return nil, errors.New("zynko: falta o token do bot (crie um em dev.zynko.org)")
	}
	rest := NewRest(RestOptions{Token: opts.Token, API: opts.API, Retries: opts.Retries, HTTPClient: opts.HTTPClient})
	intents := opts.Intents
	if intents == 0 {
		intents = IntentGuilds | IntentMessages
	}
	gwURL := opts.Gateway
	if gwURL == "" {
		gwURL = rest.api + "/gateway"
		if strings.HasPrefix(gwURL, "http") {
			gwURL = "ws" + strings.TrimPrefix(gwURL, "http")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	c := &Client{
		Rest:        rest,
		intents:     intents,
		syncEvents:  opts.SyncEvents,
		ctx:         ctx,
		cancel:      cancel,
		done:        make(chan struct{}),
		first:       make(chan error, 1),
		guilds:      map[string]*CachedGuild{},
		channels:    map[string]Channel{},
		users:       map[string]User{},
		voiceStates: map[string]VoiceState{},
	}
	rest.client = c
	c.gw = &gateway{
		url:     gwURL,
		token:   opts.Token,
		intents: intents,
		onDispatch: func(t string, d json.RawMessage) {
			c.dispatch(t, d)
		},
		onReady: func() {},
		onReconnect: func(code int, reason string, wait time.Duration) {
			c.emit(&Reconnecting{Code: code, Reason: reason, Wait: wait})
		},
		onFatal: func(err error) {
			if c.loginWait.CompareAndSwap(true, false) {
				c.first <- err
				return
			}
			c.emit(&ErrorEvent{Err: err})
		},
	}
	return c, nil
}

// Intents are what the client asked for.
func (c *Client) Intents() Intents { return c.intents }

// Login connects, and returns the bot's account once READY arrives. The client then stays
// connected (reconnecting by itself) until Close. If ctx ends first, or the token is refused,
// the client is closed.
func (c *Client) Login(ctx context.Context) (*User, error) {
	c.loginMu.Lock()
	if c.started {
		c.loginMu.Unlock()
		return nil, errors.New("zynko: Login já foi chamado")
	}
	if c.ctx.Err() != nil {
		c.loginMu.Unlock()
		return nil, ErrClosed
	}
	c.started = true
	c.loginWait.Store(true)
	c.loginMu.Unlock()
	go func() {
		defer close(c.done)
		c.gw.run(c.ctx)
	}()
	select {
	case err := <-c.first:
		if err != nil {
			c.Close()
			return nil, err
		}
		u, _ := c.User()
		return &u, nil
	case <-ctx.Done():
		c.loginWait.Store(false)
		c.Close()
		return nil, ctx.Err()
	case <-c.done:
		return nil, ErrClosed
	}
}

// Close leaves voice and closes the link. The client cannot be used to log in again.
func (c *Client) Close() error {
	if v := c.Voice(); v != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = v.Leave(ctx)
		cancel()
	}
	c.gw.close()
	c.cancel()
	c.loginMu.Lock()
	started := c.started
	c.started = true
	c.loginMu.Unlock()
	if started {
		<-c.done
	}
	return nil
}

// Done is closed when the client stops for good: Close, or the token was refused.
func (c *Client) Done() <-chan struct{} {
	c.loginMu.Lock()
	defer c.loginMu.Unlock()
	if !c.started {
		// Never logged in: nothing will close it.
		ch := make(chan struct{})
		if c.ctx.Err() != nil {
			close(ch)
		}
		return ch
	}
	return c.done
}

// SendOp sends a gateway op (4: voice state). It reports whether the link was open.
func (c *Client) SendOp(op int, d any) bool { return c.gw.send(op, d) }

// ---------- shortcuts ----------

// Send sends a text message to a channel.
func (c *Client) Send(ctx context.Context, channelID, content string) (*Message, error) {
	return c.Rest.Send(ctx, channelID, SendOptions{Content: content})
}

// SendWith sends a message with files, cards or buttons.
func (c *Client) SendWith(ctx context.Context, channelID string, opts SendOptions) (*Message, error) {
	return c.Rest.Send(ctx, channelID, opts)
}

// SendDM sends someone a direct message.
func (c *Client) SendDM(ctx context.Context, userID string, opts SendOptions) (*Message, error) {
	dm, err := c.Rest.DM(ctx, userID)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.channels[dm.ID] = *dm
	c.mu.Unlock()
	return c.Rest.Send(ctx, dm.ID, opts)
}

// SetCommands replaces all of the bot's "/" commands (they show up in the apps right away).
func (c *Client) SetCommands(ctx context.Context, commands []Command) ([]Command, error) {
	return c.Rest.SetCommands(ctx, commands)
}

// Link turns an avatar, emoji or other /api/... path the API hands out into a full link.
func (c *Client) Link(path string) string { return c.Rest.Link(path) }

// ---------- voice hook ----------

// Voice is the voice connection the bot is in, if any (one at a time, as for anyone).
// zynko.org/go/voice has the typed version: voice.Of(client).
func (c *Client) Voice() VoiceHandler {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.voice
}

// SetVoice is for zynko.org/go/voice: it records the bot's voice connection. When old is
// not nil, it only replaces old (compare-and-swap); it reports whether it did.
func (c *Client) SetVoice(old, v VoiceHandler) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if old != nil && c.voice != old {
		return false
	}
	c.voice = v
	return true
}

// ---------- caches ----------

// User is the bot's own account, once connected.
func (c *Client) User() (User, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.user == nil {
		return User{}, false
	}
	return *c.user, true
}

// UserID is the bot's own id ("" before READY).
func (c *Client) UserID() string {
	u, _ := c.User()
	return u.ID
}

// SessionID is this gateway connection's id.
func (c *Client) SessionID() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.sessionID
}

// Guild is one server the bot is in.
func (c *Client) Guild(id string) (*CachedGuild, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	g, ok := c.guilds[id]
	return g, ok
}

// Guilds are the servers the bot is in.
func (c *Client) Guilds() []*CachedGuild {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return slices.Collect(maps.Values(c.guilds))
}

// Channel is any channel the bot sees, DMs included.
func (c *Client) Channel(id string) (Channel, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	ch, ok := c.channels[id]
	return ch, ok
}

// Channels are every channel the bot sees, DMs included.
func (c *Client) Channels() []Channel {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return slices.Collect(maps.Values(c.channels))
}

// CachedUser is an account the bot has seen.
func (c *Client) CachedUser(id string) (User, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	u, ok := c.users[id]
	return u, ok
}

// Users are the accounts the bot has seen.
func (c *Client) Users() []User {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return slices.Collect(maps.Values(c.users))
}

// VoiceState is where someone is in voice (needs IntentVoiceStates).
func (c *Client) VoiceState(userID string) (VoiceState, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	v, ok := c.voiceStates[userID]
	return v, ok
}

// VoiceStates are everyone in voice that the bot sees.
func (c *Client) VoiceStates() []VoiceState {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return slices.Collect(maps.Values(c.voiceStates))
}

// Member is someone as a member of a server.
func (c *Client) Member(guildID, userID string) (Member, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if g := c.guilds[guildID]; g != nil {
		m, ok := g.members[userID]
		return m, ok
	}
	return Member{}, false
}

// CachedGuild is a server as the client keeps it, updated by the gateway's events. Its
// methods are safe for concurrent use and return copies.
type CachedGuild struct {
	ID string
	mu *sync.RWMutex

	info     Guild
	channels map[string]Channel
	members  map[string]Member
	roles    map[string]Role
	emojis   []Emoji
	online   map[string]bool
}

// Info is the server's name, icon and owner.
func (g *CachedGuild) Info() Guild { g.mu.RLock(); defer g.mu.RUnlock(); return g.info }

// Name is the server's name.
func (g *CachedGuild) Name() string { return g.Info().Name }

// OwnerID is the server's owner.
func (g *CachedGuild) OwnerID() string { return g.Info().OwnerID }

// Channel is one of the server's channels the bot sees.
func (g *CachedGuild) Channel(id string) (Channel, bool) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	c, ok := g.channels[id]
	return c, ok
}

// Channels are the server's channels the bot sees, by position.
func (g *CachedGuild) Channels() []Channel {
	g.mu.RLock()
	defer g.mu.RUnlock()
	out := slices.Collect(maps.Values(g.channels))
	slices.SortFunc(out, func(a, b Channel) int { return a.Position - b.Position })
	return out
}

// Member is someone in the server.
func (g *CachedGuild) Member(userID string) (Member, bool) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	m, ok := g.members[userID]
	return m, ok
}

// Members are the server's members.
func (g *CachedGuild) Members() []Member {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return slices.Collect(maps.Values(g.members))
}

// Role is one of the server's roles (the server's id is @everyone).
func (g *CachedGuild) Role(id string) (Role, bool) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	r, ok := g.roles[id]
	return r, ok
}

// Roles are the server's roles, by position.
func (g *CachedGuild) Roles() []Role {
	g.mu.RLock()
	defer g.mu.RUnlock()
	out := slices.Collect(maps.Values(g.roles))
	slices.SortFunc(out, func(a, b Role) int { return a.Position - b.Position })
	return out
}

// Emojis are the server's custom emojis.
func (g *CachedGuild) Emojis() []Emoji {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return slices.Clone(g.emojis)
}

// Online reports whether a member is online now (needs IntentPresences to stay current).
func (g *CachedGuild) Online(userID string) bool {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.online[userID]
}

// OnlineIDs are the members online now.
func (g *CachedGuild) OnlineIDs() []string {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return slices.Collect(maps.Keys(g.online))
}

// ---------- permissions ----------

// PermissionsIn is what someone may do in a channel, the server's way: @everyone and their
// roles, then the channel's overwrites. Owners and admins may do everything. 0 when the
// client does not know the channel or the member.
func (c *Client) PermissionsIn(channelID, userID string) Permission {
	c.mu.RLock()
	defer c.mu.RUnlock()
	ch, ok := c.channels[channelID]
	if !ok || ch.GuildID == "" {
		return 0
	}
	g := c.guilds[ch.GuildID]
	if g == nil {
		return 0
	}
	m, ok := g.members[userID]
	if !ok {
		return 0
	}
	return ComputePermissions(g.info, g.roles, m, ch)
}

// Can reports whether someone has a permission (or all of several) in a channel.
func (c *Client) Can(channelID, userID string, p Permission) bool {
	return c.PermissionsIn(channelID, userID).Has(p)
}

// ComputePermissions is the server's permission math, for a member in a channel of guild.
// roles holds the server's roles by id, @everyone (the server's id) included.
func ComputePermissions(guild Guild, roles map[string]Role, m Member, ch Channel) Permission {
	if guild.OwnerID == m.User.ID || m.IsAdmin {
		return PermAll
	}
	base := roles[guild.ID].Permissions
	for _, r := range m.Roles {
		base |= roles[r].Permissions
	}
	if base&PermAdministrator != 0 {
		return PermAll
	}
	p := base
	for _, o := range ch.PermissionOverwrites {
		if o.Type == 0 && o.ID == guild.ID {
			p = (p &^ o.Deny) | o.Allow
			break
		}
	}
	var allow, deny Permission
	for _, o := range ch.PermissionOverwrites {
		if o.Type == 0 && o.ID != guild.ID && slices.Contains(m.Roles, o.ID) {
			allow |= o.Allow
			deny |= o.Deny
		}
	}
	p = (p &^ deny) | allow
	for _, o := range ch.PermissionOverwrites {
		if o.Type == 1 && o.ID == m.User.ID {
			p = (p &^ o.Deny) | o.Allow
			break
		}
	}
	if p&PermViewChannel == 0 {
		return 0
	}
	if p&PermSendMessages == 0 {
		p &^= PermAttachFiles
	}
	if p&PermConnect == 0 {
		p &^= PermSpeak | PermStream
	}
	return p
}

// ---------- events into caches ----------

func (c *Client) newGuild(g GuildFull) *CachedGuild {
	cg := &CachedGuild{
		ID:       g.ID,
		mu:       &c.mu,
		info:     g.Guild,
		channels: make(map[string]Channel, len(g.Channels)),
		members:  make(map[string]Member, len(g.Members)),
		roles:    make(map[string]Role, len(g.Roles)),
		emojis:   g.Emojis,
		online:   make(map[string]bool, len(g.Presences)),
	}
	for _, ch := range g.Channels {
		cg.channels[ch.ID] = ch
		c.channels[ch.ID] = ch
	}
	for _, m := range g.Members {
		cg.members[m.User.ID] = m
		c.users[m.User.ID] = m.User
	}
	for _, r := range g.Roles {
		cg.roles[r.ID] = r
	}
	for _, id := range g.Presences {
		cg.online[id] = true
	}
	c.guilds[g.ID] = cg
	return cg
}

func (c *Client) dispatch(t string, d json.RawMessage) {
	c.emit(&Raw{Type: t, Data: d})
	switch t {
	case "READY":
		var r ReadyData
		if json.Unmarshal(d, &r) != nil {
			return
		}
		c.mu.Lock()
		c.user = &r.User
		c.sessionID = r.SessionID
		clear(c.guilds)
		clear(c.channels)
		clear(c.voiceStates)
		guilds := make([]*CachedGuild, 0, len(r.Guilds))
		for _, g := range r.Guilds {
			guilds = append(guilds, c.newGuild(g))
		}
		for _, ch := range r.DMChannels {
			c.channels[ch.ID] = ch
			for _, u := range ch.Recipients {
				c.users[u.ID] = u
			}
		}
		for _, v := range r.VoiceStates {
			if v.ChannelID != "" {
				c.voiceStates[v.UserID] = v
			}
		}
		voice := c.voice
		c.mu.Unlock()
		if c.loginWait.CompareAndSwap(true, false) {
			c.first <- nil
		}
		if voice != nil {
			voice.HandleResync()
		}
		c.emit(&Ready{User: r.User})
		for _, g := range guilds {
			c.emit(&GuildCreate{Guild: g})
		}
	case "GUILD_CREATE":
		var g GuildFull
		if json.Unmarshal(d, &g) != nil {
			return
		}
		c.mu.Lock()
		if old := c.guilds[g.ID]; old != nil {
			c.dropGuild(old)
		}
		cg := c.newGuild(g)
		c.mu.Unlock()
		c.emit(&GuildCreate{Guild: cg})
	case "GUILD_UPDATE":
		var g Guild
		if json.Unmarshal(d, &g) != nil {
			return
		}
		c.mu.Lock()
		cg := c.guilds[g.ID]
		if cg != nil {
			cg.info = g
		}
		c.mu.Unlock()
		if cg != nil {
			c.emit(&GuildUpdate{Guild: cg})
		}
	case "GUILD_DELETE":
		var g struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(d, &g) != nil {
			return
		}
		c.mu.Lock()
		if cg := c.guilds[g.ID]; cg != nil {
			c.dropGuild(cg)
		}
		c.mu.Unlock()
		c.emit(&GuildDelete{GuildID: g.ID})
	case "GUILD_ROLE_CREATE", "GUILD_ROLE_UPDATE":
		var r Role
		if json.Unmarshal(d, &r) != nil {
			return
		}
		c.mu.Lock()
		if cg := c.guilds[r.GuildID]; cg != nil {
			cg.roles[r.ID] = r
		}
		c.mu.Unlock()
	case "GUILD_ROLE_DELETE":
		var r struct {
			GuildID string `json:"guild_id"`
			RoleID  string `json:"role_id"`
			ID      string `json:"id"`
		}
		if json.Unmarshal(d, &r) != nil {
			return
		}
		if r.RoleID == "" {
			r.RoleID = r.ID
		}
		c.mu.Lock()
		if cg := c.guilds[r.GuildID]; cg != nil {
			delete(cg.roles, r.RoleID)
		}
		c.mu.Unlock()
	case "GUILD_EMOJIS_UPDATE":
		var e struct {
			GuildID string  `json:"guild_id"`
			Emojis  []Emoji `json:"emojis"`
		}
		if json.Unmarshal(d, &e) != nil {
			return
		}
		c.mu.Lock()
		if cg := c.guilds[e.GuildID]; cg != nil {
			cg.emojis = e.Emojis
		}
		c.mu.Unlock()
	case "CHANNEL_CREATE", "CHANNEL_UPDATE":
		var ch Channel
		if json.Unmarshal(d, &ch) != nil {
			return
		}
		c.mu.Lock()
		prev, had := c.channels[ch.ID]
		if had && prev.Recipients != nil && ch.Recipients == nil {
			ch.Recipients = prev.Recipients
		}
		c.channels[ch.ID] = ch
		if ch.GuildID != "" {
			if cg := c.guilds[ch.GuildID]; cg != nil {
				cg.channels[ch.ID] = ch
			}
		}
		for _, u := range ch.Recipients {
			c.users[u.ID] = u
		}
		voice := c.voice
		c.mu.Unlock()
		if t == "CHANNEL_CREATE" {
			c.emit(&ChannelCreate{Channel: ch})
			return
		}
		if voice != nil {
			voice.HandleChannelUpdate(ch)
		}
		var p *Channel
		if had {
			p = &prev
		}
		c.emit(&ChannelUpdate{Channel: ch, Previous: p})
	case "CHANNEL_DELETE":
		var ch Channel
		if json.Unmarshal(d, &ch) != nil {
			return
		}
		c.mu.Lock()
		delete(c.channels, ch.ID)
		if ch.GuildID != "" {
			if cg := c.guilds[ch.GuildID]; cg != nil {
				delete(cg.channels, ch.ID)
			}
		}
		c.mu.Unlock()
		c.emit(&ChannelDelete{Channel: ch})
	case "GUILD_MEMBER_ADD", "GUILD_MEMBER_UPDATE":
		var m Member
		if json.Unmarshal(d, &m) != nil {
			return
		}
		c.mu.Lock()
		var p *Member
		if cg := c.guilds[m.GuildID]; cg != nil {
			if prev, ok := cg.members[m.User.ID]; ok {
				p = &prev
			}
			cg.members[m.User.ID] = m
		}
		c.users[m.User.ID] = m.User
		c.mu.Unlock()
		if t == "GUILD_MEMBER_ADD" {
			c.emit(&MemberAdd{Member: m})
		} else {
			c.emit(&MemberUpdate{Member: m, Previous: p})
		}
	case "GUILD_MEMBER_REMOVE":
		var e MemberRemove
		if json.Unmarshal(d, &e) != nil {
			return
		}
		c.mu.Lock()
		if cg := c.guilds[e.GuildID]; cg != nil {
			delete(cg.members, e.UserID)
			delete(cg.online, e.UserID)
		}
		c.mu.Unlock()
		c.emit(&e)
	case "PRESENCE_UPDATE":
		var e PresenceUpdate
		if json.Unmarshal(d, &e) != nil {
			return
		}
		c.mu.Lock()
		for _, cg := range c.guilds {
			if _, ok := cg.members[e.UserID]; !ok {
				continue
			}
			if e.Status == "online" {
				cg.online[e.UserID] = true
			} else {
				delete(cg.online, e.UserID)
			}
		}
		c.mu.Unlock()
		c.emit(&e)
	case "USER_UPDATE":
		var u User
		if json.Unmarshal(d, &u) != nil {
			return
		}
		c.mu.Lock()
		c.users[u.ID] = u
		if c.user != nil && c.user.ID == u.ID {
			c.user = &u
		}
		c.mu.Unlock()
	case "MESSAGE_CREATE":
		var m Message
		if json.Unmarshal(d, &m) != nil {
			return
		}
		c.mu.Lock()
		if ch, ok := c.channels[m.ChannelID]; ok {
			ch.LastMessageID = m.ID
			c.channels[m.ChannelID] = ch
			if cg := c.guilds[ch.GuildID]; cg != nil {
				if gch, ok := cg.channels[ch.ID]; ok {
					gch.LastMessageID = m.ID
					cg.channels[ch.ID] = gch
				}
			}
		}
		c.users[m.Author.ID] = m.Author
		c.mu.Unlock()
		c.emit(&MessageCreate{Message: c.Rest.bind(&m)})
	case "MESSAGE_UPDATE":
		var m Message
		if json.Unmarshal(d, &m) != nil {
			return
		}
		c.emit(&MessageUpdate{Message: c.Rest.bind(&m)})
	case "MESSAGE_DELETE":
		var e MessageDelete
		if json.Unmarshal(d, &e) == nil {
			c.emit(&e)
		}
	case "MESSAGE_REACTION_ADD":
		var e ReactionAdd
		if json.Unmarshal(d, &e.Reaction) == nil {
			c.emit(&e)
		}
	case "MESSAGE_REACTION_REMOVE":
		var e ReactionRemove
		if json.Unmarshal(d, &e.Reaction) == nil {
			c.emit(&e)
		}
	case "TYPING_START":
		var e TypingStart
		if json.Unmarshal(d, &e.Typing) == nil {
			c.emit(&e)
		}
	case "INTERACTION_CREATE":
		var data InteractionData
		if json.Unmarshal(d, &data) != nil {
			return
		}
		c.emit(&InteractionCreate{Interaction: newInteraction(c, data)})
	case "VOICE_STATE_UPDATE":
		var v VoiceState
		if json.Unmarshal(d, &v) != nil {
			return
		}
		c.mu.Lock()
		prev, had := c.voiceStates[v.UserID]
		if v.ChannelID != "" {
			c.voiceStates[v.UserID] = v
		} else {
			delete(c.voiceStates, v.UserID)
		}
		own := c.user != nil && v.UserID == c.user.ID
		voice := c.voice
		c.mu.Unlock()
		if own && voice != nil {
			voice.HandleOwnVoiceState(v)
		}
		var p *VoiceState
		if had {
			p = &prev
		}
		c.emit(&VoiceStateUpdate{State: v, Previous: p})
	}
}

// dropGuild forgets a server and its channels. Call with c.mu held.
func (c *Client) dropGuild(cg *CachedGuild) {
	for id := range cg.channels {
		delete(c.channels, id)
	}
	delete(c.guilds, cg.ID)
}
