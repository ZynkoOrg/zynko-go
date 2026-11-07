package zynko

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// The realtime link (docs.zynko.org/api/gateway): HELLO, IDENTIFY with the token and the
// intents, heartbeats, and a fresh IDENTIFY after any drop (there is no RESUME: READY always
// brings the whole state again).

type gatewayFrame struct {
	Op int             `json:"op"`
	T  string          `json:"t,omitempty"`
	D  json.RawMessage `json:"d,omitempty"`
}

type gateway struct {
	url     string
	token   string
	intents Intents

	onDispatch func(t string, d json.RawMessage)
	onReady    func()
	// onReconnect is told about each drop that will be retried.
	onReconnect func(code int, reason string, wait time.Duration)
	// onFatal: the gateway gave up (token refused).
	onFatal func(err error)

	mu   sync.Mutex
	conn *websocket.Conn
}

// errHeartbeat closes a link whose heartbeats stopped being answered.
var errHeartbeat = errors.New("heartbeat timeout")

// run connects and reconnects until ctx ends or the token is refused.
func (g *gateway) run(ctx context.Context) {
	retry := 0
	for ctx.Err() == nil {
		readied, code, reason := g.session(ctx, &retry)
		if ctx.Err() != nil {
			return
		}
		if code == 4004 {
			g.onFatal(fmt.Errorf("token recusado pelo gateway (%d %s)", code, reason))
			return
		}
		if readied {
			retry = 0
		}
		// 1s, 2s, 4s... up to 30s, with some jitter so a server restart is not hit all at once.
		wait := min(30*time.Second, time.Second<<min(retry, 5))
		wait = time.Duration(float64(wait) * (0.75 + rand.Float64()/2))
		retry++
		g.onReconnect(code, reason, wait)
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// session is one connection, from dial to close. It reports whether READY came, and the
// close code and reason.
func (g *gateway) session(ctx context.Context, retry *int) (readied bool, code int, reason string) {
	dialCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	conn, _, err := websocket.Dial(dialCtx, g.url, nil)
	cancel()
	if err != nil {
		return false, 0, err.Error()
	}
	// READY brings every server with its members: far over the default 32 KB.
	conn.SetReadLimit(256 << 20)
	g.mu.Lock()
	g.conn = conn
	g.mu.Unlock()
	defer func() {
		g.mu.Lock()
		if g.conn == conn {
			g.conn = nil
		}
		g.mu.Unlock()
		conn.CloseNow()
	}()

	sctx, stop := context.WithCancelCause(ctx)
	defer stop(nil)
	var lastAck sync.Mutex
	acked := time.Now()
	setAck := func() { lastAck.Lock(); acked = time.Now(); lastAck.Unlock() }
	sinceAck := func() time.Duration { lastAck.Lock(); defer lastAck.Unlock(); return time.Since(acked) }

	for {
		_, data, err := conn.Read(sctx)
		if err != nil {
			if cause := context.Cause(sctx); errors.Is(cause, errHeartbeat) {
				return readied, 4900, "heartbeat timeout"
			}
			var ce websocket.CloseError
			if errors.As(err, &ce) {
				return readied, int(ce.Code), ce.Reason
			}
			return readied, 1006, err.Error()
		}
		var f gatewayFrame
		if json.Unmarshal(data, &f) != nil {
			continue
		}
		switch f.Op {
		case 10:
			var hello struct {
				HeartbeatInterval int `json:"heartbeat_interval"`
			}
			_ = json.Unmarshal(f.D, &hello)
			every := time.Duration(hello.HeartbeatInterval) * time.Millisecond
			if every <= 0 {
				every = 30 * time.Second
			}
			if err := g.write(sctx, conn, 2, map[string]any{"token": g.token, "intents": g.intents}); err != nil {
				return readied, 1006, err.Error()
			}
			setAck()
			go func() {
				t := time.NewTicker(every)
				defer t.Stop()
				for {
					select {
					case <-sctx.Done():
						return
					case <-t.C:
						// No answer to two beats in a row: the link is dead even if the socket says otherwise.
						if sinceAck() > every*2+5*time.Second {
							stop(errHeartbeat)
							return
						}
						_ = g.write(sctx, conn, 1, nil)
					}
				}
			}()
		case 11:
			setAck()
		case 0:
			setAck()
			if f.T == "READY" {
				readied = true
				*retry = 0
				g.onReady()
			}
			if f.T != "" {
				g.onDispatch(f.T, f.D)
			}
		}
	}
}

func (g *gateway) write(ctx context.Context, conn *websocket.Conn, op int, d any) error {
	raw, err := json.Marshal(map[string]any{"op": op, "d": d})
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return conn.Write(wctx, websocket.MessageText, raw)
}

// send sends an op on the current link. It reports whether the link was open.
func (g *gateway) send(op int, d any) bool {
	g.mu.Lock()
	conn := g.conn
	g.mu.Unlock()
	if conn == nil {
		return false
	}
	return g.write(context.Background(), conn, op, d) == nil
}

// close closes the current link politely.
func (g *gateway) close() {
	g.mu.Lock()
	conn := g.conn
	g.conn = nil
	g.mu.Unlock()
	if conn != nil {
		_ = conn.Close(websocket.StatusNormalClosure, "")
	}
}
