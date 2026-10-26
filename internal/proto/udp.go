package proto

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"hash"
	"math"
	"time"

	"github.com/mhk-os/server/internal/wire"
)

// Realtime UDP framing (docs/protocol.md, "Realtime UDP"): type u8, token u64, seq u32, payload, 8-byte MAC.
const (
	UDPMaxDatagram = 1200
	UDPHeaderSize  = 1 + 8 + 4
	UDPMacSize     = 8

	UDPPing  uint8 = 1
	UDPPong  uint8 = 2
	UDPState uint8 = 3
	UDPWorld uint8 = 4
)

// KartState flag bits.
const (
	KartCut        uint8 = 1 << 0
	KartControls   uint8 = 1 << 1
	KartFinished   uint8 = 1 << 2
	KartEliminated uint8 = 1 << 3
	KartIntangible uint8 = 1 << 4
	KartAirborne   uint8 = 1 << 5
	KartLookBack   uint8 = 1 << 6
)

// UDPHeader is the clear part in front of every datagram's payload.
type UDPHeader struct {
	Type  uint8
	Token uint64
	Seq   uint32
}

// ParseUDP splits a datagram into header and payload (MAC excluded). It does not verify the MAC: the key
// depends on the token.
func ParseUDP(b []byte) (h UDPHeader, payload []byte, ok bool) {
	if len(b) < UDPHeaderSize+UDPMacSize || len(b) > UDPMaxDatagram {
		return h, nil, false
	}
	h.Type = b[0]
	h.Token = binary.LittleEndian.Uint64(b[1:])
	h.Seq = binary.LittleEndian.Uint32(b[9:])
	return h, b[UDPHeaderSize : len(b)-UDPMacSize], true
}

// AppendUDPHeader starts a datagram in dst; append the payload, then UDPMac.Seal.
func AppendUDPHeader(dst []byte, h UDPHeader) []byte {
	dst = append(dst, h.Type)
	dst = binary.LittleEndian.AppendUint64(dst, h.Token)
	return binary.LittleEndian.AppendUint32(dst, h.Seq)
}

// UDPMac computes datagram MACs with one racer key: the first 8 bytes of HMAC-SHA256(udpKey, preceding bytes).
// It reuses its buffers, so one UDPMac must not be used by two goroutines at once.
type UDPMac struct {
	h   hash.Hash
	sum []byte
}

// NewUDPMac returns a MAC for key (RaceSetup.udpKey).
func NewUDPMac(key []byte) *UDPMac {
	return &UDPMac{h: hmac.New(sha256.New, key), sum: make([]byte, 0, sha256.Size)}
}

func (m *UDPMac) compute(b []byte) []byte {
	m.h.Reset()
	m.h.Write(b)
	m.sum = m.h.Sum(m.sum[:0])
	return m.sum[:UDPMacSize]
}

// Seal appends the MAC over b to b.
func (m *UDPMac) Seal(b []byte) []byte { return append(b, m.compute(b)...) }

// Verify checks the trailing MAC of a whole datagram in constant time.
func (m *UDPMac) Verify(b []byte) bool {
	if len(b) < UDPHeaderSize+UDPMacSize {
		return false
	}
	n := len(b) - UDPMacSize
	return hmac.Equal(m.compute(b[:n]), b[n:])
}

// UDPPingPayload is the payload of type 1. EchoServerTime is the serverTime of the newest Pong the client received
// (zero = none yet) and EchoHoldMs how long the client held it before this Ping: the server's RTT sample is
// receive time − EchoServerTime − EchoHoldMs. Both are optional trailing fields.
type UDPPingPayload struct {
	ClientTime     time.Time
	EchoServerTime time.Time
	EchoHoldMs     uint16
}

func (m *UDPPingPayload) Encode(w *wire.Writer) {
	w.Time(m.ClientTime)
	optTime(w, m.EchoServerTime)
	w.U16(m.EchoHoldMs)
}

func (m *UDPPingPayload) Decode(r *wire.Reader) error {
	m.ClientTime = r.Time()
	if r.Remaining() >= 8+2 {
		m.EchoServerTime = readOptTime(r)
		m.EchoHoldMs = r.U16()
	}
	return r.Err()
}

// UDPPongPayload is the payload of type 2.
type UDPPongPayload struct {
	ClientTime time.Time
	ServerTime time.Time
}

func (m *UDPPongPayload) Encode(w *wire.Writer) { w.Time(m.ClientTime); w.Time(m.ServerTime) }
func (m *UDPPongPayload) Decode(r *wire.Reader) error {
	m.ClientTime = r.Time()
	m.ServerTime = r.Time()
	return r.Err()
}

// KartState is a racer's own kart snapshot (30 Hz).
type KartState struct {
	Tick         uint32
	Pos          Vec3
	Rot          [4]int16 // quaternion × 32767
	Vel          [3]int16 // cm/s
	Keys         uint8
	Flags        uint8
	Steer        int8
	Lap          uint8
	RaceDistance float32
	Health       uint8
	Effects      []uint16 // indexes into RaceLoaded.effects
}

func (m *KartState) Encode(w *wire.Writer) {
	w.U32(m.Tick)
	writeVec3(w, m.Pos)
	for _, c := range m.Rot {
		w.U16(uint16(c))
	}
	for _, c := range m.Vel {
		w.U16(uint16(c))
	}
	w.U8(m.Keys)
	w.U8(m.Flags)
	w.U8(uint8(m.Steer))
	w.U8(m.Lap)
	w.F32(m.RaceDistance)
	w.U8(m.Health)
	w.U8(uint8(len(m.Effects)))
	for _, e := range m.Effects {
		w.U16(e)
	}
}

// Decode reads a KartState, reusing m.Effects' storage.
func (m *KartState) Decode(r *wire.Reader) error {
	m.Tick = r.U32()
	m.Pos = readVec3(r)
	for i := range m.Rot {
		m.Rot[i] = int16(r.U16())
	}
	for i := range m.Vel {
		m.Vel[i] = int16(r.U16())
	}
	m.Keys = r.U8()
	m.Flags = r.U8()
	m.Steer = int8(r.U8())
	m.Lap = r.U8()
	m.RaceDistance = r.FiniteF32()
	m.Health = r.U8()
	n := int(r.U8())
	if r.Remaining() < 2*n {
		r.Fail(wire.ErrTruncated)
		n = 0
	}
	m.Effects = m.Effects[:0]
	for range n {
		m.Effects = append(m.Effects, r.U16())
	}
	return r.Err()
}

// Speed is the magnitude of Vel in m/s.
func (m *KartState) Speed() float64 {
	var s float64
	for _, c := range m.Vel {
		v := float64(c) / 100
		s += v * v
	}
	return math.Sqrt(s)
}

// ProjectileReport is a client's view of a live projectile, piggybacked on State datagrams.
type ProjectileReport struct {
	UseID uint32
	Tick  uint32
	Pos   Vec3
	Vel   Vec3
}

const projectileReportSize = 4 + 4 + 12 + 12

func (m *ProjectileReport) Encode(w *wire.Writer) {
	w.U32(m.UseID)
	w.U32(m.Tick)
	writeVec3(w, m.Pos)
	writeVec3(w, m.Vel)
}

func (m *ProjectileReport) Decode(r *wire.Reader) error {
	m.UseID = r.U32()
	m.Tick = r.U32()
	m.Pos = readVec3(r)
	m.Vel = readVec3(r)
	return r.Err()
}

// UDPStatePayload is the payload of type 3.
type UDPStatePayload struct {
	State   KartState
	Reports []ProjectileReport
}

func (m *UDPStatePayload) Encode(w *wire.Writer) {
	m.State.Encode(w)
	w.U8(uint8(len(m.Reports)))
	for i := range m.Reports {
		m.Reports[i].Encode(w)
	}
}

// Decode reads the payload, reusing the slices of m.
func (m *UDPStatePayload) Decode(r *wire.Reader) error {
	m.State.Decode(r)
	n := int(r.U8())
	if r.Remaining() < n*projectileReportSize {
		r.Fail(wire.ErrTruncated)
		n = 0
	}
	m.Reports = m.Reports[:0]
	for range n {
		var p ProjectileReport
		p.Decode(r)
		m.Reports = append(m.Reports, p)
	}
	return r.Err()
}

// WorldEntry is one other racer in a World datagram.
type WorldEntry struct {
	Slot  uint8
	RTTMs uint16
	State KartState
}

// UDPWorldPayload is the payload of type 4. The server writes it from raw state bytes (AppendWorldEntry); tests
// and bots decode it.
type UDPWorldPayload struct {
	ServerTime time.Time
	Entries    []WorldEntry
}

func (m *UDPWorldPayload) Encode(w *wire.Writer) {
	w.Time(m.ServerTime)
	w.U8(uint8(len(m.Entries)))
	for i := range m.Entries {
		w.U8(m.Entries[i].Slot)
		w.U16(m.Entries[i].RTTMs)
		m.Entries[i].State.Encode(w)
	}
}

func (m *UDPWorldPayload) Decode(r *wire.Reader) error {
	m.ServerTime = r.Time()
	n := int(r.U8())
	m.Entries = make([]WorldEntry, 0, min(n, MaxRacers))
	for range n {
		var e WorldEntry
		e.Slot = r.U8()
		e.RTTMs = r.U16()
		e.State.Decode(r)
		if r.Err() != nil {
			break
		}
		m.Entries = append(m.Entries, e)
	}
	return r.Err()
}
