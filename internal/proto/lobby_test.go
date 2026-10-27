package proto

import (
	"bytes"
	"encoding/binary"
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
		AIWhenAlone: true, MaxPlayers: 8, TrackPick: PickVote, RacesPerSet: 4, CupID: "mhk2.cup.gold",
	}
}

func sampleRow(slot uint8) RaceResultRow {
	return RaceResultRow{Slot: slot, AccountID: 10 + uint64(slot), Username: "Kurt", Position: slot + 1,
		FinishMs: 95_000, Points: 12, SetPoints: 27, Outcome: 0, AI: slot == 3}
}

func lobbyMessages() []Message {
	summary := LobbySummary{LobbyID: 4, Kind: LobbyPublic, Category: 3, Instance: 2, Name: "Races #2",
		Mode: RoomRaceSet, Anticheat: true, Packs: []string{"mhk2", "mhk3"}, Players: 3, MaxPlayers: 10,
		Phase: PhaseVoting, ContentVersion: 7, EngineClass: 2, SetRace: 3, SetSize: 4}
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
		&ContentManifests{Manifests: []PackManifest{{PackID: "p", Items: []ManifestItem{}}}},
		new(sampleSettings()),
		&RoomSettings{Packs: []string{}, MaxPlayers: 2, RacesPerSet: 1},
		&summary,
		&LobbyList{Lobbies: []LobbySummary{summary, summary}},
		&member,
		new(sampleRow(2)),
		&LobbyState{Summary: summary, Code: "ABC234", Settings: sampleSettings(), Members: []LobbyMember{member},
			PhaseEnds: when, SetRace: 2, SetSize: 4, Votes: []LobbyVote{{1, "mhk2.rome"}, {2, ""}},
			TrackID: "mhk2.rome", RaceID: 99, LastResults: []RaceResultRow{sampleRow(0), sampleRow(3)}},
		&LobbyState{Summary: summary, Settings: RoomSettings{Packs: []string{}}, Members: []LobbyMember{},
			Votes: []LobbyVote{}, LastResults: []RaceResultRow{}},
		&JoinLobby{LobbyID: 5, Code: "XYZ234"},
		&QuickJoin{Category: 10},
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

func TestLobbySummaryBytes(t *testing.T) {
	s := LobbySummary{LobbyID: 1, Kind: LobbyPrivate, Category: 0, Instance: 2, Name: "n", Mode: RoomBattleSet,
		Anticheat: true, Packs: []string{"p"}, Players: 3, MaxPlayers: 4, Phase: PhaseResults, ContentVersion: -5,
		EngineClass: 1, SetRace: 2, SetSize: 16}
	le := binary.LittleEndian
	want := le.AppendUint64(nil, 1)
	want = append(want, 1, 0)
	want = le.AppendUint16(want, 2)
	want = append(want, 1, 'n', 1, 1, 1, 1, 'p', 3, 4, 5)
	want = le.AppendUint32(want, uint32(0xfffffffb))
	want = append(want, 1, 2, 16) // engineClass, setRace, setSize
	if got := Encode(&s); !bytes.Equal(got, want) {
		t.Fatalf("got  % x\nwant % x", got, want)
	}
	if lobbySummaryMinSize != len(want)-1-2 { // name "n" (+1), packs ["p"] (+2) beyond the empty minimum
		t.Fatalf("lobbySummaryMinSize out of date")
	}
}

func TestRaceSlotBytes(t *testing.T) {
	s := RaceSlot{Slot: 3, AccountID: 2, Username: "u", Country: "SK", CharacterID: "c", KartID: "k",
		Platform: PlatformIOS}
	want := []byte{3}
	want = binary.LittleEndian.AppendUint64(want, 2)
	want = append(want, 1, 'u', 2, 'S', 'K', 1, 'c', 1, 'k', 5)
	if got := Encode(&s); !bytes.Equal(got, want) {
		t.Fatalf("got  % x\nwant % x", got, want)
	}
	if raceSlotMinSize != len(want)-1-2-1-1 {
		t.Fatalf("raceSlotMinSize out of date")
	}
	var back RaceSlot
	if err := back.Decode(wire.NewReader(want)); err != nil || back != s {
		t.Fatalf("decoded %+v, %v", back, err)
	}
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
	r := RaceResultRow{Slot: 1, AccountID: 2, Username: "u", Position: 3, FinishMs: 4, Points: 5, SetPoints: 6,
		Outcome: 4, AI: true}
	le := binary.LittleEndian
	want := []byte{1}
	want = le.AppendUint64(want, 2)
	want = append(want, 1, 'u', 3)
	want = le.AppendUint32(want, 4)
	want = le.AppendUint32(want, 5)
	want = le.AppendUint32(want, 6)
	want = append(want, 4, 1)
	if got := Encode(&r); !bytes.Equal(got, want) {
		t.Fatalf("got  % x\nwant % x", got, want)
	}
	if raceResultRowMinSize != len(want)-1 {
		t.Fatalf("raceResultRowMinSize out of date")
	}
}

func TestRoomSettingsBytes(t *testing.T) {
	s := RoomSettings{Name: "n", Mode: 1, Packs: []string{"p"}, Anticheat: true, EngineClass: 2, Laps: 3,
		Items: false, ItemOdds: []ItemOdds{{"x", 1.5}}, AIWhenAlone: true, MaxPlayers: 10, TrackPick: 1,
		RacesPerSet: 4, CupID: "c"}
	want := []byte{1, 'n', 1, 1, 1, 'p', 1, 2, 3, 0, 1, 1, 'x', 0, 0, 0xc0, 0x3f, 1, 10, 1, 4, 1, 'c'}
	if got := Encode(&s); !bytes.Equal(got, want) {
		t.Fatalf("got  % x\nwant % x", got, want)
	}
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
	decodeFails("11 members", &LobbyState{}, Encode(&LobbyState{Members: make([]LobbyMember, MaxLobbyMembers+1)}))
	decodeFails("11 votes", &LobbyState{}, Encode(&LobbyState{Votes: make([]LobbyVote, MaxLobbyMembers+1)}))
	decodeFails("11 last results", &LobbyState{},
		Encode(&LobbyState{LastResults: make([]RaceResultRow, MaxLobbyMembers+1)}))
	decodeFails("9-byte code", &JoinLobby{}, Encode(&JoinLobby{Code: "ABCDEFGHJ"}))
	decodeFails("17 result rows", &RaceResults{}, Encode(&RaceResults{Rows: make([]RaceResultRow, MaxRaceRows+1)}))
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
