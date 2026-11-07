package voice

import (
	"encoding/binary"
	"io"
	"os/exec"
	"sync"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media/oggwriter"
)

// Receiving: each person's track is read as RTP; its payloads are Opus packets, handed to
// OnOpus as they come. For OnAudio, the packets are wrapped in Ogg and decoded by one ffmpeg
// per person (no cgo), to 48 kHz mono PCM.

type receivers struct {
	conn *Connection
	mu   sync.Mutex
	all  map[*webrtc.TrackRemote]*receiver
}

type receiver struct {
	stop chan struct{}
	dec  *decoder
}

func (rs *receivers) hear(track *webrtc.TrackRemote, userID string) {
	c := rs.conn
	r := &receiver{stop: make(chan struct{})}
	rs.mu.Lock()
	if rs.all == nil {
		rs.all = map[*webrtc.TrackRemote]*receiver{}
	}
	rs.all[track] = r
	rs.mu.Unlock()
	defer func() {
		rs.mu.Lock()
		delete(rs.all, track)
		rs.mu.Unlock()
		if r.dec != nil {
			r.dec.close()
		}
	}()

	c.hmu.Lock()
	wantPCM := len(c.onAudio) > 0
	c.hmu.Unlock()
	if wantPCM {
		dec, err := startDecoder(c.ffmpeg(), func(samples []int16) {
			c.hmu.Lock()
			hs := append([]func(string, []int16){}, c.onAudio...)
			c.hmu.Unlock()
			for _, h := range hs {
				h(userID, samples)
			}
		})
		if err != nil {
			c.emitError(err)
		} else {
			r.dec = dec
		}
	}

	for {
		select {
		case <-r.stop:
			return
		case <-c.done:
			return
		default:
		}
		pkt, _, err := track.ReadRTP()
		if err != nil {
			return
		}
		if len(pkt.Payload) == 0 {
			continue
		}
		c.hmu.Lock()
		hs := append([]func(string, []byte){}, c.onOpus...)
		c.hmu.Unlock()
		for _, h := range hs {
			h(userID, pkt.Payload)
		}
		if r.dec != nil {
			r.dec.write(pkt)
		}
	}
}

// closeAll stops every receiver (leaving, or moving to another server).
func (rs *receivers) closeAll() {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	for t, r := range rs.all {
		select {
		case <-r.stop:
		default:
			close(r.stop)
		}
		delete(rs.all, t)
	}
}

// decoder is an ffmpeg that turns one person's Opus (in Ogg) into PCM.
type decoder struct {
	cmd  *exec.Cmd
	in   io.WriteCloser
	ogg  *oggwriter.OggWriter
	mu   sync.Mutex
	dead bool
}

func startDecoder(ffmpeg string, onPCM func([]int16)) (*decoder, error) {
	cmd := exec.Command(ffmpeg,
		"-hide_banner", "-loglevel", "error", "-nostdin",
		"-fflags", "nobuffer", "-probesize", "4096", "-analyzeduration", "0",
		"-f", "ogg", "-i", "pipe:0",
		"-f", "s16le", "-ar", "48000", "-ac", "1", "-flush_packets", "1", "pipe:1")
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	ogg, err := oggwriter.NewWith(in, SampleRate, Channels)
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, err
	}
	d := &decoder{cmd: cmd, in: in, ogg: ogg}
	go func() {
		buf := make([]byte, FrameSamples*2) // 20 ms of mono
		for {
			if _, err := io.ReadFull(out, buf); err != nil {
				_ = cmd.Wait()
				return
			}
			samples := make([]int16, FrameSamples)
			for i := range samples {
				samples[i] = int16(binary.LittleEndian.Uint16(buf[i*2:]))
			}
			onPCM(samples)
		}
	}()
	return d, nil
}

func (d *decoder) write(pkt *rtp.Packet) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.dead {
		return
	}
	if err := d.ogg.WriteRTP(pkt); err != nil {
		d.dead = true
	}
}

func (d *decoder) close() {
	d.mu.Lock()
	d.dead = true
	d.mu.Unlock()
	_ = d.in.Close()
	if d.cmd.Process != nil {
		_ = d.cmd.Process.Kill()
	}
}
