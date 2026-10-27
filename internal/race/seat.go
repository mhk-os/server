package race

import (
	"net/netip"
	"sync/atomic"
	"time"

	"github.com/mhk-os/server/internal/proto"
)

// seat is one racer's place in a race. Fields are owned by the race goroutine unless marked otherwise.
type seat struct {
	slot  uint8
	racer Racer
	token uint64
	key   []byte

	// Shared with the UDP reader goroutine.
	addr  atomic.Pointer[netip.AddrPort] // latest address a valid datagram came from
	txSeq atomic.Uint32                  // server → client sequence, shared by Pong (reader) and World (race)
	rttUs atomic.Int64                   // EWMA of server-measured RTT in µs, 0 = no sample yet

	// UDP reader goroutine only.
	rxMac        *proto.UDPMac
	pongMac      *proto.UDPMac
	replay       replayWindow
	badMacLogged bool

	// Race goroutine only.
	worldMac      *proto.UDPMac
	everKicked    bool // ever cheat-kicked: reports and claims never count
	storedReports int  // cheat reports of this racer written to cheat_reports

	loaded     *proto.RaceLoaded
	loadedHash [32]byte

	out     bool
	outcome Outcome
	outAt   time.Time

	finished bool
	finishMs int32
	finishAt time.Time
	lapMs    []int32

	eliminated     bool
	elimOrder      int
	elimAt         time.Time
	zeroHealthFrom time.Time // battles: first state with health 0 of the current streak

	hasState bool
	stateBad bool // the latest State datagram did not decode
	state    proto.KartState
	stateRaw []byte // the KartState bytes as received, relayed verbatim in World

	held      string // powerup id, "" = none
	charges   uint8
	usableAt  time.Time // grant + roulette: earlier uses are rejected (cooldown)
	strikes   []time.Time
	strikeWhy []string

	soloPosition uint8
}

// rtt returns the server-measured round trip; ok is false before the first sample.
func (s *seat) rtt() (time.Duration, bool) {
	us := s.rttUs.Load()
	return time.Duration(us) * time.Microsecond, us > 0
}

// rttMs is the RTT for World and lobbies, clamped to u16 (0 = unknown).
func (s *seat) rttMs() uint16 {
	d, ok := s.rtt()
	if !ok {
		return 0
	}
	return uint16(min(max(d.Milliseconds(), 1), 65535))
}

// rttOr returns the RTT, or def before the first sample.
func (s *seat) rttOr(def time.Duration) time.Duration {
	if d, ok := s.rtt(); ok {
		return d
	}
	return def
}

// addRTTSample folds one sample into the EWMA (alpha 1/8, as TCP's SRTT). Only the UDP reader writes.
func (s *seat) addRTTSample(d time.Duration) {
	us := max(d.Microseconds(), 1)
	if old := s.rttUs.Load(); old > 0 {
		us = old + (us-old)/8
	}
	s.rttUs.Store(max(us, 1))
}

// replayWindow drops replayed and stale datagrams of one token. States must be newer than every datagram seen
// (docs: "not newer than the newest seen"); pings, which only measure, are accepted out of order within 64
// sequence numbers but never twice.
type replayWindow struct {
	seen   bool
	newest uint32
	bits   uint64 // bit i = newest − i was seen
}

func (w *replayWindow) accept(seq uint32, strict bool) bool {
	if !w.seen || seq > w.newest {
		shift := uint64(64)
		if w.seen {
			shift = uint64(seq - w.newest)
		}
		if shift >= 64 {
			w.bits = 0
		} else {
			w.bits <<= shift
		}
		w.bits |= 1
		w.newest, w.seen = seq, true
		return true
	}
	if strict {
		return false
	}
	diff := w.newest - seq
	if diff >= 64 || w.bits&(1<<diff) != 0 {
		return false
	}
	w.bits |= 1 << diff
	return true
}
