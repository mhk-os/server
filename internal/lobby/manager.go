// Package lobby runs public lobbies and private rooms (docs/protocol.md, "Multiplayer (phase 3)"): membership,
// content checks, loadouts, voting, phases and timers, sets and points, kicks. Races run in package race; a lobby
// starts them through race.Races and learns about them through race.Callbacks.
//
// All lobbies share one mutex: every operation is short (pushes only enqueue), and one lock keeps moves between
// lobbies, instance balancing and race callbacks free of lock-order problems.
package lobby

import (
	"context"
	crand "crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/mhk-os/server/internal/content"
	"github.com/mhk-os/server/internal/proto"
	"github.com/mhk-os/server/internal/race"
	"github.com/mhk-os/server/internal/server"
	"github.com/mhk-os/server/internal/store"
)

// Points per race by position (1st…10th); every later finished place scores 1, DNF 0.
var positionPoints = [...]int32{15, 12, 10, 8, 7, 6, 5, 4, 3, 2}

// recordTimeout bounds writing a race's results to the racers' profiles.
const recordTimeout = 15 * time.Second

// gamePack is the built-in pack: its items are content of every lobby.
const gamePack = "game"

// codeAlphabet leaves out look-alikes (I, O, 0, 1).
const (
	codeAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	codeLength   = 6
)

// Timing holds the phase durations; zero fields take DefaultTiming's.
type Timing struct {
	Voting     time.Duration
	Results    time.Duration
	SetResults time.Duration
	Countdown  time.Duration // RaceGo announces GO 3.5 s ahead
	KickBan    time.Duration
	Ping       time.Duration // how often member pings are refreshed (and pushed when they changed)
}

// DefaultTiming holds the values from docs/protocol.md.
var DefaultTiming = Timing{
	Voting:     20 * time.Second,
	Results:    10 * time.Second,
	SetResults: 15 * time.Second,
	Countdown:  3500 * time.Millisecond,
	KickBan:    10 * time.Minute,
	Ping:       5 * time.Second,
}

// Pusher delivers pushes to every connection of an account (server.Hub).
type Pusher interface {
	Push(accountID uint64, typ proto.Type, m proto.Message) int
}

// Catalog answers the content questions of lobbies (content.Catalog).
type Catalog interface {
	// Ready reports whether the connection announced its content completely (lobby requests need it).
	Ready(conn any, accountID uint64) bool
	Announced(accountID uint64) (content.Announcement, bool)
	Differs(accountID uint64, packID string) bool
	Has(accountID uint64, packID string, kind uint8, id string) bool
	Items(accountID uint64, packID string, kind uint8) []string
	Overrides(ann content.Announcement) []content.Override
}

// Auditor writes the moderation audit log (store.Store).
type Auditor interface {
	RecordAction(ctx context.Context, a store.Action) error
}

// Scores adds online races to the racers' profiles (store.Store).
type Scores interface {
	RecordRace(ctx context.Context, results []store.RaceResult) error
}

// RTTSource is implemented by race engines that measure players' round trip times; pings stay 0 without one.
type RTTSource interface {
	RTT(accountID uint64) (ms uint16, ok bool)
}

// Config wires a Manager.
type Config struct {
	Races   race.Races
	Catalog Catalog
	Pusher  Pusher
	Audit   Auditor
	Scores  Scores
	Log     *slog.Logger
	Clock   Clock      // nil = real time
	Rand    *rand.Rand // nil = randomly seeded
	Timing  Timing
}

// Player is the account behind a request, as the connection knows it.
type Player struct {
	AccountID uint64
	Username  string
	Country   string
	Moderator bool
	Platform  uint8 // proto.Platform* of the connection that made the request (kept from the join)
}

type member struct {
	Player
	character, kart string
	ready           bool
	inRace          bool
	ping            uint16
	points          int32 // in the current set
	joined          time.Time
}

func (m *member) hasLoadout() bool { return m.character != "" && m.kart != "" }

type lobby struct {
	id             uint64
	public         bool
	category       uint8
	instance       uint16
	contentVersion int32
	set            contentSet // anticheat lobbies: what every member announces; nil otherwise
	code           string
	settings       proto.RoomSettings
	members        []*member // join order: members[0] is the longest present
	owner          uint64    // private rooms

	phase   uint8
	ends    time.Time // zero = no timer
	timer   Timer
	gen     uint64 // invalidates timers of earlier phases
	setRace uint8  // races started in the current set; 0 = between sets
	votes   map[uint64]string
	track   string
	raceID  uint64
	solo    bool // the current race is a solo race: outside the set, no points, no results phase
	humans  int  // humans in the current race
	aiRaced int  // AI karts in the current race (slots 0…aiRaced−1)
	roster  []*aiDriver
	results []proto.RaceResultRow
	bans    map[uint64]time.Time // kicked accounts and until when
	closed  bool
}

// aiDriver is one AI driver of the lobby's current set; race n of the set uses the first drivers in this order.
type aiDriver struct {
	character, kart string
	points          int32 // in the current set
}

func (l *lobby) member(accountID uint64) *member {
	for _, m := range l.members {
		if m.AccountID == accountID {
			return m
		}
	}
	return nil
}

func (l *lobby) full() bool { return len(l.members) >= int(l.settings.MaxPlayers) }

func (l *lobby) setSize() uint8 {
	if l.public {
		return publicSetSize
	}
	return l.settings.RacesPerSet
}

// minVoters is how many humans the automatic cycle of a public lobby needs: one in AI categories, else two.
func (l *lobby) minVoters() int {
	if categories[l.category].ai {
		return 1
	}
	return 2
}

// autoCycle reports whether a public lobby runs its voting cycle by itself.
func (l *lobby) autoCycle() bool { return l.public && len(l.members) >= l.minVoters() }

func (l *lobby) racing() bool {
	return l.phase == proto.PhaseLoading || l.phase == proto.PhaseCountdown || l.phase == proto.PhaseRacing
}

// trackKind is the manifest kind of the lobby's tracks: arenas for battle sets, race tracks otherwise.
func (l *lobby) trackKind() uint8 {
	if l.settings.Mode == proto.RoomBattleSet {
		return proto.ContentArena
	}
	return proto.ContentTrack
}

// instanceKey groups the public instances players are matched into: anticheat categories also by content set.
type instanceKey struct {
	category       uint8
	contentVersion int32
	set            string // contentSet.String()
}

func (l *lobby) key() instanceKey { return instanceKey{l.category, l.contentVersion, l.set.String()} }

// publicKey returns the instance key of category cat for an announcement and the content set of its instances.
func publicKey(cat uint8, ann content.Announcement) (instanceKey, contentSet) {
	var set contentSet
	if categories[cat].anticheat {
		set = setOf(ann, categories[cat].packs)
	}
	return instanceKey{cat, ann.ContentVersion, set.String()}, set
}

// contentSet is the manifest hash of each lobby pack and of the game pack, by lowercase pack id ("" = not
// announced). Every member of an anticheat lobby announces the same.
type contentSet map[string]string

func setOf(ann content.Announcement, packs []string) contentSet {
	s := contentSet{}
	for _, pack := range withGame(packs) {
		ref, _ := ann.Pack(pack)
		s[strings.ToLower(pack)] = ref.ManifestHash
	}
	return s
}

// String is the canonical form instances are keyed by; "" for nil.
func (s contentSet) String() string {
	var b strings.Builder
	for _, pack := range slices.Sorted(maps.Keys(s)) {
		b.WriteString(pack)
		b.WriteByte('=')
		b.WriteString(s[pack])
		b.WriteByte(';')
	}
	return b.String()
}

// withGame returns the game pack followed by packs.
func withGame(packs []string) []string { return append([]string{gamePack}, packs...) }

// Manager owns every lobby.
type Manager struct {
	cfg    Config
	log    *slog.Logger
	clock  Clock
	timing Timing
	rtt    RTTSource

	mu        sync.Mutex
	rng       *rand.Rand
	nextID    uint64
	lobbies   map[uint64]*lobby
	codes     map[string]*lobby
	instances map[instanceKey][]*lobby // sorted by instance number
	byAccount map[uint64]*lobby
	pingTimer Timer
	stopped   bool
}

// New returns a Manager; Close stops its timers.
func New(cfg Config) *Manager {
	m := &Manager{
		cfg:       cfg,
		log:       cfg.Log,
		clock:     cfg.Clock,
		timing:    cfg.Timing,
		rng:       cfg.Rand,
		lobbies:   map[uint64]*lobby{},
		codes:     map[string]*lobby{},
		instances: map[instanceKey][]*lobby{},
		byAccount: map[uint64]*lobby{},
	}
	if m.log == nil {
		m.log = slog.Default()
	}
	if m.clock == nil {
		m.clock = realClock{}
	}
	if m.rng == nil {
		var seed [32]byte
		crand.Read(seed[:])
		m.rng = rand.New(rand.NewChaCha8(seed))
	}
	d := DefaultTiming
	for _, f := range []struct {
		v   *time.Duration
		def time.Duration
	}{
		{&m.timing.Voting, d.Voting}, {&m.timing.Results, d.Results}, {&m.timing.SetResults, d.SetResults},
		{&m.timing.Countdown, d.Countdown}, {&m.timing.KickBan, d.KickBan}, {&m.timing.Ping, d.Ping},
	} {
		if *f.v <= 0 {
			*f.v = f.def
		}
	}
	m.rtt, _ = cfg.Races.(RTTSource)
	if m.rtt != nil {
		m.mu.Lock()
		m.schedulePings()
		m.mu.Unlock()
	}
	return m
}

// Close stops every timer; the Manager must not be used afterwards.
func (m *Manager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stopped = true
	if m.pingTimer != nil {
		m.pingTimer.Stop()
	}
	for _, l := range m.lobbies {
		if l.timer != nil {
			l.timer.Stop()
		}
	}
}

func errorf(status proto.Status, format string, args ...any) error {
	return server.Errorf(status, format, args...)
}

var (
	errNotInLobby   = errorf(proto.StatusBadRequest, "You are not in a lobby.")
	errNotAnnounced = errorf(proto.StatusBadRequest, "content not announced")
	errNoLoadouts   = errorf(proto.StatusBadRequest, "Nobody has chosen a character and a kart yet.")
)

// ---- lookups and checks ----

// announced returns the caller's content announcement.
func (m *Manager) announced(p Player) (content.Announcement, error) {
	ann, ok := m.cfg.Catalog.Announced(p.AccountID)
	if !ok {
		return content.Announcement{}, errNotAnnounced
	}
	return ann, nil
}

func hasPack(packs []string, id string) bool {
	return slices.ContainsFunc(packs, func(p string) bool { return strings.EqualFold(p, id) })
}

// isContent reports whether id is lobby content of the given kind for the account: its pack (the text before the
// first dot) is a lobby pack or the built-in game pack, and the deciding manifest of the account's version lists it.
func (m *Manager) isContent(packs []string, accountID uint64, kind uint8, id string) bool {
	pack := content.PackOf(id)
	if pack == "" || (!strings.EqualFold(pack, gamePack) && !hasPack(packs, pack)) {
		return false
	}
	return m.cfg.Catalog.Has(accountID, pack, kind, id)
}

// checkContent applies the content rules of every lobby: same content version, every pack enabled, and no pack
// replacing an item of a lobby pack or of the game pack.
func (m *Manager) checkContent(ann content.Announcement, contentVersion int32, packs []string) error {
	if ann.ContentVersion != contentVersion {
		return errorf(proto.StatusBadRequest,
			"This lobby runs another game version (content version %d, yours %d). Please update the game.",
			contentVersion, ann.ContentVersion)
	}
	var missing []string
	for _, pack := range packs {
		if _, ok := ann.Pack(pack); !ok {
			missing = append(missing, pack)
		}
	}
	if len(missing) > 0 {
		return errorf(proto.StatusBadRequest, "Missing content packs: %s.", strings.Join(missing, ", "))
	}
	var mods, ids []string
	n := 0
	for _, o := range m.cfg.Catalog.Overrides(ann) {
		if !hasPack(withGame(packs), content.PackOf(o.ID)) {
			continue
		}
		n++
		if !slices.Contains(mods, o.Pack) {
			mods = append(mods, o.Pack)
		}
		if len(ids) < 3 {
			ids = append(ids, o.ID)
		}
	}
	if n == 0 {
		return nil
	}
	more := ""
	if n > len(ids) {
		more = fmt.Sprintf(" and %d more", n-len(ids))
	}
	return errorf(proto.StatusBadRequest, "%s replaces content of this lobby (%s%s). Everybody must race with the "+
		"same content: disable it on the Mods screen to join.", strings.Join(mods, ", "), strings.Join(ids, ", "), more)
}

// checkAnticheat applies the content rules of anticheat lobbies: no lobby pack (nor the game pack) differing from
// the catalog majority, and the lobby's content set when it has one.
func (m *Manager) checkAnticheat(p Player, ann content.Announcement, packs []string, set contentSet) error {
	for _, pack := range withGame(packs) {
		if m.cfg.Catalog.Differs(p.AccountID, pack) {
			return errorf(proto.StatusForbidden, "Your copy of %s differs from other players'.", pack)
		}
	}
	if set == nil {
		return nil
	}
	for _, pack := range withGame(packs) {
		if ref, _ := ann.Pack(pack); ref.ManifestHash != set[strings.ToLower(pack)] {
			return errorf(proto.StatusForbidden,
				"Your version of %s differs from this lobby's (update your mods or pick another lobby).", pack)
		}
	}
	return nil
}

func (m *Manager) checkJoin(l *lobby, p Player, ann content.Announcement) error {
	if err := m.checkContent(ann, l.contentVersion, l.settings.Packs); err != nil {
		return err
	}
	if until, ok := l.bans[p.AccountID]; ok {
		if m.clock.Now().Before(until) {
			return errorf(proto.StatusForbidden, "You were kicked from this lobby. Try again later.")
		}
		delete(l.bans, p.AccountID)
	}
	if l.full() {
		return errorf(proto.StatusForbidden, "This lobby is full.")
	}
	if !l.settings.Anticheat {
		return nil
	}
	return m.checkAnticheat(p, ann, l.settings.Packs, l.set)
}

// ---- state pushes ----

func (m *Manager) summary(l *lobby) proto.LobbySummary {
	s := proto.LobbySummary{
		LobbyID: l.id, Kind: proto.LobbyPrivate, Name: l.settings.Name, Mode: l.settings.Mode,
		Anticheat: l.settings.Anticheat, Packs: l.settings.Packs, Players: uint8(len(l.members)),
		MaxPlayers: l.settings.MaxPlayers, Phase: l.phase, ContentVersion: l.contentVersion,
		EngineClass: l.settings.EngineClass, SetRace: l.setRace, SetSize: l.setSize(), MaxAI: l.settings.MaxAI,
	}
	if l.public {
		s.Kind, s.Category, s.Instance = proto.LobbyPublic, l.category, l.instance
		s.Name = fmt.Sprintf("%s #%d", l.settings.Name, l.instance)
	}
	return s
}

func (m *Manager) state(l *lobby) *proto.LobbyState {
	s := &proto.LobbyState{
		Summary: m.summary(l), Code: l.code, Settings: l.settings, PhaseEnds: l.ends, SetRace: l.setRace,
		SetSize: l.setSize(), TrackID: l.track, RaceID: l.raceID,
		Members: make([]proto.LobbyMember, len(l.members)), Votes: []proto.LobbyVote{},
		LastResults: l.results, AIDrivers: make([]proto.AIDriver, len(l.roster)),
	}
	if s.LastResults == nil {
		s.LastResults = []proto.RaceResultRow{}
	}
	for i, d := range l.roster {
		s.AIDrivers[i] = proto.AIDriver{CharacterID: d.character, KartID: d.kart, SetPoints: d.points}
	}
	for i, mem := range l.members {
		s.Members[i] = proto.LobbyMember{
			AccountID: mem.AccountID, Username: mem.Username, Country: mem.Country, CharacterID: mem.character,
			KartID: mem.kart, Ready: mem.ready, Owner: !l.public && mem.AccountID == l.owner,
			Moderator: mem.Moderator, InRace: mem.inRace, PingMs: mem.ping, SetPoints: mem.points,
			Joined: mem.joined, Platform: mem.Platform,
		}
		if t, ok := l.votes[mem.AccountID]; ok {
			s.Votes = append(s.Votes, proto.LobbyVote{AccountID: mem.AccountID, TrackID: t})
		}
	}
	return s
}

// broadcast pushes LobbyState to every member except one (0 = nobody), who gets it as a response.
func (m *Manager) broadcast(l *lobby, except uint64) {
	if l.closed || len(l.members) == 0 {
		return
	}
	s := m.state(l)
	for _, mem := range l.members {
		if mem.AccountID != except {
			m.cfg.Pusher.Push(mem.AccountID, proto.PushLobbyState, s)
		}
	}
}

// ---- phases and timers ----

// setPhase enters a phase; after d (0 = no timer) next runs under the manager lock unless the lobby moved on.
func (m *Manager) setPhase(l *lobby, phase uint8, d time.Duration, next func()) {
	if l.timer != nil {
		l.timer.Stop()
		l.timer = nil
	}
	l.gen++
	l.phase = phase
	l.ends = time.Time{}
	if d <= 0 {
		return
	}
	l.ends = m.clock.Now().Add(d).Truncate(time.Millisecond)
	gen := l.gen
	l.timer = m.clock.AfterFunc(d, func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.stopped || l.closed || l.gen != gen {
			return
		}
		l.timer = nil
		next()
	})
}

// idle returns an empty lobby to Waiting and forgets its set.
func (m *Manager) idle(l *lobby) {
	m.setPhase(l, proto.PhaseWaiting, 0, nil)
	l.setRace, l.results, l.track, l.votes, l.roster = 0, nil, "", nil, nil
}

// between moves to what follows a race or the results: Voting in public lobbies with enough humans (two, one in
// AI categories: the automatic cycle), otherwise Waiting (a lone player, or a private room's owner, starts the
// next race).
func (m *Manager) between(l *lobby) {
	switch {
	case len(l.members) == 0:
		m.idle(l)
	case l.autoCycle():
		m.startVoting(l)
	default:
		m.setPhase(l, proto.PhaseWaiting, 0, nil)
		l.track, l.votes = "", nil
	}
}

func (m *Manager) startVoting(l *lobby) {
	l.votes, l.track = map[uint64]string{}, ""
	m.setPhase(l, proto.PhaseVoting, m.timing.Voting, func() {
		m.endVoting(l)
		m.broadcast(l, 0)
	})
}

func (m *Manager) allVoted(l *lobby) bool {
	for _, mem := range l.members {
		if _, ok := l.votes[mem.AccountID]; !ok {
			return false
		}
	}
	return len(l.members) > 0
}

// endVoting draws one vote at random (in member order, so a seeded RNG is reproducible) and starts the race. A
// Random vote, or no vote, takes a random track of the lobby content. When nothing can start (no loadouts, no
// track), public lobbies vote again and private rooms wait for the owner.
func (m *Manager) endVoting(l *lobby) {
	var drawn []string
	for _, mem := range l.members {
		if t, ok := l.votes[mem.AccountID]; ok {
			drawn = append(drawn, t)
		}
	}
	track := ""
	if len(drawn) > 0 {
		track = drawn[m.rng.IntN(len(drawn))]
	}
	if track == "" {
		track = m.randomTrack(l)
	}
	err := errNoLoadouts
	if track != "" {
		err = m.startRace(l, track, false)
	}
	if err == nil {
		return
	}
	m.log.Info("lobby race not started", "lobby", l.id, "err", err)
	if l.autoCycle() {
		m.startVoting(l)
	} else {
		m.setPhase(l, proto.PhaseWaiting, 0, nil)
		l.votes = nil
	}
}

// commonItems returns the items of a kind that are lobby content and that the catalog lists for every given member
// (lowercase id → id): what everybody racing can load.
func (m *Manager) commonItems(l *lobby, racers []*member, kind uint8) map[string]string {
	packs := withGame(l.settings.Packs)
	var common map[string]string
	for _, mem := range racers {
		mine := map[string]string{}
		for _, pack := range packs {
			for _, id := range m.cfg.Catalog.Items(mem.AccountID, pack, kind) {
				if strings.EqualFold(content.PackOf(id), pack) {
					mine[strings.ToLower(id)] = id
				}
			}
		}
		if common == nil {
			common = mine
			continue
		}
		for k := range common {
			if _, ok := mine[k]; !ok {
				delete(common, k)
			}
		}
	}
	return common
}

// sortedKeys returns the lowercase ids of common in ordinal order.
func sortedKeys(common map[string]string) []string {
	return slices.Sorted(maps.Keys(common))
}

// racersOf returns the members with a loadout: the humans of the next race.
func racersOf(l *lobby) []*member {
	var out []*member
	for _, mem := range l.members {
		if mem.hasLoadout() {
			out = append(out, mem)
		}
	}
	return out
}

// randomTrack picks a random track (or arena) of the lobby content that the catalog lists for every member with a
// loadout (the racers of the next race), "" when there is none.
func (m *Manager) randomTrack(l *lobby) string {
	common := m.commonItems(l, racersOf(l), l.trackKind())
	if len(common) == 0 {
		return ""
	}
	keys := sortedKeys(common)
	return common[keys[m.rng.IntN(len(keys))]]
}

// drivers returns the n AI drivers of a race of the given humans. Set races use the lobby's roster: drivers whose
// character or kart the humans do not all have are drawn again (keeping their points), missing ones are added. A
// solo race draws throwaway drivers. A draw takes characters in a shuffled order (characters no human races with
// first, then those no kept driver has, cycling when there are more drivers than characters) and a uniformly
// random kart. Without a common character or kart the race has no AI.
func (m *Manager) drivers(l *lobby, humans []*member, n int, solo bool) []*aiDriver {
	if n <= 0 {
		return nil
	}
	chars := m.commonItems(l, humans, proto.ContentCharacter)
	karts := m.commonItems(l, humans, proto.ContentKart)
	if len(chars) == 0 || len(karts) == 0 {
		m.log.Info("race without AI: the racers share no character or kart", "lobby", l.id, "characters",
			len(chars), "karts", len(karts))
		return nil
	}
	var roster []*aiDriver
	if !solo {
		roster = l.roster
	}
	valid := func(d *aiDriver) bool {
		_, okChar := chars[strings.ToLower(d.character)]
		_, okKart := karts[strings.ToLower(d.kart)]
		return okChar && okKart
	}
	chosen, kept := map[string]bool{}, map[string]bool{}
	for _, mem := range humans {
		chosen[strings.ToLower(mem.character)] = true
	}
	for _, d := range roster {
		if valid(d) {
			kept[strings.ToLower(d.character)] = true
		}
	}
	order := sortedKeys(chars)
	m.rng.Shuffle(len(order), func(i, j int) { order[i], order[j] = order[j], order[i] })
	rank := func(k string) int {
		r := 0
		if chosen[k] {
			r += 2
		}
		if kept[k] {
			r++
		}
		return r
	}
	slices.SortStableFunc(order, func(a, b string) int { return rank(a) - rank(b) })
	kartKeys := sortedKeys(karts)
	next := 0
	draw := func(d *aiDriver) {
		d.character = chars[order[next%len(order)]]
		d.kart = karts[kartKeys[m.rng.IntN(len(kartKeys))]]
		next++
	}
	for _, d := range roster {
		if !valid(d) {
			draw(d)
		}
	}
	for len(roster) < n {
		d := &aiDriver{}
		draw(d)
		roster = append(roster, d)
	}
	if !solo {
		l.roster = roster
	}
	return roster[:n]
}

// startRace starts a race on track: every member with a loadout races, in join order shuffled, behind the race's
// AI karts (min(maxAi, maxPlayers − humans) of the set's drivers). A solo race has the lone member against 6
// throwaway AI drivers, outside the set (no points, the set does not advance).
func (m *Manager) startRace(l *lobby, track string, solo bool) error {
	humans := racersOf(l)
	if len(humans) == 0 {
		return errNoLoadouts
	}
	if len(humans) > proto.MaxRacers {
		humans = humans[:proto.MaxRacers]
	}
	m.rng.Shuffle(len(humans), func(i, j int) { humans[i], humans[j] = humans[j], humans[i] })
	s := l.settings
	spec := race.Spec{
		LobbyID: l.id, TrackID: track, Mode: race.ModeRace, EngineClass: s.EngineClass, Laps: s.Laps,
		Items: s.Items, ItemOdds: s.ItemOdds, Packs: slices.Clone(s.Packs), Anticheat: s.Anticheat,
		SetRace: l.setRace + 1, SetSize: l.setSize(),
	}
	if s.Mode == proto.RoomBattleSet {
		spec.Mode = race.ModeBattle
	}
	nAI := int(s.MaxAI)
	if solo {
		// Not part of the set: RaceSetup announces 0/0.
		spec.SetRace, spec.SetSize, nAI = 0, 0, soloAICount
	} else {
		nAI = min(nAI, int(s.MaxPlayers)-len(humans))
	}
	drivers := m.drivers(l, humans, min(nAI, proto.MaxRacers-len(humans)), solo)
	racers := make([]race.Racer, 0, len(drivers)+len(humans))
	for _, d := range drivers {
		racers = append(racers, race.Racer{AI: true, CharacterID: d.character, KartID: d.kart})
	}
	for _, mem := range humans {
		racers = append(racers, race.Racer{AccountID: mem.AccountID, Username: mem.Username, Country: mem.Country,
			CharacterID: mem.character, KartID: mem.kart, Platform: mem.Platform})
	}
	// Callbacks run on the race's goroutine and must not call back into the engine: hand them over.
	id, err := m.cfg.Races.Start(spec, racers, race.Callbacks{
		Going:    func(raceID uint64) { go m.going(l, raceID) },
		Finished: func(raceID uint64, p []race.Placement) { go m.finished(l, raceID, p) },
	})
	if err != nil {
		return fmt.Errorf("starting race: %w", err)
	}
	l.raceID, l.track, l.solo, l.votes = id, track, solo, nil
	l.humans, l.aiRaced = len(humans), len(drivers)
	if !solo {
		l.setRace++
	}
	for _, mem := range l.members {
		mem.ready = false
	}
	for _, mem := range humans {
		mem.inRace = true
	}
	m.setPhase(l, proto.PhaseLoading, 0, nil)
	return nil
}

func (m *Manager) going(l *lobby, raceID uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stopped || l.closed || l.raceID != raceID || l.phase != proto.PhaseLoading {
		return
	}
	m.setPhase(l, proto.PhaseCountdown, m.timing.Countdown, func() {
		m.setPhase(l, proto.PhaseRacing, 0, nil)
		m.broadcast(l, 0)
	})
	m.broadcast(l, 0)
}

// points are the position points of a placement, AI karts included: 15 … 2 for places 1–10, 1 for every later
// finished place, 0 for DNF and the other outcomes.
func points(p race.Placement) int32 {
	switch {
	case p.Outcome != race.OutcomeFinished || p.Position < 1:
		return 0
	case int(p.Position) <= len(positionPoints):
		return positionPoints[p.Position-1]
	}
	return 1
}

// finished ends the lobby's race and adds it to the human racers' profiles (races with two or more humans).
func (m *Manager) finished(l *lobby, raceID uint64, placements []race.Placement) {
	results := m.settle(l, raceID, placements)
	if len(results) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), recordTimeout)
	defer cancel()
	if err := m.cfg.Scores.RecordRace(ctx, results); err != nil {
		m.log.Error("recording race results", "lobby", l.id, "race", raceID, "err", err)
	}
}

// settle pushes the race's results, adds its points to the set (members and AI drivers) and moves the lobby on.
// It returns what the human racers' profiles get: nothing for solo races and races with fewer than two humans.
func (m *Manager) settle(l *lobby, raceID uint64, placements []race.Placement) []store.RaceResult {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stopped || l.closed || l.raceID != raceID {
		return nil
	}
	solo := l.solo
	humans := 0
	for _, p := range placements {
		if !p.AI {
			humans++
		}
	}
	rows := make([]proto.RaceResultRow, 0, len(placements))
	var results []store.RaceResult
	for _, p := range placements {
		row := proto.RaceResultRow{Slot: p.Slot, AccountID: p.AccountID, Username: p.Username, Position: p.Position,
			FinishMs: p.FinishMs, Outcome: uint8(p.Outcome), AI: p.AI, CharacterID: p.CharacterID}
		if !solo {
			row.Points = points(p)
		}
		switch {
		case p.AI:
			// AI slot n is driver n of the set's roster (solo races use throwaway drivers).
			if !solo && int(p.Slot) < l.aiRaced && int(p.Slot) < len(l.roster) {
				d := l.roster[p.Slot]
				d.points += row.Points
				row.SetPoints = d.points
			}
		default:
			if mem := l.member(p.AccountID); mem != nil {
				mem.points += row.Points
				row.SetPoints = mem.points
			}
			if !solo && humans >= 2 {
				// Every human who raced counts, kicked and departed racers too (they score 0).
				done := p.Outcome == race.OutcomeFinished && p.Position >= 1
				results = append(results, store.RaceResult{AccountID: p.AccountID, Points: uint32(row.Points),
					Win: done && p.Position == 1, Podium: done && p.Position <= 3})
			}
		}
		rows = append(rows, row)
	}
	if len(rows) > proto.MaxRaceRows {
		rows = rows[:proto.MaxRaceRows]
	}
	setDone := !solo && l.setRace >= l.setSize()
	l.raceID, l.solo, l.humans, l.aiRaced = 0, false, 0, 0
	for _, mem := range l.members {
		mem.inRace = false
	}
	l.results = rows
	res := &proto.RaceResults{RaceID: raceID, Rows: rows, SetDone: setDone}
	for _, mem := range l.members {
		m.cfg.Pusher.Push(mem.AccountID, proto.PushRaceResults, res)
	}
	switch {
	case len(l.members) == 0:
		m.idle(l)
		if l.public {
			m.rebalance(l.key(), l.set)
		}
		return results
	case solo:
		// Solo races have no results phase: the player may start the next one at once, and players who waited
		// for the race to end get the next race (or its voting) straight away.
		m.between(l)
	case setDone:
		m.setPhase(l, proto.PhaseSetResults, m.timing.SetResults, func() {
			for _, mem := range l.members {
				mem.points = 0
			}
			// The next set draws its AI drivers again.
			l.setRace, l.results, l.roster = 0, nil, nil
			m.between(l)
			m.broadcast(l, 0)
		})
	default:
		m.setPhase(l, proto.PhaseResults, m.timing.Results, func() {
			m.between(l)
			m.broadcast(l, 0)
		})
	}
	m.broadcast(l, 0)
	return results
}

// ---- membership ----

func (m *Manager) newLobby(contentVersion int32, settings proto.RoomSettings) *lobby {
	m.nextID++
	l := &lobby{id: m.nextID, contentVersion: contentVersion, settings: settings, bans: map[uint64]time.Time{}}
	m.lobbies[l.id] = l
	return l
}

func (m *Manager) closeLobby(l *lobby) {
	if l.timer != nil {
		l.timer.Stop()
		l.timer = nil
	}
	l.closed = true
	delete(m.lobbies, l.id)
	if l.code != "" {
		delete(m.codes, l.code)
	}
	if l.public {
		key := l.key()
		m.instances[key] = slices.DeleteFunc(m.instances[key], func(x *lobby) bool { return x == l })
		if len(m.instances[key]) == 0 {
			delete(m.instances, key)
		}
	}
	for _, mem := range l.members {
		delete(m.byAccount, mem.AccountID)
		m.cfg.Pusher.Push(mem.AccountID, proto.PushLobbyLeft,
			&proto.LobbyLeft{Reason: proto.LeftLobbyClosed, Message: "The lobby was closed."})
	}
	l.members = nil
}

// rebalance keeps the public instances under a key: empty instances close while another one has room, and when
// none has room a new one opens (with the key's content set).
func (m *Manager) rebalance(key instanceKey, set contentSet) {
	list := m.instances[key]
	for i := len(list) - 1; i >= 0; i-- {
		l := list[i]
		if len(l.members) > 0 {
			continue
		}
		for _, other := range m.instances[key] {
			if other != l && !other.full() {
				m.closeLobby(l)
				break
			}
		}
	}
	for _, l := range m.instances[key] {
		if !l.full() {
			return
		}
	}
	m.openInstance(key, set)
}

func (m *Manager) openInstance(key instanceKey, set contentSet) *lobby {
	list := m.instances[key]
	number := uint16(1)
	for _, l := range list { // sorted: the first gap is the lowest free number
		if l.instance != number {
			break
		}
		number++
	}
	l := m.newLobby(key.contentVersion, publicSettings(key.category))
	l.public, l.category, l.instance, l.set = true, key.category, number, set
	i, _ := slices.BinarySearchFunc(list, number, func(x *lobby, n uint16) int { return int(x.instance) - int(n) })
	m.instances[key] = slices.Insert(list, i, l)
	return l
}

// join adds p to l (checks done) and leaves the previous lobby.
func (m *Manager) join(l *lobby, p Player) *proto.LobbyState {
	if old := m.byAccount[p.AccountID]; old != nil && old != l {
		// No LobbyLeft: the player asked for the new lobby, and a push about the old one could reach the client
		// after the join answer and be taken for the new one.
		m.removeMember(old, p.AccountID, proto.LeftSelf, "", race.OutcomeLeft, false)
	}
	mem := &member{Player: p, joined: m.clock.Now().Truncate(time.Millisecond)}
	l.members = append(l.members, mem)
	m.byAccount[p.AccountID] = l
	if !l.public && l.owner == 0 {
		l.owner = p.AccountID
	}
	if l.humans == 1 && l.racing() {
		// The only human racing may end the race early for whoever waits.
		for _, r := range l.members {
			if r.inRace {
				m.cfg.Pusher.Push(r.AccountID, proto.PushSoloRaceJoinWaiting,
					&proto.SoloRaceJoinWaiting{Username: p.Username})
			}
		}
	}
	if l.public {
		// Enough humans start the automatic cycle; during a race or its results the cycle continues later.
		if l.phase == proto.PhaseWaiting && l.autoCycle() {
			m.startVoting(l)
		}
		m.rebalance(l.key(), l.set)
	}
	m.broadcast(l, p.AccountID)
	return m.state(l)
}

// remove takes accountID out of l (and of its race, with outcome) and tells it why.
func (m *Manager) remove(l *lobby, accountID uint64, reason uint8, message string, outcome race.Outcome) {
	m.removeMember(l, accountID, reason, message, outcome, true)
}

// removeMember is remove with the LobbyLeft push optional.
func (m *Manager) removeMember(l *lobby, accountID uint64, reason uint8, message string, outcome race.Outcome, notify bool) {
	mem := l.member(accountID)
	if mem == nil {
		return
	}
	if mem.inRace && l.raceID != 0 {
		m.cfg.Races.Remove(l.raceID, accountID, outcome, message)
	}
	l.members = slices.DeleteFunc(l.members, func(x *member) bool { return x == mem })
	delete(m.byAccount, accountID)
	delete(l.votes, accountID)
	if notify {
		m.cfg.Pusher.Push(accountID, proto.PushLobbyLeft, &proto.LobbyLeft{Reason: reason, Message: message})
	}

	if !l.public {
		if len(l.members) == 0 {
			m.closeLobby(l)
			return
		}
		if l.owner == accountID {
			l.owner = l.members[0].AccountID
		}
	}
	if len(l.members) == 0 && !l.racing() {
		m.idle(l)
	}
	switch {
	case l.phase == proto.PhaseVoting && l.public && !l.autoCycle():
		// Public voting needs two humans (one in AI categories); a lone player waits and may start a solo race.
		// A private room's voting goes on with whoever is left.
		m.setPhase(l, proto.PhaseWaiting, 0, nil)
		l.votes = nil
	case l.phase == proto.PhaseVoting && m.allVoted(l):
		m.endVoting(l)
	}
	if l.public {
		m.rebalance(l.key(), l.set)
	}
	m.broadcast(l, 0)
}

func (m *Manager) newCode() string {
	b := make([]byte, codeLength)
	for {
		for i := range b {
			b[i] = codeAlphabet[m.rng.IntN(len(codeAlphabet))]
		}
		if m.codes[string(b)] == nil {
			return string(b)
		}
	}
}

// ---- requests ----

// List returns the public instances of every category for the caller's content version (in anticheat categories,
// those of the caller's content set), opening the first instance of a category when it has none.
func (m *Manager) List(p Player) ([]proto.LobbySummary, error) {
	ann, err := m.announced(p)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []proto.LobbySummary
	for cat := uint8(minCategory); cat <= maxCategory; cat++ {
		key, set := publicKey(cat, ann)
		m.rebalance(key, set)
		for _, l := range m.instances[key] {
			// The list is capped by the protocol; 500 instances would mean thousands of players per category.
			if len(out) < proto.MaxLobbies {
				out = append(out, m.summary(l))
			}
		}
	}
	return out, nil
}

// Join enters a lobby by code (when non-empty) or id.
func (m *Manager) Join(p Player, lobbyID uint64, code string) (*proto.LobbyState, error) {
	ann, err := m.announced(p)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var l *lobby
	if code != "" {
		l = m.codes[strings.ToUpper(strings.TrimSpace(code))]
	} else {
		l = m.lobbies[lobbyID]
	}
	if l == nil {
		return nil, errorf(proto.StatusNotFound, "This lobby does not exist (any more).")
	}
	if m.byAccount[p.AccountID] == l {
		return m.state(l), nil
	}
	if err := m.checkJoin(l, p, ann); err != nil {
		return nil, err
	}
	return m.join(l, p), nil
}

// QuickJoin enters the fullest public instance of a category (of the caller's content set in anticheat categories)
// that has room and that the caller may join: a kick ban from one instance falls through to the next.
func (m *Manager) QuickJoin(p Player, cat uint8) (*proto.LobbyState, error) {
	if cat < minCategory || cat > maxCategory {
		return nil, errorf(proto.StatusBadRequest, "Unknown lobby category %d.", cat)
	}
	ann, err := m.announced(p)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	key, set := publicKey(cat, ann)
	m.rebalance(key, set)
	candidates := slices.Clone(m.instances[key])
	if cur := m.byAccount[p.AccountID]; cur != nil && slices.Contains(candidates, cur) {
		return m.state(cur), nil
	}
	// Stable: equally full instances keep their number order.
	slices.SortStableFunc(candidates, func(a, b *lobby) int { return len(b.members) - len(a.members) })
	var firstErr error
	for _, l := range candidates {
		if l.full() {
			continue
		}
		err := m.checkJoin(l, p, ann)
		if err == nil {
			return m.join(l, p), nil
		}
		if firstErr == nil {
			firstErr = err
		}
	}
	return nil, firstErr
}

// validateSettings checks room settings against the creator's content and normalizes them.
func (m *Manager) validateSettings(p Player, ann content.Announcement, s proto.RoomSettings) (proto.RoomSettings, error) {
	bad := func(format string, args ...any) (proto.RoomSettings, error) {
		return s, errorf(proto.StatusBadRequest, format, args...)
	}
	s.Name = strings.TrimSpace(s.Name)
	if s.Name == "" {
		s.Name = truncate(p.Username+"'s room", proto.MaxRoomName)
	}
	switch {
	case strings.ContainsFunc(s.Name, func(r rune) bool { return r < 0x20 || r == 0x7f }):
		return bad("The room name contains control characters.")
	case s.Mode > proto.RoomChampionship:
		return bad("Unknown room mode %d.", s.Mode)
	case s.EngineClass > 2:
		return bad("Engine class must be 0-2.")
	case s.Laps > 99:
		return bad("Laps must be 0-99.")
	case s.MaxPlayers < 2 || s.MaxPlayers > proto.MaxLobbyMembers:
		return bad("Max players must be 2-%d.", proto.MaxLobbyMembers)
	case s.MaxAI > s.MaxPlayers-1:
		return bad("At most %d AI drivers in a room for %d (max players − 1).", s.MaxPlayers-1, s.MaxPlayers)
	case s.TrackPick > proto.PickVote:
		return bad("Unknown track pick mode %d.", s.TrackPick)
	case len(s.TrackIDs) > proto.MaxCustomTracks:
		return bad("A championship lists at most %d tracks.", proto.MaxCustomTracks)
	case (s.Mode != proto.RoomChampionship || len(s.TrackIDs) == 0) && (s.RacesPerSet < 1 || s.RacesPerSet > 16):
		return bad("Races per set must be 1-16.")
	}
	for i, pack := range s.Packs {
		if pack == "" || hasPack(s.Packs[:i], pack) {
			return bad("Pack %q is listed twice or empty.", pack)
		}
	}
	if err := m.checkContent(ann, ann.ContentVersion, s.Packs); err != nil {
		return s, err
	}
	if s.Anticheat {
		if err := m.checkAnticheat(p, ann, s.Packs, nil); err != nil {
			return s, err
		}
	}
	seen := map[string]bool{}
	for _, o := range s.ItemOdds {
		if seen[strings.ToLower(o.PowerupID)] {
			return bad("Item odds list %s twice.", o.PowerupID)
		}
		seen[strings.ToLower(o.PowerupID)] = true
		if !m.isContent(s.Packs, p.AccountID, proto.ContentPowerup, o.PowerupID) {
			return bad("%s is not a powerup of the room's packs.", o.PowerupID)
		}
	}
	if s.Mode == proto.RoomChampionship {
		// The owner's client starts every track in order: never a vote.
		s.TrackPick = proto.PickOwner
		switch {
		case s.CupID != "" && len(s.TrackIDs) > 0:
			return bad("A championship races a cup or a track list, not both.")
		case len(s.TrackIDs) > 0:
			for _, id := range s.TrackIDs {
				if !m.isContent(s.Packs, p.AccountID, proto.ContentTrack, id) {
					return bad("%s is not a race track of the room's packs.", id)
				}
			}
			s.RacesPerSet = uint8(len(s.TrackIDs))
		case !m.isContent(s.Packs, p.AccountID, proto.ContentCup, s.CupID):
			return bad("Choose a cup or race tracks of the room's packs for a championship.")
		}
	} else {
		s.CupID, s.TrackIDs = "", nil
	}
	return s, nil
}

// CreateRoom opens a private room owned by the caller; an anticheat room keeps the caller's content set.
func (m *Manager) CreateRoom(p Player, settings proto.RoomSettings) (*proto.LobbyState, error) {
	ann, err := m.announced(p)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	s, err := m.validateSettings(p, ann, settings)
	if err != nil {
		return nil, err
	}
	l := m.newLobby(ann.ContentVersion, s)
	if s.Anticheat {
		l.set = setOf(ann, s.Packs)
	}
	l.code = m.newCode()
	m.codes[l.code] = l
	return m.join(l, p), nil
}

// UpdateRoom changes the settings of the caller's room while it waits. Every member must be able to stay: the
// update is refused when one lacks a new pack, runs a mod replacing content of one, or (anticheat rooms, whose
// content set becomes the owner's) holds a differing copy, or when the room would be over its new size. Loadouts
// that stop being room content are cleared.
func (m *Manager) UpdateRoom(p Player, settings proto.RoomSettings) error {
	ann, err := m.announced(p)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	l := m.byAccount[p.AccountID]
	switch {
	case l == nil:
		return errNotInLobby
	case l.public || l.owner != p.AccountID:
		return errorf(proto.StatusForbidden, "Only the room owner can change the settings.")
	case l.phase != proto.PhaseWaiting:
		return errorf(proto.StatusBadRequest, "Settings can only change while the room waits.")
	}
	s, err := m.validateSettings(p, ann, settings)
	if err != nil {
		return err
	}
	if len(l.members) > int(s.MaxPlayers) {
		return errorf(proto.StatusBadRequest, "The room has %d players, more than %d.", len(l.members), s.MaxPlayers)
	}
	var set contentSet
	if s.Anticheat {
		set = setOf(ann, s.Packs)
	}
	for _, mem := range l.members {
		mann, ok := m.cfg.Catalog.Announced(mem.AccountID)
		if !ok {
			return errorf(proto.StatusBadRequest, "%s has not announced their content.", mem.Username)
		}
		err := m.checkContent(mann, l.contentVersion, s.Packs)
		if err == nil && s.Anticheat {
			err = m.checkAnticheat(mem.Player, mann, s.Packs, set)
		}
		if err != nil {
			msg := err.Error()
			if se := (*server.StatusError)(nil); errors.As(err, &se) {
				msg = se.Message
			}
			return errorf(proto.StatusBadRequest, "%s cannot play with these settings: %s", mem.Username, msg)
		}
	}
	l.settings, l.set, l.roster = s, set, nil // the set's AI drivers are drawn again
	for _, mem := range l.members {
		if mem.character != "" && !m.isContent(s.Packs, mem.AccountID, proto.ContentCharacter, mem.character) {
			mem.character = ""
		}
		if mem.kart != "" && !m.isContent(s.Packs, mem.AccountID, proto.ContentKart, mem.kart) {
			mem.kart = ""
		}
	}
	m.broadcast(l, 0)
	return nil
}

// Leave takes the account out of its lobby (LeaveLobby, or its last connection closed).
func (m *Manager) Leave(accountID uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if l := m.byAccount[accountID]; l != nil {
		m.remove(l, accountID, proto.LeftSelf, "", race.OutcomeLeft)
	}
}

// Rename shows an account's new username (ChangeUsername) in its lobby: the member and its rows of the last results.
// A race already running keeps the name it started with.
func (m *Manager) Rename(accountID uint64, username string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	l := m.byAccount[accountID]
	if l == nil {
		return
	}
	l.member(accountID).Username = username
	// A fresh slice: pushed states may still share the old one.
	results := slices.Clone(l.results)
	for i := range results {
		if results[i].AccountID == accountID {
			results[i].Username = username
		}
	}
	l.results = results
	m.broadcast(l, 0)
}

// SetLoadout chooses the caller's character and kart ("" clears); both must be lobby content, and the caller's
// content must still pass the lobby's content rules.
func (m *Manager) SetLoadout(p Player, character, kart string) error {
	ann, err := m.announced(p)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	l := m.byAccount[p.AccountID]
	if l == nil {
		return errNotInLobby
	}
	if character != "" || kart != "" {
		if err := m.checkContent(ann, l.contentVersion, l.settings.Packs); err != nil {
			return err
		}
		if l.settings.Anticheat {
			if err := m.checkAnticheat(p, ann, l.settings.Packs, l.set); err != nil {
				return err
			}
		}
	}
	if character != "" && !m.isContent(l.settings.Packs, p.AccountID, proto.ContentCharacter, character) {
		return errorf(proto.StatusBadRequest, "%s is not a character of this lobby.", character)
	}
	if kart != "" && !m.isContent(l.settings.Packs, p.AccountID, proto.ContentKart, kart) {
		return errorf(proto.StatusBadRequest, "%s is not a kart of this lobby.", kart)
	}
	mem := l.member(p.AccountID)
	mem.character, mem.kart = character, kart
	m.broadcast(l, 0)
	return nil
}

// SetReady marks the caller ready in a private room (informational: the owner starts races).
func (m *Manager) SetReady(p Player, ready bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	l := m.byAccount[p.AccountID]
	switch {
	case l == nil:
		return errNotInLobby
	case l.public:
		return errorf(proto.StatusBadRequest, "Ready is for private rooms.")
	}
	l.member(p.AccountID).ready = ready
	m.broadcast(l, 0)
	return nil
}

// Vote records the caller's track vote ("" = Random); the voting ends once every member voted.
func (m *Manager) Vote(p Player, track string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	l := m.byAccount[p.AccountID]
	switch {
	case l == nil:
		return errNotInLobby
	case l.phase != proto.PhaseVoting:
		return errorf(proto.StatusBadRequest, "Voting is not open.")
	case track != "" && !m.isContent(l.settings.Packs, p.AccountID, l.trackKind(), track):
		return errorf(proto.StatusBadRequest, "%s is not a track of this lobby.", track)
	}
	l.votes[p.AccountID] = track
	if m.allVoted(l) {
		m.endVoting(l)
	}
	m.broadcast(l, 0)
	return nil
}

// StartRace starts a race on request. The lone member of a waiting public lobby without AI gets a solo race against
// 6 AI drivers on track ("" = a random lobby track of the right kind), outside the set. A private room's owner, with
// company or alone, starts the next race of the set: on track when the owner picks ("" draws a random one), by
// opening the voting in vote rooms, the cup's next track (which the owner's client sends) or the custom list's
// next track ("" or that id) in championships.
func (m *Manager) StartRace(p Player, track string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	l := m.byAccount[p.AccountID]
	switch {
	case l == nil:
		return errNotInLobby
	case l.public && (len(l.members) > 1 || categories[l.category].ai):
		return errorf(proto.StatusForbidden, "Public lobbies start races by voting.")
	case !l.public && l.owner != p.AccountID:
		return errorf(proto.StatusForbidden, "Only the room owner can start races.")
	case l.phase != proto.PhaseWaiting:
		return errorf(proto.StatusBadRequest, "A race can only start while the lobby waits.")
	}
	solo := l.public
	if !slices.ContainsFunc(l.members, (*member).hasLoadout) {
		return errNoLoadouts
	}
	s := l.settings
	if !solo && s.TrackPick == proto.PickVote {
		if track != "" {
			return errorf(proto.StatusBadRequest, "This room votes on tracks.")
		}
		m.startVoting(l)
		m.broadcast(l, 0)
		return nil
	}
	switch {
	case !solo && s.Mode == proto.RoomChampionship && len(s.TrackIDs) > 0:
		next := s.TrackIDs[min(int(l.setRace), len(s.TrackIDs)-1)]
		switch {
		case track == "":
			track = next
		case !strings.EqualFold(track, next):
			return errorf(proto.StatusBadRequest, "The next track of this championship is %s.", next)
		}
	case track == "" && !solo && s.Mode == proto.RoomChampionship:
		return errorf(proto.StatusBadRequest, "Choose the next track of the cup.")
	case track == "":
		if track = m.randomTrack(l); track == "" {
			return errorf(proto.StatusBadRequest, "The lobby's packs have no track all racers know.")
		}
	case !m.isContent(s.Packs, p.AccountID, l.trackKind(), track):
		return errorf(proto.StatusBadRequest, "%s is not a track of this lobby.", track)
	}
	if err := m.startRace(l, track, solo); err != nil {
		m.log.Error("starting race", "lobby", l.id, "err", err)
		return errorf(proto.StatusServerError, "The race could not start.")
	}
	m.broadcast(l, 0)
	return nil
}

// EndRaceEarly ends the race of its only human racer (someone may be waiting to race them); every kart still
// driving is DNF.
func (m *Manager) EndRaceEarly(p Player) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	l := m.byAccount[p.AccountID]
	if l == nil {
		return errNotInLobby
	}
	if mem := l.member(p.AccountID); l.raceID == 0 || l.humans != 1 || !mem.inRace {
		return errorf(proto.StatusBadRequest, "You are not racing alone.")
	}
	m.cfg.Races.EndEarly(l.raceID)
	return nil
}

// Kick removes a member and bans them from that lobby for KickBan. Moderators kick anyone anywhere (also out of a
// running race, audit-logged); room owners kick within their room.
func (m *Manager) Kick(ctx context.Context, p Player, target uint64, reason string) error {
	if target == p.AccountID {
		return errorf(proto.StatusBadRequest, "You cannot kick yourself.")
	}
	m.mu.Lock()
	l := m.byAccount[target]
	if l == nil {
		m.mu.Unlock()
		return errorf(proto.StatusNotFound, "This player is not in a lobby.")
	}
	if !p.Moderator {
		defer m.mu.Unlock()
		if l.public || m.byAccount[p.AccountID] != l || l.owner != p.AccountID {
			return errorf(proto.StatusForbidden, "Only the room owner or a moderator can kick players.")
		}
		m.ban(l, target)
		m.remove(l, target, proto.LeftOwnerKick, reason, race.OutcomeLeft)
		return nil
	}
	lobbyID, username := l.id, l.member(target).Username
	m.mu.Unlock()

	// The audit row comes first: a moderator kick that cannot be logged does not happen.
	t := target
	act := store.Action{Actor: &p.AccountID, Action: store.ActionLobbyKick, TargetAccount: &t,
		TargetRef: truncate(fmt.Sprintf("%s in lobby %d", username, lobbyID), 255), Reason: truncate(reason, 255),
		CreatedAt: store.Now()}
	if err := m.cfg.Audit.RecordAction(ctx, act); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if l = m.byAccount[target]; l != nil {
		m.ban(l, target)
		m.remove(l, target, proto.LeftModKick, reason, race.OutcomeModKick)
	}
	m.log.Info("moderator kicked a player", "moderator", p.AccountID, "target", target, "lobby", lobbyID)
	return nil
}

func (m *Manager) ban(l *lobby, accountID uint64) {
	l.bans[accountID] = m.clock.Now().Add(m.timing.KickBan)
}

// ---- pings ----

func (m *Manager) schedulePings() {
	m.pingTimer = m.clock.AfterFunc(m.timing.Ping, func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.stopped {
			return
		}
		m.refreshPings()
		m.schedulePings()
	})
}

// refreshPings takes the latest measured round trip times; members keep their last value between races.
func (m *Manager) refreshPings() {
	for _, l := range m.lobbies {
		changed := false
		for _, mem := range l.members {
			if ms, ok := m.rtt.RTT(mem.AccountID); ok && ms != mem.ping {
				mem.ping, changed = ms, true
			}
		}
		if changed {
			m.broadcast(l, 0)
		}
	}
}

// truncate cuts s to at most n bytes without splitting a UTF-8 sequence.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && s[n]&0xc0 == 0x80 {
		n--
	}
	return s[:n]
}
