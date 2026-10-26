// Package ghost parses and validates the game's .mhkghost files (game repo Runtime/Ghosts: GhostFile.cs,
// GhostHeaderCodec.cs, GhostBodyCodec.cs, GhostChannelCodec.cs) and applies the upload checks of docs/protocol.md.
// It decodes every byte the game would read, without keeping the frames: the server needs the summary only.
package ghost

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"time"
)

// Limits from docs/protocol.md "Upload validation".
const (
	MaxCompressed  = 8 << 20
	MaxInflated    = 48 << 20
	FormatVersion  = 1
	MaxIDLength    = 96
	MaxLaps        = 99
	MaxFinish      = 3 * 60 * 60 // s, exclusive
	LapSumSlack    = 0.05        // s the lap times may exceed the finish time by (float rounding)
	MaxRunUp       = 60          // s from GO to the first line crossing (finishTime minus the lap times)
	TailSlack      = 10          // s of frames and events allowed past the finish (and events before GO)
	MaxEffects     = 4096
	MaxEffectID    = 128 // bytes
	MaxEvents      = 1_000_000
	MaxPosition    = 100_000 // m from the origin on every axis
	MinTickRate    = 30      // ticks per second
	MaxTickRate    = 240
	TickTolerance  = 1e-6
	frameTolerance = 1e-3 // frames: float rounding of finishTime / tickSeconds
)

var magic = []byte("MHKG")

// Channel layout of the body (GhostBodyCodec): 3 position and 4 rotation channels as second differences, then
// 3 velocity and 14 scalar/int channels as first differences.
const (
	PositionChannels   = 3
	RotationChannels   = 4
	FirstOrderChannels = 3 + 14
	PosScale           = 1000
)

// Event kinds (GhostEventKind).
const (
	EventAction        = 0
	EventRelease       = 1
	EventSnap          = 2
	EventEffectApplied = 3
	EventEffectEnded   = 4
)

// Header is the header block of a ghost file, field for field.
type Header struct {
	ID               string
	GameBuild        string
	SimVersion       int32
	PlayerName       string
	PlayerID         string
	DateTicks        int64 // .NET DateTime ticks, UTC
	TrackID          string
	TrackPackID      string
	TrackPackVersion string
	TrackHash        string
	KartID           string
	CharacterID      string
	EngineClass      int32
	EngineClassLabel string
	Laps             int32
	SetupHash        string
	TickSeconds      float32
	SpeedScale       float32
	FinishTime       float32
	LapTimes         []float32
	FrameCount       int32
}

// Summary is a validated ghost: its header plus the derived values the server stores.
type Summary struct {
	Header
	Recorded  time.Time
	FinishMs  int32
	LapMs     []int32
	BestLapMs int32
	Effects   int
	Events    int
}

// Error is a validation failure; Msg names the problem for the player.
type Error struct{ Msg string }

func (e *Error) Error() string { return e.Msg }

func fail(format string, args ...any) error { return &Error{Msg: fmt.Sprintf(format, args...)} }

// Parse inflates, decodes and validates a complete .mhkghost file. Every error is an *Error.
func Parse(file []byte) (*Summary, error) {
	if len(file) > MaxCompressed {
		return nil, fail("file is larger than %d MiB", MaxCompressed>>20)
	}
	data, err := inflate(file)
	if err != nil {
		return nil, err
	}
	r := &reader{b: data}
	if m := r.bytes(len(magic)); r.err != nil || !bytes.Equal(m, magic) {
		return nil, fail("not a ghost file")
	}
	if v := r.u16(); r.err != nil || v != FormatVersion {
		return nil, fail("unsupported ghost format version %d (want %d)", v, FormatVersion)
	}
	headerBlock, err := r.block("header")
	if err != nil {
		return nil, err
	}
	bodyBlock, err := r.block("body")
	if err != nil {
		return nil, err
	}
	if r.remaining() != 0 {
		return nil, fail("%d unexpected bytes after the body block", r.remaining())
	}
	s := &Summary{}
	if err := readHeader(&reader{b: headerBlock}, &s.Header); err != nil {
		return nil, err
	}
	if err := s.validateHeader(); err != nil {
		return nil, err
	}
	if err := s.readBody(&reader{b: bodyBlock}); err != nil {
		return nil, err
	}
	return s, nil
}

func inflate(file []byte) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(file))
	if err != nil {
		return nil, fail("not a GZip file")
	}
	var out bytes.Buffer
	n, err := out.ReadFrom(io.LimitReader(zr, MaxInflated+1))
	if err != nil {
		return nil, fail("GZip data is corrupt: %v", err)
	}
	if n > MaxInflated {
		return nil, fail("inflated size is larger than %d MiB", MaxInflated>>20)
	}
	return out.Bytes(), nil
}

// reader decodes .NET BinaryReader primitives. The first failure is sticky.
type reader struct {
	b   []byte
	off int
	err error
}

var errTruncated = errors.New("truncated")

func (r *reader) remaining() int { return len(r.b) - r.off }

func (r *reader) bytes(n int) []byte {
	if r.err != nil {
		return nil
	}
	if n < 0 || n > r.remaining() {
		r.err = errTruncated
		return nil
	}
	s := r.b[r.off : r.off+n]
	r.off += n
	return s
}

func (r *reader) u8() uint8 {
	if b := r.bytes(1); b != nil {
		return b[0]
	}
	return 0
}

func (r *reader) u16() uint16 {
	if b := r.bytes(2); b != nil {
		return binary.LittleEndian.Uint16(b)
	}
	return 0
}

func (r *reader) i16() int16 { return int16(r.u16()) }

func (r *reader) i32() int32 {
	if b := r.bytes(4); b != nil {
		return int32(binary.LittleEndian.Uint32(b))
	}
	return 0
}

func (r *reader) i64() int64 {
	if b := r.bytes(8); b != nil {
		return int64(binary.LittleEndian.Uint64(b))
	}
	return 0
}

func (r *reader) f32() float32 { return math.Float32frombits(uint32(r.i32())) }

// int7 is BinaryReader.Read7BitEncodedInt: at most 5 bytes, the 5th carrying the top 4 bits of an int32.
func (r *reader) int7() int32 {
	var v uint32
	for i := range 5 {
		b := r.u8()
		if r.err != nil {
			return 0
		}
		if i == 4 && b > 0x0f {
			r.err = errors.New("bad 7-bit encoded length")
			return 0
		}
		v |= uint32(b&0x7f) << (7 * i)
		if b&0x80 == 0 {
			break
		}
	}
	return int32(v)
}

// str is BinaryReader.ReadString: a 7-bit encoded byte length and UTF-8 bytes.
func (r *reader) str() string {
	n := r.int7()
	if r.err == nil && n < 0 {
		r.err = errors.New("negative string length")
	}
	return string(r.bytes(int(n)))
}

// varint is GhostChannelCodec.ReadVarint: unsigned LEB128 of up to 10 bytes, bits past 64 dropped.
func (r *reader) varint() uint64 {
	var v uint64
	for shift := 0; shift < 64; shift += 7 {
		b := r.u8()
		if r.err != nil {
			return 0
		}
		v |= uint64(b&0x7f) << shift
		if b&0x80 == 0 {
			return v
		}
	}
	r.err = errors.New("bad varint")
	return 0
}

func (r *reader) block(name string) ([]byte, error) {
	n := r.i32()
	if r.err != nil {
		return nil, fail("file ends before the %s block", name)
	}
	if n < 0 || int(n) > r.remaining() {
		return nil, fail("%s block length %d exceeds the file", name, n)
	}
	return r.bytes(int(n)), nil
}

func readHeader(r *reader, h *Header) error {
	h.ID = r.str()
	h.GameBuild = r.str()
	h.SimVersion = r.i32()
	h.PlayerName = r.str()
	h.PlayerID = r.str()
	h.DateTicks = r.i64()
	h.TrackID = r.str()
	h.TrackPackID = r.str()
	h.TrackPackVersion = r.str()
	h.TrackHash = r.str()
	h.KartID = r.str()
	h.CharacterID = r.str()
	h.EngineClass = r.i32()
	h.EngineClassLabel = r.str()
	h.Laps = r.i32()
	h.SetupHash = r.str()
	h.TickSeconds = r.f32()
	h.SpeedScale = r.f32()
	h.FinishTime = r.f32()
	laps := r.i32()
	if r.err != nil {
		return fail("header is truncated")
	}
	if laps < 0 || laps > MaxLaps {
		return fail("header has %d lap times (at most %d)", laps, MaxLaps)
	}
	h.LapTimes = make([]float32, laps)
	for i := range h.LapTimes {
		h.LapTimes[i] = r.f32()
	}
	h.FrameCount = r.i32()
	if r.err != nil {
		return fail("header is truncated")
	}
	if r.remaining() != 0 {
		return fail("%d unexpected bytes at the end of the header block", r.remaining())
	}
	return nil
}

func isHex32(s string) bool {
	if len(s) != 32 {
		return false
	}
	for i := range len(s) {
		c := s[i]
		if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F') {
			return false
		}
	}
	return true
}

// ValidContentID reports whether s is 1–96 characters of [A-Za-z0-9_.-] (track, pack, kart and character ids).
func ValidContentID(s string) bool {
	if len(s) == 0 || len(s) > MaxIDLength {
		return false
	}
	for i := range len(s) {
		c := s[i]
		if !('0' <= c && c <= '9' || 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || c == '_' || c == '.' || c == '-') {
			return false
		}
	}
	return true
}

func finite(f float32) bool { return !math.IsNaN(float64(f)) && !math.IsInf(float64(f), 0) }

// .NET DateTime ticks (100 ns since 0001-01-01) of the Unix epoch, and the accepted recording dates (the server
// stores them as DATETIME).
const (
	UnixEpochTicks = 621_355_968_000_000_000
	TicksPerMilli  = 10_000
)

var (
	minRecorded = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	maxRecorded = time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)
)

func (s *Summary) validateHeader() error {
	h := &s.Header
	if !isHex32(h.ID) {
		return fail("ghost id must be 32 hex characters")
	}
	for _, f := range []struct{ name, value string }{
		{"track id", h.TrackID}, {"track pack id", h.TrackPackID}, {"kart id", h.KartID},
		{"character id", h.CharacterID},
	} {
		if !ValidContentID(f.value) {
			return fail("%s %q must be 1-%d characters of A-Z a-z 0-9 _ . -", f.name, truncate(f.value), MaxIDLength)
		}
	}
	if !isHex32(h.TrackHash) {
		return fail("track hash must be 32 hex characters")
	}
	if !isHex32(h.SetupHash) {
		return fail("setup hash must be 32 hex characters")
	}
	if h.EngineClass < 0 || h.EngineClass > 2 {
		return fail("engine class %d is not 0-2", h.EngineClass)
	}
	if h.Laps < 1 || h.Laps > MaxLaps {
		return fail("lap count %d is not 1-%d", h.Laps, MaxLaps)
	}
	if int(h.Laps) != len(h.LapTimes) {
		return fail("%d lap times for %d laps", len(h.LapTimes), h.Laps)
	}
	for _, f := range []struct {
		name string
		v    float32
	}{{"tick", h.TickSeconds}, {"speed scale", h.SpeedScale}, {"finish time", h.FinishTime}} {
		if !finite(f.v) {
			return fail("%s is not a finite number", f.name)
		}
	}
	tick := float64(h.TickSeconds)
	if rate := math.Round(1 / tick); tick <= 0 || rate < MinTickRate || rate > MaxTickRate ||
		math.Abs(tick-1/rate) > TickTolerance {
		return fail("tick %g s is not 1/n s for n = %d-%d", tick, MinTickRate, MaxTickRate)
	}
	finish := float64(h.FinishTime)
	if finish <= 0 || finish >= MaxFinish {
		return fail("finish time %.3f s is not between 0 and 3 hours", finish)
	}
	sum := 0.0
	for i, t := range h.LapTimes {
		if !finite(t) || t <= 0 {
			return fail("lap %d time %g s is not positive", i+1, t)
		}
		sum += float64(t)
	}
	// finishTime counts from GO, the first lap from the first line crossing: the difference is the run-up.
	if sum > finish+LapSumSlack {
		return fail("lap times add up to %.3f s, more than the finish time %.3f s", sum, finish)
	}
	if finish-sum > MaxRunUp {
		return fail("finish time %.3f s is %.0f s longer than the laps (run-up at most %d s)", finish, finish-sum,
			MaxRunUp)
	}
	if h.SpeedScale <= 0 {
		return fail("speed scale %g is not positive", h.SpeedScale)
	}
	frames := float64(h.FrameCount)
	if h.FrameCount <= 0 {
		return fail("ghost has no frames")
	}
	if frames > (finish+TailSlack)/tick+frameTolerance {
		return fail("%d frames run more than %d s past the finish", h.FrameCount, TailSlack)
	}
	if frames < finish/tick-frameTolerance {
		return fail("%d frames end before the finish at %.3f s", h.FrameCount, finish)
	}
	if h.DateTicks < UnixEpochTicks {
		return fail("recording date is out of range")
	}
	s.Recorded = time.UnixMilli((h.DateTicks - UnixEpochTicks) / TicksPerMilli).UTC()
	if s.Recorded.Before(minRecorded) || !s.Recorded.Before(maxRecorded) {
		return fail("recording date %s is out of range", s.Recorded.Format(time.DateOnly))
	}
	s.FinishMs = int32(math.Round(finish * 1000))
	s.LapMs = make([]int32, len(h.LapTimes))
	for i, t := range h.LapTimes {
		s.LapMs[i] = int32(math.Round(float64(t) * 1000))
		if i == 0 || s.LapMs[i] < s.BestLapMs {
			s.BestLapMs = s.LapMs[i]
		}
	}
	return nil
}

func truncate(s string) string {
	if len(s) > 40 {
		return s[:40] + "..."
	}
	return s
}

func (s *Summary) readBody(r *reader) error {
	effects, err := readEffectTable(r)
	if err != nil {
		return err
	}
	s.Effects = effects
	n := int(s.FrameCount)
	for c := range PositionChannels {
		axis := "xyz"[c]
		if err := readChannel(r, n, 2, func(i int, q int64) error {
			if v := float32(q) / PosScale; v < -MaxPosition || v > MaxPosition {
				return fail("position %c at frame %d is %.0f m from the origin (at most %d km)", axis, i, v,
					MaxPosition/1000)
			}
			return nil
		}); err != nil {
			return channelError(err, fmt.Sprintf("position %c channel", axis))
		}
	}
	for c := range RotationChannels {
		if err := readChannel(r, n, 2, nil); err != nil {
			return channelError(err, fmt.Sprintf("rotation channel %d", c))
		}
	}
	for c := range FirstOrderChannels {
		if err := readChannel(r, n, 1, nil); err != nil {
			return channelError(err, fmt.Sprintf("channel %d", PositionChannels+RotationChannels+c))
		}
	}
	events, err := s.readEvents(r, effects)
	if err != nil {
		return err
	}
	s.Events = events
	if r.remaining() != 0 {
		return fail("%d unexpected bytes at the end of the body block", r.remaining())
	}
	return nil
}

func channelError(err error, name string) error {
	var ve *Error
	if errors.As(err, &ve) {
		return err
	}
	return fail("%s does not hold all frames", name)
}

func readEffectTable(r *reader) (int, error) {
	n := r.varint()
	if r.err != nil {
		return 0, fail("body is truncated in the effect table")
	}
	if n > MaxEffects {
		return 0, fail("effect table has %d entries (at most %d)", n, MaxEffects)
	}
	for i := range int(n) {
		id := r.str()
		if r.err != nil {
			return 0, fail("body is truncated in the effect table")
		}
		if len(id) > MaxEffectID {
			return 0, fail("effect id %d is longer than %d bytes", i, MaxEffectID)
		}
	}
	return int(n), nil
}

// readChannel is GhostChannelCodec.ReadChannel: n zig-zag varints of first or second differences, reconstructed
// with wrapping int64 arithmetic exactly like the C# code. check (optional) sees every decoded value.
func readChannel(r *reader, n, order int, check func(i int, q int64) error) error {
	var prev, prev2 int64
	for i := range n {
		z := r.varint()
		if r.err != nil {
			return r.err
		}
		d := int64(z>>1) ^ -int64(z&1)
		if i >= 1 {
			d += prev
		}
		if order == 2 && i >= 2 {
			d += prev - prev2
		}
		if check != nil {
			if err := check(i, d); err != nil {
				return err
			}
		}
		prev2, prev = prev, d
	}
	return nil
}

func (s *Summary) readEvents(r *reader, effects int) (int, error) {
	n := r.varint()
	if r.err != nil {
		return 0, fail("body is truncated before the events")
	}
	if n > MaxEvents {
		return 0, fail("ghost has %d events (at most %d)", n, MaxEvents)
	}
	lo, hi := float32(-TailSlack), s.FinishTime+TailSlack
	for i := range int(n) {
		t := r.f32()
		kind := r.u8()
		if r.err != nil {
			return 0, fail("body is truncated in event %d", i)
		}
		if !finite(t) || t < lo || t > hi {
			return 0, fail("event %d time %g s is outside -%d s to finish + %d s", i, t, TailSlack, TailSlack)
		}
		switch kind {
		case EventAction:
			r.i16()
			r.u8()
		case EventRelease:
			r.i16()
		case EventSnap:
		case EventEffectApplied, EventEffectEnded:
			idx := r.varint()
			if r.err == nil && idx >= uint64(effects) {
				return 0, fail("event %d uses effect %d of %d", i, idx, effects)
			}
		default:
			return 0, fail("event %d has unknown kind %d", i, kind)
		}
		if r.err != nil {
			return 0, fail("body is truncated in event %d", i)
		}
	}
	return int(n), nil
}
