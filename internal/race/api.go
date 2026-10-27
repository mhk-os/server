// Package race runs multiplayer races (docs/protocol.md, "Multiplayer (phase 3)"): race setup, load agreement, GO,
// the UDP realtime relay, item arbitration, projectile consensus, hit arbitration, finish order and the anticheat
// reports. Lobbies (package lobby) decide who races where and own points and sets; they talk to races only through
// this file's API.
package race

import "github.com/mhk-os/server/internal/proto"

// Outcome is how a racer's race ended (RaceResultRow.outcome).
type Outcome uint8

const (
	OutcomeFinished  Outcome = 0
	OutcomeDNF       Outcome = 1
	OutcomeLeft      Outcome = 2
	OutcomeCheatKick Outcome = 3
	OutcomeModKick   Outcome = 4
	OutcomeDesync    Outcome = 5
)

// Mode of a race.
const (
	ModeRace   uint8 = 0
	ModeBattle uint8 = 1
)

// Racer is a human taking part, in grid order (slot = index in the slice given to Start).
type Racer struct {
	AccountID   uint64
	Username    string
	Country     string
	CharacterID string
	KartID      string
	Platform    uint8 // proto.Platform* of the racer's connection, shown in RaceSlot
}

// Spec is what the lobby decided for one race.
type Spec struct {
	LobbyID     uint64
	TrackID     string
	Mode        uint8
	EngineClass uint8
	Laps        uint8 // 0 = the track's default (the clients resolve it and report it in RaceLoaded)
	Items       bool
	ItemOdds    []proto.ItemOdds
	Packs       []string // the lobby's packs; specials of other packs (except "game") are dropped from RaceLoaded
	Anticheat   bool
	Solo        bool  // exactly one human: AI opponents simulated by that client, nothing arbitrated
	AICount     uint8 // AI opponents of a solo race
	SetRace     uint8
	SetSize     uint8
}

// Placement is one racer's result. Position is 1-based over every racer including AI rows of solo races (AI rows
// have AccountID 0 and AI true); Points are assigned by the lobby.
type Placement struct {
	Slot      uint8
	AccountID uint64
	Username  string
	Position  uint8
	FinishMs  int32 // 0 = did not finish
	Outcome   Outcome
	AI        bool
}

// Callbacks tell the lobby about a race's progress. They are called from the race's own goroutine; they must not
// block and must not call back into the Engine synchronously.
type Callbacks struct {
	// Going is called once when GO is scheduled (RaceGo sent): the lobby moves to Countdown/Racing.
	Going func(raceID uint64)
	// Finished is called once when the race ended, with every racer's placement in position order.
	Finished func(raceID uint64, placements []Placement)
}

// Catalog answers content questions about racers; the lobby's content catalog implements it.
type Catalog interface {
	// PackCommit is the commit of packID the account announced in ContentHello, "" when none.
	PackCommit(accountID uint64, packID string) string
	// InMajority reports whether the catalog-majority manifest of the account's version of packID lists (kind, id).
	InMajority(accountID uint64, packID string, kind uint8, id string) bool
	// Strike records a content strike against the account (stored, visible to moderators).
	Strike(accountID uint64, packID, reason string)
}

// Races is what lobbies use. The implementation (Engine) also registers the race notify handlers on the server and
// serves UDP; HelloOk announces its UDPPort.
type Races interface {
	// Start creates a race and pushes RaceSetup to every racer. racers are in grid order.
	Start(spec Spec, racers []Racer, cb Callbacks) (raceID uint64, err error)
	// Remove takes a racer out of a running race (disconnect, moderator kick); others get PlayerLeftRace.
	Remove(raceID, accountID uint64, outcome Outcome, message string)
	// EndEarly ends a race now (the solo racer chose to end its AI race); unfinished racers are DNF.
	EndEarly(raceID uint64)
	// UDPPort is the realtime port to announce, 0 when UDP is not serving.
	UDPPort() uint16
}
