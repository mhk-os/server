package lobby

import "github.com/mhk-os/server/internal/proto"

// Public lobby rules (docs/protocol.md, "Public lobby categories").
const (
	publicMaxPlayers  = 10
	publicEngineClass = 2 // 150cc
	publicSetSize     = 4
	soloAICount       = 6 // AI drivers of a solo race (the lone member of a public lobby without AI)
	minCategory       = 1
	maxCategory       = 20
	aiCategories      = 10 // categories 11–20 are the AI versions of 1–10
)

type category struct {
	name      string
	mode      uint8
	packs     []string
	anticheat bool
	ai        bool // AI karts fill every slot no human takes
}

var (
	classicPacks = []string{"mhk2", "mhk3", "mhke", "mhkt"}
	tincanPacks  = []string{"mhk2", "mhk3", "mhke", "mhkt", "tincan"}
	mhk2Packs    = []string{"mhk2"}
)

// categories is indexed by the category number; index 0 is unused. Names fit RoomSettings.name (32 bytes).
var categories = [maxCategory + 1]category{
	1:  {"Races", proto.RoomRaceSet, classicPacks, true, false},
	2:  {"Races, no anticheat", proto.RoomRaceSet, classicPacks, false, false},
	3:  {"Races + Tin Can", proto.RoomRaceSet, tincanPacks, true, false},
	4:  {"Races + Tin Can, no anticheat", proto.RoomRaceSet, tincanPacks, false, false},
	5:  {"Battles", proto.RoomBattleSet, classicPacks, true, false},
	6:  {"Battles, no anticheat", proto.RoomBattleSet, classicPacks, false, false},
	7:  {"Battles + Tin Can", proto.RoomBattleSet, tincanPacks, true, false},
	8:  {"Battles + Tin Can, no anticheat", proto.RoomBattleSet, tincanPacks, false, false},
	9:  {"MHK2 races", proto.RoomRaceSet, mhk2Packs, true, false},
	10: {"MHK2 races, no anticheat", proto.RoomRaceSet, mhk2Packs, false, false},
	11: {"Races + AI", proto.RoomRaceSet, classicPacks, true, true},
	12: {"Races, no anticheat + AI", proto.RoomRaceSet, classicPacks, false, true},
	13: {"Races + Tin Can + AI", proto.RoomRaceSet, tincanPacks, true, true},
	14: {"Races + Tin Can, no AC + AI", proto.RoomRaceSet, tincanPacks, false, true},
	15: {"Battles + AI", proto.RoomBattleSet, classicPacks, true, true},
	16: {"Battles, no anticheat + AI", proto.RoomBattleSet, classicPacks, false, true},
	17: {"Battles + Tin Can + AI", proto.RoomBattleSet, tincanPacks, true, true},
	18: {"Battles + Tin Can, no AC + AI", proto.RoomBattleSet, tincanPacks, false, true},
	19: {"MHK2 races + AI", proto.RoomRaceSet, mhk2Packs, true, true},
	20: {"MHK2 races, no anticheat + AI", proto.RoomRaceSet, mhk2Packs, false, true},
}

// publicSettings are the effective settings of a category's instances: 150cc, track laps, items with default
// odds, voting, sets of four races; AI categories fill every free slot with AI karts.
func publicSettings(cat uint8) proto.RoomSettings {
	c := categories[cat]
	s := proto.RoomSettings{
		Name:        c.name,
		Mode:        c.mode,
		Packs:       append([]string(nil), c.packs...),
		Anticheat:   c.anticheat,
		EngineClass: publicEngineClass,
		Items:       true,
		MaxPlayers:  publicMaxPlayers,
		TrackPick:   proto.PickVote,
		RacesPerSet: publicSetSize,
	}
	if c.ai {
		s.MaxAI = publicMaxPlayers - 1
	}
	return s
}
