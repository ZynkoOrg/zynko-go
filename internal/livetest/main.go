// Command livetest checks the library against a running Zynko server, with the accounts in a
// JSON file (api, bot_token, human_token, guild_id, text_channel_id, voice_channel_id...).
// Tokens are read at runtime and never printed.
//
//	go run ./internal/livetest <sdk-go.json> text
//	go run ./internal/livetest <sdk-go.json> voice [media.mp3]
//	go run ./internal/livetest <sdk-go.json> listen [seconds]
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"math/rand/v2"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"zynko.org/go"
	"zynko.org/go/voice"
)

type config struct {
	API            string `json:"api"`
	BotID          string `json:"bot_id"`
	BotToken       string `json:"bot_token"`
	ListenerToken  string `json:"listener_token"`
	HumanToken     string `json:"human_token"`
	GuildID        string `json:"guild_id"`
	VoiceChannelID string `json:"voice_channel_id"`
	TextChannelID  string `json:"text_channel_id"`
}

var cfg config

func main() {
	log.SetFlags(log.Ltime | log.Lmicroseconds)
	if len(os.Args) < 3 {
		log.Fatal("uso: livetest <config.json> text|voice|listen")
	}
	raw, err := os.ReadFile(os.Args[1])
	if err != nil {
		log.Fatal(err)
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		log.Fatal(err)
	}
	switch os.Args[2] {
	case "text":
		err = text()
	case "voice":
		media := ""
		if len(os.Args) > 3 {
			media = os.Args[3]
		}
		err = playVoice(media)
	case "musica":
		err = musica(os.Args[3])
	case "move":
		err = move()
	case "listen":
		secs := 20
		if len(os.Args) > 3 {
			fmt.Sscan(os.Args[3], &secs)
		}
		err = listen(secs)
	default:
		err = errors.New("modo desconhecido")
	}
	if err != nil {
		log.Fatal("FALHOU: ", err)
	}
	log.Print("OK")
}

// human calls the API as the human test account.
func human(ctx context.Context, method, path string, body, out any) error {
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequestWithContext(ctx, method, strings.TrimRight(cfg.API, "/")+path, r)
	req.Header.Set("Authorization", cfg.HumanToken)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(res.Body)
	if res.StatusCode >= 300 {
		return fmt.Errorf("%s %s: %d %s", method, path, res.StatusCode, data)
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

func check(ok bool, format string, args ...any) error {
	if !ok {
		return fmt.Errorf(format, args...)
	}
	log.Printf("  ok: "+format, args...)
	return nil
}

func text() error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	client, err := zynko.New(zynko.Options{Token: cfg.BotToken, API: cfg.API, Intents: zynko.IntentGuilds | zynko.IntentMessages | zynko.IntentGuildMembers})
	if err != nil {
		return err
	}
	defer client.Close()
	client.OnError(func(err error) { log.Print("erro: ", err) })

	// 1. Login and caches.
	var guildEvents sync.WaitGroup
	guildEvents.Add(1)
	var once sync.Once
	zynko.On(client, func(ctx context.Context, e *zynko.GuildCreate) error {
		if e.Guild.ID == cfg.GuildID {
			once.Do(guildEvents.Done)
		}
		return nil
	})
	me, err := client.Login(ctx)
	if err != nil {
		return err
	}
	log.Printf("1. conectado como %s (%s)", me.DisplayName, me.ID)
	if err := check(me.ID == cfg.BotID, "bot id confere"); err != nil {
		return err
	}
	g, ok := client.Guild(cfg.GuildID)
	if err := check(ok, "servidor de teste no cache (%d servidores)", len(client.Guilds())); err != nil {
		return err
	}
	if err := check(len(g.Channels()) > 0, "servidor %q com %d canais, %d membros, %d cargos", g.Name(), len(g.Channels()), len(g.Members()), len(g.Roles())); err != nil {
		return err
	}
	_, ok = client.Channel(cfg.TextChannelID)
	if err := check(ok, "canal de texto no cache"); err != nil {
		return err
	}
	guildEvents.Wait()
	perms := client.PermissionsIn(cfg.TextChannelID, me.ID)
	if err := check(client.Can(cfg.TextChannelID, me.ID, zynko.PermSendMessages|zynko.PermViewChannel), "permissões do bot no canal: %b", perms); err != nil {
		return err
	}

	// 2. !ping -> pong.
	client.OnMessageCreate(func(ctx context.Context, m *zynko.Message) error {
		if m.Mine() || m.Content != "!ping" {
			return nil
		}
		_, err := m.Reply(ctx, "pong")
		return err
	})
	// A handler that fails must not kill the bot.
	client.OnMessageCreate(func(ctx context.Context, m *zynko.Message) error {
		if m.Content == "!ping" {
			panic("teste de pânico")
		}
		return nil
	})
	errSeen := make(chan error, 4)
	client.OnError(func(err error) { errSeen <- err })

	var sent zynko.Message
	waitPong, cancelWait := context.WithTimeout(ctx, 15*time.Second)
	defer cancelWait()
	pongCh := make(chan *zynko.MessageCreate, 1)
	go func() {
		e, err := zynko.Wait(waitPong, client, func(e *zynko.MessageCreate) bool { return e.Mine() && e.Content == "pong" })
		if err == nil {
			pongCh <- e
		}
		close(pongCh)
	}()
	time.Sleep(100 * time.Millisecond)
	if err := human(ctx, "POST", "/channels/"+cfg.TextChannelID+"/messages", map[string]any{"content": "!ping"}, &sent); err != nil {
		return err
	}
	log.Printf("2. humano mandou !ping (%s)", sent.ID)
	pong := <-pongCh
	if err := check(pong != nil, "Wait viu o pong pelo gateway"); err != nil {
		return err
	}
	var msgs []zynko.Message
	if err := human(ctx, "GET", "/channels/"+cfg.TextChannelID+"/messages?limit=10", nil, &msgs); err != nil {
		return err
	}
	found := false
	for _, m := range msgs {
		if m.Author.ID == cfg.BotID && m.Content == "pong" && m.ReplyTo != nil && m.ReplyTo.ID == sent.ID {
			found = true
		}
	}
	if err := check(found, "humano vê o pong em resposta ao !ping (GET messages)"); err != nil {
		return err
	}
	select {
	case e := <-errSeen:
		_ = check(strings.Contains(e.Error(), "teste de pânico"), "pânico num handler virou evento de erro")
	case <-time.After(3 * time.Second):
		return errors.New("o pânico não virou evento de erro")
	}

	// Message helpers: react, edit, files, delete.
	if err := pong.React(ctx, "👍"); err != nil {
		return fmt.Errorf("react: %w", err)
	}
	if err := pong.Unreact(ctx, "👍"); err != nil {
		return fmt.Errorf("unreact: %w", err)
	}
	edited, err := pong.Edit(ctx, "pong (editado)")
	if err != nil {
		return fmt.Errorf("edit: %w", err)
	}
	_ = check(edited.Content == "pong (editado)", "react, unreact e edit")
	withFile, err := client.SendWith(ctx, cfg.TextChannelID, zynko.SendOptions{
		Content: "arquivo de teste",
		Files:   []zynko.File{{Name: "teste.txt", Data: []byte("olá do Go\n")}},
	})
	if err != nil {
		return fmt.Errorf("send com arquivo: %w", err)
	}
	if err := check(len(withFile.Attachments) == 1 && withFile.Attachments[0].Filename == "teste.txt", "mensagem com arquivo (%d anexo)", len(withFile.Attachments)); err != nil {
		return err
	}
	if err := withFile.Delete(ctx); err != nil {
		return fmt.Errorf("delete: %w", err)
	}
	log.Print("  ok: delete")
	if _, err := client.SendDM(ctx, humanID(ctx), zynko.SendOptions{Content: "Oi! Teste de DM da biblioteca Go."}); err != nil {
		return fmt.Errorf("DM: %w", err)
	}
	log.Print("  ok: DM enviada")
	var zerr *zynko.Error
	_, err = client.Rest.Profile(ctx, "1")
	if err := check(errors.As(err, &zerr), "erro tipado da API: %v", err); err != nil {
		return err
	}

	// 3. Commands, ephemeral reply, card with a button, click, update.
	cmds, err := client.SetCommands(ctx, []zynko.Command{
		{Name: "dado", Description: "Rola um dado", Options: []zynko.CommandOption{
			{Name: "lados", Description: "Quantos lados", Type: zynko.OptionInteger, MinValue: zynko.Limit(2), MaxValue: zynko.Limit(100)},
		}},
		{Name: "segredo", Description: "Resposta só para você"},
	})
	if err != nil {
		return fmt.Errorf("setCommands: %w", err)
	}
	log.Printf("3. %d comandos cadastrados", len(cmds))
	replies := make(chan *zynko.Message, 4)
	client.OnInteraction(func(ctx context.Context, i *zynko.Interaction) error {
		switch {
		case i.Command == "segredo":
			m, err := i.ReplyEphemeral(ctx, "Só você vê isto.")
			if err == nil {
				replies <- m
			}
			return err
		case i.Command == "dado":
			lados, ok := i.Int("lados")
			if !ok {
				lados = 6
			}
			m, err := i.ReplyWith(ctx, zynko.ReplyOptions{
				Cards:      []zynko.Card{{Title: "Dado", Description: fmt.Sprintf("Saiu **%d** (d%d)", 1+rand.Int64N(lados), lados), Color: 0xa594f9}},
				Components: zynko.Components{{{Style: zynko.ButtonPrimary, Emoji: "🎲", Label: "De novo", CustomID: "de-novo"}}},
			})
			if err == nil {
				replies <- m
			}
			return err
		case i.IsButton() && i.CustomID == "de-novo":
			m, err := i.UpdateWith(ctx, zynko.EditOptions{
				Cards:      []zynko.Card{{Title: "Dado", Description: "Rolado de novo por " + i.User.DisplayName, Color: 0xa594f9}},
				Components: zynko.Components{},
			})
			if err == nil {
				replies <- m
			}
			return err
		}
		return nil
	})
	var ack struct {
		ID string `json:"id"`
	}
	if err := human(ctx, "POST", "/channels/"+cfg.TextChannelID+"/interactions", map[string]any{"type": "command", "bot_id": cfg.BotID, "name": "segredo"}, &ack); err != nil {
		return err
	}
	eph, err := recv(replies)
	if err != nil {
		return err
	}
	if err := check(eph.Ephemeral && eph.Content == "Só você vê isto.", "resposta efêmera (ephemeral=%v)", eph.Ephemeral); err != nil {
		return err
	}
	if err := human(ctx, "POST", "/channels/"+cfg.TextChannelID+"/interactions", map[string]any{"type": "command", "bot_id": cfg.BotID, "name": "dado", "options": []map[string]any{{"name": "lados", "value": 20}}}, &ack); err != nil {
		return err
	}
	card, err := recv(replies)
	if err != nil {
		return err
	}
	if err := check(len(card.Cards) == 1 && len(card.Components) == 1 && card.Interaction != nil && card.Interaction.Name == "dado", "cartão com botão: %q", card.Cards[0].Description); err != nil {
		return err
	}
	if err := human(ctx, "POST", "/channels/"+cfg.TextChannelID+"/interactions", map[string]any{"type": "button", "message_id": card.ID, "custom_id": "de-novo"}, &ack); err != nil {
		return err
	}
	if _, err := recv(replies); err != nil {
		return err
	}
	msgs = nil
	if err := human(ctx, "GET", "/channels/"+cfg.TextChannelID+"/messages?limit=10", nil, &msgs); err != nil {
		return err
	}
	for _, m := range msgs {
		if m.ID == card.ID {
			return check(len(m.Cards) == 1 && strings.HasPrefix(m.Cards[0].Description, "Rolado de novo") && len(m.Components) == 0,
				"o clique mudou a mensagem: %q, %d fileiras de botões", m.Cards[0].Description, len(m.Components))
		}
	}
	return errors.New("mensagem do cartão não encontrada")
}

func humanID(ctx context.Context) string {
	var u zynko.User
	_ = human(ctx, "GET", "/users/@me", nil, &u)
	return u.ID
}

func recv(ch chan *zynko.Message) (*zynko.Message, error) {
	select {
	case m := <-ch:
		return m, nil
	case <-time.After(15 * time.Second):
		return nil, errors.New("o bot não respondeu à interação")
	}
}

func playVoice(media string) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client, err := zynko.New(zynko.Options{Token: cfg.BotToken, API: cfg.API, Intents: zynko.IntentGuilds | zynko.IntentVoiceStates})
	if err != nil {
		return err
	}
	defer client.Close()
	if _, err := client.Login(ctx); err != nil {
		return err
	}
	jctx, jcancel := context.WithTimeout(ctx, 30*time.Second)
	conn, err := voice.Join(jctx, client, cfg.VoiceChannelID, voice.Options{Relay: os.Getenv("LIVETEST_RELAY") == "1"})
	jcancel()
	if err != nil {
		return err
	}
	log.Printf("4. na sala %s (servidor de voz %s, pode falar: %v)", conn.ChannelID(), conn.Server(), conn.CanSpeak())
	conn.OnDisconnect(func(r string) { log.Print("saiu da voz: ", r) })
	conn.OnError(func(err error) { log.Print("erro de voz: ", err) })
	p := conn.Player
	p.OnStart(func(t *voice.Track) { log.Printf("  start %v", t.Meta) })
	p.OnFinish(func(t *voice.Track) { log.Printf("  finish %v", t.Meta) })
	p.OnError(func(err error, t *voice.Track) { log.Printf("  error %v: %v", t.Meta, err) })
	idle := make(chan struct{}, 4)
	p.OnIdle(func() { log.Print("  idle"); idle <- struct{}{} })

	p.Enqueue(voice.Tone(440, 10), "tom 440 Hz")
	if media != "" {
		p.Enqueue(voice.URL(media), "música")
	}
	p.Enqueue(voice.Tone(660, 30), "tom para pular")
	time.Sleep(12 * time.Second)
	log.Printf("  tocando %v, fila %d", p.Current().Meta, len(p.Queue()))
	time.Sleep(4 * time.Second)
	p.Pause()
	log.Printf("  pausado=%v", p.Paused())
	time.Sleep(2 * time.Second)
	p.Resume()
	p.SetVolume(0.5)
	log.Printf("  continuou, volume %.1f", p.Volume())
	time.Sleep(3 * time.Second)
	p.SetVolume(5)
	log.Printf("  volume pedido 5, ficou %.1f", p.Volume())
	p.SetVolume(1)
	p.Skip()
	log.Printf("  pulou; agora %v", metaOf(p.Current()))
	time.Sleep(3 * time.Second)
	p.Stop()
	select {
	case <-idle:
	case <-time.After(5 * time.Second):
		return errors.New("o tocador não ficou ocioso depois de Stop")
	}
	if err := conn.Leave(ctx); err != nil {
		return err
	}
	time.Sleep(500 * time.Millisecond)
	return check(voice.Of(client) == nil, "saiu da sala")
}

func metaOf(t *voice.Track) any {
	if t == nil {
		return nil
	}
	return t.Meta
}

// listen joins as the listener bot with Receive and prints, per user, frames and loudness.
func listen(secs int) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client, err := zynko.New(zynko.Options{Token: cfg.ListenerToken, API: cfg.API, Intents: zynko.IntentGuilds | zynko.IntentVoiceStates})
	if err != nil {
		return err
	}
	defer client.Close()
	if _, err := client.Login(ctx); err != nil {
		return err
	}
	jctx, jcancel := context.WithTimeout(ctx, 30*time.Second)
	conn, err := voice.Join(jctx, client, cfg.VoiceChannelID, voice.Options{Receive: true})
	jcancel()
	if err != nil {
		return err
	}
	type stat struct {
		opus, frames int
		sq           float64
		n            int
	}
	var mu sync.Mutex
	stats := map[string]*stat{}
	get := func(id string) *stat {
		s := stats[id]
		if s == nil {
			s = &stat{}
			stats[id] = s
		}
		return s
	}
	conn.OnOpus(func(id string, _ []byte) { mu.Lock(); get(id).opus++; mu.Unlock() })
	conn.OnAudio(func(id string, samples []int16) {
		mu.Lock()
		s := get(id)
		s.frames++
		for _, v := range samples {
			s.sq += float64(v) * float64(v)
		}
		s.n += len(samples)
		mu.Unlock()
	})
	conn.OnSpeaking(func(ids []string) { log.Print("speaking ", strings.Join(ids, ",")) })
	log.Printf("ouvindo %d s", secs)
	time.Sleep(time.Duration(secs) * time.Second)
	mu.Lock()
	ids := make([]string, 0, len(stats))
	for id := range stats {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		s := stats[id]
		tag := ""
		if id == cfg.BotID {
			tag = " <- bot Go"
		}
		log.Printf("%s opus %d, pcm frames (20 ms) %d, rms %d%s", id, s.opus, s.frames, int(math.Sqrt(s.sq/math.Max(1, float64(s.n)))), tag)
	}
	if len(stats) == 0 {
		log.Print("sem áudio")
	}
	mu.Unlock()
	return conn.Leave(ctx)
}

// move checks that a connection follows its room to another voice server, with playback
// going on. It uses a voice room of its own, deleted at the end, so it does not move the
// shared test room under other people's feet.
func move() error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	bot, err := zynko.New(zynko.Options{Token: cfg.BotToken, API: cfg.API, Intents: zynko.IntentGuilds | zynko.IntentVoiceStates})
	if err != nil {
		return err
	}
	defer bot.Close()
	if _, err := bot.Login(ctx); err != nil {
		return err
	}
	room, err := bot.Rest.CreateChannel(ctx, cfg.GuildID, zynko.NewChannel{Name: "go-teste-troca", Type: zynko.ChannelVoice})
	if err != nil {
		return err
	}
	defer bot.Rest.DeleteChannel(context.Background(), room.ID)
	log.Printf("5. sala própria %s", room.ID)

	listener, err := zynko.New(zynko.Options{Token: cfg.ListenerToken, API: cfg.API, Intents: zynko.IntentGuilds | zynko.IntentVoiceStates})
	if err != nil {
		return err
	}
	defer listener.Close()
	if _, err := listener.Login(ctx); err != nil {
		return err
	}
	time.Sleep(300 * time.Millisecond) // CHANNEL_CREATE reaches both
	conn, err := voice.Join(ctx, bot, room.ID, voice.Options{})
	if err != nil {
		return err
	}
	readies := make(chan string, 4)
	conn.OnReady(func() { readies <- conn.Server() })
	conn.OnError(func(err error) { log.Print("erro de voz: ", err) })
	ear, err := voice.Join(ctx, listener, room.ID, voice.Options{Receive: true})
	if err != nil {
		return err
	}
	var heard atomic.Int64
	ear.OnOpus(func(id string, _ []byte) {
		if id == cfg.BotID {
			heard.Add(1)
		}
	})
	from := conn.Server()
	to := "br1"
	if from == "br1" {
		to = "br2"
	}
	finished := make(chan struct{}, 1)
	conn.Player.OnFinish(func(*voice.Track) { finished <- struct{}{} })
	conn.Player.Play(voice.Tone(440, 20), "tom de 20 s")
	time.Sleep(5 * time.Second)
	before := heard.Load()
	if err := check(before > 150, "antes da troca: %d pacotes do bot em %s", before, from); err != nil {
		return err
	}
	if err := human(ctx, "PUT", "/channels/"+room.ID+"/voice/server", map[string]any{"server": to}, nil); err != nil {
		return err
	}
	log.Printf("  trocou a sala para %s", to)
	select {
	case s := <-readies:
		if err := check(s == to, "o bot seguiu a sala para %s", s); err != nil {
			return err
		}
	case <-time.After(30 * time.Second):
		return errors.New("o bot não reconectou no servidor novo")
	}
	start := time.Now()
	time.Sleep(6 * time.Second)
	after := heard.Load()
	if err := check(conn.Player.Current() != nil && after-before > 150, "depois da troca: %d pacotes a mais (o ouvinte também seguiu, em %v)", after-before, time.Since(start).Round(time.Second)); err != nil {
		return err
	}
	select {
	case <-finished:
		return errors.New("o tom acabou cedo: a troca perdeu áudio")
	default:
	}
	_ = ear.Leave(ctx)
	return conn.Leave(ctx)
}

// musica drives the example music bot (running apart, with the bot's token) as the human:
// joins the voice room over the gateway, serves a file over HTTP, uses /tocar and the card's
// buttons.
func musica(file string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	// The human's own gateway connection, to be in the voice room (the bot plays where its
	// caller is).
	h, err := zynko.New(zynko.Options{Token: strings.TrimPrefix(cfg.HumanToken, "Bearer "), API: cfg.API, Intents: zynko.IntentGuilds | zynko.IntentVoiceStates})
	if err != nil {
		return err
	}
	defer h.Close()
	if _, err := h.Login(ctx); err != nil {
		return err
	}
	h.SendOp(4, map[string]any{"channel_id": cfg.VoiceChannelID, "self_mute": true, "self_deaf": false})
	defer h.SendOp(4, map[string]any{"channel_id": nil})
	time.Sleep(time.Second)

	srv := &http.Server{Addr: "127.0.0.1:18077", Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, file)
	})}
	go srv.ListenAndServe()
	defer srv.Close()

	var ack struct {
		ID string `json:"id"`
	}
	if err := human(ctx, "POST", "/channels/"+cfg.TextChannelID+"/interactions", map[string]any{
		"type": "command", "bot_id": cfg.BotID, "name": "tocar", "options": []map[string]any{{"name": "link", "value": "http://127.0.0.1:18077/musica-longa.mp3"}},
	}, &ack); err != nil {
		return err
	}
	log.Print("6. /tocar enviado")
	// The "Tocando agora" card.
	var card *zynko.Message
	for range 20 {
		time.Sleep(500 * time.Millisecond)
		var msgs []*zynko.Message
		if err := human(ctx, "GET", "/channels/"+cfg.TextChannelID+"/messages?limit=5", nil, &msgs); err != nil {
			return err
		}
		for _, m := range msgs {
			if m.Author.ID == cfg.BotID && len(m.Cards) == 1 && m.Cards[0].Author != nil && m.Cards[0].Author.Name == "Tocando agora" {
				card = m
				break
			}
		}
		if card != nil {
			break
		}
	}
	if err := check(card != nil, "cartão Tocando agora com %d botões", len(card.Components[0])); err != nil {
		return err
	}
	time.Sleep(4 * time.Second)
	if err := human(ctx, "POST", "/channels/"+cfg.TextChannelID+"/interactions", map[string]any{"type": "button", "message_id": card.ID, "custom_id": "pausar"}, &ack); err != nil {
		return err
	}
	time.Sleep(time.Second)
	var msgs []*zynko.Message
	if err := human(ctx, "GET", "/channels/"+cfg.TextChannelID+"/messages?limit=10", nil, &msgs); err != nil {
		return err
	}
	for _, m := range msgs {
		if m.ID == card.ID {
			if err := check(m.Cards[0].Author.Name == "Pausado" && m.Components[0][0].Label == "Continuar", "botão Pausar mudou o cartão para %q", m.Cards[0].Author.Name); err != nil {
				return err
			}
		}
	}
	time.Sleep(2 * time.Second)
	if err := human(ctx, "POST", "/channels/"+cfg.TextChannelID+"/interactions", map[string]any{"type": "button", "message_id": card.ID, "custom_id": "pausar"}, &ack); err != nil {
		return err
	}
	log.Print("  continuou; tocando mais 8 s")
	time.Sleep(8 * time.Second)
	if err := human(ctx, "POST", "/channels/"+cfg.TextChannelID+"/interactions", map[string]any{"type": "button", "message_id": card.ID, "custom_id": "parar"}, &ack); err != nil {
		return err
	}
	time.Sleep(time.Second)
	msgs = nil
	if err := human(ctx, "GET", "/channels/"+cfg.TextChannelID+"/messages?limit=10", nil, &msgs); err != nil {
		return err
	}
	for _, m := range msgs {
		if m.ID == card.ID {
			if err := check(m.Cards[0].Title == "Nada tocando", "botão Parar: %q", m.Cards[0].Title); err != nil {
				return err
			}
		}
	}
	// The old way, with "!".
	var sent zynko.Message
	if err := human(ctx, "POST", "/channels/"+cfg.TextChannelID+"/messages", map[string]any{"content": "!sair"}, &sent); err != nil {
		return err
	}
	time.Sleep(1500 * time.Millisecond)
	msgs = nil
	if err := human(ctx, "GET", "/channels/"+cfg.TextChannelID+"/messages?limit=5", nil, &msgs); err != nil {
		return err
	}
	for _, m := range msgs {
		if m.ReplyTo != nil && m.ReplyTo.ID == sent.ID {
			return check(strings.Contains(m.Content, "Saí da sala"), "!sair respondeu %q", m.Content)
		}
	}
	return errors.New("!sair sem resposta")
}
