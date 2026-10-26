package lobby

import "github.com/mhk-os/server/internal/proto"

// Public lobby rules (docs/protocol.md, "Public lobby categories").
const (
	publicMaxPlayers  = 10
	publicEngineClass = 2 // 150cc
	publicSetSize     = 4
	publicAICount     = 6
	minCategory       = 1
	maxCategory       = 10
)

type category struct {
	name      string
	mode      uint8
	packs     []string
	anticheat bool
}

var (
	classicPacks = []string{"mhk2", "mhk3", "mhke", "mhkt"}
	tincanPacks  = []string{"mhk2", "mhk3", "mhke", "mhkt", "tincan"}
	mhk2Packs    = []string{"mhk2"}
)

// categories is indexed by the category number; index 0 is unused. Names fit RoomSettings.name (32 bytes).
var categories = [maxCategory + 1]category{
	1:  {"Races", proto.RoomRaceSet, classicPacks, true},
	2:  {"Races, no anticheat", proto.RoomRaceSet, classicPacks, false},
	3:  {"Races + Tin Can", proto.RoomRaceSet, tincanPacks, true},
	4:  {"Races + Tin Can, no anticheat", proto.RoomRaceSet, tincanPacks, false},
	5:  {"Battles", proto.RoomBattleSet, classicPacks, true},
	6:  {"Battles, no anticheat", proto.RoomBattleSet, classicPacks, false},
	7:  {"Battles + Tin Can", proto.RoomBattleSet, tincanPacks, true},
	8:  {"Battles + Tin Can, no anticheat", proto.RoomBattleSet, tincanPacks, false},
	9:  {"MHK2 races", proto.RoomRaceSet, mhk2Packs, true},
	10: {"MHK2 races, no anticheat", proto.RoomRaceSet, mhk2Packs, false},
}

// publicSettings are the effective settings of a category's instances: 150cc, track laps, items with default
// odds, voting, sets of four races, AI only for a lone player.
func publicSettings(cat uint8) proto.RoomSettings {
	c := categories[cat]
	return proto.RoomSettings{
		Name:        c.name,
		Mode:        c.mode,
		Packs:       append([]string(nil), c.packs...),
		Anticheat:   c.anticheat,
		EngineClass: publicEngineClass,
		Items:       true,
		AIWhenAlone: true,
		MaxPlayers:  publicMaxPlayers,
		TrackPick:   proto.PickVote,
		RacesPerSet: publicSetSize,
	}
}
