// Package voice puts a Zynko bot in a voice room: it plays audio (files, links, streams, PCM)
// and can hear the room. The Zynko server hands out a media token (POST /channels/{id}/voice)
// and learns where the bot is from gateway op 4; the audio itself travels through LiveKit,
// the media server the apps use, with the pure-Go LiveKit SDK (no cgo).
//
// Pion has no Opus encoder, so audio is encoded by ffmpeg: the player decodes inputs to PCM
// (48 kHz, stereo, s16le) and paces 20 ms frames in real time; one long-lived ffmpeg per
// connection turns them into Opus, and the packets go to a LiveKit track. ffmpeg must be on
// the PATH (or in FFMPEG_PATH, or Options.FFmpeg).
//
//	conn, err := voice.Join(ctx, client, channelID, voice.Options{})
//	conn.Player.Enqueue(voice.URL("https://exemplo.com/musica.mp3"), nil)
package voice

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/stdr"
	"github.com/livekit/protocol/livekit"
	lklogger "github.com/livekit/protocol/logger"
	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"

	"zynko.org/go"
)

func init() {
	// LiveKit's SDK logs every step to the standard logger; a bot does not want that, unless
	// ZYNKO_LIVEKIT_LOG is set (to a verbosity: 1, 2...).
	if v, err := strconv.Atoi(os.Getenv("ZYNKO_LIVEKIT_LOG")); err == nil && v > 0 {
		stdr.SetVerbosity(v)
		lksdk.SetLogger(lklogger.LogRLogger(stdr.New(log.New(os.Stderr, "livekit ", log.LstdFlags))))
		return
	}
	lksdk.SetLogger(lklogger.LogRLogger(logr.Discard()))
}

// Options are how to join a room.
type Options struct {
	// Hear the room: OnOpus (and, through ffmpeg, OnAudio) with each person's sound. Off by
	// default, so a bot that only plays does not download everyone's audio.
	Receive bool
	// "Conexão alternativa": media only through the voice server's relay (TURN).
	Relay bool
	// Show the bot as deafened in the room (it still plays).
	Deaf bool
	// ffmpeg's path. Default: FFMPEG_PATH, or ffmpeg on the PATH.
	FFmpeg string
	// Hand http(s) links to yt-dlp first: its path ("yt-dlp" when it is on the PATH). For
	// pages of sites it supports instead of direct audio links.
	YTDLP string
	// Opus bitrate in bits per second. Default 128000.
	Bitrate int
}

// Status is where a connection stands.
type Status string

const (
	StatusConnecting   Status = "connecting"
	StatusConnected    Status = "connected"
	StatusReconnecting Status = "reconnecting"
	StatusClosed       Status = "closed"
)

// Connection is the bot in a voice room. One at a time per client, as for anyone.
type Connection struct {
	client    *zynko.Client
	channelID string
	opts      Options
	// Player plays audio into the room.
	Player *Player

	mu       sync.Mutex
	status   Status
	server   string
	canSpeak bool
	room     *lksdk.Room
	track    *lksdk.LocalTrack
	leaving  bool
	// The server confirmed the bot is in the room (a VOICE_STATE_UPDATE): only after that does
	// a state elsewhere mean the bot was moved out (an older one may still be on its way).
	confirmed bool
	moving    chan struct{} // open while moving to another voice server
	done      chan struct{}
	reason    string

	frames  chan []byte
	levels  chan uint8 // each encoded frame's loudness, in order, for its packet
	enc     *encoder
	pacerOn chan struct{}

	hmu          sync.Mutex
	onReady      []func()
	onSpeaking   []func([]string)
	onOpus       []func(string, []byte)
	onAudio      []func(string, []int16)
	onDisconnect []func(string)
	onError      []func(error)

	recv receivers
}

// Of is the client's voice connection, or nil.
func Of(c *zynko.Client) *Connection {
	v, _ := c.Voice().(*Connection)
	return v
}

// Join joins a voice room to play (and optionally hear) audio. The bot needs Entrar em salas
// and Falar there, and the client IntentVoiceStates. Already in that room: the same
// connection; in another: it leaves that one first.
func Join(ctx context.Context, c *zynko.Client, channelID string, opts Options) (*Connection, error) {
	if old := Of(c); old != nil {
		if old.channelID == channelID && old.Status() != StatusClosed {
			return old, nil
		}
		_ = old.Leave(ctx)
	}
	if opts.Bitrate <= 0 {
		opts.Bitrate = 128_000
	}
	conn := &Connection{
		client:    c,
		channelID: channelID,
		opts:      opts,
		status:    StatusConnecting,
		canSpeak:  true,
		done:      make(chan struct{}),
		frames:    make(chan []byte, 3),
		levels:    make(chan uint8, 64),
		pacerOn:   make(chan struct{}),
	}
	conn.recv.conn = conn
	conn.Player = newPlayer(conn, opts.FFmpeg, opts.YTDLP)
	c.SetVoice(nil, conn)
	if err := conn.connect(ctx); err != nil {
		c.SetVoice(conn, nil)
		conn.end("não conseguiu entrar")
		return nil, err
	}
	return conn, nil
}

// ChannelID is the room.
func (c *Connection) ChannelID() string { return c.channelID }

// Server is the voice server the room is on (Brasil 1, Brasil 2...).
func (c *Connection) Server() string { c.mu.Lock(); defer c.mu.Unlock(); return c.server }

// CanSpeak reports whether the bot may speak here: a room's roles may let it only listen.
func (c *Connection) CanSpeak() bool { c.mu.Lock(); defer c.mu.Unlock(); return c.canSpeak }

// Status is where the connection stands.
func (c *Connection) Status() Status { c.mu.Lock(); defer c.mu.Unlock(); return c.status }

// Done is closed when the connection ends for good.
func (c *Connection) Done() <-chan struct{} { return c.done }

// OnReady runs fn when connected to the room's media (again, after a server move).
func (c *Connection) OnReady(fn func()) {
	c.hmu.Lock()
	c.onReady = append(c.onReady, fn)
	c.hmu.Unlock()
}

// OnSpeaking runs fn with who is speaking now, by user id (LiveKit's active speakers).
func (c *Connection) OnSpeaking(fn func(userIDs []string)) {
	c.hmu.Lock()
	c.onSpeaking = append(c.onSpeaking, fn)
	c.hmu.Unlock()
}

// OnOpus runs fn with each Opus packet someone sends (20 ms, 48 kHz), as it arrives. Needs
// Receive. Runs on that person's own goroutine: keep it quick.
func (c *Connection) OnOpus(fn func(userID string, packet []byte)) {
	c.hmu.Lock()
	c.onOpus = append(c.onOpus, fn)
	c.hmu.Unlock()
}

// OnAudio runs fn with each person's sound decoded: 48 kHz mono 16-bit, 20 ms (960 samples)
// at a time. Needs Receive; decoding takes one ffmpeg per person speaking. Register it before
// people start talking: their decoders start with their track.
func (c *Connection) OnAudio(fn func(userID string, samples []int16)) {
	c.hmu.Lock()
	c.onAudio = append(c.onAudio, fn)
	c.hmu.Unlock()
}

// OnDisconnect runs fn when the connection ends for good: left, removed, or the room went
// away.
func (c *Connection) OnDisconnect(fn func(reason string)) {
	c.hmu.Lock()
	c.onDisconnect = append(c.onDisconnect, fn)
	c.hmu.Unlock()
}

// OnError runs fn for errors that do not end the connection (and the one that did, on a
// failed server move).
func (c *Connection) OnError(fn func(err error)) {
	c.hmu.Lock()
	c.onError = append(c.onError, fn)
	c.hmu.Unlock()
}

func (c *Connection) emitError(err error) {
	c.hmu.Lock()
	hs := append([]func(error){}, c.onError...)
	c.hmu.Unlock()
	for _, h := range hs {
		h(err)
	}
}

// connect gets a media token, joins the LiveKit room, publishes the track and tells the
// server where the bot is.
func (c *Connection) connect(ctx context.Context) error {
	join, err := c.client.Rest.VoiceToken(ctx, c.channelID)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.server = join.Server
	c.canSpeak = join.CanSpeak
	c.mu.Unlock()

	var room *lksdk.Room
	cb := lksdk.NewRoomCallback()
	cb.OnDisconnectedWithReason = func(reason lksdk.DisconnectionReason) {
		c.mu.Lock()
		current := c.room == room
		ignore := !current || c.leaving || c.moving != nil
		c.mu.Unlock()
		// The room closed under us: a server move is followed through CHANNEL_UPDATE;
		// anything else (removed, room deleted) ends the connection.
		if !ignore {
			go c.end(fmt.Sprintf("desconectado (%s)", reason))
		}
	}
	cb.OnActiveSpeakersChanged = func(ps []lksdk.Participant) {
		ids := make([]string, 0, len(ps))
		for _, p := range ps {
			ids = append(ids, p.Identity())
		}
		c.hmu.Lock()
		hs := append([]func([]string){}, c.onSpeaking...)
		c.hmu.Unlock()
		for _, h := range hs {
			h(ids)
		}
	}
	cb.OnTrackSubscribed = func(track *webrtc.TrackRemote, _ *lksdk.RemoteTrackPublication, rp *lksdk.RemoteParticipant) {
		if track.Kind() != webrtc.RTPCodecTypeAudio || !c.opts.Receive {
			return
		}
		go c.recv.hear(track, rp.Identity())
	}
	room = lksdk.NewRoom(cb)
	connectOpts := []lksdk.ConnectOption{lksdk.WithAutoSubscribe(c.opts.Receive)}
	if c.opts.Relay {
		connectOpts = append(connectOpts, lksdk.WithICETransportPolicy(webrtc.ICETransportPolicyRelay))
	}
	joined := make(chan error, 1)
	go func() { joined <- room.JoinWithToken(join.URL, join.Token, connectOpts...) }()
	select {
	case err = <-joined:
	case <-ctx.Done():
		go func() {
			if <-joined == nil {
				room.Disconnect()
			}
		}()
		return ctx.Err()
	}
	if err != nil {
		return fmt.Errorf("voz: não conectou ao LiveKit: %w", err)
	}

	var track *lksdk.LocalTrack
	if join.CanSpeak {
		track, err = lksdk.NewLocalSampleTrack(webrtc.RTPCodecCapability{
			MimeType:    webrtc.MimeTypeOpus,
			ClockRate:   SampleRate,
			Channels:    Channels,
			SDPFmtpLine: "minptime=10;useinbandfec=1;stereo=1;sprop-stereo=1",
		})
		if err == nil {
			_, err = room.LocalParticipant.PublishTrack(track, &lksdk.TrackPublicationOptions{
				Name:       "zynko",
				Source:     livekit.TrackSource_MICROPHONE,
				DisableDTX: true,
				Stereo:     true,
			})
		}
		if err != nil {
			room.Disconnect()
			return fmt.Errorf("voz: não publicou o áudio: %w", err)
		}
	}

	c.mu.Lock()
	if c.status == StatusClosed {
		c.mu.Unlock()
		room.Disconnect()
		return errors.New("voz: conexão fechada")
	}
	c.room, c.track = room, track
	startPacer := c.enc == nil && join.CanSpeak
	c.mu.Unlock()
	if startPacer {
		enc, err := startEncoder(c.ffmpeg(), c.opts.Bitrate, c.writePacket)
		if err != nil {
			return err
		}
		c.mu.Lock()
		c.enc = enc
		c.mu.Unlock()
	}
	select {
	case <-c.pacerOn:
	default:
		close(c.pacerOn)
		go c.pace()
	}
	c.announce()
	c.mu.Lock()
	c.status = StatusConnected
	c.mu.Unlock()
	c.hmu.Lock()
	hs := append([]func(){}, c.onReady...)
	c.hmu.Unlock()
	for _, h := range hs {
		h()
	}
	return nil
}

func (c *Connection) ffmpeg() string { return c.Player.ffmpeg }

// writePacket sends one Opus packet to the room (dropped while moving between servers).
func (c *Connection) writePacket(pkt []byte) {
	c.mu.Lock()
	track := c.track
	c.mu.Unlock()
	// The encoder gives one packet per frame, in order: this packet's loudness is the
	// oldest one waiting.
	level := uint8(127)
	select {
	case level = <-c.levels:
	default:
	}
	if track != nil {
		// The audio level header (RFC 6464) is what LiveKit's active speakers come from: without
		// it the bot never shows as speaking.
		_ = track.WriteSample(media.Sample{Data: pkt, Duration: 20 * time.Millisecond}, &lksdk.SampleWriteOptions{AudioLevel: &level})
	}
}

var silence = make([]byte, FrameBytes)

// audioLevel is a frame's loudness as RFC 6464 has it: 0 (loudest) to 127 (silence), in
// -dBov.
func audioLevel(frame []byte) uint8 {
	var sum float64
	n := len(frame) / 2
	if n == 0 {
		return 127
	}
	for i := 0; i+1 < len(frame); i += 2 {
		v := float64(int16(binary.LittleEndian.Uint16(frame[i:]))) / 32768
		sum += v * v
	}
	rms := math.Sqrt(sum / float64(n))
	if rms == 0 {
		return 127
	}
	db := -20 * math.Log10(rms)
	return uint8(max(0, min(127, math.Round(db))))
}

// pace hands the encoder one frame every 20 ms, in real time: the player's next frame, or
// silence when nothing is playing (or it is paused).
func (c *Connection) pace() {
	next := time.Now()
	for {
		select {
		case <-c.done:
			return
		default:
		}
		var frame []byte
		select {
		case frame = <-c.frames:
		default:
			frame = silence
		}
		c.mu.Lock()
		enc := c.enc
		c.mu.Unlock()
		if enc != nil {
			if err := enc.write(frame); err == nil {
				select {
				case c.levels <- audioLevel(frame):
				default:
				}
			} else {
				c.emitError(fmt.Errorf("voz: o ffmpeg do codificador parou: %w (%s)", err, enc.stderr.String()))
				c.mu.Lock()
				if c.enc == enc {
					c.enc = nil
				}
				c.mu.Unlock()
			}
		}
		next = next.Add(20 * time.Millisecond)
		d := time.Until(next)
		if d < -200*time.Millisecond {
			// Far behind (the machine stalled): start counting again from now.
			next = time.Now()
		} else if d > 0 {
			time.Sleep(d)
		}
	}
}

// capture is the player's way in: it blocks while the buffer (60 ms) is full, which keeps
// the player in real time.
func (c *Connection) capture(ctx context.Context, frame []byte) error {
	// Moving to another server: wait for the new connection, then go on where it was.
	c.mu.Lock()
	moving := c.moving
	c.mu.Unlock()
	if moving != nil {
		select {
		case <-moving:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	select {
	case c.frames <- frame:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-c.done:
		return errors.New("voz: conexão fechada")
	}
}

func (c *Connection) flush() {
	for {
		select {
		case <-c.frames:
		default:
			return
		}
	}
}

// Leave leaves the room.
func (c *Connection) Leave(ctx context.Context) error {
	c.mu.Lock()
	if c.status == StatusClosed {
		c.mu.Unlock()
		return nil
	}
	c.leaving = true
	c.mu.Unlock()
	c.Player.Stop()
	c.client.SendOp(4, map[string]any{"channel_id": nil, "self_mute": false, "self_deaf": false})
	c.end("saiu")
	return nil
}

// ---------- zynko.VoiceHandler, from the client ----------

// HandleChannelUpdate follows the room to another voice server (someone picked Brasil 1 or
// 2): it reconnects there, and playback goes on.
func (c *Connection) HandleChannelUpdate(ch zynko.Channel) {
	c.mu.Lock()
	if ch.ID != c.channelID || c.status != StatusConnected || ch.VoiceServer == "" || ch.VoiceServer == c.server {
		c.mu.Unlock()
		return
	}
	moving := make(chan struct{})
	c.moving = moving
	c.status = StatusReconnecting
	old := c.room
	c.room, c.track = nil, nil
	c.mu.Unlock()
	go func() {
		defer func() {
			c.mu.Lock()
			c.moving = nil
			c.mu.Unlock()
			close(moving)
		}()
		c.recv.closeAll()
		if old != nil {
			old.Disconnect()
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := c.connect(ctx); err != nil {
			c.end("não conseguiu entrar no servidor novo")
			c.emitError(err)
		}
	}()
}

// HandleOwnVoiceState ends the connection when the server says the bot is elsewhere, or
// nowhere.
func (c *Connection) HandleOwnVoiceState(v zynko.VoiceState) {
	c.mu.Lock()
	if v.ChannelID == c.channelID {
		c.confirmed = true
	}
	skip := c.status == StatusClosed || c.leaving || !c.confirmed || v.ChannelID == c.channelID
	c.mu.Unlock()
	if skip {
		return
	}
	if v.ChannelID != "" {
		go c.end("entrou em outra sala")
	} else {
		go c.end("removido da sala")
	}
}

// HandleResync says again where the bot is, after the gateway reconnects.
func (c *Connection) HandleResync() {
	if s := c.Status(); s == StatusConnected || s == StatusReconnecting {
		c.announce()
	}
}

func (c *Connection) announce() {
	c.mu.Lock()
	mute := !c.canSpeak
	c.mu.Unlock()
	c.client.SendOp(4, map[string]any{"channel_id": c.channelID, "self_mute": mute, "self_deaf": c.opts.Deaf})
}

func (c *Connection) end(reason string) {
	c.mu.Lock()
	if c.status == StatusClosed {
		c.mu.Unlock()
		return
	}
	c.status = StatusClosed
	c.reason = reason
	room, enc := c.room, c.enc
	c.room, c.track, c.enc = nil, nil, nil
	close(c.done)
	c.mu.Unlock()
	c.Player.Stop()
	c.recv.closeAll()
	if room != nil {
		room.Disconnect()
	}
	if enc != nil {
		enc.close()
	}
	c.client.SetVoice(c, nil)
	c.Player.close()
	c.hmu.Lock()
	hs := append([]func(string){}, c.onDisconnect...)
	c.hmu.Unlock()
	for _, h := range hs {
		h(reason)
	}
}
