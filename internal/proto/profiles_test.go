package proto

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
	"time"

	"github.com/mhk-os/server/internal/wire"
)

func samplePlayer(id uint64) PlayerSummary {
	return PlayerSummary{AccountID: id, Username: "Hans_Huhn", Country: "DE", Role: RoleModerator, Created: when,
		LastActive: when.Add(time.Hour), Online: true, Score: 1234, Rank: 7}
}

func profileMessages() []Message {
	return []Message{
		&SetProfileListed{Listed: true},
		&ListPlayers{Offset: 100, Limit: 50, Search: "huhn", Sort: PlayerSortName},
		new(samplePlayer(3)),
		&PlayerSummary{AccountID: 4, Username: "Neu", Created: when},
		&PlayerPage{Total: 2, Players: []PlayerSummary{samplePlayer(1), samplePlayer(2)}},
		&GetProfiles{AccountIDs: []uint64{1, 2, 1 << 60}},
		&PlayerSummaries{Players: []PlayerSummary{samplePlayer(5)}},
		&PlayerProfile{Player: samplePlayer(6), Listed: true, Races: 40, Wins: 3, Podiums: 9, TTRuns: 12, TTBoards: 4,
			TTBestRank: 2, Disabled: true, DisabledReason: "cheating", CheatKicks: 1, ContentStrikes: 2, HiddenRuns: 3,
			FlaggedRuns: 4},
		&ScoreLeaderboard{Offset: 10, Limit: 100, Search: "x"},
		&ScoreRanking{Total: 1, Players: []PlayerSummary{samplePlayer(7)}, Mine: new(samplePlayer(8))},
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
	b[len(b)-1] = 1 // hasMine without a PlayerSummary behind it
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
