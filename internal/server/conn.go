package server

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/mhk-os/server/internal/proto"
	"github.com/mhk-os/server/internal/store"
	"github.com/mhk-os/server/internal/wire"
)

// outQueue bounds frames waiting for the writer; a client that falls this far behind is dropped instead of
// letting it pin server memory.
const outQueue = 256

// Conn is one client control connection. Requests are handled one at a time on the read goroutine, so handlers
// may use the connection state freely; writes go through a single writer goroutine so pushes from other
// goroutines never interleave with responses.
type Conn struct {
	s   *Server
	nc  net.Conn
	ip  netip.Addr
	log *slog.Logger

	out        chan []byte // nil entry = flush, then close
	kill       chan struct{}
	killOnce   sync.Once
	writerDone chan struct{}

	mu      sync.Mutex
	account *store.Account
	session store.TokenHash
	hello   proto.Hello

	malformed int
}

// IP returns the client address.
func (c *Conn) IP() netip.Addr { return c.ip }

// Hello returns what the client announced in its Hello.
func (c *Conn) Hello() proto.Hello {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hello
}

// Account returns a copy of the logged-in account, or nil.
func (c *Conn) Account() *store.Account {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.account == nil {
		return nil
	}
	a := *c.account
	return &a
}

// AccountID returns the logged-in account id; ok is false when logged out.
func (c *Conn) AccountID() (id uint64, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.account == nil {
		return 0, false
	}
	return c.account.ID, true
}

func (c *Conn) loggedIn() bool {
	_, ok := c.AccountID()
	return ok
}

// sessionHash returns the session of the connection; ok is false when logged out.
func (c *Conn) sessionHash() (store.TokenHash, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.session, c.account != nil
}

// login binds the connection to an account session and registers it in the hub.
func (c *Conn) login(a *store.Account, hash store.TokenHash) {
	c.mu.Lock()
	prev := c.account
	c.account, c.session = a, hash
	c.mu.Unlock()
	if prev != nil && prev.ID != a.ID {
		c.s.hub.unbind(c, prev.ID)
	}
	c.s.hub.bind(c, a.ID)
}

// logout returns the connection to the logged-out state.
func (c *Conn) logout() {
	c.mu.Lock()
	prev := c.account
	c.account, c.session = nil, store.TokenHash{}
	c.mu.Unlock()
	if prev != nil {
		c.s.hub.unbind(c, prev.ID)
	}
}

// updateAccount replaces the cached account if the connection is still logged in as the same account.
func (c *Conn) updateAccount(a *store.Account) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.account != nil && c.account.ID == a.ID {
		c.account = a
	}
}

// enqueue hands a frame to the writer. A full queue means the client stopped reading: drop it.
func (c *Conn) enqueue(frame []byte) {
	select {
	case <-c.kill:
	case c.out <- frame:
	default:
		c.log.Warn("send queue full, dropping connection")
		c.abort()
	}
}

func (c *Conn) send(kind proto.Kind, typ proto.Type, id uint32, body []byte) {
	c.enqueue(wire.AppendFrame(make([]byte, 0, 4+wire.HeaderSize+len(body)), uint8(kind), uint16(typ), id, body))
}

// Push sends a push message.
func (c *Conn) Push(typ proto.Type, m proto.Message) { c.pushBody(typ, proto.Encode(m)) }

func (c *Conn) pushBody(typ proto.Type, body []byte) { c.send(proto.KindPush, typ, 0, body) }

// Disconnect pushes Disconnect(status, message) and closes the connection once it is flushed.
func (c *Conn) Disconnect(status proto.Status, message string) {
	c.Push(proto.PushDisconnect, &proto.Disconnect{Status: status, Message: message})
	c.closeAfterFlush()
}

func (c *Conn) closeAfterFlush() { c.enqueue(nil) }

// abort closes the connection immediately, dropping queued frames.
func (c *Conn) abort() {
	c.killOnce.Do(func() {
		close(c.kill)
		c.nc.Close()
	})
}

func (c *Conn) writeLoop() {
	defer close(c.writerDone)
	defer c.abort()
	for {
		select {
		case <-c.kill:
			return
		case frame := <-c.out:
			if frame == nil {
				return
			}
			c.nc.SetWriteDeadline(time.Now().Add(c.s.cfg.WriteTimeout))
			if _, err := c.nc.Write(frame); err != nil {
				return
			}
		}
	}
}

// serve runs the read loop until the connection ends, then flushes and closes it.
func (c *Conn) serve(ctx context.Context) {
	go c.writeLoop()
	defer func() {
		c.logout()
		c.closeAfterFlush()
		<-c.writerDone
		c.s.hub.remove(c)
	}()

	r := bufio.NewReaderSize(c.nc, 4096)
	if !c.handshake(ctx, r) {
		return
	}
	for {
		frame, err := c.readFrame(r)
		if err != nil {
			return
		}
		switch proto.Kind(frame.Kind) {
		case proto.KindRequest:
			if frame.ID == 0 {
				c.log.Debug("request with id 0")
				return
			}
			if !c.handleRequest(ctx, frame) {
				return
			}
		case proto.KindNotify:
			c.handleNotify(ctx, frame)
		default:
			c.log.Debug("client sent a server-only frame kind", "kind", frame.Kind)
			return
		}
	}
}

func (c *Conn) readFrame(r *bufio.Reader) (wire.Frame, error) {
	max := wire.MaxFramePreLogin
	if c.loggedIn() {
		max = wire.MaxFrame
	}
	c.nc.SetReadDeadline(time.Now().Add(c.s.cfg.IdleTimeout))
	frame, err := wire.ReadFrame(r, max)
	switch {
	case errors.Is(err, wire.ErrFrameTooLarge):
		c.log.Info("frame too large", "max", max)
		c.Disconnect(proto.StatusTooLarge, "Message too large.")
	case errors.Is(err, wire.ErrFrameTooShort):
		c.log.Debug("frame shorter than its header")
	case err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed):
		c.log.Debug("read failed", "err", err)
	}
	return frame, err
}

// handshake enforces Hello as the first request. It answers HelloOk or VersionMismatch; anything else first
// closes the connection without a response.
func (c *Conn) handshake(ctx context.Context, r *bufio.Reader) bool {
	frame, err := c.readFrame(r)
	if err != nil {
		return false
	}
	if proto.Kind(frame.Kind) != proto.KindRequest || proto.Type(frame.Type) != proto.TypeHello || frame.ID == 0 {
		c.log.Debug("first frame is not Hello", "kind", frame.Kind, "type", frame.Type)
		return false
	}
	var hello proto.Hello
	if err := hello.Decode(wire.NewReader(frame.Body)); err != nil || hello.Magic != proto.Magic {
		c.log.Debug("malformed Hello", "err", err)
		return false
	}
	if hello.Protocol != proto.Version {
		c.log.Info("protocol mismatch", "client", hello.Protocol, "server", proto.Version)
		c.send(proto.KindResponse, proto.TypeHello, frame.ID, proto.EncodeError(proto.StatusVersionMismatch,
			"This game version cannot talk to the server. Please update the game."))
		return false
	}
	c.mu.Lock()
	c.hello = hello
	c.mu.Unlock()
	c.log.Debug("hello", "build", hello.GameBuild, "commit", hello.GameCommit, "platform", hello.Platform)
	c.send(proto.KindResponse, proto.TypeHello, frame.ID, proto.EncodeOK(&proto.HelloOk{
		Protocol:      proto.Version,
		ServerVersion: c.s.cfg.Version,
		UDPPort:       c.s.cfg.UDPPort,
		ServerTime:    time.Now(),
	}))
	return true
}

// handleRequest dispatches one request and sends its response. It returns false when the connection must close.
func (c *Conn) handleRequest(ctx context.Context, frame wire.Frame) bool {
	typ := proto.Type(frame.Type)
	h, ok := c.s.handlers[typ]
	var (
		payload proto.Message
		err     error
	)
	switch {
	case !ok:
		err = Errorf(proto.StatusUnsupported, "This request is not supported by the server.")
	case h.needLogin && !c.loggedIn():
		err = Errorf(proto.StatusNotLoggedIn, "Please log in first.")
	default:
		rctx, cancel := context.WithTimeout(ctx, c.s.cfg.RequestTimeout)
		payload, err = h.fn(rctx, c, wire.NewReader(frame.Body))
		cancel()
	}
	if err == nil {
		c.send(proto.KindResponse, typ, frame.ID, proto.EncodeOK(payload))
		return true
	}
	status, msg := c.errorStatus(typ, err)
	c.send(proto.KindResponse, typ, frame.ID, proto.EncodeError(status, msg))
	if status == proto.StatusBadRequest {
		c.malformed++
		if c.malformed >= c.s.cfg.MaxMalformed {
			c.log.Info("too many malformed requests")
			c.Disconnect(proto.StatusBadRequest, "Too many invalid requests.")
			return false
		}
	}
	return true
}

func (c *Conn) errorStatus(typ proto.Type, err error) (proto.Status, string) {
	var se *StatusError
	switch {
	case errors.As(err, &se):
		return se.Status, se.Message
	case errors.Is(err, wire.ErrTruncated), errors.Is(err, wire.ErrInvalid), errors.Is(err, wire.ErrTooLong):
		c.log.Debug("malformed request", "type", typ, "err", err)
		return proto.StatusBadRequest, "Malformed request."
	default:
		c.log.Error("request failed", "type", typ, "err", err)
		return proto.StatusServerError, "Something went wrong on the server. Please try again later."
	}
}

func (c *Conn) handleNotify(ctx context.Context, frame wire.Frame) {
	h, ok := c.s.notifies[proto.Type(frame.Type)]
	if !ok || (h.needLogin && !c.loggedIn()) {
		return
	}
	if err := h.fn(ctx, c, wire.NewReader(frame.Body)); err != nil {
		c.log.Debug("notify failed", "type", frame.Type, "err", err)
	}
}
