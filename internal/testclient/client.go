// Package testclient is a minimal protocol client for integration tests, bots and load tests.
package testclient

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mhk-os/server/internal/proto"
	"github.com/mhk-os/server/internal/wire"
)

// ErrClosed is returned for requests on a closed connection.
var ErrClosed = errors.New("testclient: connection closed")

// StatusError is a non-Ok response.
type StatusError struct {
	Status  proto.Status
	Message string
}

func (e *StatusError) Error() string { return fmt.Sprintf("%s: %s", e.Status, e.Message) }

// StatusOf returns the status of a request error (Ok for nil, ServerError for transport failures).
func StatusOf(err error) proto.Status {
	if err == nil {
		return proto.StatusOk
	}
	var se *StatusError
	if errors.As(err, &se) {
		return se.Status
	}
	return proto.StatusServerError
}

// Push is a server push.
type Push struct {
	Type proto.Type
	Body []byte
}

type response struct {
	status proto.Status
	body   *wire.Reader // positioned after the status
	msg    string
}

// Client is one control connection. Requests may be issued concurrently.
type Client struct {
	nc     net.Conn
	nextID atomic.Uint32

	wmu sync.Mutex

	mu      sync.Mutex
	pending map[uint32]chan response
	err     error

	// Pushes receives every push; it is closed when the connection ends. Unread pushes beyond its buffer are dropped.
	Pushes chan Push
	done   chan struct{}
}

// Dial connects to addr; tlsConfig nil means plaintext.
func Dial(ctx context.Context, addr string, tlsConfig *tls.Config) (*Client, error) {
	var d net.Dialer
	nc, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	if tlsConfig != nil {
		tc := tls.Client(nc, tlsConfig)
		if err := tc.HandshakeContext(ctx); err != nil {
			nc.Close()
			return nil, err
		}
		nc = tc
	}
	c := &Client{
		nc:      nc,
		pending: map[uint32]chan response{},
		Pushes:  make(chan Push, 64),
		done:    make(chan struct{}),
	}
	go c.readLoop()
	return c, nil
}

// Close closes the connection.
func (c *Client) Close() error { return c.nc.Close() }

// Done is closed when the connection has ended.
func (c *Client) Done() <-chan struct{} { return c.done }

// Err returns why the connection ended (after Done).
func (c *Client) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

func (c *Client) readLoop() {
	r := bufio.NewReader(c.nc)
	var err error
	for {
		var f wire.Frame
		f, err = wire.ReadFrame(r, wire.MaxFrame)
		if err != nil {
			break
		}
		switch proto.Kind(f.Kind) {
		case proto.KindResponse:
			c.mu.Lock()
			ch := c.pending[f.ID]
			delete(c.pending, f.ID)
			c.mu.Unlock()
			if ch == nil {
				continue
			}
			br := wire.NewReader(f.Body)
			resp := response{status: proto.Status(br.U16()), body: br}
			if resp.status != proto.StatusOk {
				resp.msg = br.String(proto.MaxMessage)
			}
			ch <- resp
		case proto.KindPush:
			select {
			case c.Pushes <- Push{Type: proto.Type(f.Type), Body: f.Body}:
			default:
			}
		}
	}
	c.mu.Lock()
	c.err = err
	for id, ch := range c.pending {
		close(ch)
		delete(c.pending, id)
	}
	c.mu.Unlock()
	close(c.Pushes)
	close(c.done)
}

// WriteRaw writes bytes as they are, for tests of malformed input.
func (c *Client) WriteRaw(b []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_, err := c.nc.Write(b)
	return err
}

// Request sends a request and decodes the Ok payload into resp (nil to ignore it).
func (c *Client) Request(ctx context.Context, typ proto.Type, req proto.Message, resp proto.Message) error {
	var body []byte
	if req != nil {
		body = proto.Encode(req)
	}
	r, err := c.RequestRaw(ctx, typ, body)
	if err != nil {
		return err
	}
	if resp != nil {
		return resp.Decode(r)
	}
	return nil
}

// RequestRaw sends a request with a raw body and returns a reader over the Ok payload.
func (c *Client) RequestRaw(ctx context.Context, typ proto.Type, body []byte) (*wire.Reader, error) {
	id := c.nextID.Add(1)
	ch := make(chan response, 1)
	c.mu.Lock()
	if c.err != nil || isClosed(c.done) {
		c.mu.Unlock()
		return nil, ErrClosed
	}
	c.pending[id] = ch
	c.mu.Unlock()

	frame := wire.AppendFrame(nil, uint8(proto.KindRequest), uint16(typ), id, body)
	if err := c.WriteRaw(frame); err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, err
	}
	select {
	case resp, ok := <-ch:
		if !ok {
			return nil, ErrClosed
		}
		if resp.status != proto.StatusOk {
			return nil, &StatusError{Status: resp.status, Message: resp.msg}
		}
		return resp.body, nil
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, ctx.Err()
	}
}

func isClosed(ch chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// DefaultHello is a valid Hello for the current protocol.
func DefaultHello() *proto.Hello {
	return &proto.Hello{
		Magic:      proto.Magic,
		Protocol:   proto.Version,
		GameCommit: "testclient",
		Platform:   "Go",
	}
}

// Hello performs the handshake.
func (c *Client) Hello(ctx context.Context, h *proto.Hello) (*proto.HelloOk, error) {
	var ok proto.HelloOk
	if err := c.Request(ctx, proto.TypeHello, h, &ok); err != nil {
		return nil, err
	}
	return &ok, nil
}

// Connect dials addr and performs the default Hello.
func Connect(ctx context.Context, addr string, tlsConfig *tls.Config) (*Client, error) {
	c, err := Dial(ctx, addr, tlsConfig)
	if err != nil {
		return nil, err
	}
	if _, err := c.Hello(ctx, DefaultHello()); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

// Ping measures the round trip.
func (c *Client) Ping(ctx context.Context) (*proto.Pong, error) {
	var pong proto.Pong
	err := c.Request(ctx, proto.TypePing, &proto.Ping{ClientTime: time.Now()}, &pong)
	return &pong, err
}

func (c *Client) session(ctx context.Context, typ proto.Type, req proto.Message) (*proto.Session, error) {
	var s proto.Session
	if err := c.Request(ctx, typ, req, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

func (c *Client) Register(ctx context.Context, username, password string) (*proto.Session, error) {
	return c.session(ctx, proto.TypeRegister, &proto.Credentials{Username: username, Password: password})
}

func (c *Client) Login(ctx context.Context, username, password string) (*proto.Session, error) {
	return c.session(ctx, proto.TypeLogin, &proto.Credentials{Username: username, Password: password})
}

func (c *Client) Resume(ctx context.Context, token string) (*proto.Session, error) {
	return c.session(ctx, proto.TypeResume, &proto.Resume{Token: token})
}

func (c *Client) Logout(ctx context.Context) error {
	return c.Request(ctx, proto.TypeLogout, proto.Empty{}, nil)
}

func (c *Client) SetCountry(ctx context.Context, country string) (*proto.Account, error) {
	var a proto.Account
	if err := c.Request(ctx, proto.TypeSetCountry, &proto.SetCountry{Country: country}, &a); err != nil {
		return nil, err
	}
	return &a, nil
}

// WaitPush waits for the next push of type typ, skipping others.
func (c *Client) WaitPush(ctx context.Context, typ proto.Type) (*Push, error) {
	for {
		select {
		case p, ok := <-c.Pushes:
			if !ok {
				return nil, ErrClosed
			}
			if p.Type == typ {
				return &p, nil
			}
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// WaitDisconnect waits for the Disconnect push.
func (c *Client) WaitDisconnect(ctx context.Context) (*proto.Disconnect, error) {
	p, err := c.WaitPush(ctx, proto.PushDisconnect)
	if err != nil {
		return nil, err
	}
	var d proto.Disconnect
	if err := d.Decode(wire.NewReader(p.Body)); err != nil {
		return nil, err
	}
	return &d, nil
}
