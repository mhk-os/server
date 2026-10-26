// Package server runs the TCP (optionally TLS) control endpoint: connection lifecycle, request dispatch,
// the hub of live connections and the periodic session re-validation.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mhk-os/server/internal/auth"
	"github.com/mhk-os/server/internal/geo"
	"github.com/mhk-os/server/internal/proto"
	"github.com/mhk-os/server/internal/store"
	"github.com/mhk-os/server/internal/wire"
)

// Config tunes the server; zero fields take the defaults from DefaultConfig.
type Config struct {
	Version            string        // build label announced in HelloOk
	UDPPort            uint16        // realtime port announced in HelloOk, 0 = none
	IdleTimeout        time.Duration // close connections silent for this long
	WriteTimeout       time.Duration // per frame write deadline
	RequestTimeout     time.Duration // per request handler deadline
	RevalidateInterval time.Duration // how often live sessions are checked against the database
	ShutdownGrace      time.Duration // how long shutdown waits for connections to flush
	ConnLimit          auth.Rate     // new connections per IP
	MaxMalformed       int           // malformed requests before a connection is dropped
}

// DefaultConfig holds the values from docs/protocol.md.
var DefaultConfig = Config{
	Version:            "dev",
	IdleTimeout:        45 * time.Second,
	WriteTimeout:       10 * time.Second,
	RequestTimeout:     15 * time.Second,
	RevalidateInterval: 30 * time.Second,
	ShutdownGrace:      5 * time.Second,
	ConnLimit:          auth.Rate{N: 30, Per: time.Minute},
	MaxMalformed:       10,
}

func (c *Config) applyDefaults() {
	d := DefaultConfig
	if c.Version == "" {
		c.Version = d.Version
	}
	if c.IdleTimeout <= 0 {
		c.IdleTimeout = d.IdleTimeout
	}
	if c.WriteTimeout <= 0 {
		c.WriteTimeout = d.WriteTimeout
	}
	if c.RequestTimeout <= 0 {
		c.RequestTimeout = d.RequestTimeout
	}
	if c.RevalidateInterval <= 0 {
		c.RevalidateInterval = d.RevalidateInterval
	}
	if c.ShutdownGrace <= 0 {
		c.ShutdownGrace = d.ShutdownGrace
	}
	if c.ConnLimit == (auth.Rate{}) {
		c.ConnLimit = d.ConnLimit
	}
	if c.MaxMalformed <= 0 {
		c.MaxMalformed = d.MaxMalformed
	}
}

// Deps are the services handlers use.
type Deps struct {
	Log      *slog.Logger
	Auth     *auth.Service
	Accounts store.Accounts
	Sessions store.Sessions
	Geo      *geo.DB // nil = no GeoIP database
}

// HandlerFunc handles one request. It decodes the body from r and returns the response payload (nil for an
// empty one) or an error: *StatusError for an expected failure, a wire error for a malformed body (BadRequest),
// anything else becomes ServerError and is logged.
type HandlerFunc func(ctx context.Context, c *Conn, r *wire.Reader) (proto.Message, error)

// NotifyFunc handles one notify; notifies are never answered.
type NotifyFunc func(ctx context.Context, c *Conn, r *wire.Reader) error

type handler struct {
	fn        HandlerFunc
	needLogin bool
}

type notifyHandler struct {
	fn        NotifyFunc
	needLogin bool
}

// Server accepts connections and dispatches their requests.
type Server struct {
	cfg  Config
	deps Deps
	log  *slog.Logger

	handlers map[proto.Type]handler
	notifies map[proto.Type]notifyHandler

	hub       *Hub
	connLimit *auth.Limiter
	nextID    atomic.Uint64
	wg        sync.WaitGroup
}

// New returns a server with the phase 1 connection and account handlers registered.
func New(cfg Config, deps Deps) *Server {
	cfg.applyDefaults()
	if deps.Log == nil {
		deps.Log = slog.Default()
	}
	s := &Server{
		cfg:       cfg,
		deps:      deps,
		log:       deps.Log,
		handlers:  map[proto.Type]handler{},
		notifies:  map[proto.Type]notifyHandler{},
		hub:       newHub(),
		connLimit: auth.NewLimiter(cfg.ConnLimit),
	}
	s.registerConnection()
	s.registerAccounts()
	return s
}

// Handle registers a request handler; needLogin answers NotLoggedIn for logged-out connections.
func (s *Server) Handle(typ proto.Type, needLogin bool, fn HandlerFunc) {
	if _, dup := s.handlers[typ]; dup {
		panic(fmt.Sprintf("server: duplicate handler for type %d", typ))
	}
	s.handlers[typ] = handler{fn: fn, needLogin: needLogin}
}

// HandleNotify registers a notify handler.
func (s *Server) HandleNotify(typ proto.Type, needLogin bool, fn NotifyFunc) {
	if _, dup := s.notifies[typ]; dup {
		panic(fmt.Sprintf("server: duplicate notify handler for type %d", typ))
	}
	s.notifies[typ] = notifyHandler{fn: fn, needLogin: needLogin}
}

// Hub returns the registry of live connections.
func (s *Server) Hub() *Hub { return s.hub }

// Serve accepts connections from ln until ctx is cancelled, then tells every client the server is restarting,
// waits up to ShutdownGrace for them to flush and returns.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	// Requests in flight should finish even while shutting down; only accepting stops on cancel.
	connCtx := context.WithoutCancel(ctx)
	stop := context.AfterFunc(ctx, func() { ln.Close() })
	defer stop()

	bg, bgCancel := context.WithCancel(ctx)
	defer bgCancel()
	go s.revalidateLoop(bg)
	go s.sweepLoop(bg)

	var acceptErr error
	backoff := 5 * time.Millisecond
	for {
		nc, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				break
			}
			// Typically EMFILE: back off instead of spinning.
			s.log.Warn("accept failed", "err", err)
			time.Sleep(backoff)
			backoff = min(backoff*2, time.Second)
			continue
		}
		backoff = 5 * time.Millisecond
		s.accept(connCtx, nc)
	}
	if ctx.Err() == nil {
		acceptErr = errors.New("server: listener closed")
	}
	s.shutdown()
	return acceptErr
}

func (s *Server) accept(ctx context.Context, nc net.Conn) {
	ip := remoteIP(nc)
	if !s.connLimit.Allow(ip.String()) {
		s.log.Info("connection rate limited", "ip", ip)
		nc.Close()
		return
	}
	id := s.nextID.Add(1)
	c := &Conn{
		s:          s,
		nc:         nc,
		ip:         ip,
		log:        s.log.With("conn", id, "ip", ip.String()),
		out:        make(chan []byte, outQueue),
		kill:       make(chan struct{}),
		writerDone: make(chan struct{}),
	}
	s.hub.add(c)
	s.wg.Go(func() { c.serve(ctx) })
}

func (s *Server) shutdown() {
	conns := s.hub.Conns()
	s.log.Info("shutting down", "connections", len(conns))
	for _, c := range conns {
		c.Disconnect(proto.StatusServerError, "Server is restarting")
	}
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(s.cfg.ShutdownGrace):
		for _, c := range s.hub.Conns() {
			c.abort()
		}
		<-done
	}
}

func remoteIP(nc net.Conn) netip.Addr {
	if ap, err := netip.ParseAddrPort(nc.RemoteAddr().String()); err == nil {
		return ap.Addr().Unmap()
	}
	return netip.Addr{}
}

// revalidateLoop drops connections whose session was revoked or whose account was disabled by another process
// (the sysadmin CLI) or another connection, checking all of them in one query per interval.
func (s *Server) revalidateLoop(ctx context.Context) {
	t := time.NewTicker(s.cfg.RevalidateInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.revalidate(ctx)
		}
	}
}

func (s *Server) revalidate(ctx context.Context) {
	type entry struct {
		c    *Conn
		hash store.TokenHash
	}
	var live []entry
	var hashes []store.TokenHash
	for _, c := range s.hub.Conns() {
		if h, ok := c.sessionHash(); ok {
			live = append(live, entry{c, h})
			hashes = append(hashes, h)
		}
	}
	if len(hashes) == 0 {
		return
	}
	status, err := s.deps.Sessions.CheckSessions(ctx, hashes, store.Now())
	if err != nil {
		s.log.Error("session revalidation failed", "err", err)
		return
	}
	for _, e := range live {
		// The connection may have logged out or switched sessions while the query ran.
		if h, ok := e.c.sessionHash(); !ok || h != e.hash {
			continue
		}
		st, found := status[e.hash]
		switch {
		case found && st.Disabled:
			e.c.log.Info("account disabled, disconnecting")
			e.c.Disconnect(proto.StatusAccountDisabled, disabledMessage(st.DisabledReason))
		case !found || !st.Valid:
			e.c.log.Info("session no longer valid, disconnecting")
			e.c.Disconnect(proto.StatusSessionInvalid, msgSessionInvalid)
		default:
			e.c.mu.Lock()
			if e.c.account != nil && e.c.session == e.hash && e.c.account.Role != st.Role {
				a := *e.c.account
				a.Role = st.Role
				e.c.account = &a
			}
			e.c.mu.Unlock()
		}
	}
}

func (s *Server) sweepLoop(ctx context.Context) {
	t := time.NewTicker(5 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.connLimit.Sweep()
			s.deps.Auth.Sweep()
		}
	}
}
