package proto

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/mhk-os/server/internal/wire"
)

func sampleSettings() RoomSettings {
	return RoomSettings{
		Name: "Hühner-Cup", Mode: RoomChampionship, Packs: []string{"mhk2", "tincan"}, Anticheat: true,
		EngineClass: 1, Laps: 5, Items: true, ItemOdds: []ItemOdds{{"mhk2.rocket", 25}, {"mhk2.oil", 75}},
		MaxAI: 7, MaxPlayers: 8, TrackPick: PickOwner, RacesPerSet: 3, CupID: "",
		TrackIDs: []string{"mhk2.rome", "mhk2.alps", "mhk2.rome"},
	}
}

func sampleRow(slot uint8) RaceResultRow {
	r := RaceResultRow{Slot: slot, AccountID: 10 + uint64(slot), Username: "Kurt", Position: slot + 1,
		FinishMs: 95_000, Points: 12, SetPoints: 27, Outcome: 0, CharacterID: "mhk2.char.kurt"}
	if slot == 3 {
		r.AccountID, r.Username, r.AI = 0, "", true
	}
	return r
}

func lobbyMessages() []Message {
	summary := LobbySummary{LobbyID: 4, Kind: LobbyPublic, Category: 3, Instance: 2, Name: "Races #2",
		Mode: RoomRaceSet, Anticheat: true, Packs: []string{"mhk2", "mhk3"}, Players: 3, MaxPlayers: 10,
		Phase: PhaseVoting, ContentVersion: 7, EngineClass: 2, SetRace: 3, SetSize: 4, MaxAI: 9}
	member := LobbyMember{AccountID: 1, Username: "Hans", Country: "DE", CharacterID: "mhk2.char.huhn",
		KartID: "mhk2.kart.speedy", Ready: true, Owner: true, Moderator: true, InRace: true, PingMs: 48,
		SetPoints: 37, Joined: when, Platform: PlatformMacOS}
	return []Message{
		&PackRef{PackID: "mhk2", Commit: "v1.2", ManifestHash: "00112233445566778899aabbccddeeff"},
		&ContentHello{ContentVersion: 7, Packs: []PackRef{{"game", "abc", "ff"}, {"mhk2", "", "00"}}},
		&ContentHello{ContentVersion: -1, Packs: []PackRef{}},
		&ContentHelloResult{Missing: []string{"aa", "bb"}, Strikes: []ContentStrike{{"mhk3", "differs"}}},
		&ContentHelloResult{Missing: []string{}, Strikes: []ContentStrike{}},
		&PackManifest{PackID: "mhk2", Commit: "c", ContentVersion: 7, ManifestHash: "h",
			Items: []ManifestItem{{ContentTrack, "mhk2.rome", "f1"}, {ContentArena, "mhk2.arena", ""}}},
		new(sampleSettings()),
		&RoomSettings{Packs: []string{}, MaxPlayers: 2, RacesPerSet: 1, TrackIDs: []string{}},
		&summary,
		&LobbyList{Lobbies: []LobbySummary{summary, summary}},
		&member,
		new(sampleRow(2)),
		new(sampleRow(3)),
		&LobbyState{Summary: summary, Code: "ABC234", Settings: sampleSettings(), Members: []LobbyMember{member},
			PhaseEnds: when, SetRace: 2, SetSize: 4, Votes: []LobbyVote{{1, "mhk2.rome"}, {2, ""}},
			TrackID: "mhk2.rome", RaceID: 99, LastResults: []RaceResultRow{sampleRow(0), sampleRow(3)},
			AIDrivers: []AIDriver{{"mhk2.char.kurt", "mhk2.kart.speedy", 15}, {"game.huhn", "game.kart", 0}}},
		&LobbyState{Summary: summary, Settings: RoomSettings{Packs: []string{}, TrackIDs: []string{}},
			Members: []LobbyMember{}, Votes: []LobbyVote{}, LastResults: []RaceResultRow{}, AIDrivers: []AIDriver{}},
		&JoinLobby{LobbyID: 5, Code: "XYZ234"},
		&QuickJoin{Category: 20},
		&SetLoadout{CharacterID: "mhk2.char.huhn", KartID: ""},
		&SetReady{Ready: true},
		&TrackChoice{TrackID: "mhk3.arena"},
		&KickMember{AccountID: 3, Reason: "rude"},
		&LobbyLeft{Reason: LeftModKick, Message: "cheating"},
		&SoloRaceJoinWaiting{Username: "Kurt"},
		&RaceResults{RaceID: 8, Rows: []RaceResultRow{sampleRow(0), sampleRow(1)}, SetDone: true},
	}
}

func TestLobbyRoundTrips(t *testing.T) {
	for _, m := range lobbyMessages() {
		b := Encode(m)
		got := reflect.New(reflect.TypeOf(m).Elem()).Interface().(Message)
		if err := got.Decode(wire.NewReader(b)); err != nil {
			t.Fatalf("%T: %v", m, err)
		}
		if !reflect.DeepEqual(got, m) {
			t.Fatalf("%T: got %+v, want %+v", m, got, m)
		}
		for n := range len(b) {
			if err := got.Decode(wire.NewReader(b[:n])); err == nil {
				t.Fatalf("%T: prefix of %d/%d bytes decoded", m, n, len(b))
			}
		}
		if err := got.Decode(wire.NewReader(append(b, 9, 9))); err != nil {
			t.Fatalf("%T with trailing bytes: %v", m, err)
		}
	}
}

// TestLobbyMemberBytes and TestRaceResultRowBytes pin layouts the C# codec must reproduce; they also keep the
// minimum item sizes used for count checks honest.
func TestLobbyMemberBytes(t *testing.T) {
	m := LobbyMember{AccountID: 1, Username: "a", Country: "DE", CharacterID: "c", KartID: "k", Ready: true,
		Owner: false, Moderator: true, InRace: false, PingMs: 2, SetPoints: -3, Joined: time.UnixMilli(4).UTC(),
		Platform: PlatformAndroid}
	le := binary.LittleEndian
	want := le.AppendUint64(nil, 1)
	want = append(want, 1, 'a', 2, 'D', 'E', 1, 'c', 1, 'k', 1, 0, 1, 0)
	want = le.AppendUint16(want, 2)
	want = le.AppendUint32(want, uint32(0xfffffffd))
	want = le.AppendUint64(want, 4)
	want = append(want, 4) // platform
	if got := Encode(&m); !bytes.Equal(got, want) {
		t.Fatalf("got  % x\nwant % x", got, want)
	}
	if lobbyMemberMinSize != len(want)-1-2-1-1 {
		t.Fatalf("lobbyMemberMinSize out of date")
	}
}

// pinHex checks that m encodes to the hex vector want (the C# codec tests assert the same strings) and decodes back.
func pinHex(t *testing.T, m Message, want string) []byte {
	t.Helper()
	got := Encode(m)
	if hex.EncodeToString(got) != want {
		t.Fatalf("%T encoded\n%x\nwant\n%s", m, got, want)
	}
	back := reflect.New(reflect.TypeOf(m).Elem()).Interface().(Message)
	if err := back.Decode(wire.NewReader(got)); err != nil || !bytes.Equal(Encode(back), got) {
		t.Fatalf("%T decoded %+v (%v), want %+v", m, back, err, m)
	}
	return got
}

func TestLobbySummaryBytes(t *testing.T) {
	s := LobbySummary{LobbyID: 1, Kind: LobbyPrivate, Category: 0, Instance: 2, Name: "n", Mode: RoomBattleSet,
		Anticheat: true, Packs: []string{"p"}, Players: 3, MaxPlayers: 4, Phase: PhaseResults, ContentVersion: -5,
		EngineClass: 1, SetRace: 2, SetSize: 16, MaxAI: 3}
	want := strings.Join([]string{
		"0100000000000000", "01", "00", "0200", // lobbyId, kind, category, instance
		"016e", "01", "01", "010170", // name, mode, anticheat, packs
		"03", "04", "05", "fbffffff", // players, maxPlayers, phase, contentVersion
		"01", "02", "10", "03", // engineClass, setRace, setSize, maxAi
	}, "")
	b := pinHex(t, &s, want)
	if lobbySummaryMinSize != len(b)-1-2 { // name "n" (+1), packs ["p"] (+2) beyond the empty minimum
		t.Fatalf("lobbySummaryMinSize out of date")
	}
}

func TestRaceSlotBytes(t *testing.T) {
	human := RaceSlot{Slot: 3, AccountID: 2, Username: "u", Country: "SK", CharacterID: "c", KartID: "k",
		Platform: PlatformIOS}
	b := pinHex(t, &human, "03"+"0200000000000000"+"0175"+"02534b"+"0163"+"016b"+"05"+"00")
	if raceSlotMinSize != len(b)-1-2-1-1 {
		t.Fatalf("raceSlotMinSize out of date")
	}
	ai := RaceSlot{Slot: 0, CharacterID: "c", KartID: "k", AI: true}
	pinHex(t, &ai, "00"+"0000000000000000"+"00"+"00"+"0163"+"016b"+"00"+"01")
}

func TestPlatformOf(t *testing.T) {
	for name, want := range map[string]uint8{
		"WindowsPlayer": PlatformWindows, "WindowsEditor": PlatformWindows, "OSXPlayer": PlatformMacOS,
		"OSXEditor": PlatformMacOS, "LinuxPlayer": PlatformLinux, "LinuxEditor": PlatformLinux,
		"Android": PlatformAndroid, "IPhonePlayer": PlatformIOS, "": PlatformUnknown, "mhk-bot": PlatformUnknown,
		"windowsplayer": PlatformUnknown, "WebGLPlayer": PlatformUnknown,
	} {
		if got := PlatformOf(name); got != want {
			t.Errorf("PlatformOf(%q) = %d, want %d", name, got, want)
		}
	}
}

func TestRaceResultRowBytes(t *testing.T) {
	r := RaceResultRow{Slot: 1, AccountID: 0, Username: "", Position: 3, FinishMs: 4, Points: 5, SetPoints: 6,
		Outcome: 1, AI: true, CharacterID: "c"}
	want := strings.Join([]string{
		"01", "0000000000000000", "00", "03", // slot, accountId, username, position
		"04000000", "05000000", "06000000", // finishMs, points, setPoints
		"01", "01", "0163", // outcome, ai, characterId
	}, "")
	b := pinHex(t, &r, want)
	if raceResultRowMinSize != len(b)-1 {
		t.Fatalf("raceResultRowMinSize out of date")
	}
}

func TestRoomSettingsBytes(t *testing.T) {
	s := RoomSettings{Name: "n", Mode: RoomChampionship, Packs: []string{"p"}, Anticheat: true, EngineClass: 2,
		Laps: 3, Items: false, ItemOdds: []ItemOdds{{"x", 1.5}}, MaxAI: 9, MaxPlayers: 10, TrackPick: PickOwner,
		RacesPerSet: 2, CupID: "", TrackIDs: []string{"a.b", "a.c"}}
	want := strings.Join([]string{
		"016e", "02", "010170", "01", "02", "03", "00", // name, mode, packs, anticheat, engineClass, laps, items
		"01" + "0178" + "0000c03f",   // itemOdds
		"09", "0a", "00", "02", "00", // maxAi, maxPlayers, trackPick, racesPerSet, cupId
		"02" + "03612e62" + "03612e63", // trackIds
	}, "")
	pinHex(t, &s, want)
}

func TestLobbyStateBytes(t *testing.T) {
	s := LobbyState{Settings: RoomSettings{Packs: []string{}, TrackIDs: []string{}}, Members: []LobbyMember{},
		Votes: []LobbyVote{}, LastResults: []RaceResultRow{},
		AIDrivers: []AIDriver{{CharacterID: "c", KartID: "k", SetPoints: -1}}}
	z := func(n int) string { return strings.Repeat("00", n) }
	want := strings.Join([]string{
		z(lobbySummaryMinSize), "00", z(14), "00", // summary, code, settings, members
		z(8), "00", "00", "00", "00", z(8), "00", // phaseEnds, setRace, setSize, votes, trackId, raceId, lastResults
		"01" + "0163" + "016b" + "ffffffff", // aiDrivers
	}, "")
	pinHex(t, &s, want)
}

func TestLobbyLimits(t *testing.T) {
	decodeFails := func(name string, m Message, b []byte) {
		t.Helper()
		if err := m.Decode(wire.NewReader(b)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	decodeFails("65 announced packs", &ContentHello{},
		Encode(&ContentHello{Packs: make([]PackRef, MaxAnnouncedPacks+1)}))
	decodeFails("65-byte pack id", &ContentHello{},
		Encode(&ContentHello{Packs: []PackRef{{PackID: strings.Repeat("p", MaxPackID+1)}}}))
	decodeFails("41-byte commit", &PackRef{}, Encode(&PackRef{Commit: strings.Repeat("c", MaxPackCommit+1)}))
	decodeFails("33-byte hash", &PackRef{}, Encode(&PackRef{ManifestHash: strings.Repeat("h", 33)}))
	decodeFails("4097 manifest items", &PackManifest{},
		Encode(&PackManifest{Items: make([]ManifestItem, MaxManifestItems+1)}))
	decodeFails("33-byte fingerprint", &PackManifest{},
		Encode(&PackManifest{Items: []ManifestItem{{1, "a", strings.Repeat("f", 33)}}}))
	decodeFails("33-byte room name", &RoomSettings{}, Encode(&RoomSettings{Name: strings.Repeat("n", 33)}))
	decodeFails("33 room packs", &RoomSettings{}, Encode(&RoomSettings{Packs: make([]string, MaxRoomPacks+1)}))
	decodeFails("65 item odds", &RoomSettings{}, Encode(&RoomSettings{ItemOdds: make([]ItemOdds, MaxItemOdds+1)}))
	decodeFails("NaN odds", &RoomSettings{}, Encode(&RoomSettings{ItemOdds: []ItemOdds{{"x", float32(math.NaN())}}}))
	decodeFails("101 percent", &RoomSettings{}, Encode(&RoomSettings{ItemOdds: []ItemOdds{{"x", 101}}}))
	decodeFails("51 members", &LobbyState{}, Encode(&LobbyState{Members: make([]LobbyMember, MaxLobbyMembers+1)}))
	decodeFails("51 votes", &LobbyState{}, Encode(&LobbyState{Votes: make([]LobbyVote, MaxLobbyMembers+1)}))
	decodeFails("51 last results", &LobbyState{},
		Encode(&LobbyState{LastResults: make([]RaceResultRow, MaxRaceRows+1)}))
	decodeFails("50 AI drivers", &LobbyState{}, Encode(&LobbyState{AIDrivers: make([]AIDriver, MaxAIDrivers+1)}))
	decodeFails("36 custom tracks", &RoomSettings{},
		Encode(&RoomSettings{TrackIDs: make([]string, MaxCustomTracks+1)}))
	decodeFails("97-byte custom track", &RoomSettings{},
		Encode(&RoomSettings{TrackIDs: []string{strings.Repeat("t", MaxTrackID+1)}}))
	decodeFails("9-byte code", &JoinLobby{}, Encode(&JoinLobby{Code: "ABCDEFGHJ"}))
	decodeFails("51 result rows", &RaceResults{}, Encode(&RaceResults{Rows: make([]RaceResultRow, MaxRaceRows+1)}))
	decodeFails("501 lobbies", &LobbyList{}, Encode(&LobbyList{Lobbies: make([]LobbySummary, MaxLobbies+1)}))
	decodeFails("257-byte kick reason", &KickMember{}, Encode(&KickMember{Reason: strings.Repeat("r", 257)}))
	decodeFails("21-byte username", &SoloRaceJoinWaiting{}, Encode(&SoloRaceJoinWaiting{Username: strings.Repeat("u", 21)}))
	decodeFails("bool 2", &SetReady{}, []byte{2})

	// A large count with a short body is rejected before allocating.
	w := wire.NewWriter(8)
	w.VarUint(MaxManifestItems)
	decodeFails("count beyond the body", &PackManifest{}, append([]byte{0, 0, 0, 0, 0, 0, 0}, w.Bytes()...))
}

func TestLobbyHostileBodies(t *testing.T) {
	seed := uint32(7)
	next := func() byte {
		seed = seed*1664525 + 1013904223
		return byte(seed >> 24)
	}
	for _, m := range lobbyMessages() {
		for range 300 {
			b := make([]byte, int(next())%96)
			for i := range b {
				b[i] = next()
			}
			m.Decode(wire.NewReader(b))
		}
	}
}

func TestLobbyRanges(t *testing.T) {
	for typ := TypeContentHello; typ <= TypeKickMember; typ++ {
		if !RangeLobbies.Contains(typ) {
			t.Errorf("%d not in lobby range", typ)
		}
	}
	if !RangeRaceControl.Contains(PushRaceResults) {
		t.Error("RaceResults outside the race-control range")
	}
}
