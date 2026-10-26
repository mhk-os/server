package main

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"time"

	"github.com/mhk-os/server/internal/proto"
	"github.com/mhk-os/server/internal/testclient"
	"github.com/mhk-os/server/internal/wire"
)

type botConfig struct {
	addr      string // host:port of the control connection
	host      string // for the UDP address
	tls       *tls.Config
	pass      string
	join      string // room code, or "" for quick
	quick     uint8
	character string
	kart      string
	trackID   string // the ghost's track: voted for when the lobby offers it
	replay    *replay
	manifests *manifestFile
	keepAlive time.Duration // TCP Ping interval
}

// bot is one simulated player: lobby member and racer replaying the ghost.
type bot struct {
	cfg    *botConfig
	name   string
	log    *slog.Logger
	c      *testclient.Client
	acc    uint64
	offset time.Duration // server clock − local clock, from HelloOk

	votedRound time.Time // PhaseEnds of the Voting phase already voted in
	raceID     uint64
	slot       uint8
	raceGo     chan time.Time
	raceStop   context.CancelFunc
}

func (b *bot) serverNow() time.Time { return time.Now().Add(b.offset) }

func (b *bot) run(ctx context.Context) error {
	if err := b.connect(ctx); err != nil {
		return err
	}
	defer b.c.Close()
	defer b.stopRace()
	st, err := b.joinLobby(ctx)
	if err != nil {
		return err
	}
	b.onLobbyState(ctx, st)
	for {
		select {
		case <-ctx.Done():
			return nil
		case p, ok := <-b.c.Pushes:
			if !ok {
				return fmt.Errorf("connection closed: %v", b.c.Err())
			}
			if err := b.onPush(ctx, p); err != nil {
				return err
			}
		}
	}
}

func (b *bot) connect(ctx context.Context) error {
	c, err := testclient.Dial(ctx, b.cfg.addr, b.cfg.tls)
	if err != nil {
		return err
	}
	b.c = c
	hello := testclient.DefaultHello()
	hello.GameCommit, hello.Platform = "mhk-bot", "mhk-bot"
	hello.SimVersion = b.cfg.manifests.SimVersion
	sent := time.Now()
	ok, err := c.Hello(ctx, hello)
	if err != nil {
		c.Close()
		return fmt.Errorf("hello: %w", err)
	}
	b.offset = ok.ServerTime.Sub(sent.Add(time.Since(sent) / 2))
	// Races are UDP only, so without these the server's idle timeout drops the bot mid-race.
	c.KeepAlive(ctx, b.cfg.keepAlive)
	s, err := c.Login(ctx, b.name, b.cfg.pass)
	if testclient.StatusOf(err) == proto.StatusInvalidCredentials {
		s, err = c.Register(ctx, b.name, b.cfg.pass)
	}
	if err != nil {
		c.Close()
		return fmt.Errorf("login %s: %w", b.name, err)
	}
	b.acc = s.Account.ID
	res, err := c.ContentHello(ctx, b.cfg.manifests.SimVersion, b.cfg.manifests.refs())
	if err != nil {
		c.Close()
		return fmt.Errorf("content hello: %w", err)
	}
	for _, s := range res.Strikes {
		b.log.Warn("content differs from the server's majority", "pack", s.PackID, "message", s.Message)
	}
	if len(res.Missing) > 0 {
		if err := c.ContentManifests(ctx, b.cfg.manifests.manifests(res.Missing)); err != nil {
			c.Close()
			return fmt.Errorf("content manifests: %w", err)
		}
	}
	return nil
}

func (b *bot) joinLobby(ctx context.Context) (*proto.LobbyState, error) {
	var st *proto.LobbyState
	var err error
	if b.cfg.join != "" {
		st, err = b.c.JoinLobby(ctx, 0, b.cfg.join)
	} else {
		st, err = b.c.QuickJoin(ctx, b.cfg.quick)
	}
	if err != nil {
		return nil, fmt.Errorf("join: %w", err)
	}
	if err := b.c.SetLoadout(ctx, b.cfg.character, b.cfg.kart); err != nil {
		return nil, fmt.Errorf("loadout: %w", err)
	}
	b.log.Info("joined", "lobby", st.Summary.LobbyID, "name", st.Summary.Name)
	return st, nil
}

func (b *bot) onPush(ctx context.Context, p testclient.Push) error {
	r := wire.NewReader(p.Body)
	switch p.Type {
	case proto.PushLobbyState:
		var st proto.LobbyState
		if st.Decode(r) == nil {
			b.onLobbyState(ctx, &st)
		}
	case proto.PushLobbyLeft:
		var m proto.LobbyLeft
		m.Decode(r)
		return fmt.Errorf("left the lobby (reason %d): %s", m.Reason, m.Message)
	case proto.PushDisconnect:
		var m proto.Disconnect
		m.Decode(r)
		return fmt.Errorf("disconnected (%s): %s", m.Status, m.Message)
	case proto.PushRaceSetup:
		var m proto.RaceSetup
		if m.Decode(r) == nil {
			b.startRace(ctx, &m)
		}
	case proto.PushRaceGo:
		var m proto.RaceGo
		if m.Decode(r) == nil && m.RaceID == b.raceID {
			select {
			case b.raceGo <- m.Go:
			default:
			}
		}
	case proto.PushPlayerLeftRace:
		var m proto.PlayerLeftRace
		if m.Decode(r) == nil && m.RaceID == b.raceID && m.Slot == b.slot {
			b.log.Warn("left the race", "outcome", m.Outcome, "message", m.Message)
			b.stopRace()
		}
	case proto.PushRaceResults:
		var m proto.RaceResults
		if m.Decode(r) == nil && m.RaceID == b.raceID {
			for _, row := range m.Rows {
				if row.AccountID == b.acc {
					b.log.Info("race result", "position", row.Position, "points", row.Points, "outcome", row.Outcome)
				}
			}
			b.stopRace()
		}
	}
	return nil
}

func (b *bot) onLobbyState(ctx context.Context, st *proto.LobbyState) {
	switch st.Summary.Phase {
	case proto.PhaseVoting:
		if st.PhaseEnds.Equal(b.votedRound) {
			return
		}
		b.votedRound = st.PhaseEnds
		// The ghost only makes sense on its own track; Random when the lobby does not offer it.
		if err := b.c.Vote(ctx, b.cfg.trackID); err != nil {
			if err := b.c.Vote(ctx, ""); err != nil {
				b.log.Warn("vote failed", "err", err)
			}
		}
	case proto.PhaseWaiting:
		if st.Summary.Kind != proto.LobbyPrivate {
			return
		}
		for _, m := range st.Members {
			if m.AccountID == b.acc && !m.Ready {
				if err := b.c.SetReady(ctx, true); err != nil {
					b.log.Warn("ready failed", "err", err)
				}
			}
		}
	}
}

func (b *bot) stopRace() {
	if b.raceStop != nil {
		b.raceStop()
		b.raceStop = nil
	}
}

func (b *bot) startRace(ctx context.Context, setup *proto.RaceSetup) {
	b.stopRace()
	rctx, cancel := context.WithCancel(ctx)
	b.raceStop, b.raceID, b.slot = cancel, setup.RaceID, setup.YourSlot
	b.raceGo = make(chan time.Time, 1)
	goCh := b.raceGo
	go func() {
		if err := b.race(rctx, setup, goCh); err != nil {
			b.log.Warn("race failed", "race", setup.RaceID, "err", err)
		}
	}()
}

// loadedHash is unique per bot: content agreement then sees no majority among bots, and a lone human (or bots
// only) ties with everyone, so nobody leaves as desync. The real track data is unknown to a bot.
func loadedHash(accountID uint64) string {
	h := sha256.Sum256([]byte("mhk-bot:" + strconv.FormatUint(accountID, 10)))
	return hex.EncodeToString(h[:16])
}

// race runs one race: UDP, RaceLoaded, the ghost replay from GO, Finished at the ghost's finish time.
func (b *bot) race(ctx context.Context, setup *proto.RaceSetup, goCh <-chan time.Time) error {
	rp := b.cfg.replay
	udp, err := testclient.DialUDP(net.JoinHostPort(b.cfg.host, strconv.Itoa(int(setup.UDPPort))), setup.UDPToken,
		setup.UDPKey)
	if err != nil {
		return err
	}
	defer udp.Close()
	udp.SetState(rp.state(-1))
	udp.Run(ctx, true)
	laps := setup.Laps
	if laps == 0 {
		laps = uint8(rp.g.Laps)
	}
	if err := b.c.Notify(proto.NotifyRaceLoaded, &proto.RaceLoaded{RaceID: setup.RaceID,
		TrackHash: loadedHash(b.acc), Laps: laps}); err != nil {
		return err
	}
	if int(laps) != len(rp.g.LapMs) {
		b.log.Warn("race laps differ from the ghost's: the server will time the finish itself", "race", laps,
			"ghost", len(rp.g.LapMs))
	}
	b.log.Info("race loaded", "race", setup.RaceID, "track", setup.TrackID, "slot", setup.YourSlot, "solo", setup.Solo)
	var goAt time.Time
	select {
	case <-ctx.Done():
		return nil
	case goAt = <-goCh:
	}
	tick := time.NewTicker(time.Second / 30)
	defer tick.Stop()
	finished := false
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
		t := b.serverNow().Sub(goAt)
		st := rp.state(t)
		udp.SetState(st)
		if finished || t < rp.finish() {
			continue
		}
		finished = true
		var position uint8
		if setup.Solo {
			position = 1
		}
		if err := b.c.Notify(proto.NotifyFinished, &proto.Finished{RaceID: setup.RaceID, FinishMs: rp.g.FinishMs,
			LapMs: rp.g.LapMs, Tick: st.Tick, Position: position}); err != nil {
			return err
		}
		b.log.Info("finished", "race", setup.RaceID, "ms", rp.g.FinishMs)
	}
}
