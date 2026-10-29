package testclient

import (
	"context"
	"errors"
	"net"
	"sync"
	"time"

	"github.com/mhk-os/server/internal/proto"
	"github.com/mhk-os/server/internal/wire"
)

// Notify sends a notify (kind 3); the server never answers it.
func (c *Client) Notify(typ proto.Type, m proto.Message) error {
	if isClosed(c.done) {
		return ErrClosed
	}
	return c.WriteRaw(wire.AppendFrame(nil, uint8(proto.KindNotify), uint16(typ), 0, proto.Encode(m)))
}

// WaitPushMsg waits for the next push of type typ and decodes it into m.
func (c *Client) WaitPushMsg(ctx context.Context, typ proto.Type, m proto.Message) error {
	p, err := c.WaitPush(ctx, typ)
	if err != nil {
		return err
	}
	return m.Decode(wire.NewReader(p.Body))
}

// UDPBot is a racer's realtime side: it pings 4×/s (with the RTT echo), sends its kart state at 30 Hz once
// started, and keeps the newest World from the server.
type UDPBot struct {
	conn  *net.UDPConn
	token uint64
	tx    *proto.UDPMac
	rx    *proto.UDPMac

	mu       sync.Mutex
	seq      uint32
	state    proto.KartState
	reports  []proto.ProjectileReport
	lastPong time.Time // serverTime of the newest Pong
	pongAt   time.Time // when it arrived
	world    *proto.UDPWorldPayload
	worlds   int
	noEcho   bool // never echo Pongs: the server cannot measure the RTT, the racer is ineligible

	done chan struct{}
	wg   sync.WaitGroup
}

// DialUDP opens the realtime socket of a race seat (RaceSetup.udpToken/udpKey) to the server at addr.
func DialUDP(addr string, token uint64, key []byte) (*UDPBot, error) {
	ua, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return nil, err
	}
	conn, err := net.DialUDP("udp", nil, ua)
	if err != nil {
		return nil, err
	}
	b := &UDPBot{conn: conn, token: token, tx: proto.NewUDPMac(key), rx: proto.NewUDPMac(key),
		done: make(chan struct{})}
	b.wg.Go(b.readLoop)
	return b, nil
}

// Close stops the bot.
func (b *UDPBot) Close() {
	select {
	case <-b.done:
	default:
		close(b.done)
	}
	b.conn.Close()
	b.wg.Wait()
}

// SetState sets the kart state sent from now on.
func (b *UDPBot) SetState(st proto.KartState) {
	b.mu.Lock()
	b.state = st
	b.mu.Unlock()
}

// Report queues projectile reports for the next State datagram.
func (b *UDPBot) Report(r ...proto.ProjectileReport) {
	b.mu.Lock()
	b.reports = append(b.reports, r...)
	b.mu.Unlock()
}

// NoEcho stops echoing Pongs in Pings: the server never measures the bot's RTT, as with an old or broken client.
func (b *UDPBot) NoEcho() {
	b.mu.Lock()
	b.noEcho = true
	b.mu.Unlock()
}

// World returns the newest World and how many arrived so far.
func (b *UDPBot) World() (*proto.UDPWorldPayload, int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.world, b.worlds
}

// Run pings 4×/s and, when states is true, sends the kart state at 30 Hz until ctx ends or Close.
func (b *UDPBot) Run(ctx context.Context, states bool) {
	b.wg.Go(func() {
		ping := time.NewTicker(250 * time.Millisecond)
		state := time.NewTicker(time.Second / 30)
		defer ping.Stop()
		defer state.Stop()
		b.Ping()
		for {
			select {
			case <-ctx.Done():
				return
			case <-b.done:
				return
			case <-ping.C:
				b.Ping()
			case <-state.C:
				if states {
					b.SendState()
				}
			}
		}
	})
}

func (b *UDPBot) send(typ uint8, m proto.Message) error {
	b.mu.Lock()
	b.seq++
	buf := proto.AppendUDPHeader(nil, proto.UDPHeader{Type: typ, Token: b.token, Seq: b.seq})
	w := wire.NewWriter(64)
	m.Encode(w)
	buf = b.tx.Seal(append(buf, w.Bytes()...))
	b.mu.Unlock()
	_, err := b.conn.Write(buf)
	return err
}

// Ping sends one Ping echoing the newest Pong.
func (b *UDPBot) Ping() error {
	b.mu.Lock()
	p := &proto.UDPPingPayload{ClientTime: time.Now()}
	if !b.lastPong.IsZero() && !b.noEcho {
		p.EchoServerTime = b.lastPong
		p.EchoHoldMs = uint16(min(time.Since(b.pongAt).Milliseconds(), 65535))
	}
	b.mu.Unlock()
	return b.send(proto.UDPPing, p)
}

// SendState sends the current kart state with the queued projectile reports.
func (b *UDPBot) SendState() error {
	b.mu.Lock()
	p := &proto.UDPStatePayload{State: b.state, Reports: b.reports}
	b.reports = nil
	b.mu.Unlock()
	return b.send(proto.UDPState, p)
}

// SendAiProgress sends one AiProgress datagram (as the AI host) about the given AI karts.
func (b *UDPBot) SendAiProgress(entries ...proto.AiProgressEntry) error {
	return b.send(proto.UDPAiProgress, &proto.UDPAiProgressPayload{Entries: entries})
}

func (b *UDPBot) readLoop() {
	buf := make([]byte, 2048)
	for {
		n, err := b.conn.Read(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			select {
			case <-b.done:
				return
			default:
				continue
			}
		}
		h, payload, ok := proto.ParseUDP(buf[:n])
		if !ok || h.Token != b.token || !b.rx.Verify(buf[:n]) {
			continue
		}
		switch h.Type {
		case proto.UDPPong:
			var p proto.UDPPongPayload
			if p.Decode(wire.NewReader(payload)) == nil {
				b.mu.Lock()
				b.lastPong, b.pongAt = p.ServerTime, time.Now()
				b.mu.Unlock()
			}
		case proto.UDPWorld:
			var w proto.UDPWorldPayload
			if w.Decode(wire.NewReader(payload)) == nil {
				b.mu.Lock()
				b.world = &w
				b.worlds++
				b.mu.Unlock()
			}
		}
	}
}
