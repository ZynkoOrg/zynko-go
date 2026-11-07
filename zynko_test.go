package zynko

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

const gid = "100"

func perms(t *testing.T, roles []Role, m Member, ch Channel, owner string) Permission {
	t.Helper()
	byID := map[string]Role{}
	for _, r := range roles {
		byID[r.ID] = r
	}
	return ComputePermissions(Guild{ID: gid, OwnerID: owner}, byID, m, ch)
}

func TestPermissions(t *testing.T) {
	everyone := Role{ID: gid, Permissions: PermViewChannel | PermSendMessages | PermAttachFiles | PermConnect | PermSpeak}
	mod := Role{ID: "200", Permissions: PermKickMembers | PermManageMessages}
	admin := Role{ID: "300", Permissions: PermAdministrator}
	user := func(roles ...string) Member { return Member{User: User{ID: "1"}, Roles: roles} }
	ch := Channel{ID: "10", GuildID: gid}

	cases := []struct {
		name  string
		m     Member
		ch    Channel
		owner string
		want  Permission
	}{
		{"everyone only", user(), ch, "9", everyone.Permissions},
		{"plus a role", user("200"), ch, "9", everyone.Permissions | mod.Permissions},
		{"owner has all", user(), ch, "1", PermAll},
		{"is_admin has all", Member{User: User{ID: "1"}, IsAdmin: true}, ch, "9", PermAll},
		{"administrator role has all", user("300"), ch, "9", PermAll},
		{"everyone overwrite denies send, so attach goes too", user(), Channel{ID: "10", GuildID: gid, PermissionOverwrites: []Overwrite{
			{ID: gid, Type: 0, Deny: PermSendMessages},
		}}, "9", PermViewChannel | PermConnect | PermSpeak},
		{"role overwrite allows back what everyone denied", user("200"), Channel{ID: "10", GuildID: gid, PermissionOverwrites: []Overwrite{
			{ID: gid, Type: 0, Deny: PermSendMessages},
			{ID: "200", Type: 0, Allow: PermSendMessages},
		}}, "9", everyone.Permissions | mod.Permissions},
		{"member overwrite beats roles", user("200"), Channel{ID: "10", GuildID: gid, PermissionOverwrites: []Overwrite{
			{ID: "200", Type: 0, Allow: PermManageChannels},
			{ID: "1", Type: 1, Deny: PermManageChannels | PermKickMembers},
		}}, "9", everyone.Permissions | PermManageMessages},
		{"role overwrite of a role the member lacks is ignored", user(), Channel{ID: "10", GuildID: gid, PermissionOverwrites: []Overwrite{
			{ID: "200", Type: 0, Deny: PermSendMessages},
		}}, "9", everyone.Permissions},
		{"role deny and allow on the same bit: allow wins", user("200", "201"), Channel{ID: "10", GuildID: gid, PermissionOverwrites: []Overwrite{
			{ID: "200", Type: 0, Deny: PermSpeak},
			{ID: "201", Type: 0, Allow: PermSpeak},
		}}, "9", everyone.Permissions | mod.Permissions},
		{"no view means nothing", user(), Channel{ID: "10", GuildID: gid, PermissionOverwrites: []Overwrite{
			{ID: gid, Type: 0, Deny: PermViewChannel},
		}}, "9", 0},
		{"no connect takes speak and stream", user(), Channel{ID: "10", GuildID: gid, PermissionOverwrites: []Overwrite{
			{ID: "1", Type: 1, Deny: PermConnect, Allow: PermStream},
		}}, "9", PermViewChannel | PermSendMessages | PermAttachFiles},
	}
	for _, c := range cases {
		got := perms(t, []Role{everyone, mod, admin, {ID: "201"}}, c.m, c.ch, c.owner)
		if got != c.want {
			t.Errorf("%s: got %b, want %b", c.name, got, c.want)
		}
	}
	if !PermAll.Has(PermStream|PermAdministrator) || Permission(0).Has(PermSpeak) {
		t.Error("Has")
	}
	if PermAll != 1<<18-1 || IntentsAll != 127 {
		t.Error("constants")
	}
}

func TestIDTime(t *testing.T) {
	// 1 << 22 is one millisecond after the epoch.
	if got := IDTime("4194304").UnixMilli(); got != epochMS+1 {
		t.Errorf("IDTime = %d", got)
	}
	if !IDTime("x").IsZero() {
		t.Error("IDTime of junk")
	}
}

func TestInteractionOptions(t *testing.T) {
	c, _ := New(Options{Token: "x"})
	var d InteractionData
	raw := `{"id":"1","token":"t","type":"command","channel_id":"2","guild_id":null,"user":{"id":"3"},"member":null,
	"command":{"name":"dado","options":[{"name":"lados","type":"integer","value":20},{"name":"txt","type":"string","value":"oi"},
	{"name":"n","type":"number","value":"2.5"},{"name":"b","type":"boolean","value":true},{"name":"u","type":"user","value":"55"}]}}`
	if err := json.Unmarshal([]byte(raw), &d); err != nil {
		t.Fatal(err)
	}
	i := newInteraction(c, d)
	if i.Command != "dado" || !i.IsCommand() || i.IsButton() || i.GuildID != "" {
		t.Fatal("fields")
	}
	if v, ok := i.Int("lados"); !ok || v != 20 {
		t.Error("Int", v, ok)
	}
	if v, ok := i.String("txt"); !ok || v != "oi" {
		t.Error("String", v)
	}
	if v, ok := i.String("lados"); !ok || v != "20" {
		t.Error("String of a number", v)
	}
	if v, ok := i.Float("n"); !ok || v != 2.5 {
		t.Error("Float from text", v)
	}
	if _, ok := i.Int("n"); ok {
		t.Error("2.5 is not an integer")
	}
	if v, ok := i.Bool("b"); !ok || !v {
		t.Error("Bool")
	}
	if v, _ := i.String("u"); v != "55" {
		t.Error("user id")
	}
	if i.Has("nada") {
		t.Error("Has")
	}
	if _, err := i.Update(context.Background(), "x"); err == nil {
		t.Error("Update on a command must fail")
	}
}

func TestEditBody(t *testing.T) {
	b, _ := json.Marshal(EditOptions{Content: "oi"}.body())
	if string(b) != `{"content":"oi"}` {
		t.Error(string(b))
	}
	b, _ = json.Marshal(EditOptions{Components: Components{}}.body())
	if string(b) != `{"components":[]}` {
		t.Error("empty components must be sent to remove buttons:", string(b))
	}
	card, _ := json.Marshal(Card{Title: "t", Fields: []CardField{{Name: "a", Value: "b", Inline: true}}})
	if string(card) != `{"title":"t","fields":[{"name":"a","value":"b","inline":true}]}` {
		t.Error(string(card))
	}
}

func TestRestRetryAndError(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bot tok" {
			t.Error("auth header", r.Header.Get("Authorization"))
		}
		switch r.URL.Path {
		case "/api/users/@me":
			if calls.Add(1) < 3 {
				w.WriteHeader(429)
				w.Write([]byte(`{"code":"rate_limited","message":"devagar","retry_after_ms":10}`))
				return
			}
			w.Write([]byte(`{"id":"7","username":"b","display_name":"B","avatar":null,"bot":true}`))
		default:
			w.WriteHeader(403)
			w.Write([]byte(`{"code":"forbidden","message":"Sem permissão"}`))
		}
	}))
	defer srv.Close()
	r := NewRest(RestOptions{Token: "tok", API: srv.URL + "/api/"})
	u, err := r.Me(context.Background())
	if err != nil || u.ID != "7" || calls.Load() != 3 {
		t.Fatal("retry:", err, calls.Load())
	}
	err = r.Kick(context.Background(), "1", "2")
	var ze *Error
	if !errors.As(err, &ze) || ze.Status != 403 || ze.Code != "forbidden" || ze.Message != "Sem permissão" {
		t.Fatal("typed error:", err)
	}
	if r.Link("/api/media/x") != srv.URL+"/api/media/x" || r.Link("https://a/b") != "https://a/b" {
		t.Error("Link", r.Link("/api/media/x"))
	}
}

// fakeGateway speaks just enough of the gateway: HELLO, READY after IDENTIFY, ACKs, and
// whatever the test pushes.
func fakeGateway(t *testing.T, push chan string, conns *atomic.Int32) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		n := conns.Add(1)
		ctx := r.Context()
		c.Write(ctx, websocket.MessageText, []byte(`{"op":10,"d":{"heartbeat_interval":50}}`))
		for {
			_, data, err := c.Read(ctx)
			if err != nil {
				return
			}
			var f gatewayFrame
			json.Unmarshal(data, &f)
			switch f.Op {
			case 1:
				c.Write(ctx, websocket.MessageText, []byte(`{"op":11}`))
			case 2:
				var id struct {
					Token   string `json:"token"`
					Intents int    `json:"intents"`
				}
				json.Unmarshal(f.D, &id)
				if id.Token == "bad" {
					c.Close(4004, "token")
					return
				}
				ready := `{"op":0,"t":"READY","d":{"user":{"id":"7","username":"b","display_name":"B","bot":true},
				"guilds":[{"id":"100","name":"Casa","icon":null,"owner_id":"1","channels":[{"id":"10","guild_id":"100","type":0,"name":"geral","position":0}],
				"members":[{"guild_id":"100","user":{"id":"7","username":"b","display_name":"B","bot":true},"nick":null,"is_admin":false,"joined_at":0,"roles":[]}],
				"presences":["7"],"roles":[{"id":"100","guild_id":"100","name":"@everyone","color":null,"position":0,"permissions":16896,"hoist":false}],"emojis":[]}],
				"dm_channels":[],"presences":[],"voice_states":[],"session_id":"s` + string(rune('0'+n)) + `"}}`
				c.Write(ctx, websocket.MessageText, []byte(ready))
				go func() {
					for {
						select {
						case m := <-push:
							if m == "drop" {
								c.Close(4000, "drop")
								return
							}
							c.Write(ctx, websocket.MessageText, []byte(m))
						case <-ctx.Done():
							return
						}
					}
				}()
			}
		}
	}))
}

func TestClientGateway(t *testing.T) {
	push := make(chan string, 8)
	var conns atomic.Int32
	srv := fakeGateway(t, push, &conns)
	defer srv.Close()
	c, err := New(Options{Token: "good", API: srv.URL + "/api"})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	errs := make(chan error, 4)
	c.OnError(func(err error) { errs <- err })
	c.OnMessageCreate(func(ctx context.Context, m *Message) error {
		if m.Content == "boom" {
			return errors.New("falhou de propósito")
		}
		return nil
	})
	u, err := c.Login(ctx)
	if err != nil || u.ID != "7" {
		t.Fatal("login", err)
	}
	g, ok := c.Guild("100")
	if !ok || g.Name() != "Casa" || len(g.Channels()) != 1 || !g.Online("7") {
		t.Fatal("READY cache")
	}
	if !c.Can("10", "7", PermSendMessages) || c.Can("10", "7", PermKickMembers) {
		t.Error("permissions from cache", c.PermissionsIn("10", "7"))
	}

	// Wait + events into caches.
	go func() {
		push <- `{"op":0,"t":"CHANNEL_CREATE","d":{"id":"11","guild_id":"100","type":2,"name":"voz","position":1}}`
		push <- `{"op":0,"t":"MESSAGE_CREATE","d":{"id":"50","channel_id":"10","author":{"id":"8","username":"a","display_name":"A","bot":false},"content":"boom","timestamp":1,"edited_timestamp":null,"reply_to":null,"attachments":[],"reactions":[],"nonce":null,"embeds":[]}}`
	}()
	e, err := Wait(ctx, c, func(e *MessageCreate) bool { return e.Content == "boom" })
	if err != nil || e.Mine() || e.GuildID() != "100" {
		t.Fatal("Wait", err)
	}
	if _, ok := c.Channel("11"); !ok {
		t.Error("CHANNEL_CREATE cache")
	}
	if ch, _ := c.Channel("10"); ch.LastMessageID != "50" {
		t.Error("last_message_id")
	}
	select {
	case err := <-errs:
		if !strings.Contains(err.Error(), "de propósito") {
			t.Error(err)
		}
	case <-ctx.Done():
		t.Fatal("handler error did not become an error event")
	}

	// A drop: the client reconnects and READY rebuilds the caches.
	ready := make(chan struct{}, 2)
	c.OnReady(func(context.Context, User) error { ready <- struct{}{}; return nil })
	push <- "drop"
	select {
	case <-ready:
	case <-ctx.Done():
		t.Fatal("no reconnect")
	}
	if conns.Load() != 2 || c.SessionID() != "s2" {
		t.Error("second session", conns.Load(), c.SessionID())
	}
	if _, ok := c.Channel("11"); ok {
		t.Error("READY should have rebuilt the cache")
	}
	// Heartbeats keep going (interval 50 ms): still connected after a while.
	time.Sleep(300 * time.Millisecond)
	if !c.SendOp(1, nil) {
		t.Error("link should be open")
	}

	// Wait times out with the context.
	short, cancel2 := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel2()
	if _, err := Wait[*GuildDelete](short, c, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Error("Wait timeout", err)
	}
}

func TestLoginBadToken(t *testing.T) {
	var conns atomic.Int32
	srv := fakeGateway(t, make(chan string), &conns)
	defer srv.Close()
	c, _ := New(Options{Token: "bad", API: srv.URL + "/api"})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := c.Login(ctx)
	if err == nil || !strings.Contains(err.Error(), "4004") {
		t.Fatal("want 4004 error, got", err)
	}
	select {
	case <-c.Done():
	case <-ctx.Done():
		t.Fatal("client should stop")
	}
	if _, err := New(Options{}); err == nil {
		t.Error("missing token")
	}
}
