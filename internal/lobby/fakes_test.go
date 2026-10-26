package lobby

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mhk-os/server/internal/content"
	"github.com/mhk-os/server/internal/proto"
	"github.com/mhk-os/server/internal/race"
	"github.com/mhk-os/server/internal/server"
	"github.com/mhk-os/server/internal/store"
	"github.com/mhk-os/server/internal/wire"
)

// fakeClock runs AfterFunc callbacks synchronously from Advance, in due order.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
}

type fakeTimer struct {
	c       *fakeClock
	when    time.Time
	f       func()
	stopped bool
}

func (t *fakeTimer) Stop() bool {
	t.c.mu.Lock()
	defer t.c.mu.Unlock()
	was := !t.stopped
	t.stopped = true
	return was
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) AfterFunc(d time.Duration, f func()) Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &fakeTimer{c: c, when: c.now.Add(d), f: f}
	c.timers = append(c.timers, t)
	return t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	end := c.now.Add(d)
	for {
		var next *fakeTimer
		for _, t := range c.timers {
			if !t.stopped && !t.when.After(end) && (next == nil || t.when.Before(next.when)) {
				next = t
			}
		}
		if next == nil {
			break
		}
		next.stopped = true
		c.now = next.when
		c.mu.Unlock()
		next.f()
		c.mu.Lock()
	}
	c.now = end
	c.timers = slices.DeleteFunc(c.timers, func(t *fakeTimer) bool { return t.stopped })
	c.mu.Unlock()
}

type startCall struct {
	id     uint64
	spec   race.Spec
	racers []race.Racer
	cb     race.Callbacks
}

type removeCall struct {
	raceID, accountID uint64
	outcome           race.Outcome
	message           string
}

// fakeRaces records what the lobby asks of races; tests drive the callbacks.
type fakeRaces struct {
	mu      sync.Mutex
	nextID  uint64
	starts  []startCall
	removes []removeCall
	ended   []uint64
	rtt     map[uint64]uint16
	fail    error
}

func (r *fakeRaces) Start(spec race.Spec, racers []race.Racer, cb race.Callbacks) (uint64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fail != nil {
		return 0, r.fail
	}
	r.nextID++
	r.starts = append(r.starts, startCall{r.nextID, spec, slices.Clone(racers), cb})
	return r.nextID, nil
}

func (r *fakeRaces) Remove(raceID, accountID uint64, outcome race.Outcome, message string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.removes = append(r.removes, removeCall{raceID, accountID, outcome, message})
}

func (r *fakeRaces) EndEarly(raceID uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ended = append(r.ended, raceID)
}

func (r *fakeRaces) UDPPort() uint16 { return 0 }

func (r *fakeRaces) RTT(accountID uint64) (uint16, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	ms, ok := r.rtt[accountID]
	return ms, ok
}

func (r *fakeRaces) last() startCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.starts[len(r.starts)-1]
}

func (r *fakeRaces) startCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.starts)
}

// fakeCatalog: every account has the same content unless announced otherwise.
type fakeCatalog struct {
	mu      sync.Mutex
	ann     map[uint64]content.Announcement
	items   map[string][]proto.ManifestItem // by lowercase pack
	differs map[uint64]string               // account → pack whose copy differs
}

func newFakeCatalog() *fakeCatalog {
	it := func(kind uint8, ids ...string) []proto.ManifestItem {
		var out []proto.ManifestItem
		for _, id := range ids {
			out = append(out, proto.ManifestItem{Kind: kind, ID: id})
		}
		return out
	}
	return &fakeCatalog{
		ann: map[uint64]content.Announcement{},
		items: map[string][]proto.ManifestItem{
			"game": slices.Concat(it(proto.ContentCharacter, "game.huhn"), it(proto.ContentKart, "game.kart")),
			"mhk2": slices.Concat(it(proto.ContentTrack, "mhk2.rome", "mhk2.alps"), it(proto.ContentArena, "mhk2.arena"),
				it(proto.ContentKart, "mhk2.speedy"), it(proto.ContentPowerup, "mhk2.rocket"),
				it(proto.ContentCup, "mhk2.cup")),
			"mhk3":   it(proto.ContentTrack, "mhk3.paris"),
			"tincan": it(proto.ContentKart, "tincan.can"),
		},
		differs: map[uint64]string{},
	}
}

var allPacks = []string{"game", "mhk2", "mhk3", "mhke", "mhkt"}

func (c *fakeCatalog) announce(id uint64, sim int32, packs ...string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	a := content.Announcement{SimVersion: sim, Packs: map[string]proto.PackRef{}}
	for _, p := range packs {
		a.Packs[strings.ToLower(p)] = proto.PackRef{PackID: p, Commit: "v1", ManifestHash: strings.Repeat("a", 32)}
	}
	c.ann[id] = a
}

func (c *fakeCatalog) Ready(any, uint64) bool { return true }

func (c *fakeCatalog) Announced(id uint64) (content.Announcement, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	a, ok := c.ann[id]
	return a, ok
}

func (c *fakeCatalog) Differs(id uint64, pack string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.EqualFold(c.differs[id], pack)
}

func (c *fakeCatalog) Has(id uint64, pack string, kind uint8, item string) bool {
	return slices.ContainsFunc(c.Items(id, pack, kind), func(x string) bool { return strings.EqualFold(x, item) })
}

func (c *fakeCatalog) Items(id uint64, pack string, kind uint8) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.ann[id].Packs[strings.ToLower(pack)]; !ok {
		return nil
	}
	var out []string
	for _, it := range c.items[strings.ToLower(pack)] {
		if it.Kind == kind {
			out = append(out, it.ID)
		}
	}
	return out
}

type push struct {
	to  uint64
	typ proto.Type
	msg proto.Message
}

type fakePusher struct {
	mu     sync.Mutex
	pushes []push
}

func (p *fakePusher) Push(id uint64, typ proto.Type, m proto.Message) int {
	// Encode now, like the hub does, so later state changes cannot leak into recorded pushes.
	b := proto.Encode(m)
	var copyMsg proto.Message
	switch typ {
	case proto.PushLobbyState:
		copyMsg = &proto.LobbyState{}
	case proto.PushLobbyLeft:
		copyMsg = &proto.LobbyLeft{}
	case proto.PushSoloRaceJoinWaiting:
		copyMsg = &proto.SoloRaceJoinWaiting{}
	case proto.PushRaceResults:
		copyMsg = &proto.RaceResults{}
	default:
		panic(fmt.Sprintf("unexpected push %d", typ))
	}
	if err := copyMsg.Decode(wire.NewReader(b)); err != nil {
		panic(fmt.Sprintf("push %d does not decode: %v", typ, err))
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pushes = append(p.pushes, push{id, typ, copyMsg})
	return 1
}

// of returns the pushes of a type to an account, oldest first.
func (p *fakePusher) of(id uint64, typ proto.Type) []proto.Message {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []proto.Message
	for _, x := range p.pushes {
		if x.to == id && x.typ == typ {
			out = append(out, x.msg)
		}
	}
	return out
}

func (p *fakePusher) lastState(t *testing.T, id uint64) *proto.LobbyState {
	t.Helper()
	list := p.of(id, proto.PushLobbyState)
	if len(list) == 0 {
		t.Fatalf("no LobbyState pushed to %d", id)
	}
	return list[len(list)-1].(*proto.LobbyState)
}

type fakeAudit struct {
	mu      sync.Mutex
	actions []store.Action
	fail    error
}

func (a *fakeAudit) RecordAction(_ context.Context, act store.Action) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.fail != nil {
		return a.fail
	}
	a.actions = append(a.actions, act)
	return nil
}

type env struct {
	t     *testing.T
	m     *Manager
	clock *fakeClock
	races *fakeRaces
	cat   *fakeCatalog
	push  *fakePusher
	audit *fakeAudit
}

const testSim = 7

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{
		t:     t,
		clock: &fakeClock{now: time.UnixMilli(1_800_000_000_000).UTC()},
		races: &fakeRaces{rtt: map[uint64]uint16{}},
		cat:   newFakeCatalog(),
		push:  &fakePusher{},
		audit: &fakeAudit{},
	}
	var seed [32]byte
	binary.LittleEndian.PutUint64(seed[:], 42)
	e.m = New(Config{Races: e.races, Catalog: e.cat, Pusher: e.push, Audit: e.audit, Clock: e.clock,
		Rand: rand.New(rand.NewChaCha8(seed))})
	t.Cleanup(e.m.Close)
	return e
}

// player returns a player with the default content announced.
func (e *env) player(id uint64) Player {
	e.cat.announce(id, testSim, allPacks...)
	return Player{AccountID: id, Username: fmt.Sprintf("p%d", id), Country: "DE"}
}

// lobbyOf returns the lobby of the account under the manager lock (nil when none).
func (e *env) lobbyOf(id uint64) *lobby {
	e.m.mu.Lock()
	defer e.m.mu.Unlock()
	return e.m.byAccount[id]
}

func (e *env) phase(l *lobby) uint8 {
	e.m.mu.Lock()
	defer e.m.mu.Unlock()
	return l.phase
}

// waitPhase waits for a phase reached through a race callback (they run on their own goroutines).
func (e *env) waitPhase(t *testing.T, l *lobby, phase uint8) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for e.phase(l) != phase {
		if time.Now().After(deadline) {
			t.Fatalf("lobby %d in phase %d, want %d", l.id, e.phase(l), phase)
		}
		time.Sleep(time.Millisecond)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// ok fails the test on err and returns the state of a join.
func (e *env) ok(s *proto.LobbyState, err error) *proto.LobbyState {
	e.t.Helper()
	must(e.t, err)
	return s
}

func wantStatus(t *testing.T, err error, want proto.Status, contains string) {
	t.Helper()
	var se *server.StatusError
	if !errors.As(err, &se) || se.Status != want {
		t.Fatalf("error %v, want status %s", err, want)
	}
	if !strings.Contains(se.Message, contains) {
		t.Fatalf("message %q does not contain %q", se.Message, contains)
	}
}
