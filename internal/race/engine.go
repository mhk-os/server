package race

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mhk-os/server/internal/proto"
	"github.com/mhk-os/server/internal/server"
	"github.com/mhk-os/server/internal/store"
	"github.com/mhk-os/server/internal/wire"
)

const (
	worldRate     = 30 // Hz, also the timer resolution of every race rule
	callQueue     = 256
	stateQueue    = 512
	maxEchoAge    = 5 * time.Second
	storeTimeout  = 10 * time.Second
	lookupTimeout = 5 * time.Second
)

// Pusher sends pushes to every connection of an account (server.Hub).
type Pusher interface {
	Push(accountID uint64, typ proto.Type, m proto.Message) int
}

// Kicks is the cheat-kick storage (store.Store).
type Kicks interface {
	CheatKicked(ctx context.Context, accountIDs []uint64) (map[uint64]bool, error)
	AddCheatKick(ctx context.Context, k *store.CheatKick) error
}

// Config wires an Engine.
type Config struct {
	Log     *slog.Logger
	Hub     Pusher
	Kicks   Kicks
	Catalog Catalog      // nil: no content strikes, UnknownContent never counts as fabricated
	UDP     *net.UDPConn // realtime socket; the Engine owns it from now on. nil = no UDP (UDPPort 0)
}

// Engine runs every race of the server: one goroutine per race plus one UDP reader.
type Engine struct {
	cfg     Config
	log     *slog.Logger
	udpPort uint16

	mu       sync.RWMutex
	races    map[uint64]*runner
	tokens   map[uint64]seatRef
	accounts map[uint64]*seat // seat of each account in a live race, for RTT

	nextID    atomic.Uint64
	stop      chan struct{}
	closeOnce sync.Once
	wg        sync.WaitGroup
}

type seatRef struct {
	run  *runner
	seat *seat
}

var _ Races = (*Engine)(nil)

// NewEngine starts serving cfg.UDP.
func NewEngine(cfg Config) *Engine {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	e := &Engine{
		cfg:      cfg,
		log:      cfg.Log.With("component", "race"),
		races:    map[uint64]*runner{},
		tokens:   map[uint64]seatRef{},
		accounts: map[uint64]*seat{},
		stop:     make(chan struct{}),
	}
	// Race ids only need to be unique; starting at the clock keeps them unique across restarts for cheat_kicks.
	e.nextID.Store(uint64(time.Now().UnixMilli()) * 1000)
	if cfg.UDP != nil {
		if ap, err := netip.ParseAddrPort(cfg.UDP.LocalAddr().String()); err == nil {
			e.udpPort = ap.Port()
		}
		e.wg.Go(e.readUDP)
	}
	return e
}

// UDPPort is the realtime port to announce in HelloOk.
func (e *Engine) UDPPort() uint16 { return e.udpPort }

// RTT returns the server-measured UDP round trip of the account's seat in a live race.
func (e *Engine) RTT(accountID uint64) (uint16, bool) {
	e.mu.RLock()
	s := e.accounts[accountID]
	e.mu.RUnlock()
	if s == nil {
		return 0, false
	}
	ms := s.rttMs()
	return ms, ms > 0
}

// Close stops every race without callbacks, closes the UDP socket and waits for pending cheat-kick writes.
func (e *Engine) Close() {
	e.closeOnce.Do(func() {
		close(e.stop)
		if e.cfg.UDP != nil {
			e.cfg.UDP.Close()
		}
	})
	e.wg.Wait()
}

// ErrInvalidRace is returned by Start for racer lists the protocol cannot express.
var ErrInvalidRace = errors.New("race: invalid racers")

// Start creates a race, pushes RaceSetup to every racer and runs it.
func (e *Engine) Start(spec Spec, racers []Racer, cb Callbacks) (uint64, error) {
	if len(racers) == 0 || len(racers) > proto.MaxRacers || (spec.Solo && len(racers) != 1) {
		return 0, fmt.Errorf("%w: %d racers (solo %v)", ErrInvalidRace, len(racers), spec.Solo)
	}
	ids := make([]uint64, len(racers))
	for i, r := range racers {
		for _, id := range ids[:i] {
			if id == r.AccountID {
				return 0, fmt.Errorf("%w: account %d twice", ErrInvalidRace, id)
			}
		}
		ids[i] = r.AccountID
	}
	kicked := map[uint64]bool{}
	if e.cfg.Kicks != nil && spec.Anticheat && !spec.Solo {
		ctx, cancel := context.WithTimeout(context.Background(), lookupTimeout)
		k, err := e.cfg.Kicks.CheatKicked(ctx, ids)
		cancel()
		if err != nil {
			return 0, fmt.Errorf("race: cheat kick lookup: %w", err)
		}
		kicked = k
	}

	var seed [32]byte
	rand.Read(seed[:])
	run := &runner{e: e, cb: cb, calls: make(chan func(time.Time), callQueue),
		states: make(chan stateIn, stateQueue), done: make(chan struct{})}
	seats := make([]*seat, len(racers))
	for i, r := range racers {
		key := make([]byte, proto.UDPKeySize)
		rand.Read(key)
		seats[i] = &seat{slot: uint8(i), racer: r, key: key, everKicked: kicked[r.AccountID],
			rxMac: proto.NewUDPMac(key), pongMac: proto.NewUDPMac(key), worldMac: proto.NewUDPMac(key)}
	}
	id := e.nextID.Add(1)
	run.rs = newRaceState(id, spec, seats, run, e.cfg.Catalog, seed, time.Now())

	e.mu.Lock()
	for _, s := range seats {
		for s.token == 0 || e.tokens[s.token].seat != nil {
			var b [8]byte
			rand.Read(b[:])
			s.token = binary.LittleEndian.Uint64(b[:])
		}
		e.tokens[s.token] = seatRef{run, s}
		e.accounts[s.racer.AccountID] = s
	}
	e.races[id] = run
	e.mu.Unlock()

	grid := make([]proto.RaceSlot, len(racers))
	for i, r := range racers {
		grid[i] = proto.RaceSlot{Slot: uint8(i), AccountID: r.AccountID, Username: r.Username, Country: r.Country,
			CharacterID: r.CharacterID, KartID: r.KartID}
	}
	raceSeed := binary.LittleEndian.Uint64(seed[:8]) ^ binary.LittleEndian.Uint64(seed[8:16])
	for _, s := range seats {
		e.cfg.Hub.Push(s.racer.AccountID, proto.PushRaceSetup, &proto.RaceSetup{
			RaceID: id, LobbyID: spec.LobbyID, TrackID: spec.TrackID, Mode: spec.Mode, EngineClass: spec.EngineClass,
			Laps: spec.Laps, Items: spec.Items, ItemOdds: spec.ItemOdds, Anticheat: spec.Anticheat, Solo: spec.Solo,
			AICount: spec.AICount, Seed: raceSeed, Grid: grid, YourSlot: s.slot, UDPPort: e.udpPort,
			UDPToken: s.token, UDPKey: s.key, SetRace: spec.SetRace, SetSize: spec.SetSize,
		})
	}
	e.wg.Go(run.loop)
	e.log.Info("race started", "race", id, "lobby", spec.LobbyID, "track", spec.TrackID, "racers", len(racers),
		"solo", spec.Solo)
	return id, nil
}

// Remove takes a racer out of a running race; unknown races and racers already out are ignored.
func (e *Engine) Remove(raceID, accountID uint64, outcome Outcome, message string) {
	if message == "" {
		message = defaultLeaveMessage(outcome)
	}
	e.call(raceID, func(rs *raceState, now time.Time) {
		if s := rs.seatOf(accountID); s != nil {
			rs.leave(s, outcome, message, now)
		}
	})
}

func defaultLeaveMessage(o Outcome) string {
	switch o {
	case OutcomeModKick:
		return "Removed from the race by a moderator."
	case OutcomeCheatKick:
		return "Removed from the race: cheating detected."
	case OutcomeDesync:
		return "Your game content differs from the other players'."
	}
	return "Left the race."
}

// EndEarly ends a race now; unfinished racers are DNF.
func (e *Engine) EndEarly(raceID uint64) {
	e.call(raceID, func(rs *raceState, now time.Time) { rs.end(now) })
}

func (e *Engine) call(raceID uint64, f func(rs *raceState, now time.Time)) {
	e.mu.RLock()
	run := e.races[raceID]
	e.mu.RUnlock()
	if run != nil {
		run.call(func(now time.Time) { f(run.rs, now) })
	}
}

// Register installs the race notify handlers (528–538) on srv.
func (e *Engine) Register(srv *server.Server) {
	on(e, srv, proto.NotifyRaceLoaded, func(m *proto.RaceLoaded) uint64 { return m.RaceID }, (*raceState).onLoaded)
	on(e, srv, proto.NotifyItemPickup, func(m *proto.ItemPickup) uint64 { return m.RaceID }, (*raceState).onPickup)
	on(e, srv, proto.NotifyItemUse, func(m *proto.ItemUse) uint64 { return m.RaceID }, (*raceState).onUse)
	on(e, srv, proto.NotifyHitTaken, func(m *proto.HitTaken) uint64 { return m.RaceID }, (*raceState).onHitTaken)
	on(e, srv, proto.NotifyHitSeen, func(m *proto.HitSeen) uint64 { return m.RaceID }, (*raceState).onHitSeen)
	on(e, srv, proto.NotifyFinished, func(m *proto.Finished) uint64 { return m.RaceID }, (*raceState).onFinished)
	on(e, srv, proto.NotifyCheatReport, func(m *proto.CheatReport) uint64 { return m.RaceID },
		(*raceState).onCheatReport)
	on(e, srv, proto.NotifyUnknownContent, func(m *proto.UnknownContent) uint64 { return m.RaceID },
		(*raceState).onUnknownContent)
	on(e, srv, proto.NotifyEliminatedSelf, func(m *proto.EliminatedSelf) uint64 { return m.RaceID },
		(*raceState).onEliminatedSelf)
	on(e, srv, proto.NotifyBridgeTrigger, func(m *proto.BridgeTrigger) uint64 { return m.RaceID },
		(*raceState).onBridgeTrigger)
	on(e, srv, proto.NotifyProjectileGone, func(m *proto.ProjectileGone) uint64 { return m.RaceID },
		(*raceState).onProjectileGone)
}

// on registers one race notify: decode, find the race, and run fn on the race goroutine for the sender's seat.
// Notifies for races the sender is not (or no longer) in are ignored.
func on[T any, P interface {
	*T
	proto.Message
}](e *Engine, srv *server.Server, typ proto.Type, raceID func(P) uint64, fn func(*raceState, *seat, P, time.Time)) {
	srv.HandleNotify(typ, true, func(_ context.Context, c *server.Conn, r *wire.Reader) error {
		acc, ok := c.AccountID()
		if !ok {
			return nil
		}
		m := P(new(T))
		if err := m.Decode(r); err != nil {
			return err
		}
		e.call(raceID(m), func(rs *raceState, now time.Time) {
			if s := rs.seatOf(acc); s != nil && !s.out {
				fn(rs, s, m, now)
			}
		})
		return nil
	})
}

// readUDP validates datagrams (size, token, MAC, replay window), learns addresses, answers pings and hands kart
// states to their race.
func (e *Engine) readUDP() {
	buf := make([]byte, proto.UDPMaxDatagram+1)
	pong := make([]byte, 0, proto.UDPHeaderSize+16+proto.UDPMacSize)
	for {
		n, addr, err := e.cfg.UDP.ReadFromUDPAddrPort(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		now := time.Now()
		h, payload, ok := proto.ParseUDP(buf[:n])
		if !ok {
			continue
		}
		e.mu.RLock()
		ref := e.tokens[h.Token]
		e.mu.RUnlock()
		s := ref.seat
		if s == nil || !s.rxMac.Verify(buf[:n]) {
			continue
		}
		switch h.Type {
		case proto.UDPPing:
			var p proto.UDPPingPayload
			if !s.replay.accept(h.Seq, false) || p.Decode(wire.NewReader(payload)) != nil {
				continue
			}
			learnAddr(s, addr)
			if echo := p.EchoServerTime; !echo.IsZero() && !echo.After(now) && now.Sub(echo) <= maxEchoAge {
				if d := now.Sub(echo) - time.Duration(p.EchoHoldMs)*time.Millisecond; d >= 0 {
					s.addRTTSample(d)
				}
			}
			pong = proto.AppendUDPHeader(pong[:0], proto.UDPHeader{Type: proto.UDPPong, Token: s.token,
				Seq: s.txSeq.Add(1)})
			pong = binary.LittleEndian.AppendUint64(pong, uint64(p.ClientTime.UnixMilli()))
			pong = binary.LittleEndian.AppendUint64(pong, uint64(now.UnixMilli()))
			pong = s.pongMac.Seal(pong)
			e.cfg.UDP.WriteToUDPAddrPort(pong, addr)
		case proto.UDPState:
			if !s.replay.accept(h.Seq, true) {
				continue
			}
			learnAddr(s, addr)
			select {
			case ref.run.states <- stateIn{seat: s, payload: bytes.Clone(payload), at: now}:
			default: // the race is behind; a newer state follows within 33 ms
			}
		}
	}
}

func learnAddr(s *seat, addr netip.AddrPort) {
	if old := s.addr.Load(); old == nil || *old != addr {
		s.addr.Store(&addr)
	}
}

// runner owns one race's goroutine and implements env for it.
type runner struct {
	e      *Engine
	rs     *raceState
	cb     Callbacks
	calls  chan func(time.Time)
	states chan stateIn
	done   chan struct{}
}

type stateIn struct {
	seat    *seat
	payload []byte
	at      time.Time
}

func (r *runner) call(f func(time.Time)) {
	select {
	case r.calls <- f:
	case <-r.done:
	}
}

func (r *runner) loop() {
	t := time.NewTicker(time.Second / worldRate)
	defer func() {
		t.Stop()
		close(r.done)
		r.e.mu.Lock()
		delete(r.e.races, r.rs.id)
		for _, s := range r.rs.seats {
			r.e.dropSeatLocked(s)
		}
		r.e.mu.Unlock()
	}()
	for r.rs.phase != phaseEnded {
		select {
		case f := <-r.calls:
			f(time.Now())
		case in := <-r.states:
			r.rs.onState(in.seat, in.payload, in.at)
		case <-t.C:
			r.rs.tick(time.Now())
		case <-r.e.stop:
			return
		}
	}
}

func (r *runner) push(accountID uint64, typ proto.Type, m proto.Message) {
	r.e.cfg.Hub.Push(accountID, typ, m)
}

func (r *runner) sendUDP(addr netip.AddrPort, b []byte) {
	if r.e.cfg.UDP != nil {
		r.e.cfg.UDP.WriteToUDPAddrPort(b, addr)
	}
}

func (r *runner) dropSeat(s *seat) {
	r.e.mu.Lock()
	r.e.dropSeatLocked(s)
	r.e.mu.Unlock()
}

func (e *Engine) dropSeatLocked(s *seat) {
	if e.tokens[s.token].seat == s {
		delete(e.tokens, s.token)
	}
	if e.accounts[s.racer.AccountID] == s {
		delete(e.accounts, s.racer.AccountID)
	}
}

func (r *runner) going(raceID uint64) {
	if r.cb.Going != nil {
		r.cb.Going(raceID)
	}
}

func (r *runner) finished(raceID uint64, placements []Placement) {
	r.e.log.Info("race ended", "race", raceID, "racers", len(placements))
	if r.cb.Finished != nil {
		r.cb.Finished(raceID, placements)
	}
}

func (r *runner) cheatKick(k store.CheatKick) {
	r.e.log.Info("cheat kick", "race", k.RaceRef, "account", k.AccountID, "reason", k.Reason, "reporters", k.Reporters)
	if r.e.cfg.Kicks == nil {
		return
	}
	r.e.wg.Go(func() {
		ctx, cancel := context.WithTimeout(context.Background(), storeTimeout)
		defer cancel()
		if err := r.e.cfg.Kicks.AddCheatKick(ctx, &k); err != nil {
			r.e.log.Error("storing cheat kick failed", "account", k.AccountID, "err", err)
		}
	})
}
