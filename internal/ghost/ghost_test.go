package ghost_test

import (
	"bytes"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/mhk-os/server/internal/ghost"
	"github.com/mhk-os/server/internal/ghost/ghosttest"
)

func TestParseValid(t *testing.T) {
	g := ghosttest.New()
	s, err := ghost.Parse(g.Encode())
	if err != nil {
		t.Fatal(err)
	}
	h := s.Header
	if h.ID != g.Header.ID || h.TrackID != "test.track" || h.KartID != "test.kart" || h.Laps != 3 ||
		h.EngineClass != 2 || h.ContentVersion != 4 || h.PlayerName != "Test Driver" || h.FrameCount != g.Header.FrameCount {
		t.Fatalf("header %+v", h)
	}
	if s.FinishMs != 90_500 || s.BestLapMs != 29_500 || len(s.LapMs) != 3 || s.LapMs[2] != 30_500 {
		t.Fatalf("times %d %d %v", s.FinishMs, s.BestLapMs, s.LapMs)
	}
	if !s.Recorded.Equal(time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)) || s.Effects != 2 || s.Events != 6 {
		t.Fatalf("recorded %v effects %d events %d", s.Recorded, s.Effects, s.Events)
	}

	// Other legal tick rates, upper-case hex and the tightest frame counts.
	for _, rate := range []float64{30, 50, 120, 240} {
		g := ghosttest.New()
		g.Header.TickSeconds = float32(1 / rate)
		g.Header.ID = strings.ToUpper(g.Header.ID)
		g.SetTimes(0.5, 30, 29.5, 30.5)
		g.SetFrames(int32(math.Ceil(float64(g.Header.FinishTime) * rate)))
		if _, err := ghost.Parse(g.Encode()); err != nil {
			t.Errorf("%v Hz: %v", rate, err)
		}
	}
	g = ghosttest.New()
	g.SetFrames(int32(math.Floor((float64(g.Header.FinishTime) + 10) * 60)))
	if _, err := ghost.Parse(g.Encode()); err != nil {
		t.Errorf("10 s tail: %v", err)
	}
}

func TestParseRejects(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(g *ghosttest.Ghost)
		file   func(g *ghosttest.Ghost) []byte // nil = g.Encode()
		want   string
	}{
		{"not gzip", nil, func(g *ghosttest.Ghost) []byte { return g.Raw() }, "not a GZip file"},
		{"corrupt gzip", nil, func(g *ghosttest.Ghost) []byte {
			b := g.Encode()
			b[len(b)/2] ^= 0xff
			return b
		}, "corrupt"},
		{"truncated gzip", nil, func(g *ghosttest.Ghost) []byte { b := g.Encode(); return b[:len(b)-10] }, "corrupt"},
		{"garbage after gzip", nil, func(g *ghosttest.Ghost) []byte { return append(g.Encode(), "junk junk"...) }, "corrupt"},
		{"compressed too large", nil, func(*ghosttest.Ghost) []byte { return make([]byte, ghost.MaxCompressed+1) },
			"larger than 8 MiB"},
		{"inflated too large", nil, func(*ghosttest.Ghost) []byte {
			return ghosttest.Gzip(make([]byte, ghost.MaxInflated+1))
		}, "inflated size"},
		{"bad magic", nil, func(g *ghosttest.Ghost) []byte {
			raw := g.Raw()
			raw[0] = 'X'
			return ghosttest.Gzip(raw)
		}, "not a ghost file"},
		{"version 2", func(g *ghosttest.Ghost) { g.Version = 2 }, nil, "format version 2"},
		{"no body", func(g *ghosttest.Ghost) { g.NoBody = true }, nil, "before the body block"},
		{"block length beyond data", nil, func(g *ghosttest.Ghost) []byte {
			raw := g.Raw()
			raw[9] = 0x7f // header length ~2 GiB
			return ghosttest.Gzip(raw)
		}, "header block length"},
		{"trailing garbage", func(g *ghosttest.Ghost) { g.Trailing = []byte{1, 2, 3} }, nil, "after the body block"},
		{"header tail", func(g *ghosttest.Ghost) { g.HeaderTail = []byte{0} }, nil, "end of the header block"},
		{"body tail", func(g *ghosttest.Ghost) { g.BodyTail = []byte{0} }, nil, "end of the body block"},
		{"short id", func(g *ghosttest.Ghost) { g.Header.ID = "abc" }, nil, "ghost id"},
		{"non-hex id", func(g *ghosttest.Ghost) { g.Header.ID = strings.Repeat("g", 32) }, nil, "ghost id"},
		{"empty track", func(g *ghosttest.Ghost) { g.Header.TrackID = "" }, nil, "track id"},
		{"track with slash", func(g *ghosttest.Ghost) { g.Header.TrackID = "a/b" }, nil, "track id"},
		{"long kart", func(g *ghosttest.Ghost) { g.Header.KartID = strings.Repeat("k", 97) }, nil, "kart id"},
		{"empty pack", func(g *ghosttest.Ghost) { g.Header.TrackPackID = "" }, nil, "track pack id"},
		{"character space", func(g *ghosttest.Ghost) { g.Header.CharacterID = "moor huhn" }, nil, "character id"},
		{"track hash", func(g *ghosttest.Ghost) { g.Header.TrackHash = "xyz" }, nil, "track hash"},
		{"setup hash", func(g *ghosttest.Ghost) { g.Header.SetupHash = strings.Repeat("0", 31) }, nil, "setup hash"},
		{"engine class 3", func(g *ghosttest.Ghost) { g.Header.EngineClass = 3 }, nil, "engine class"},
		{"engine class -1", func(g *ghosttest.Ghost) { g.Header.EngineClass = -1 }, nil, "engine class"},
		{"zero laps", func(g *ghosttest.Ghost) { g.Header.Laps = 0; g.Header.LapTimes = nil }, nil, "lap count"},
		{"100 lap times", func(g *ghosttest.Ghost) {
			g.Header.Laps = 100
			g.Header.LapTimes = make([]float32, 100)
		}, nil, "lap times (at most 99)"},
		{"laps vs lap times", func(g *ghosttest.Ghost) { g.Header.Laps = 2 }, nil, "3 lap times for 2 laps"},
		{"tick 1/25", func(g *ghosttest.Ghost) { g.Header.TickSeconds = 1.0 / 25 }, nil, "tick"},
		{"tick 1/300", func(g *ghosttest.Ghost) { g.Header.TickSeconds = 1.0 / 300 }, nil, "tick"},
		{"tick not 1/n", func(g *ghosttest.Ghost) { g.Header.TickSeconds = 0.0171 }, nil, "tick"},
		{"tick NaN", func(g *ghosttest.Ghost) { g.Header.TickSeconds = float32(math.NaN()) }, nil, "tick is not a finite"},
		{"zero finish", func(g *ghosttest.Ghost) { g.Header.FinishTime = 0 }, nil, "finish time"},
		{"finish 3 h", func(g *ghosttest.Ghost) { g.Header.FinishTime = 3 * 3600 }, nil, "finish time"},
		{"finish inf", func(g *ghosttest.Ghost) { g.Header.FinishTime = float32(math.Inf(1)) }, nil, "finish time is not a finite"},
		{"negative lap", func(g *ghosttest.Ghost) { g.Header.LapTimes[1] = -29.5 }, nil, "lap 2 time"},
		{"NaN lap", func(g *ghosttest.Ghost) { g.Header.LapTimes[0] = float32(math.NaN()) }, nil, "lap 1 time"},
		{"laps longer than finish", func(g *ghosttest.Ghost) { g.Header.LapTimes[0] += 0.6 }, nil, "more than the finish"},
		{"run-up over 60 s", func(g *ghosttest.Ghost) { g.SetTimes(61, 30, 29.5, 30.5) }, nil, "run-up"},
		{"speed scale 0", func(g *ghosttest.Ghost) { g.Header.SpeedScale = 0 }, nil, "speed scale"},
		{"speed scale NaN", func(g *ghosttest.Ghost) { g.Header.SpeedScale = float32(math.NaN()) }, nil, "speed scale is not a finite"},
		{"no frames", func(g *ghosttest.Ghost) { g.SetFrames(0) }, nil, "no frames"},
		{"frames end early", func(g *ghosttest.Ghost) { g.SetFrames(5000) }, nil, "end before the finish"},
		{"frames run long", func(g *ghosttest.Ghost) { g.SetFrames(int32((90.5+10)*60) + 2) }, nil, "past the finish"},
		{"frame count above channels", nil, func(g *ghosttest.Ghost) []byte {
			// 200 more frames than the channels hold (all zero, so misaligned reads stay in range): the last channel
			// runs out of bytes.
			for c := range g.Channels {
				clear(g.Channels[c])
			}
			g.Events = nil
			g.Header.FrameCount += 200
			return g.Encode()
		}, "channel 23 does not hold all frames"},
		{"date before 2000", func(g *ghosttest.Ghost) {
			g.Header.DateTicks = ghosttest.Ticks(time.Date(1999, 1, 1, 0, 0, 0, 0, time.UTC))
		}, nil, "recording date"},
		{"date ticks negative", func(g *ghosttest.Ghost) { g.Header.DateTicks = -1 }, nil, "recording date"},
		{"position far away", func(g *ghosttest.Ghost) { g.Channels[1][100] = 100_001_000 }, nil, "position y at frame 100"},
		{"position negative far", func(g *ghosttest.Ghost) { g.Channels[0][7] = -200_000_000 }, nil, "position x at frame 7"},
		{"4097 effects", func(g *ghosttest.Ghost) { g.Effects = make([]string, 4097) }, nil, "effect table"},
		{"long effect id", func(g *ghosttest.Ghost) { g.Effects[1] = strings.Repeat("e", 129) }, nil, "longer than 128"},
		{"event kind 5", func(g *ghosttest.Ghost) { g.Events[2].Kind = 5 }, nil, "unknown kind 5"},
		{"effect index", func(g *ghosttest.Ghost) { g.Events[2].Effect = 2 }, nil, "uses effect 2 of 2"},
		{"event before -10 s", func(g *ghosttest.Ghost) { g.Events[0].Time = -10.5 }, nil, "event 0 time"},
		{"event after finish + 10 s", func(g *ghosttest.Ghost) { g.Events[5].Time = 101 }, nil, "event 5 time"},
		{"event NaN", func(g *ghosttest.Ghost) { g.Events[3].Time = float32(math.NaN()) }, nil, "event 3 time"},
		{"too many events", nil, func(g *ghosttest.Ghost) []byte {
			g.Events = nil
			g.BodyTail = nil
			raw := g.Raw()
			// Replace the trailing "0 events" varint with 1,000,001.
			var w ghosttest.Writer
			w.Varint(ghost.MaxEvents + 1)
			raw = append(raw[:len(raw)-1], w.Bytes()...)
			fixBodyLength(raw, len(w.Bytes())-1)
			return ghosttest.Gzip(raw)
		}, "1000001 events"},
		{"truncated events", nil, func(g *ghosttest.Ghost) []byte {
			raw := g.Raw()
			raw = raw[:len(raw)-1]
			fixBodyLength(raw, -1)
			return ghosttest.Gzip(raw)
		}, "truncated in event 5"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := ghosttest.New()
			if tc.mutate != nil {
				tc.mutate(g)
			}
			var file []byte
			if tc.file != nil {
				file = tc.file(g)
			} else {
				file = g.Encode()
			}
			_, err := ghost.Parse(file)
			var ge *ghost.Error
			if !errors.As(err, &ge) {
				t.Fatalf("got %v, want *ghost.Error", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

// fixBodyLength adjusts the body block length of raw (inflated) after its last bytes changed by delta.
func fixBodyLength(raw []byte, delta int) {
	headerLen := int(int32(uint32(raw[6]) | uint32(raw[7])<<8 | uint32(raw[8])<<16 | uint32(raw[9])<<24))
	at := 10 + headerLen
	n := int(int32(uint32(raw[at])|uint32(raw[at+1])<<8|uint32(raw[at+2])<<16|uint32(raw[at+3])<<24)) + delta
	raw[at], raw[at+1], raw[at+2], raw[at+3] = byte(n), byte(n>>8), byte(n>>16), byte(n>>24)
}

func TestValidContentID(t *testing.T) {
	for _, ok := range []string{"a", "mhk3.rome", "Kart_2-b", strings.Repeat("x", 96)} {
		if !ghost.ValidContentID(ok) {
			t.Errorf("%q rejected", ok)
		}
	}
	for _, bad := range []string{"", "ä", "a b", "a/b", strings.Repeat("x", 97)} {
		if ghost.ValidContentID(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
}

func FuzzParse(f *testing.F) {
	g := ghosttest.New()
	g.SetTimes(0.2, 1, 1)
	f.Add(g.Encode())
	f.Add(g.Raw())
	g.Trailing = []byte{1}
	f.Add(g.Encode())
	f.Add(ghosttest.Gzip([]byte("MHKG\x01\x00\xff\xff\xff\x7f")))
	f.Fuzz(func(t *testing.T, file []byte) {
		s, err := ghost.Parse(file)
		if err != nil {
			var ge *ghost.Error
			if !errors.As(err, &ge) {
				t.Fatalf("non-validation error %T: %v", err, err)
			}
			return
		}
		if s.FinishMs <= 0 || len(s.LapMs) != int(s.Laps) || s.FrameCount <= 0 || len(s.ID) != 32 {
			t.Fatalf("accepted an invalid summary %+v", s)
		}
		// Raw (inflated) input must never be accepted: the file is GZip.
		if bytes.HasPrefix(file, []byte("MHKG")) {
			t.Fatal("accepted an uncompressed file")
		}
	})
}
