package voice

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"sync"
	"testing"
	"time"
)

// page builds one Ogg page holding the given lacing values and body.
func page(lacing []byte, body []byte) []byte {
	h := make([]byte, 27)
	copy(h, "OggS")
	h[26] = byte(len(lacing))
	return append(append(h, lacing...), body...)
}

func TestOggPackets(t *testing.T) {
	big := bytes.Repeat([]byte{7}, 300) // 255 + 45: two segments
	var stream []byte
	// Page 1: a 3-byte packet, a 2-byte packet, then the start of a 300-byte one.
	stream = append(stream, page([]byte{3, 2, 255}, append(append([]byte{1, 1, 1}, 2, 2), big[:255]...))...)
	// Page 2: the rest of it.
	stream = append(stream, page([]byte{45}, big[255:])...)
	o := newOggPackets(bytes.NewReader(stream))
	want := [][]byte{{1, 1, 1}, {2, 2}, big}
	for i, w := range want {
		got, err := o.next()
		if err != nil || !bytes.Equal(got, w) {
			t.Fatalf("packet %d: %v %v", i, len(got), err)
		}
	}
	if _, err := o.next(); err != io.EOF {
		t.Fatal("want EOF, got", err)
	}
	if _, err := newOggPackets(bytes.NewReader([]byte("nope, not ogg at all......"))).next(); err == nil {
		t.Fatal("junk must fail")
	}
}

func TestToneAndLevel(t *testing.T) {
	data, _ := io.ReadAll(Tone(440, 1).(pcmInput).r)
	if len(data) != SampleRate*Channels*2 {
		t.Fatalf("tone length %d", len(data))
	}
	if l := audioLevel(make([]byte, FrameBytes)); l != 127 {
		t.Error("silence level", l)
	}
	// Volume 0.2 sine: RMS 0.2/sqrt(2) = -17 dBov.
	if l := audioLevel(data[:FrameBytes]); l < 16 || l > 18 {
		t.Error("tone level", l)
	}
	full := make([]byte, 4)
	binary.LittleEndian.PutUint16(full, uint16(32767))
	binary.LittleEndian.PutUint16(full[2:], uint16(0x8000))
	if l := audioLevel(full); l != 0 {
		t.Error("full scale", l)
	}
}

func TestScale(t *testing.T) {
	f := make([]byte, 6)
	binary.LittleEndian.PutUint16(f, uint16(1000))
	binary.LittleEndian.PutUint16(f[2:], uint16(30000))
	v := int16(-30000)
	binary.LittleEndian.PutUint16(f[4:], uint16(v))
	scale(f, 2)
	got := []int16{int16(binary.LittleEndian.Uint16(f)), int16(binary.LittleEndian.Uint16(f[2:])), int16(binary.LittleEndian.Uint16(f[4:]))}
	if got[0] != 2000 || got[1] != 32767 || got[2] != -32768 {
		t.Error(got)
	}
}

// fakeSink takes frames at a fixed pace, like the connection.
type fakeSink struct {
	mu     sync.Mutex
	frames int
	peak   int16
}

func (s *fakeSink) capture(ctx context.Context, frame []byte) error {
	select {
	case <-time.After(time.Millisecond):
	case <-ctx.Done():
		return ctx.Err()
	}
	s.mu.Lock()
	s.frames++
	for i := 0; i+1 < len(frame); i += 2 {
		s.peak = max(s.peak, int16(binary.LittleEndian.Uint16(frame[i:])))
	}
	s.mu.Unlock()
	return nil
}

func (s *fakeSink) flush() {}

func TestPlayerQueue(t *testing.T) {
	sink := &fakeSink{}
	p := newPlayer(sink, "ffmpeg", "")
	var mu sync.Mutex
	var log []string
	idle := make(chan struct{}, 4)
	p.OnStart(func(t *Track) { mu.Lock(); log = append(log, "start "+t.Meta.(string)); mu.Unlock() })
	p.OnFinish(func(t *Track) { mu.Lock(); log = append(log, "finish "+t.Meta.(string)); mu.Unlock() })
	p.OnIdle(func() { idle <- struct{}{} })

	// 0.1 s of tone = 5 frames; a partial frame at the end is padded.
	p.Enqueue(PCM(bytes.NewReader(make([]byte, FrameBytes*2+100))), "a")
	p.Enqueue(Tone(440, 0.1), "b")
	if len(p.Queue()) > 2 {
		t.Fatal("queue")
	}
	<-idle
	mu.Lock()
	if got := len(log); got != 4 || log[0] != "start a" || log[3] != "finish b" {
		t.Fatal(log)
	}
	mu.Unlock()
	if sink.frames != 3+5 {
		t.Fatal("frames", sink.frames)
	}

	// Volume 2 doubles; pause holds; skip moves on; stop empties.
	p.SetVolume(9)
	if p.Volume() != 2 {
		t.Fatal("volume clamp")
	}
	sink.peak = 0
	p.Enqueue(Tone(440, 30), "long")
	p.Enqueue(Tone(440, 30), "next")
	time.Sleep(50 * time.Millisecond)
	p.Pause()
	if !p.Paused() {
		t.Fatal("pause")
	}
	sink.mu.Lock()
	n := sink.frames
	sink.mu.Unlock()
	time.Sleep(50 * time.Millisecond)
	sink.mu.Lock()
	if sink.frames > n+1 {
		t.Fatal("frames went on while paused")
	}
	if sink.peak < 13000 {
		t.Fatal("volume 2 not applied, peak", sink.peak)
	}
	sink.mu.Unlock()
	p.Resume()
	p.Skip()
	time.Sleep(30 * time.Millisecond)
	if c := p.Current(); c == nil || c.Meta != "next" {
		t.Fatal("skip")
	}
	p.Play(Tone(220, 30), "now")
	time.Sleep(30 * time.Millisecond)
	if c := p.Current(); c == nil || c.Meta != "now" || len(p.Queue()) != 0 {
		t.Fatal("play replaces the current one and keeps the queue")
	}
	p.Stop()
	select {
	case <-idle:
	case <-time.After(2 * time.Second):
		t.Fatal("no idle after stop")
	}
	if p.Current() != nil {
		t.Fatal("current after stop")
	}
}
