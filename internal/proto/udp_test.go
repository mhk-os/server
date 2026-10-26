package proto

import (
	"bytes"
	"encoding/hex"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/mhk-os/server/internal/wire"
)

func TestKartStatePinnedBytes(t *testing.T) {
	st := KartState{
		Tick: 0x01020304, Pos: Vec3{1, -2, 0.5}, Rot: [4]int16{32767, 0, -32767, 1}, Vel: [3]int16{100, -200, 300},
		Keys: 0x05, Flags: KartControls | KartAirborne, Steer: -127, Lap: 2, RaceDistance: 1234.5, Health: 200,
		Effects: []uint16{3, 0x0102},
	}
	want := strings.Join([]string{
		"04030201",                           // tick
		"0000803f" + "000000c0" + "0000003f", // pos
		"ff7f" + "0000" + "0180" + "0100",    // rot
		"6400" + "38ff" + "2c01",             // vel
		"05", "22", "81", "02",               // keys, flags, steer, lap
		"00509a44",             // raceDistance
		"c8",                   // health
		"02" + "0300" + "0201", // effects
	}, "")
	w := wire.NewWriter(64)
	st.Encode(w)
	if got := hex.EncodeToString(w.Bytes()); got != want {
		t.Fatalf("encoded\n%s\nwant\n%s", got, want)
	}
	var back KartState
	if err := back.Decode(wire.NewReader(w.Bytes())); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back, st) {
		t.Fatalf("round trip %+v, want %+v", back, st)
	}
	if got := st.Speed(); got < 3.74 || got > 3.75 { // |(1, -2, 3)| m/s
		t.Fatalf("speed %v", got)
	}
}

func TestKartStateRejectsTruncatedEffects(t *testing.T) {
	st := KartState{Effects: []uint16{1, 2}}
	w := wire.NewWriter(64)
	st.Encode(w)
	b := w.Bytes()[:w.Len()-1]
	if err := new(KartState).Decode(wire.NewReader(b)); err == nil {
		t.Fatal("truncated effects accepted")
	}
}

func TestUDPDatagramMAC(t *testing.T) {
	key := bytes.Repeat([]byte{7}, UDPKeySize)
	mac := NewUDPMac(key)
	h := UDPHeader{Type: UDPPing, Token: 0x1122334455667788, Seq: 9}
	ping := UDPPingPayload{ClientTime: time.UnixMilli(1000).UTC(), EchoServerTime: time.UnixMilli(2000).UTC(), EchoHoldMs: 30}
	w := wire.NewWriter(32)
	ping.Encode(w)
	d := mac.Seal(append(AppendUDPHeader(nil, h), w.Bytes()...))
	if len(d) != UDPHeaderSize+18+UDPMacSize {
		t.Fatalf("length %d", len(d))
	}
	if hex.EncodeToString(d[:UDPHeaderSize]) != "01"+"8877665544332211"+"09000000" {
		t.Fatalf("header % x", d[:UDPHeaderSize])
	}
	if !NewUDPMac(key).Verify(d) {
		t.Fatal("valid MAC rejected")
	}
	gh, payload, ok := ParseUDP(d)
	if !ok || gh != h {
		t.Fatalf("parse %+v %v", gh, ok)
	}
	var back UDPPingPayload
	if err := back.Decode(wire.NewReader(payload)); err != nil || back != ping {
		t.Fatalf("ping %+v %v", back, err)
	}
	for i := range d {
		bad := bytes.Clone(d)
		bad[i] ^= 1
		if mac.Verify(bad) {
			t.Fatalf("flipped byte %d accepted", i)
		}
	}
	if NewUDPMac(bytes.Repeat([]byte{8}, UDPKeySize)).Verify(d) {
		t.Fatal("wrong key accepted")
	}
	if _, _, ok := ParseUDP(make([]byte, UDPMaxDatagram+1)); ok {
		t.Fatal("oversized datagram parsed")
	}
	if _, _, ok := ParseUDP(make([]byte, UDPHeaderSize+UDPMacSize-1)); ok {
		t.Fatal("short datagram parsed")
	}
}

func TestUDPPingWithoutEcho(t *testing.T) {
	// A client predating the echo fields sends only clientTime.
	w := wire.NewWriter(8)
	w.Time(time.UnixMilli(5))
	var p UDPPingPayload
	if err := p.Decode(wire.NewReader(w.Bytes())); err != nil || !p.EchoServerTime.IsZero() || p.EchoHoldMs != 0 {
		t.Fatalf("%+v %v", p, err)
	}
}

func TestStateAndWorldRoundTrip(t *testing.T) {
	st := UDPStatePayload{State: KartState{Tick: 5, Lap: 1, Health: 9},
		Reports: []ProjectileReport{{UseID: 1<<24 | 1, Tick: 4, Pos: Vec3{1, 2, 3}, Vel: Vec3{4, 5, 6}}}}
	w := wire.NewWriter(64)
	st.Encode(w)
	var back UDPStatePayload
	if err := back.Decode(wire.NewReader(w.Bytes())); err != nil || !reflect.DeepEqual(back, st) {
		t.Fatalf("%+v %v", back, err)
	}
	world := UDPWorldPayload{ServerTime: time.UnixMilli(77).UTC(), Entries: []WorldEntry{
		{Slot: 1, RTTMs: 40, State: KartState{Tick: 3}},
		{Slot: 2, RTTMs: 0, State: KartState{Tick: 4, Effects: []uint16{1}}},
	}}
	w = wire.NewWriter(64)
	world.Encode(w)
	var wb UDPWorldPayload
	if err := wb.Decode(wire.NewReader(w.Bytes())); err != nil || !reflect.DeepEqual(wb, world) {
		t.Fatalf("%+v %v", wb, err)
	}
}

func TestRaceMessagesRoundTrip(t *testing.T) {
	msgs := []struct{ in, out Message }{
		{&RaceSetup{RaceID: 1, LobbyID: 2, TrackID: "mhk2.egypt", Mode: 1, EngineClass: 2, Laps: 3, Items: true,
			ItemOdds: []ItemOdds{{"mhk2.rocket", 50}}, Anticheat: true, AICount: 0, Seed: 99,
			Grid: []RaceSlot{{Slot: 0, AccountID: 5, Username: "a", Country: "DE", CharacterID: "c", KartID: "k",
				Platform: PlatformLinux}},
			YourSlot: 0, UDPPort: 7350, UDPToken: 42, UDPKey: bytes.Repeat([]byte{1}, 32), SetRace: 1, SetSize: 4},
			&RaceSetup{}},
		{&RaceGo{RaceID: 1, Go: time.UnixMilli(123456).UTC()}, &RaceGo{}},
		{&RaceLoaded{RaceID: 1, TrackHash: "abc", Laps: 3,
			Boxes:    []ItemBoxInfo{{Pos: Vec3{1, 2, 3}, RespawnTenths: 30, Fixed: "@special"}},
			Pool:     []PoolEntry{{PowerupID: "p", Charges: 1, BattleCharges: 3, Weights: [11]float32{1, 2}}},
			Specials: []Special{{Slot: 0, PowerupID: "s"}}, Rules: ItemRules{ReplacesHeld: true, RouletteMs: 900},
			Effects: []string{"e1", "e2"}}, &RaceLoaded{}},
		{&Finished{RaceID: 1, FinishMs: 61000, LapMs: []int32{20000, 21000, 20000}, Tick: 3660, Position: 2},
			&Finished{}},
		{&ItemUse{RaceID: 1, UseID: 1<<24 | 3, Tick: 9, Pos: Vec3{1, 1, 1}, Rot: Quat{0, 0, 0, 1}, Backwards: true,
			TargetSlot: NoSlot}, &ItemUse{}},
		{&HitSeen{RaceID: 1, VictimSlot: 2, AttackerSlot: 1, UseID: 7, EffectID: "x", Tick: 4}, &HitSeen{}},
		{&CheatReport{RaceID: 1, SuspectSlot: 3, Reason: FlagReason(1), Tick: 2, Detail: "speed"}, &CheatReport{}},
		{&PlayerLeftRace{RaceID: 1, Slot: 2, Outcome: 3, Message: "bye"}, &PlayerLeftRace{}},
	}
	for _, m := range msgs {
		b := Encode(m.in)
		if err := m.out.Decode(wire.NewReader(b)); err != nil {
			t.Fatalf("%T: %v", m.in, err)
		}
		if !reflect.DeepEqual(m.in, m.out) {
			t.Fatalf("%T: %+v, want %+v", m.in, m.out, m.in)
		}
	}
}
