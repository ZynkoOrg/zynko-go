package voice

import (
	"io"
	"os/exec"
	"sync"
	"testing"
	"time"
)

// TestEncoderLatency feeds the ffmpeg encoder real-time frames and checks that each one comes
// back as one packet, quickly. Skipped without ffmpeg.
func TestEncoderLatency(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("sem ffmpeg")
	}
	var mu sync.Mutex
	var got []time.Time
	var sizes []int
	enc, err := startEncoder("ffmpeg", 128_000, func(p []byte) {
		mu.Lock()
		got = append(got, time.Now())
		sizes = append(sizes, len(p))
		mu.Unlock()
	})
	if err != nil {
		t.Fatal(err)
	}
	tone := Tone(440, 2).(pcmInput).r
	var sent []time.Time
	frame := make([]byte, FrameBytes)
	next := time.Now()
	for {
		if _, err := io.ReadFull(tone, frame); err != nil {
			break
		}
		sent = append(sent, time.Now())
		if err := enc.write(frame); err != nil {
			t.Fatal(err)
		}
		next = next.Add(20 * time.Millisecond)
		time.Sleep(time.Until(next))
	}
	time.Sleep(300 * time.Millisecond)
	mu.Lock()
	n := len(got)
	var worst, total time.Duration
	for i := 0; i < n && i < len(sent); i++ {
		d := got[i].Sub(sent[i])
		total += d
		worst = max(worst, d)
	}
	mu.Unlock()
	enc.close()
	t.Logf("frames sent %d, packets %d, latency avg %v worst %v, first sizes %v", len(sent), n, total/time.Duration(max(1, min(n, len(sent)))), worst, sizes[:min(5, len(sizes))])
	if n < len(sent)-5 {
		t.Fatalf("poucos pacotes: %d de %d", n, len(sent))
	}
	if total/time.Duration(n) > 150*time.Millisecond {
		t.Fatalf("latência alta: %v", total/time.Duration(n))
	}
}
