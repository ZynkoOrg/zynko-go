package zynko

import (
	"strconv"
	"time"
)

// Intents are the kinds of event a bot asks the gateway for (docs.zynko.org/api/gateway#intents).
// READY, GUILD_CREATE, GUILD_DELETE, CHANNEL_CREATE, USER_UPDATE and INTERACTION_CREATE always
// arrive.
type Intents int

const (
	// IntentGuilds: servers changing, and their channels, roles and emojis.
	IntentGuilds Intents = 1 << 0
	// IntentGuildMembers: members joining, leaving or changing.
	IntentGuildMembers Intents = 1 << 1
	// IntentPresences: who is online.
	IntentPresences Intents = 1 << 2
	// IntentMessages: messages created, edited and deleted, in servers and DMs.
	IntentMessages Intents = 1 << 3
	// IntentReactions: reactions added and removed.
	IntentReactions Intents = 1 << 4
	// IntentTyping: "digitando...".
	IntentTyping Intents = 1 << 5
	// IntentVoiceStates: who is in which voice room. A bot that joins voice needs it.
	IntentVoiceStates Intents = 1 << 6
	// IntentsAll is every intent.
	IntentsAll Intents = (1 << 7) - 1
)

// Permission is a role permission bit, or several OR-ed together (docs.zynko.org/api/permissoes).
type Permission int64

const (
	PermAdministrator      Permission = 1 << 0
	PermManageGuild        Permission = 1 << 1
	PermManageChannels     Permission = 1 << 2
	PermManageRoles        Permission = 1 << 3
	PermKickMembers        Permission = 1 << 4
	PermBanMembers         Permission = 1 << 5
	PermManageMessages     Permission = 1 << 6
	PermManageNicknames    Permission = 1 << 7
	PermCreateInvite       Permission = 1 << 8
	PermSendMessages       Permission = 1 << 9
	PermAttachFiles        Permission = 1 << 10
	PermConnect            Permission = 1 << 11
	PermSpeak              Permission = 1 << 12
	PermManageEmojis       Permission = 1 << 13
	PermViewChannel        Permission = 1 << 14
	PermAddReactions       Permission = 1 << 15
	PermReadMessageHistory Permission = 1 << 16
	PermStream             Permission = 1 << 17
	// PermAll is every permission there is.
	PermAll Permission = (1 << 18) - 1
)

// Has reports whether every bit of q is in p.
func (p Permission) Has(q Permission) bool { return p&q == q }

// Channel types.
const (
	ChannelText     = 0
	ChannelDM       = 1
	ChannelVoice    = 2
	ChannelCategory = 4
	ChannelPost     = 11
	ChannelForum    = 15
)

const epochMS = 1_767_225_600_000

// IDTime is when an id was made (ids are snowflakes). The zero time for an id that is not a
// number.
func IDTime(id string) time.Time {
	n, err := strconv.ParseUint(id, 10, 64)
	if err != nil {
		return time.Time{}
	}
	return time.UnixMilli(int64(n>>22) + epochMS)
}
