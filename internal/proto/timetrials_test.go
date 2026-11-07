package proto

import (
	"bytes"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/mhk-os/server/internal/wire"
)

func sampleRun(id uint64) RunEntry {
	return RunEntry{
		RunID: id, Rank: 3, AccountID: 9, Username: "Hans_Huhn", Country: "DE", FinishMs: 93_456, BestLapMs: 30_100,
		LapMs: []int32{31_000, 30_100, 32_356}, KartID: "mhk2.kart.speedy", KartVersion: 2, CharacterID: "mhk2.char.moorhuhn",
		GhostID: "0123456789abcdef0123456789abcdef", Recorded: when, Uploaded: when.Add(time.Hour), Flags: 2,
		Hidden: true, FileSize: 67_361, BoardID: 4, TrackID: "mhk3.rome", EngineClass: 2, TrackVersion: 3,
		Variant: "core.night+core.rain", Laps: 3, RulesVersion: 1,
	}
}

func timeTrialMessages() []Message {
	return []Message{
		&ListBoards{TrackID: "mhk3.rome", EngineClass: AnyEngineClass, RulesVersion: 7, Variant: AnyVariant},
		&ListBoards{TrackID: "mhk3.rome", EngineClass: 1, RulesVersion: AnyRulesVersion, Variant: "core.reverse"},
		&BoardInfo{BoardID: 1, TrackID: "mhk3.rome", TrackVersion: 2, Variant: "core.reverse", EngineClass: 2,
			Laps: 3, RulesVersion: 7, Players: 12, BestMs: 90_000, LastUpload: when},
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
		LapMs: []int32{6, 7}, KartID: "k", KartVersion: 13, CharacterID: "c", GhostID: "g", Recorded: time.UnixMilli(8),
		Uploaded: time.UnixMilli(9), Flags: 10, Hidden: true, FileSize: 11, BoardID: 12, TrackID: "t", EngineClass: 2,
		TrackVersion: 14, Variant: "v", Laps: 3, RulesVersion: 15}
	want := strings.Join([]string{
		"0100000000000000", "02000000", "0300000000000000", // runId, rank, accountId
		"026162", "024445", // username "ab", country "DE"
		"04000000", "05000000", // finishMs, bestLapMs
		"02", "06000000", "07000000", // lapMs
		"016b", "0d000000", "0163", "0167", // kartId, kartVersion, characterId, ghostId
		"0800000000000000", "0900000000000000", // recorded, uploaded
		"0a000000", "01", "0b000000", // flags, hidden, fileSize
		"0c00000000000000", "0174", "02", // boardId, trackId, engineClass
		"0e000000", "0176", "03", "0f000000", // trackVersion, variant, laps, rulesVersion
	}, "")
	b := pinHex(t, &e, want)
	// Beyond the empty minimum: username +2, country +2, two lap times +8, one-character kart, character, ghost,
	// track and variant ids +5.
	if len(b) != runEntryMinSize+2+2+8+5 {
		t.Fatalf("runEntryMinSize out of date: %d", len(b))
	}
}

// TestBoardInfoBytes pins the BoardInfo layout.
func TestBoardInfoBytes(t *testing.T) {
	bi := BoardInfo{BoardID: 1, TrackID: "t", TrackVersion: 2, Variant: "core.night", EngineClass: 2, Laps: 3,
		RulesVersion: 4, Players: 5, BestMs: 6, LastUpload: time.UnixMilli(7)}
	want := strings.Join([]string{
		"0100000000000000", "0174", "02000000", // boardId, trackId, trackVersion
		"0a636f72652e6e69676874", // variant "core.night"
		"02", "03", "04000000",   // engineClass, laps, rulesVersion
		"05000000", "06000000", // players, bestMs
		"0700000000000000", // lastUpload
	}, "")
	b := pinHex(t, &bi, want)
	if len(b) != boardInfoMinSize+1+10 {
		t.Fatalf("boardInfoMinSize out of date: %d", len(b))
	}
}

// TestListBoardsBytes pins the ListBoards layout.
func TestListBoardsBytes(t *testing.T) {
	pinHex(t, &ListBoards{TrackID: "t", EngineClass: AnyEngineClass, RulesVersion: AnyRulesVersion,
		Variant: AnyVariant}, "0174"+"ff"+"00000000"+"012a")
	pinHex(t, &ListBoards{TrackID: "t", EngineClass: 1, RulesVersion: 2, Variant: ""}, "0174"+"01"+"02000000"+"00")
}

// TestClientVectors pins the vectors the C# client tests assert.
func TestClientVectors(t *testing.T) {
	pinHex(t, &ListBoards{TrackID: "t", EngineClass: 255, RulesVersion: 1, Variant: "*"}, "0174ff01000000012a")
	pinHex(t, &BoardInfo{BoardID: 1, TrackID: "t", TrackVersion: 2, Variant: "core.rain", EngineClass: 1, Laps: 3,
		RulesVersion: 1, Players: 4, BestMs: 5},
		"0100000000000000"+"0174"+"02000000"+"09636f72652e7261696e"+"01"+"03"+"01000000"+"04000000"+"05000000"+
			"0000000000000000")
	pinHex(t, &RunEntry{RunID: 1, AccountID: 2, Username: "u", FinishMs: 3, BestLapMs: 4, LapMs: []int32{},
		KartID: "k", KartVersion: 5, CharacterID: "c", Recorded: time.UnixMilli(0), Uploaded: time.UnixMilli(0),
		FileSize: 6, BoardID: 7, TrackID: "t", EngineClass: 1, TrackVersion: 8, Laps: 3, RulesVersion: 1},
		"0100000000000000"+"00000000"+"0200000000000000"+"0175"+"00"+"03000000"+"04000000"+"00"+"016b"+"05000000"+
			"0163"+"00"+"0000000000000000"+"0000000000000000"+"00000000"+"00"+"06000000"+"0700000000000000"+"0174"+
			"01"+"08000000"+"00"+"03"+"01000000")
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
	long := strings.Repeat("v", MaxVariant+1)
	decodeFails("129-byte list variant", &ListBoards{}, Encode(&ListBoards{TrackID: "t", Variant: long}))
	decodeFails("129-byte board variant", &BoardInfo{}, Encode(&BoardInfo{TrackID: "t", Variant: long}))
	e = sampleRun(1)
	e.Variant = long
	decodeFails("129-byte run variant", &RunEntry{}, Encode(&e))

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
