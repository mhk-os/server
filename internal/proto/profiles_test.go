package proto

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/mhk-os/server/internal/wire"
)

func samplePlayer(id uint64) PlayerSummary {
	return PlayerSummary{AccountID: id, Username: "Hans_Huhn", Country: "DE", Role: RoleModerator, Created: when,
		LastActive: when.Add(time.Hour), Online: true, Score: 1234, Rank: 7}
}

// playedPlayer is samplePlayer with a playtime, which only messages carrying summaries encode.
func playedPlayer(id uint64) PlayerSummary {
	p := samplePlayer(id)
	p.Playtime = 45_240 + id
	return p
}

// optionalTail is the byte length of the playtimes list ending m, which older servers' bodies lack (0 = none).
func optionalTail(m Message) int {
	var n int
	switch m := m.(type) {
	case *PlayerPage:
		n = len(m.Players)
	case *PlayerSummaries:
		n = len(m.Players)
	case *PlayerProfile:
		n = 1
	case *ScoreRanking:
		n = len(m.Players)
		if m.Mine != nil {
			n++
		}
	default:
		return 0
	}
	return 1 + 8*n
}

func profileMessages() []Message {
	return []Message{
		&SetProfileListed{Listed: true},
		&ListPlayers{Offset: 100, Limit: 50, Search: "huhn", Sort: PlayerSortName},
		new(samplePlayer(3)),
		&PlayerSummary{AccountID: 4, Username: "Neu", Created: when},
		&PlayerPage{Total: 2, Players: []PlayerSummary{playedPlayer(1), samplePlayer(2)}},
		&GetProfiles{AccountIDs: []uint64{1, 2, 1 << 60}},
		&PlayerSummaries{Players: []PlayerSummary{playedPlayer(5)}},
		&PlayerProfile{Player: playedPlayer(6), Listed: true, Races: 40, Wins: 3, Podiums: 9, TTRuns: 12, TTBoards: 4,
			TTBestRank: 2, Disabled: true, DisabledReason: "cheating", CheatKicks: 1, ContentStrikes: 2, HiddenRuns: 3,
			FlaggedRuns: 4},
		&ScoreLeaderboard{Offset: 10, Limit: 100, Search: "x"},
		&ScoreRanking{Total: 1, Players: []PlayerSummary{playedPlayer(7)}, Mine: new(playedPlayer(8))},
		&ScoreRanking{Players: []PlayerSummary{}},
		&PlayerRuns{AccountID: 9, Offset: 20, Limit: 10},
		&ModListHidden{BoardID: 3, Offset: 0, Limit: 100},
	}
}

// TestPlayerSummaryBytes pins the PlayerSummary layout so the C# codec can assert the same bytes.
func TestPlayerSummaryBytes(t *testing.T) {
	p := PlayerSummary{AccountID: 1, Username: "ab", Country: "DE", Role: RoleModerator, Created: time.UnixMilli(2),
		Online: true, Score: 3, Rank: 4}
	le := binary.LittleEndian
	var want []byte
	want = le.AppendUint64(want, 1)
	want = append(want, 2, 'a', 'b', 2, 'D', 'E', 1)
	want = le.AppendUint64(want, 2)
	want = le.AppendUint64(want, 0) // lastActive: never
	want = append(want, 1)
	want = le.AppendUint32(want, 3)
	want = le.AppendUint32(want, 4)
	if got := Encode(&p); !bytes.Equal(got, want) {
		t.Fatalf("got  % x\nwant % x", got, want)
	}
	if len(want) != playerSummaryMinSize+2+2 {
		t.Fatalf("playerSummaryMinSize out of date: %d", len(want))
	}
}

// TestPlaytimesBytes pins the playtimes tail (players, then mine) so the C# codec can assert the same bytes.
func TestPlaytimesBytes(t *testing.T) {
	p := PlayerSummary{AccountID: 1, Username: "ab", Country: "DE", Role: RoleModerator, Created: time.UnixMilli(2),
		Online: true, Score: 3, Rank: 4, Playtime: 3600}
	const summary = "0100000000000000" + "026162" + "024445" + "01" + "0200000000000000" + "0000000000000000" + "01" +
		"03000000" + "04000000"
	page := "01000000" + "01" + summary + "01" + "100e000000000000"
	if got := hex.EncodeToString(Encode(&PlayerPage{Total: 1, Players: []PlayerSummary{p}})); got != page {
		t.Fatalf("page\ngot  %s\nwant %s", got, page)
	}
	mine := p
	mine.Playtime = 5
	ranking := "01000000" + "01" + summary + "01" + summary + "02" + "100e000000000000" + "0500000000000000"
	if got := hex.EncodeToString(Encode(&ScoreRanking{Total: 1, Players: []PlayerSummary{p}, Mine: &mine})); got != ranking {
		t.Fatalf("ranking\ngot  %s\nwant %s", got, ranking)
	}
}

// TestPlaytimesOptional reads bodies of servers without playtimes and rejects a playtimes list of the wrong length.
func TestPlaytimesOptional(t *testing.T) {
	cases := []struct {
		m     Message
		count int
		check func(Message) []uint64
	}{
		{&PlayerPage{Total: 2, Players: []PlayerSummary{playedPlayer(1), playedPlayer(2)}}, 2, func(m Message) []uint64 {
			p := m.(*PlayerPage)
			return []uint64{p.Players[0].Playtime, p.Players[1].Playtime}
		}},
		{&PlayerSummaries{Players: []PlayerSummary{playedPlayer(3)}}, 1, func(m Message) []uint64 {
			return []uint64{m.(*PlayerSummaries).Players[0].Playtime}
		}},
		{&PlayerProfile{Player: playedPlayer(4)}, 1, func(m Message) []uint64 {
			return []uint64{m.(*PlayerProfile).Player.Playtime}
		}},
		{&ScoreRanking{Players: []PlayerSummary{playedPlayer(5)}, Mine: new(playedPlayer(6))}, 2,
			func(m Message) []uint64 {
				r := m.(*ScoreRanking)
				return []uint64{r.Players[0].Playtime, r.Mine.Playtime}
			}},
	}
	for _, c := range cases {
		full := Encode(c.m)
		old := full[:len(full)-1-8*c.count]
		got := reflect.New(reflect.TypeOf(c.m).Elem()).Interface().(Message)
		if err := got.Decode(wire.NewReader(old)); err != nil {
			t.Fatalf("%T without playtimes: %v", c.m, err)
		}
		for i, v := range c.check(got) {
			if v != 0 {
				t.Errorf("%T without playtimes: playtime %d = %d", c.m, i, v)
			}
		}
		short := append(append([]byte{}, old...), byte(c.count-1))
		for range c.count - 1 {
			short = binary.LittleEndian.AppendUint64(short, 1)
		}
		if err := got.Decode(wire.NewReader(short)); err == nil {
			t.Errorf("%T with %d playtimes for %d players accepted", c.m, c.count-1, c.count)
		}
	}
}

func TestProfileLimits(t *testing.T) {
	decodeFails := func(name string, m Message, b []byte) {
		t.Helper()
		if err := m.Decode(wire.NewReader(b)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	decodeFails("101 profile ids", &GetProfiles{}, Encode(&GetProfiles{AccountIDs: make([]uint64, MaxProfileIDs+1)}))
	decodeFails("21-byte search", &ListPlayers{}, Encode(&ListPlayers{Search: strings.Repeat("x", MaxAccountName+1)}))
	decodeFails("21-byte score search", &ScoreLeaderboard{},
		Encode(&ScoreLeaderboard{Search: strings.Repeat("x", MaxAccountName+1)}))
	decodeFails("101 players", &PlayerPage{}, Encode(&PlayerPage{Players: make([]PlayerSummary, MaxPage+1)}))
	decodeFails("257-byte disabled reason", &PlayerProfile{},
		Encode(&PlayerProfile{DisabledReason: strings.Repeat("x", MaxModReason+1)}))

	b := Encode(&ScoreRanking{Players: []PlayerSummary{}})
	b = b[:len(b)-1] // without playtimes (an older server)
	b[len(b)-1] = 1  // hasMine without a PlayerSummary behind it
	decodeFails("hasMine without mine", &ScoreRanking{}, b)
}

func TestProfileRanges(t *testing.T) {
	for typ := TypeSetProfileListed; typ <= TypeScoreLeaderboard; typ++ {
		if !RangeAccounts.Contains(typ) {
			t.Errorf("%d not in accounts range", typ)
		}
	}
	if !RangeModeration.Contains(TypeModListHidden) || !RangeTimeTrials.Contains(TypePlayerRuns) {
		t.Error("ModListHidden or PlayerRuns outside its range")
	}
}
