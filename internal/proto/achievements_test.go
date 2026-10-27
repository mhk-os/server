package proto

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
	"time"

	"github.com/mhk-os/server/internal/wire"
)

func achievementMessages() []Message {
	return []Message{
		&DeleteAccount{Password: "pässwörd!", DeleteRuns: true},
		&SyncAchievements{
			Counters: []StatValue{{Stat: "races.finished", Value: 3}},
			Maxima:   []StatValue{{Stat: "use.mhk2.shrink.hits", Value: 5}},
			Sets:     []StatSet{{Stat: "tracks.finished", Items: []string{"mhk2.farm", "mhk3.city"}}},
			Unlocked: []Unlock{{ID: "game.first_race", At: when}},
		},
		&SyncAchievements{Counters: []StatValue{}, Maxima: []StatValue{}, Sets: []StatSet{}, Unlocked: []Unlock{}},
		&AchievementState{
			Counters: []StatTotal{{Stat: "distance.m", Value: 1 << 40}},
			Maxima:   []StatValue{},
			Sets:     []StatSet{{Stat: "cups.perfect", Items: []string{}}},
			Unlocked: []Unlock{{ID: "a", At: when}, {ID: "b", At: when.Add(time.Hour)}},
		},
		&AchievementSharesRequest{IDs: []string{"game.first_race", "x"}},
		&AchievementSharesRequest{IDs: []string{}},
		&AchievementShares{Players: 1234, Shares: []AchievementShare{{ID: "game.first_race", Unlocked: 99}}},
		&RunHiddenInfo{HiddenAt: when, Moderator: "Mod", Reason: "Deleted by the player", DeletedByOwner: true,
			AccountDisabled: true},
		&RunHiddenInfo{},
	}
}

// TestAchievementBytes pins SyncAchievements and RunHiddenInfo so the C# codec can assert the same bytes.
func TestAchievementBytes(t *testing.T) {
	le := binary.LittleEndian
	m := &SyncAchievements{
		Counters: []StatValue{{Stat: "c", Value: 1}},
		Maxima:   []StatValue{{Stat: "m", Value: 2}},
		Sets:     []StatSet{{Stat: "s", Items: []string{"i"}}},
		Unlocked: []Unlock{{ID: "u", At: time.UnixMilli(3)}},
	}
	want := []byte{1, 1, 'c'}
	want = le.AppendUint32(want, 1)
	want = append(want, 1, 1, 'm')
	want = le.AppendUint32(want, 2)
	want = append(want, 1, 1, 's', 1, 1, 'i', 1, 1, 'u')
	want = le.AppendUint64(want, 3)
	if got := Encode(m); !bytes.Equal(got, want) {
		t.Fatalf("SyncAchievements\ngot  % x\nwant % x", got, want)
	}

	h := &RunHiddenInfo{Moderator: "M", Reason: "r", DeletedByOwner: true}
	want = le.AppendUint64(nil, 0) // hiddenAt: not hidden
	want = append(want, 1, 'M', 1, 'r', 1, 0)
	if got := Encode(h); !bytes.Equal(got, want) {
		t.Fatalf("RunHiddenInfo\ngot  % x\nwant % x", got, want)
	}
}

func TestAchievementLimits(t *testing.T) {
	decodeFails := func(name string, m Message, b []byte) {
		t.Helper()
		if err := m.Decode(wire.NewReader(b)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	decodeFails("257 counters", &SyncAchievements{},
		Encode(&SyncAchievements{Counters: make([]StatValue, MaxSyncStats+1)}))
	decodeFails("65 sets", &SyncAchievements{}, Encode(&SyncAchievements{Sets: make([]StatSet, MaxSyncSets+1)}))
	decodeFails("2001 set items", &SyncAchievements{},
		Encode(&SyncAchievements{Sets: []StatSet{{Stat: "s", Items: make([]string, MaxSyncSetItems+1)}}}))
	decodeFails("2001 unlocks", &SyncAchievements{},
		Encode(&SyncAchievements{Unlocked: make([]Unlock, MaxSyncUnlocked+1)}))
	decodeFails("65-byte stat", &SyncAchievements{},
		Encode(&SyncAchievements{Maxima: []StatValue{{Stat: strings.Repeat("s", MaxStatID+1)}}}))
	decodeFails("97-byte achievement id", &AchievementSharesRequest{},
		Encode(&AchievementSharesRequest{IDs: []string{strings.Repeat("a", MaxAchievementID+1)}}))
	decodeFails("2001 share ids", &AchievementSharesRequest{},
		Encode(&AchievementSharesRequest{IDs: make([]string, MaxShareIDs+1)}))
	decodeFails("513-byte password", &DeleteAccount{},
		Encode(&DeleteAccount{Password: strings.Repeat("p", MaxPasswordField+1)}))
	decodeFails("21-byte moderator", &RunHiddenInfo{},
		Encode(&RunHiddenInfo{Moderator: strings.Repeat("m", MaxAccountName+1)}))
}

func TestValidID(t *testing.T) {
	for id, want := range map[string]bool{
		"mhk2.farm": true, "A-z_0.9": true, "": false, "a b": false, "a/b": false, "ä": false,
		strings.Repeat("x", 64): true, strings.Repeat("x", 65): false,
	} {
		if got := ValidID(id, 64); got != want {
			t.Errorf("ValidID(%q) = %v", id, got)
		}
	}
}

func TestAchievementRanges(t *testing.T) {
	for _, typ := range []Type{TypeDeleteAccount, TypeSyncAchievements, TypeAchievementShares} {
		if !RangeAccounts.Contains(typ) {
			t.Errorf("%d not in accounts range", typ)
		}
	}
	if !RangeTimeTrials.Contains(TypeRunHiddenReason) {
		t.Error("RunHiddenReason outside the time-trial range")
	}
}
