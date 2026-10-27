// Package ghosttest writes synthetic .mhkghost files for tests: a port of the game's GhostFile.Write,
// GhostHeaderCodec.Write and GhostBodyCodec.Write with knobs to corrupt the result. Real player ghosts never go
// into the repository; tests build their runs here.
package ghosttest

import (
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"math"
	"time"

	"github.com/mhk-os/server/internal/ghost"
)

// Event is one ghost event; Effect indexes Ghost.Effects.
type Event struct {
	Time   float32
	Kind   uint8
	Role   int16
	Hold   bool
	Effect uint64
}

// Ghost is a run to encode. Channels holds the 24 quantised channels (3 position, 4 rotation, 3 velocity,
// 14 scalar), each Header.FrameCount long.
type Ghost struct {
	Header   ghost.Header
	Effects  []string
	Channels [][]int64
	Events   []Event

	// Corruption knobs; the zero values write a valid file.
	Version    uint16 // 0 = ghost.FormatVersion
	HeaderTail []byte // appended inside the header block
	BodyTail   []byte // appended inside the body block
	Trailing   []byte // appended after the body block
	NoBody     bool   // stop after the header block
}

// NewID returns a random ghost id (32 lower-case hex characters, like Guid.ToString("N")).
func NewID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// New returns a valid 3-lap run at 60 Hz with a random id: the laps take 90 s, the finish is at 90.5 s (0.5 s run-up)
// and recording runs 2 s past it.
func New() *Ghost {
	g := &Ghost{
		Header: ghost.Header{
			ID: NewID(), GameBuild: "v0.9 abc1234", ContentVersion: 4, PlayerName: "Test Driver", PlayerID: "player-1",
			DateTicks: Ticks(time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)),
			TrackID:   "test.track", TrackPackID: "test", TrackPackVersion: "1.0",
			TrackHash: "9b7809579ee7ae57280e58893e32df90", KartID: "test.kart", CharacterID: "test.char",
			EngineClass: 2, EngineClassLabel: "150cc", Laps: 3, SetupHash: "8a12b96cf90b6557e9d1d97d60051ba7",
			TickSeconds: 1.0 / 60, SpeedScale: 1,
		},
		Effects: []string{"test.effect.boost", "test.effect.spin"},
		Events: []Event{
			{Time: -1, Kind: ghost.EventAction, Role: 3, Hold: true},
			{Time: 0.5, Kind: ghost.EventRelease, Role: 3},
			{Time: 12, Kind: ghost.EventEffectApplied, Effect: 0},
			{Time: 14, Kind: ghost.EventEffectEnded, Effect: 0},
			{Time: 40, Kind: ghost.EventSnap},
			{Time: 91, Kind: ghost.EventEffectApplied, Effect: 1},
		},
	}
	g.SetTimes(0.5, 30, 29.5, 30.5)
	return g
}

// Ticks converts t to .NET DateTime ticks.
func Ticks(t time.Time) int64 { return ghost.UnixEpochTicks + t.UnixMilli()*ghost.TicksPerMilli }

// SetTimes sets the lap times and a finish time of runUp + their sum, and regenerates frames up to 2 s past it.
func (g *Ghost) SetTimes(runUp float32, laps ...float32) {
	finish := runUp
	for _, t := range laps {
		finish += t
	}
	g.Header.Laps = int32(len(laps))
	g.Header.LapTimes = laps
	g.Header.FinishTime = finish
	g.SetFrames(int32(math.Ceil(float64(finish)/float64(g.Header.TickSeconds))) + 120)
}

// SetFrames regenerates the channels for n frames: a circle of radius 200 m, 30 s per lap.
func (g *Ghost) SetFrames(n int32) {
	g.Header.FrameCount = n
	g.Channels = make([][]int64, ghost.PositionChannels+ghost.RotationChannels+ghost.FirstOrderChannels)
	for c := range g.Channels {
		g.Channels[c] = make([]int64, n)
	}
	for i := range int(n) {
		a := 2 * math.Pi * float64(i) * float64(g.Header.TickSeconds) / 30
		g.Channels[0][i] = int64(math.Round((1000 + 200*math.Cos(a)) * ghost.PosScale))
		g.Channels[1][i] = int64(math.Round(5 * ghost.PosScale))
		g.Channels[2][i] = int64(math.Round((-3000 + 200*math.Sin(a)) * ghost.PosScale))
		g.Channels[4][i] = int64(math.Round(math.Sin(a/2) * 1e6))
		g.Channels[6][i] = int64(math.Round(math.Cos(a/2) * 1e6))
		for c := 7; c < len(g.Channels); c++ {
			g.Channels[c][i] = int64(i%7) - 3
		}
	}
}

// Encode returns the GZip-compressed file.
func (g *Ghost) Encode() []byte { return Gzip(g.Raw()) }

// Raw returns the inflated file bytes.
func (g *Ghost) Raw() []byte {
	var raw bytes.Buffer
	raw.WriteString("MHKG")
	version := g.Version
	if version == 0 {
		version = ghost.FormatVersion
	}
	binary.Write(&raw, binary.LittleEndian, version)
	head := g.encodeHeader()
	binary.Write(&raw, binary.LittleEndian, int32(len(head)))
	raw.Write(head)
	if !g.NoBody {
		body := g.encodeBody()
		binary.Write(&raw, binary.LittleEndian, int32(len(body)))
		raw.Write(body)
	}
	raw.Write(g.Trailing)
	return raw.Bytes()
}

// Gzip compresses b as one GZip member, like GZipStream.
func Gzip(b []byte) []byte {
	var out bytes.Buffer
	zw := gzip.NewWriter(&out)
	zw.Write(b)
	zw.Close()
	return out.Bytes()
}

// Writer writes .NET BinaryWriter primitives and the ghost channel encoding.
type Writer struct{ bytes.Buffer }

func (w *Writer) I16(v int16)   { binary.Write(&w.Buffer, binary.LittleEndian, v) }
func (w *Writer) I32(v int32)   { binary.Write(&w.Buffer, binary.LittleEndian, v) }
func (w *Writer) I64(v int64)   { binary.Write(&w.Buffer, binary.LittleEndian, v) }
func (w *Writer) F32(v float32) { binary.Write(&w.Buffer, binary.LittleEndian, math.Float32bits(v)) }

// Str is BinaryWriter.Write(string): 7-bit encoded byte length, then UTF-8.
func (w *Writer) Str(s string) {
	w.Varint(uint64(len(s)))
	w.WriteString(s)
}

// Varint is GhostChannelCodec.WriteVarint (unsigned LEB128).
func (w *Writer) Varint(v uint64) {
	for v >= 0x80 {
		w.WriteByte(byte(v) | 0x80)
		v >>= 7
	}
	w.WriteByte(byte(v))
}

// Channel is GhostChannelCodec.WriteChannel: zig-zag varints of first or second differences.
func (w *Writer) Channel(q []int64, order int) {
	for i := range q {
		d := q[i]
		if i >= 1 {
			d -= q[i-1]
		}
		if order == 2 && i >= 2 {
			d -= q[i-1] - q[i-2]
		}
		w.Varint(uint64((d << 1) ^ (d >> 63)))
	}
}

func (g *Ghost) encodeHeader() []byte {
	h := &g.Header
	var w Writer
	w.Str(h.ID)
	w.Str(h.GameBuild)
	w.I32(h.ContentVersion)
	w.Str(h.PlayerName)
	w.Str(h.PlayerID)
	w.I64(h.DateTicks)
	w.Str(h.TrackID)
	w.Str(h.TrackPackID)
	w.Str(h.TrackPackVersion)
	w.Str(h.TrackHash)
	w.Str(h.KartID)
	w.Str(h.CharacterID)
	w.I32(h.EngineClass)
	w.Str(h.EngineClassLabel)
	w.I32(h.Laps)
	w.Str(h.SetupHash)
	w.F32(h.TickSeconds)
	w.F32(h.SpeedScale)
	w.F32(h.FinishTime)
	w.I32(int32(len(h.LapTimes)))
	for _, t := range h.LapTimes {
		w.F32(t)
	}
	w.I32(h.FrameCount)
	w.Write(g.HeaderTail)
	return w.Bytes()
}

func (g *Ghost) encodeBody() []byte {
	var w Writer
	w.Varint(uint64(len(g.Effects)))
	for _, s := range g.Effects {
		w.Str(s)
	}
	for c, q := range g.Channels {
		order := 1
		if c < ghost.PositionChannels+ghost.RotationChannels {
			order = 2
		}
		w.Channel(q, order)
	}
	w.Varint(uint64(len(g.Events)))
	for _, e := range g.Events {
		w.F32(e.Time)
		w.WriteByte(e.Kind)
		switch e.Kind {
		case ghost.EventAction:
			w.I16(e.Role)
			if e.Hold {
				w.WriteByte(1)
			} else {
				w.WriteByte(0)
			}
		case ghost.EventRelease:
			w.I16(e.Role)
		case ghost.EventEffectApplied, ghost.EventEffectEnded:
			w.Varint(e.Effect)
		}
	}
	w.Write(g.BodyTail)
	return w.Bytes()
}
