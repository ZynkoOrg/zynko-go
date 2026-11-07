package voice

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"regexp"
	"sync"
)

// Audio goes out as 48 kHz stereo 16-bit little-endian, in 20 ms frames.
const (
	SampleRate   = 48_000
	Channels     = 2
	FrameSamples = 960 // 20 ms per channel
	FrameBytes   = FrameSamples * Channels * 2
)

// Input is something to play: URL (a link or a path, opened by ffmpeg), Stream (encoded
// audio, decoded by ffmpeg), PCM (raw 48 kHz stereo s16le) or Tone.
type Input interface {
	// open returns the input as raw PCM, and the processes behind it.
	open(ctx context.Context, p *Player) (io.Reader, []*exec.Cmd, error)
}

type urlInput string

// URL is a link (http/https) or a file path, opened by ffmpeg: mp3, ogg, wav, flac, radio
// streams... With the YTDLP option, links go through yt-dlp first.
func URL(linkOrPath string) Input { return urlInput(linkOrPath) }

type streamInput struct{ r io.Reader }

// Stream is encoded audio (a download, a pipe), decoded by ffmpeg.
func Stream(r io.Reader) Input { return streamInput{r} }

type pcmInput struct{ r io.Reader }

// PCM is raw audio already at 48 kHz, stereo, 16-bit little-endian. It skips ffmpeg.
func PCM(r io.Reader) Input { return pcmInput{r} }

// Tone is a sine wave, for testing: hz for seconds, at volume 0.2.
func Tone(hz, seconds float64) Input {
	return pcmInput{&toneReader{hz: hz, total: int(math.Round(seconds * SampleRate))}}
}

type toneReader struct {
	hz    float64
	total int
	at    int
}

func (t *toneReader) Read(b []byte) (int, error) {
	n := 0
	for ; n+4 <= len(b) && t.at < t.total; t.at++ {
		v := int16(math.Round(math.Sin(2*math.Pi*t.hz*float64(t.at)/SampleRate) * 0.2 * 32767))
		binary.LittleEndian.PutUint16(b[n:], uint16(v))
		binary.LittleEndian.PutUint16(b[n+2:], uint16(v))
		n += 4
	}
	if n == 0 {
		return 0, io.EOF
	}
	return n, nil
}

var httpRe = regexp.MustCompile(`(?i)^https?://`)

func (u urlInput) open(ctx context.Context, p *Player) (io.Reader, []*exec.Cmd, error) {
	s := string(u)
	if httpRe.MatchString(s) && p.ytdlp != "" {
		y := exec.CommandContext(ctx, p.ytdlp, "-f", "bestaudio/best", "--no-playlist", "--quiet", "-o", "-", s)
		y.Stderr = os.Stderr
		out, err := y.StdoutPipe()
		if err != nil {
			return nil, nil, err
		}
		if err := y.Start(); err != nil {
			return nil, nil, fmt.Errorf("yt-dlp: %w", err)
		}
		r, cmds, err := p.decode(ctx, []string{"-i", "pipe:0"}, out)
		return r, append([]*exec.Cmd{y}, cmds...), err
	}
	var args []string
	if httpRe.MatchString(s) {
		args = append(args, "-reconnect", "1", "-reconnect_streamed", "1", "-reconnect_delay_max", "5")
	}
	return p.decode(ctx, append(args, "-i", s), nil)
}

func (s streamInput) open(ctx context.Context, p *Player) (io.Reader, []*exec.Cmd, error) {
	return p.decode(ctx, []string{"-i", "pipe:0"}, s.r)
}

func (s pcmInput) open(context.Context, *Player) (io.Reader, []*exec.Cmd, error) {
	return s.r, nil, nil
}

// decode runs ffmpeg to turn the input into PCM.
func (p *Player) decode(ctx context.Context, in []string, stdin io.Reader) (io.Reader, []*exec.Cmd, error) {
	args := []string{"-hide_banner"}
	if stdin == nil {
		args = append(args, "-nostdin")
	}
	args = append(args, in...)
	args = append(args, "-f", "s16le", "-ar", "48000", "-ac", "2", "-loglevel", "error", "pipe:1")
	f := exec.CommandContext(ctx, p.ffmpeg, args...)
	f.Stdin = stdin
	f.Stderr = os.Stderr
	out, err := f.StdoutPipe()
	if err != nil {
		return nil, nil, err
	}
	if err := f.Start(); err != nil {
		return nil, nil, fmt.Errorf("ffmpeg: %w", err)
	}
	return out, []*exec.Cmd{f}, nil
}

// Track is one thing in the player.
type Track struct {
	Input Input
	// Whatever you want back in events: a title, who asked for it...
	Meta any
}

// sink is where the player writes frames: the voice connection.
type sink interface {
	// capture blocks until the frame fits (that is how playback keeps real time).
	capture(ctx context.Context, frame []byte) error
	// flush drops what is buffered, so a skip or pause is heard at once.
	flush()
}

// Player plays audio into a voice connection: a queue, pause, skip and volume. Its methods
// are safe for concurrent use. Event handlers run one at a time, in order, on the player's
// own goroutine.
type Player struct {
	sink   sink
	ffmpeg string
	ytdlp  string

	mu      sync.Mutex
	queue   []*Track
	current *Track
	paused  bool
	volume  float64
	running bool
	abort   context.CancelFunc
	resume  chan struct{} // closed when paused ends

	hmu      sync.Mutex
	onStart  []func(*Track)
	onFinish []func(*Track)
	onError  []func(error, *Track)
	onIdle   []func()
	events   chan func()
	closed   bool
}

func newPlayer(s sink, ffmpeg, ytdlp string) *Player {
	if ffmpeg == "" {
		ffmpeg = os.Getenv("FFMPEG_PATH")
	}
	if ffmpeg == "" {
		ffmpeg = "ffmpeg"
	}
	p := &Player{sink: s, ffmpeg: ffmpeg, ytdlp: ytdlp, volume: 1, events: make(chan func(), 64)}
	go func() {
		for f := range p.events {
			func() {
				defer func() { _ = recover() }()
				f()
			}()
		}
	}()
	return p
}

// OnStart runs fn when a track starts.
func (p *Player) OnStart(fn func(t *Track)) {
	p.hmu.Lock()
	p.onStart = append(p.onStart, fn)
	p.hmu.Unlock()
}

// OnFinish runs fn when a track ends (played to the end, or skipped).
func (p *Player) OnFinish(fn func(t *Track)) {
	p.hmu.Lock()
	p.onFinish = append(p.onFinish, fn)
	p.hmu.Unlock()
}

// OnError runs fn when a track could not be played.
func (p *Player) OnError(fn func(err error, t *Track)) {
	p.hmu.Lock()
	p.onError = append(p.onError, fn)
	p.hmu.Unlock()
}

// OnIdle runs fn when nothing is left in the queue.
func (p *Player) OnIdle(fn func()) { p.hmu.Lock(); p.onIdle = append(p.onIdle, fn); p.hmu.Unlock() }

// close ends the event goroutine, once the connection is gone.
func (p *Player) close() {
	p.hmu.Lock()
	defer p.hmu.Unlock()
	if !p.closed {
		p.closed = true
		close(p.events)
	}
}

func (p *Player) fire(f func()) {
	defer func() { _ = recover() }() // events closed
	p.events <- f
}

// Volume is 0 (silent) to 2 (twice as loud); 1 is as it came.
func (p *Player) Volume() float64 { p.mu.Lock(); defer p.mu.Unlock(); return p.volume }

// SetVolume sets the volume, clamped to 0..2.
func (p *Player) SetVolume(v float64) {
	p.mu.Lock()
	p.volume = max(0, min(2, v))
	p.mu.Unlock()
}

// Paused reports whether playback is paused.
func (p *Player) Paused() bool { p.mu.Lock(); defer p.mu.Unlock(); return p.paused }

// Current is what is playing now; nil when nothing is.
func (p *Player) Current() *Track { p.mu.Lock(); defer p.mu.Unlock(); return p.current }

// Queue is what plays after the current one, in order (a copy).
func (p *Player) Queue() []*Track {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]*Track(nil), p.queue...)
}

// Play plays now, in place of what was playing (the queue stays).
func (p *Player) Play(in Input, meta any) *Track {
	t := &Track{Input: in, Meta: meta}
	p.mu.Lock()
	p.queue = append([]*Track{t}, p.queue...)
	running := p.running
	p.mu.Unlock()
	if running {
		p.Skip()
	} else {
		p.start()
	}
	return t
}

// Enqueue adds to the end of the queue; starts if nothing is playing.
func (p *Player) Enqueue(in Input, meta any) *Track {
	t := &Track{Input: in, Meta: meta}
	p.mu.Lock()
	p.queue = append(p.queue, t)
	p.mu.Unlock()
	p.start()
	return t
}

// Skip goes to the next in the queue.
func (p *Player) Skip() {
	p.mu.Lock()
	if p.abort != nil {
		p.abort()
	}
	p.mu.Unlock()
	p.sink.flush()
	p.Resume()
}

// Stop stops and empties the queue.
func (p *Player) Stop() {
	p.mu.Lock()
	p.queue = nil
	p.mu.Unlock()
	p.Skip()
}

// Pause pauses; the connection sends silence meanwhile.
func (p *Player) Pause() {
	p.mu.Lock()
	if p.paused || p.current == nil {
		p.mu.Unlock()
		return
	}
	p.paused = true
	p.resume = make(chan struct{})
	p.mu.Unlock()
	p.sink.flush()
}

// Resume continues after Pause.
func (p *Player) Resume() {
	p.mu.Lock()
	if p.paused {
		p.paused = false
		close(p.resume)
	}
	p.mu.Unlock()
}

func (p *Player) start() {
	p.mu.Lock()
	if p.running {
		p.mu.Unlock()
		return
	}
	p.running = true
	p.mu.Unlock()
	go p.loop()
}

func (p *Player) loop() {
	for {
		p.mu.Lock()
		if len(p.queue) == 0 {
			p.current = nil
			p.running = false
			if p.paused {
				p.paused = false
				close(p.resume)
			}
			p.mu.Unlock()
			p.emitIdle()
			return
		}
		t := p.queue[0]
		p.queue = p.queue[1:]
		p.current = t
		ctx, cancel := context.WithCancel(context.Background())
		p.abort = cancel
		p.mu.Unlock()

		p.emitStart(t)
		err := p.playOne(ctx, t)
		cancel()
		p.mu.Lock()
		p.abort = nil
		p.mu.Unlock()
		if err != nil {
			p.emitError(err, t)
		} else {
			p.emitFinish(t)
		}
	}
}

func (p *Player) playOne(ctx context.Context, t *Track) error {
	pcm, cmds, err := t.Input.open(ctx, p)
	if err != nil {
		return err
	}
	defer func() {
		for _, c := range cmds {
			if c.Process != nil {
				_ = c.Process.Kill()
			}
			_ = c.Wait()
		}
	}()
	frame := make([]byte, FrameBytes)
	for {
		n, rerr := io.ReadFull(pcm, frame)
		if n > 0 {
			if n < FrameBytes {
				// The last partial frame, padded with silence.
				clear(frame[n-n%2:])
			}
			if err := p.waitPaused(ctx); err != nil {
				return nil
			}
			out := make([]byte, FrameBytes)
			copy(out, frame)
			if v := p.Volume(); v != 1 {
				scale(out, v)
			}
			if err := p.sink.capture(ctx, out); err != nil {
				return nil // skipped or stopped
			}
		}
		if rerr != nil {
			if ctx.Err() != nil {
				return nil
			}
			if errors.Is(rerr, io.EOF) || errors.Is(rerr, io.ErrUnexpectedEOF) {
				break
			}
			return rerr
		}
	}
	// Played to the end: did the decoder fail?
	for _, c := range cmds {
		if err := c.Wait(); err != nil && ctx.Err() == nil {
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				return fmt.Errorf("%s terminou com código %d", c.Path, ee.ExitCode())
			}
			return err
		}
	}
	return nil
}

func (p *Player) waitPaused(ctx context.Context) error {
	for {
		p.mu.Lock()
		paused, ch := p.paused, p.resume
		p.mu.Unlock()
		if !paused {
			return ctx.Err()
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (p *Player) emitStart(t *Track) {
	p.hmu.Lock()
	hs := append([]func(*Track){}, p.onStart...)
	p.hmu.Unlock()
	p.fire(func() {
		for _, h := range hs {
			h(t)
		}
	})
}

func (p *Player) emitFinish(t *Track) {
	p.hmu.Lock()
	hs := append([]func(*Track){}, p.onFinish...)
	p.hmu.Unlock()
	p.fire(func() {
		for _, h := range hs {
			h(t)
		}
	})
}

func (p *Player) emitError(err error, t *Track) {
	p.hmu.Lock()
	hs := append([]func(error, *Track){}, p.onError...)
	p.hmu.Unlock()
	p.fire(func() {
		for _, h := range hs {
			h(err, t)
		}
	})
}

func (p *Player) emitIdle() {
	p.hmu.Lock()
	hs := append([]func(){}, p.onIdle...)
	p.hmu.Unlock()
	p.fire(func() {
		for _, h := range hs {
			h()
		}
	})
}

// scale changes a frame's loudness in place, clipping at the 16-bit limits.
func scale(frame []byte, v float64) {
	for i := 0; i+1 < len(frame); i += 2 {
		s := float64(int16(binary.LittleEndian.Uint16(frame[i:])))
		s = math.Round(s * v)
		s = max(-32768, min(32767, s))
		binary.LittleEndian.PutUint16(frame[i:], uint16(int16(s)))
	}
}
