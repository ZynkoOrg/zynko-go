package voice

import (
	"bytes"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"sync"
)

// encoder is one long-lived ffmpeg that turns 20 ms PCM frames into 20 ms Opus packets.
// Pion has no Opus encoder and the library avoids cgo, so ffmpeg (libopus) does it: PCM in
// on stdin, Ogg out on stdout, one packet per page and flushed at once, so a frame written
// comes out as a packet about as fast as ffmpeg can encode it.
type encoder struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	done  chan struct{}

	mu     sync.Mutex
	closed bool
	stderr bytes.Buffer
}

func encoderArgs(bitrate int) []string {
	return []string{
		"-hide_banner", "-loglevel", "error", "-nostdin",
		"-fflags", "nobuffer", "-probesize", "32", "-analyzeduration", "0",
		"-f", "s16le", "-ar", "48000", "-ac", "2", "-i", "pipe:0",
		"-c:a", "libopus", "-b:a", strconv.Itoa(bitrate), "-vbr", "on",
		"-frame_duration", "20", "-application", "audio",
		"-flush_packets", "1", "-page_duration", "20000",
		"-f", "ogg", "pipe:1",
	}
}

// startEncoder runs ffmpeg; onPacket gets each Opus packet, in order, on one goroutine.
func startEncoder(ffmpeg string, bitrate int, onPacket func([]byte)) (*encoder, error) {
	e := &encoder{done: make(chan struct{})}
	cmd := exec.Command(ffmpeg, encoderArgs(bitrate)...)
	cmd.Stderr = &e.stderr
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("ffmpeg (codificador Opus): %w", err)
	}
	e.cmd, e.stdin = cmd, in
	go func() {
		defer close(e.done)
		ogg := newOggPackets(out)
		for n := 0; ; n++ {
			pkt, err := ogg.next()
			if err != nil {
				_, _ = io.Copy(io.Discard, out)
				_ = cmd.Wait()
				return
			}
			// The first two packets are OpusHead and OpusTags.
			if n < 2 {
				continue
			}
			onPacket(pkt)
		}
	}()
	return e, nil
}

// write sends one PCM frame.
func (e *encoder) write(frame []byte) error {
	_, err := e.stdin.Write(frame)
	return err
}

func (e *encoder) close() {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return
	}
	e.closed = true
	e.mu.Unlock()
	_ = e.stdin.Close()
	if e.cmd.Process != nil {
		_ = e.cmd.Process.Kill()
	}
	<-e.done
}
