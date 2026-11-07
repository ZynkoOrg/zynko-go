// Música: joins your voice room and plays links (mp3, ogg, radio streams, anything ffmpeg
// opens), with a queue. "/tocar <link>" or "!tocar <link>"; what is playing shows as a card
// with Pausar, Pular and Parar buttons.
//
//	ZYNKO_TOKEN=bot_... go run ./examples/musica
package main

import (
	"context"
	"fmt"
	"log"
	"net/url"
	"os"
	"os/signal"
	"path"
	"regexp"
	"strings"
	"time"

	"zynko.org/go"
	"zynko.org/go/voice"
)

const lavanda = 0xa594f9

// song is what the player carries for each track.
type song struct {
	URL, Title, By, ChannelID string
}

var (
	client *zynko.Client
	link   = regexp.MustCompile(`(?i)^https?://\S+$`)
)

var commands = []zynko.Command{
	{Name: "tocar", Description: "Toca um link, ou põe na fila", Options: []zynko.CommandOption{
		{Name: "link", Description: "Link de áudio (mp3, ogg, rádio...)", Type: zynko.OptionString, Required: true},
	}},
	{Name: "pular", Description: "Vai para a próxima da fila"},
	{Name: "parar", Description: "Para e esvazia a fila"},
	{Name: "fila", Description: "O que está tocando e as próximas"},
	{Name: "sair", Description: "Sai da sala de voz"},
}

// ctx is where a command came from: a "!" message or a "/" command, answered the same way.
type ctx struct {
	context.Context
	userID, name, guildID, channelID string
	// say answers; quiet ones only the person sees, when the command came by "/".
	say func(r zynko.ReplyOptions, quiet bool) error
}

func (c ctx) text(s string, quiet bool) error { return c.say(zynko.ReplyOptions{Content: s}, quiet) }

func titleOf(s string) string {
	u, err := url.Parse(s)
	if err != nil {
		return s
	}
	if t, err := url.PathUnescape(path.Base(u.Path)); err == nil && t != "/" && t != "." {
		return t
	}
	return u.Host
}

// nowPlaying is the "Tocando agora" card, with the next ones and the buttons.
func nowPlaying() zynko.ReplyOptions {
	conn := voice.Of(client)
	if conn == nil || conn.Player.Current() == nil {
		return zynko.ReplyOptions{Cards: []zynko.Card{{Title: "Nada tocando", Color: lavanda}}, Components: zynko.Components{}}
	}
	p := conn.Player
	s := p.Current().Meta.(song)
	state, emoji, toggle := "Tocando agora", "⏸️", "Pausar"
	if p.Paused() {
		state, emoji, toggle = "Pausado", "▶️", "Continuar"
	}
	card := zynko.Card{
		Author: &zynko.CardAuthor{Name: state},
		Title:  s.Title,
		URL:    s.URL,
		Color:  lavanda,
		Fields: []zynko.CardField{{Name: "Pedido por", Value: s.By, Inline: true}},
	}
	var next []string
	for i, t := range p.Queue() {
		if i == 5 {
			break
		}
		next = append(next, fmt.Sprintf("%d. %s", i+1, t.Meta.(song).Title))
	}
	if len(next) > 0 {
		card.Fields = append(card.Fields, zynko.CardField{Name: fmt.Sprintf("Próximas (%d)", len(p.Queue())), Value: strings.Join(next, "\n")})
	}
	return zynko.ReplyOptions{
		Cards: []zynko.Card{card},
		Components: zynko.Components{{
			{Style: zynko.ButtonSecondary, Emoji: emoji, Label: toggle, CustomID: "pausar"},
			{Style: zynko.ButtonPrimary, Emoji: "⏭️", Label: "Pular", CustomID: "pular"},
			{Style: zynko.ButtonDanger, Emoji: "⏹️", Label: "Parar", CustomID: "parar"},
		}},
	}
}

// voiceFor joins the author's voice room, or says why not.
func voiceFor(c ctx) (*voice.Connection, error) {
	vs, ok := client.VoiceState(c.userID)
	room, _ := client.Channel(vs.ChannelID)
	if !ok || room.GuildID != c.guildID {
		return nil, c.text("Entre numa sala de voz deste servidor primeiro.", true)
	}
	if conn := voice.Of(client); conn != nil && conn.ChannelID() == room.ID {
		return conn, nil
	}
	if !client.Can(room.ID, client.UserID(), zynko.PermConnect|zynko.PermSpeak) {
		return nil, c.text("Não tenho permissão para entrar e falar nessa sala.", true)
	}
	conn, err := voice.Join(c, client, room.ID, voice.Options{YTDLP: os.Getenv("YTDLP")})
	if err != nil {
		return nil, err
	}
	conn.Player.OnStart(func(t *voice.Track) {
		s := t.Meta.(song)
		np := nowPlaying()
		_, _ = client.SendWith(context.Background(), s.ChannelID, zynko.SendOptions{Cards: np.Cards, Components: np.Components})
	})
	conn.Player.OnError(func(err error, t *voice.Track) {
		s := t.Meta.(song)
		_, _ = client.Send(context.Background(), s.ChannelID, fmt.Sprintf("Não consegui tocar **%s**: %v", s.Title, err))
	})
	conn.OnDisconnect(func(reason string) { log.Print("saiu da voz: ", reason) })
	return conn, nil
}

func run(c ctx, name, arg string) error {
	conn := voice.Of(client)
	switch name {
	case "tocar":
		if !link.MatchString(arg) {
			return c.text("Use: /tocar <link de áudio>", true)
		}
		conn, err := voiceFor(c)
		if conn == nil {
			return err
		}
		s := song{URL: arg, Title: titleOf(arg), By: c.name, ChannelID: c.channelID}
		playing := conn.Player.Current() != nil
		conn.Player.Enqueue(voice.URL(arg), s)
		if playing {
			return c.text(fmt.Sprintf("Na fila (%d): **%s**", len(conn.Player.Queue()), s.Title), false)
		}
		return c.text("Vai tocar: **"+s.Title+"**", true)
	case "pular":
		if conn == nil || conn.Player.Current() == nil {
			return c.text("Nada tocando.", true)
		}
		conn.Player.Skip()
		return c.text("⏭️ Pulei.", true)
	case "parar":
		if conn != nil {
			conn.Player.Stop()
		}
		return c.text("⏹️ Parei e esvaziei a fila.", true)
	case "fila":
		return c.say(nowPlaying(), false)
	case "sair":
		if conn != nil {
			_ = conn.Leave(c)
		}
		return c.text("👋 Saí da sala.", true)
	}
	return nil
}

func main() {
	signals, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	var err error
	client, err = zynko.New(zynko.Options{
		Token:   os.Getenv("ZYNKO_TOKEN"),
		API:     os.Getenv("ZYNKO_API"),
		Intents: zynko.IntentGuilds | zynko.IntentMessages | zynko.IntentVoiceStates,
	})
	if err != nil {
		log.Fatal(err)
	}

	// The old way: "!tocar <link>", "!pular"...
	client.OnMessageCreate(func(cx context.Context, m *zynko.Message) error {
		if m.Mine() || m.Author.Bot || m.GuildID() == "" || !strings.HasPrefix(m.Content, "!") {
			return nil
		}
		name, arg, _ := strings.Cut(strings.TrimSpace(m.Content[1:]), " ")
		member, _ := m.Member()
		name = strings.ToLower(name)
		if name == "" {
			return nil
		}
		return run(ctx{
			Context: cx, userID: m.Author.ID, name: member.Name(), guildID: m.GuildID(), channelID: m.ChannelID,
			say: func(r zynko.ReplyOptions, _ bool) error {
				_, err := m.ReplyWith(cx, zynko.SendOptions{Content: r.Content, Cards: r.Cards, Components: r.Components})
				return err
			},
		}, name, strings.TrimSpace(arg))
	})

	client.OnInteraction(func(cx context.Context, i *zynko.Interaction) error {
		// The buttons on the "Tocando agora" card: act, then refresh that card.
		if i.IsButton() {
			if conn := voice.Of(client); conn != nil {
				p := conn.Player
				switch i.CustomID {
				case "pausar":
					if p.Paused() {
						p.Resume()
					} else {
						p.Pause()
					}
				case "pular":
					p.Skip()
				case "parar":
					p.Stop()
				}
				if i.CustomID != "pausar" {
					time.Sleep(300 * time.Millisecond) // a skip only shows the next song once it starts
				}
			}
			np := nowPlaying()
			_, err := i.UpdateWith(cx, zynko.EditOptions{Cards: np.Cards, Components: np.Components})
			return err
		}
		name := ""
		if i.Member != nil {
			name = i.Member.Name()
		} else {
			name = i.User.DisplayName
		}
		arg, _ := i.String("link")
		return run(ctx{
			Context: cx, userID: i.User.ID, name: name, guildID: i.GuildID, channelID: i.ChannelID,
			say: func(r zynko.ReplyOptions, quiet bool) error {
				r.Ephemeral = quiet
				if quiet {
					r.Components = nil
				}
				_, err := i.ReplyWith(cx, r)
				return err
			},
		}, i.Command, arg)
	})

	client.OnError(func(err error) { log.Print(err) })
	zynko.On(client, func(_ context.Context, e *zynko.Reconnecting) error {
		log.Printf("gateway caiu, reconectando em %v", e.Wait.Round(time.Second))
		return nil
	})

	me, err := client.Login(signals)
	if err != nil {
		log.Fatal(err)
	}
	if _, err := client.SetCommands(signals, commands); err != nil {
		log.Fatal(err)
	}
	log.Printf("%s conectado, com %d comandos", me.DisplayName, len(commands))

	<-signals.Done()
	client.Close()
}
