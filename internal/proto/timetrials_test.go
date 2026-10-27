package proto

import (
	"bytes"
	"encoding/binary"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/mhk-os/server/internal/wire"
)

func sampleRun(id uint64) RunEntry {
	return RunEntry{
		RunID: id, Rank: 3, AccountID: 9, Username: "Hans_Huhn", Country: "DE", FinishMs: 93_456, BestLapMs: 30_100,
		LapMs: []int32{31_000, 30_100, 32_356}, KartID: "mhk2.kart.speedy", CharacterID: "mhk2.char.moorhuhn",
		GhostID: "0123456789abcdef0123456789abcdef", Recorded: when, Uploaded: when.Add(time.Hour), Flags: 2,
		SetupMismatch: true, Hidden: true, FileSize: 67_361, BoardID: 4, TrackID: "mhk3.rome", EngineClass: 2,
	}
}

func timeTrialMessages() []Message {
	return []Message{
		&ListBoards{TrackID: "mhk3.rome", EngineClass: AnyEngineClass, ContentVersion: 7},
		&BoardInfo{BoardID: 1, TrackID: "mhk3.rome", EngineClass: 2, Laps: 3, ContentVersion: 7,
			TrackHash: "ffeeddccbbaa99887766554433221100", Players: 12, BestMs: 90_000, LastUpload: when},
		&BoardList{Boards: []BoardInfo{{BoardID: 1, TrackID: "a", LastUpload: when}, {BoardID: 2, TrackID: "b"}}},
		new(sampleRun(5)),
		&LeaderboardFilter{Username: "huhn", Country: "SK", Continent: "EU", KartID: "k", CharacterID: "c",
			IncludeHidden: true},
		&GetLeaderboard{BoardID: 3, Offset: 100, Limit: 50, Filter: LeaderboardFilter{Continent: "NA"}},
		&Leaderboard{Total: 2, Entries: []RunEntry{sampleRun(1), sampleRun(2)}, Mine: new(sampleRun(8))},
		&Leaderboard{Total: 0, Entries: []RunEntry{}},
		&UploadRun{File: []byte{0x1f, 0x8b, 8, 0}},
		&UploadResult{Run: sampleRun(6), PersonalBest: true, Duplicate: true},
		&RunID{RunID: 77},
		&DownloadResult{Run: sampleRun(7), File: []byte("ghost bytes")},
		&ReportRun{RunID: 5, Reason: FlagTeleport, AtSeconds: 12.5, Detail: "jumped 40 m at tick 750"},
		&ReportResult{Flags: 4},
		&MyRuns{TrackID: "mhk3.rome"},
		&RunList{Runs: []RunEntry{sampleRun(1)}},
		&ModHideRun{RunID: 5, Reason: "teleports"},
		&ModDisableAccount{AccountID: 6, Reason: "cheating"},
		&AccountID{AccountID: 6},
		&ModListFlagged{Offset: 0, Limit: 100, IncludeHidden: true},
		&RunPage{Total: 30, Runs: []RunEntry{sampleRun(3)}},
		&RunReport{Reporter: "Kurt", Reason: FlagOther, AtSeconds: -1, Detail: "odd", Created: when, Counted: true},
		&RunReports{Reports: []RunReport{{Reporter: "a", Reason: FlagSpeed, Created: when}}},
		&ModAccountInfo{Account: Account{ID: 2, Username: "Hans", Created: when}, Disabled: true,
			DisabledReason: "x", Runs: 4, HiddenRuns: 1, CheatKicks: 2, Created: when, LastLogin: when},
		&ModAccountInfo{Account: Account{ID: 3, Username: "Neu", Created: when}, Created: when},
	}
}

// TestRunEntryBytes pins the RunEntry layout so the C# codec can assert the same bytes.
func TestRunEntryBytes(t *testing.T) {
	e := RunEntry{RunID: 1, Rank: 2, AccountID: 3, Username: "ab", Country: "DE", FinishMs: 4, BestLapMs: 5,
		LapMs: []int32{6, 7}, KartID: "k", CharacterID: "c", GhostID: "g", Recorded: time.UnixMilli(8),
		Uploaded: time.UnixMilli(9), Flags: 10, SetupMismatch: true, Hidden: false, FileSize: 11, BoardID: 12,
		TrackID: "t", EngineClass: 2}
	le := binary.LittleEndian
	var want []byte
	want = le.AppendUint64(want, 1)
	want = le.AppendUint32(want, 2)
	want = le.AppendUint64(want, 3)
	want = append(want, 2, 'a', 'b', 2, 'D', 'E')
	want = le.AppendUint32(want, 4)
	want = le.AppendUint32(want, 5)
	want = append(want, 2)
	want = le.AppendUint32(want, 6)
	want = le.AppendUint32(want, 7)
	want = append(want, 1, 'k', 1, 'c', 1, 'g')
	want = le.AppendUint64(want, 8)
	want = le.AppendUint64(want, 9)
	want = le.AppendUint32(want, 10)
	want = append(want, 1, 0)
	want = le.AppendUint32(want, 11)
	want = le.AppendUint64(want, 12)
	want = append(want, 1, 't', 2)
	if got := Encode(&e); !bytes.Equal(got, want) {
		t.Fatalf("got  % x\nwant % x", got, want)
	}
	if len(want) != runEntryMinSize+2+2+8+1+1+1+1 {
		t.Fatalf("runEntryMinSize out of date: %d", len(want))
	}
}

func TestOptionalTimes(t *testing.T) {
	b := Encode(&BoardInfo{TrackID: "t"})
	if !bytes.Equal(b[len(b)-8:], make([]byte, 8)) {
		t.Fatalf("zero lastUpload encodes as % x", b[len(b)-8:])
	}
	var bi BoardInfo
	if err := bi.Decode(wire.NewReader(b)); err != nil || !bi.LastUpload.IsZero() {
		t.Fatalf("%v %v", bi.LastUpload, err)
	}
}

func TestTimeTrialLimits(t *testing.T) {
	decodeFails := func(name string, m Message, b []byte) {
		t.Helper()
		if err := m.Decode(wire.NewReader(b)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}

	e := sampleRun(1)
	e.LapMs = make([]int32, MaxLapTimes+1)
	decodeFails("100 lap times", &RunEntry{}, Encode(&e))

	l := Leaderboard{Entries: make([]RunEntry, MaxPage+1)}
	decodeFails("101 leaderboard entries", &Leaderboard{}, Encode(&l))

	decodeFails("501 my runs", &RunList{}, Encode(&RunList{Runs: make([]RunEntry, MaxMyRuns+1)}))
	decodeFails("201 boards", &BoardList{}, Encode(&BoardList{Boards: make([]BoardInfo, MaxBoards+1)}))

	decodeFails("513-byte report detail", &ReportRun{},
		Encode(&ReportRun{RunID: 1, Reason: FlagSpeed, Detail: strings.Repeat("x", MaxReportDetail+1)}))
	decodeFails("NaN atSeconds", &ReportRun{}, Encode(&ReportRun{RunID: 1, AtSeconds: float32(math.NaN())}))
	decodeFails("+Inf atSeconds", &ReportRun{}, Encode(&ReportRun{RunID: 1, AtSeconds: float32(math.Inf(1))}))
	decodeFails("257-byte hide reason", &ModHideRun{},
		Encode(&ModHideRun{RunID: 1, Reason: strings.Repeat("x", MaxModReason+1)}))
	decodeFails("97-byte track id", &MyRuns{}, Encode(&MyRuns{TrackID: strings.Repeat("t", MaxTrackID+1)}))
	decodeFails("3-byte continent", &LeaderboardFilter{}, Encode(&LeaderboardFilter{Continent: "EUR"}))

	// The upload length prefix is checked before the bytes: no 8 MiB+1 allocation for a short body.
	w := wire.NewWriter(8)
	w.VarUint(MaxGhostFile + 1)
	decodeFails("8 MiB + 1 upload", &UploadRun{}, w.Bytes())
	w = wire.NewWriter(8)
	w.VarUint(MaxGhostFile)
	decodeFails("upload longer than the body", &UploadRun{}, w.Bytes())

	// A huge list count with few bytes behind it is rejected before allocating.
	w = wire.NewWriter(16)
	w.U32(1)
	w.VarUint(MaxPage)
	decodeFails("count beyond the body", &RunPage{}, w.Bytes())

	b := Encode(&UploadResult{Run: sampleRun(1)})
	b[len(b)-1] = 7
	decodeFails("bool 7", &UploadResult{}, b)

	b = Encode(&Leaderboard{Entries: []RunEntry{}})
	b[len(b)-1] = 1 // hasMine without a RunEntry behind it
	decodeFails("hasMine without mine", &Leaderboard{}, b)
}

func TestHostileBodies(t *testing.T) {
	// Random-looking garbage must decode to an error or a value, never panic or over-allocate.
	seed := uint32(1)
	next := func() byte {
		seed = seed*1664525 + 1013904223
		return byte(seed >> 24)
	}
	for _, m := range messages() {
		for range 200 {
			b := make([]byte, int(next())%64)
			for i := range b {
				b[i] = next()
			}
			m.Decode(wire.NewReader(b))
		}
	}
}

func TestFlagReasons(t *testing.T) {
	for _, r := range []FlagReason{FlagSpeed, FlagData, FlagOther} {
		if !r.Valid() {
			t.Errorf("%s invalid", r)
		}
	}
	for _, r := range []FlagReason{0, 10, 254} {
		if r.Valid() {
			t.Errorf("%d valid", r)
		}
	}
	if FlagWallClip.String() != "WallClip" || FlagReason(42).String() != "FlagReason(42)" {
		t.Error("names")
	}
}

func TestPhase2Ranges(t *testing.T) {
	for typ := TypeModHideRun; typ <= TypeModGetAccount; typ++ {
		if !RangeModeration.Contains(typ) {
			t.Errorf("%d not in moderation range", typ)
		}
	}
	for typ := TypeListBoards; typ <= TypeMyRuns; typ++ {
		if !RangeTimeTrials.Contains(typ) {
			t.Errorf("%d not in time-trial range", typ)
		}
	}
}
