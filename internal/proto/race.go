package proto

import (
	"time"

	"github.com/mhk-os/server/internal/wire"
)

// Race control pushes (docs/protocol.md, "Race messages (TCP)"). 523 RaceResults belongs to the lobby messages.
const (
	PushRaceSetup         Type = 512
	PushRaceGo            Type = 513
	PushItemGranted       Type = 514
	PushItemBoxTaken      Type = 515
	PushItemDenied        Type = 516
	PushItemUsed          Type = 517
	PushItemUseRejected   Type = 518
	PushProjectileCorrect Type = 519
	PushHitApplied        Type = 520
	PushHitForce          Type = 521
	PushPlayerFinished    Type = 522
	PushPlayerLeftRace    Type = 524
	PushEliminated        Type = 525
	PushBridgeTriggered   Type = 526
)

// Race notifies (client → server, kind 3).
const (
	NotifyRaceLoaded     Type = 528
	NotifyItemPickup     Type = 529
	NotifyItemUse        Type = 530
	NotifyHitTaken       Type = 531
	NotifyHitSeen        Type = 532
	NotifyFinished       Type = 533
	NotifyCheatReport    Type = 534
	NotifyUnknownContent Type = 535
	NotifyEliminatedSelf Type = 536
	NotifyBridgeTrigger  Type = 537
	NotifyProjectileGone Type = 538
)

// Limits of the race messages.
const (
	MaxRacers      = 10
	MaxItemBoxes   = 512
	MaxPool        = 128
	MaxEffects     = 1024
	MaxCheatDetail = 256
	UDPKeySize     = 32
	PoolWeights    = 11
	NoSlot         = 255 // targetSlot / attackerSlot "none"

	raceSlotMinSize = 1 + 8 + 1 + 1 + 1 + 1
	boxInfoMinSize  = 12 + 2 + 1
	poolMinSize     = 1 + 1 + 1 + PoolWeights*4
	specialMinSize  = 1 + 1
	effectMinSize   = 1
)

// ItemDenied reasons.
const (
	DenyNotAvailable  uint8 = 1
	DenyTooFar        uint8 = 2
	DenyCannotReceive uint8 = 3
)

// ItemUseRejected reasons.
const (
	RejectNotHeld  uint8 = 1
	RejectCooldown uint8 = 2
)

// Vec3 is a position or velocity (f32 x, y, z).
type Vec3 [3]float32

// Quat is a rotation (f32 x, y, z, w).
type Quat [4]float32

func writeVec3(w *wire.Writer, v Vec3) {
	for _, c := range v {
		w.F32(c)
	}
}

func readVec3(r *wire.Reader) (v Vec3) {
	for i := range v {
		v[i] = r.FiniteF32()
	}
	return v
}

func writeQuat(w *wire.Writer, q Quat) {
	for _, c := range q {
		w.F32(c)
	}
}

func readQuat(r *wire.Reader) (q Quat) {
	for i := range q {
		q[i] = r.FiniteF32()
	}
	return q
}

// RaceSlot is one entry of RaceSetup.grid.
type RaceSlot struct {
	Slot        uint8
	AccountID   uint64
	Username    string
	Country     string
	CharacterID string
	KartID      string
}

func (m *RaceSlot) Encode(w *wire.Writer) {
	w.U8(m.Slot)
	w.U64(m.AccountID)
	w.String(m.Username)
	w.String(m.Country)
	w.String(m.CharacterID)
	w.String(m.KartID)
}

func (m *RaceSlot) Decode(r *wire.Reader) error {
	m.Slot = r.U8()
	m.AccountID = r.U64()
	m.Username = r.String(MaxAccountName)
	m.Country = r.String(MaxCountry)
	m.CharacterID = r.String(MaxContentID)
	m.KartID = r.String(MaxContentID)
	return r.Err()
}

// RaceSetup is push 512, personalised per racer (yourSlot, udpToken, udpKey).
type RaceSetup struct {
	RaceID      uint64
	LobbyID     uint64
	TrackID     string
	Mode        uint8
	EngineClass uint8
	Laps        uint8
	Items       bool
	ItemOdds    []ItemOdds
	Anticheat   bool
	Solo        bool
	AICount     uint8
	Seed        uint64
	Grid        []RaceSlot
	YourSlot    uint8
	UDPPort     uint16
	UDPToken    uint64
	UDPKey      []byte
	SetRace     uint8
	SetSize     uint8
}

func (m *RaceSetup) Encode(w *wire.Writer) {
	w.U64(m.RaceID)
	w.U64(m.LobbyID)
	w.String(m.TrackID)
	w.U8(m.Mode)
	w.U8(m.EngineClass)
	w.U8(m.Laps)
	w.Bool(m.Items)
	EncodeItemOdds(w, m.ItemOdds)
	w.Bool(m.Anticheat)
	w.Bool(m.Solo)
	w.U8(m.AICount)
	w.U64(m.Seed)
	w.Count(len(m.Grid))
	for i := range m.Grid {
		m.Grid[i].Encode(w)
	}
	w.U8(m.YourSlot)
	w.U16(m.UDPPort)
	w.U64(m.UDPToken)
	w.Raw(m.UDPKey)
	w.U8(m.SetRace)
	w.U8(m.SetSize)
}

func (m *RaceSetup) Decode(r *wire.Reader) error {
	m.RaceID = r.U64()
	m.LobbyID = r.U64()
	m.TrackID = r.String(MaxTrackID)
	m.Mode = r.U8()
	m.EngineClass = r.U8()
	m.Laps = r.U8()
	m.Items = r.Bool()
	m.ItemOdds = DecodeItemOdds(r)
	m.Anticheat = r.Bool()
	m.Solo = r.Bool()
	m.AICount = r.U8()
	m.Seed = r.U64()
	m.Grid = make([]RaceSlot, r.Count(MaxRacers, raceSlotMinSize))
	for i := range m.Grid {
		m.Grid[i].Decode(r)
	}
	m.YourSlot = r.U8()
	m.UDPPort = r.U16()
	m.UDPToken = r.U64()
	m.UDPKey = r.Raw(UDPKeySize)
	m.SetRace = r.U8()
	m.SetSize = r.U8()
	return r.Err()
}

// RaceGo is push 513.
type RaceGo struct {
	RaceID uint64
	Go     time.Time // server clock instant of GO
}

func (m *RaceGo) Encode(w *wire.Writer) { w.U64(m.RaceID); w.Time(m.Go) }
func (m *RaceGo) Decode(r *wire.Reader) error {
	m.RaceID = r.U64()
	m.Go = r.Time()
	return r.Err()
}

// ItemGranted is push 514 (to the picker only).
type ItemGranted struct {
	RaceID    uint64
	BoxID     uint16
	PowerupID string
	Charges   uint8
	Tick      uint32
}

func (m *ItemGranted) Encode(w *wire.Writer) {
	w.U64(m.RaceID)
	w.U16(m.BoxID)
	w.String(m.PowerupID)
	w.U8(m.Charges)
	w.U32(m.Tick)
}

func (m *ItemGranted) Decode(r *wire.Reader) error {
	m.RaceID = r.U64()
	m.BoxID = r.U16()
	m.PowerupID = r.String(MaxContentID)
	m.Charges = r.U8()
	m.Tick = r.U32()
	return r.Err()
}

// ItemBoxTaken is push 515 (to everyone but the picker).
type ItemBoxTaken struct {
	RaceID uint64
	Slot   uint8
	BoxID  uint16
	Tick   uint32
}

func (m *ItemBoxTaken) Encode(w *wire.Writer) {
	w.U64(m.RaceID)
	w.U8(m.Slot)
	w.U16(m.BoxID)
	w.U32(m.Tick)
}

func (m *ItemBoxTaken) Decode(r *wire.Reader) error {
	m.RaceID = r.U64()
	m.Slot = r.U8()
	m.BoxID = r.U16()
	m.Tick = r.U32()
	return r.Err()
}

// ItemDenied is push 516.
type ItemDenied struct {
	RaceID uint64
	BoxID  uint16
	Reason uint8
}

func (m *ItemDenied) Encode(w *wire.Writer) { w.U64(m.RaceID); w.U16(m.BoxID); w.U8(m.Reason) }
func (m *ItemDenied) Decode(r *wire.Reader) error {
	m.RaceID = r.U64()
	m.BoxID = r.U16()
	m.Reason = r.U8()
	return r.Err()
}

// ItemUsed is push 517 (to everyone but the user).
type ItemUsed struct {
	RaceID     uint64
	Slot       uint8
	UseID      uint32
	PowerupID  string
	Tick       uint32
	Pos        Vec3
	Rot        Quat
	Backwards  bool
	TargetSlot uint8
}

func (m *ItemUsed) Encode(w *wire.Writer) {
	w.U64(m.RaceID)
	w.U8(m.Slot)
	w.U32(m.UseID)
	w.String(m.PowerupID)
	w.U32(m.Tick)
	writeVec3(w, m.Pos)
	writeQuat(w, m.Rot)
	w.Bool(m.Backwards)
	w.U8(m.TargetSlot)
}

func (m *ItemUsed) Decode(r *wire.Reader) error {
	m.RaceID = r.U64()
	m.Slot = r.U8()
	m.UseID = r.U32()
	m.PowerupID = r.String(MaxContentID)
	m.Tick = r.U32()
	m.Pos = readVec3(r)
	m.Rot = readQuat(r)
	m.Backwards = r.Bool()
	m.TargetSlot = r.U8()
	return r.Err()
}

// ItemUseRejected is push 518.
type ItemUseRejected struct {
	RaceID uint64
	UseID  uint32
	Reason uint8
}

func (m *ItemUseRejected) Encode(w *wire.Writer) { w.U64(m.RaceID); w.U32(m.UseID); w.U8(m.Reason) }
func (m *ItemUseRejected) Decode(r *wire.Reader) error {
	m.RaceID = r.U64()
	m.UseID = r.U32()
	m.Reason = r.U8()
	return r.Err()
}

// ProjectileCorrect is push 519.
type ProjectileCorrect struct {
	RaceID uint64
	UseID  uint32
	Tick   uint32
	Pos    Vec3
	Vel    Vec3
}

func (m *ProjectileCorrect) Encode(w *wire.Writer) {
	w.U64(m.RaceID)
	w.U32(m.UseID)
	w.U32(m.Tick)
	writeVec3(w, m.Pos)
	writeVec3(w, m.Vel)
}

func (m *ProjectileCorrect) Decode(r *wire.Reader) error {
	m.RaceID = r.U64()
	m.UseID = r.U32()
	m.Tick = r.U32()
	m.Pos = readVec3(r)
	m.Vel = readVec3(r)
	return r.Err()
}

// HitApplied is push 520.
type HitApplied struct {
	RaceID       uint64
	VictimSlot   uint8
	AttackerSlot uint8
	UseID        uint32
	EffectID     string
	Tick         uint32
}

func (m *HitApplied) Encode(w *wire.Writer) {
	w.U64(m.RaceID)
	w.U8(m.VictimSlot)
	w.U8(m.AttackerSlot)
	w.U32(m.UseID)
	w.String(m.EffectID)
	w.U32(m.Tick)
}

func (m *HitApplied) Decode(r *wire.Reader) error {
	m.RaceID = r.U64()
	m.VictimSlot = r.U8()
	m.AttackerSlot = r.U8()
	m.UseID = r.U32()
	m.EffectID = r.String(MaxContentID)
	m.Tick = r.U32()
	return r.Err()
}

// HitForce is push 521 (to the victim).
type HitForce struct {
	RaceID       uint64
	AttackerSlot uint8
	UseID        uint32
	EffectID     string
}

func (m *HitForce) Encode(w *wire.Writer) {
	w.U64(m.RaceID)
	w.U8(m.AttackerSlot)
	w.U32(m.UseID)
	w.String(m.EffectID)
}

func (m *HitForce) Decode(r *wire.Reader) error {
	m.RaceID = r.U64()
	m.AttackerSlot = r.U8()
	m.UseID = r.U32()
	m.EffectID = r.String(MaxContentID)
	return r.Err()
}

// PlayerFinished is push 522.
type PlayerFinished struct {
	RaceID   uint64
	Slot     uint8
	FinishMs int32
	LapMs    []int32
}

func (m *PlayerFinished) Encode(w *wire.Writer) {
	w.U64(m.RaceID)
	w.U8(m.Slot)
	w.I32(m.FinishMs)
	writeLapMs(w, m.LapMs)
}

func (m *PlayerFinished) Decode(r *wire.Reader) error {
	m.RaceID = r.U64()
	m.Slot = r.U8()
	m.FinishMs = r.I32()
	m.LapMs = readLapMs(r)
	return r.Err()
}

func writeLapMs(w *wire.Writer, laps []int32) {
	w.Count(len(laps))
	for _, l := range laps {
		w.I32(l)
	}
}

func readLapMs(r *wire.Reader) []int32 {
	n := r.Count(MaxLapTimes, 4)
	if n == 0 {
		return nil
	}
	laps := make([]int32, n)
	for i := range laps {
		laps[i] = r.I32()
	}
	return laps
}

// PlayerLeftRace is push 524.
type PlayerLeftRace struct {
	RaceID  uint64
	Slot    uint8
	Outcome uint8
	Message string
}

func (m *PlayerLeftRace) Encode(w *wire.Writer) {
	w.U64(m.RaceID)
	w.U8(m.Slot)
	w.U8(m.Outcome)
	w.String(m.Message)
}

func (m *PlayerLeftRace) Decode(r *wire.Reader) error {
	m.RaceID = r.U64()
	m.Slot = r.U8()
	m.Outcome = r.U8()
	m.Message = r.String(MaxMessage)
	return r.Err()
}

// Eliminated is push 525 (battles).
type Eliminated struct {
	RaceID       uint64
	Slot         uint8
	AttackerSlot uint8
	Tick         uint32
}

func (m *Eliminated) Encode(w *wire.Writer) {
	w.U64(m.RaceID)
	w.U8(m.Slot)
	w.U8(m.AttackerSlot)
	w.U32(m.Tick)
}

func (m *Eliminated) Decode(r *wire.Reader) error {
	m.RaceID = r.U64()
	m.Slot = r.U8()
	m.AttackerSlot = r.U8()
	m.Tick = r.U32()
	return r.Err()
}

// BridgeTriggered is push 526.
type BridgeTriggered struct {
	RaceID   uint64
	BridgeID uint16
	Tick     uint32
}

func (m *BridgeTriggered) Encode(w *wire.Writer) { w.U64(m.RaceID); w.U16(m.BridgeID); w.U32(m.Tick) }
func (m *BridgeTriggered) Decode(r *wire.Reader) error {
	m.RaceID = r.U64()
	m.BridgeID = r.U16()
	m.Tick = r.U32()
	return r.Err()
}

// ItemBoxInfo describes one item box of the loaded track; its box id is its index in RaceLoaded.Boxes.
type ItemBoxInfo struct {
	Pos           Vec3
	RespawnTenths uint16
	Fixed         string // "" random, a powerup id, "@special" or a status effect id
}

func (m *ItemBoxInfo) Encode(w *wire.Writer) {
	writeVec3(w, m.Pos)
	w.U16(m.RespawnTenths)
	w.String(m.Fixed)
}

func (m *ItemBoxInfo) Decode(r *wire.Reader) error {
	m.Pos = readVec3(r)
	m.RespawnTenths = r.U16()
	m.Fixed = r.String(MaxContentID)
	return r.Err()
}

// PoolEntry is one powerup of the item pool with its weights at position fractions 0, 0.1, … 1.
type PoolEntry struct {
	PowerupID     string
	Charges       uint8
	BattleCharges uint8
	Weights       [PoolWeights]float32
}

func (m *PoolEntry) Encode(w *wire.Writer) {
	w.String(m.PowerupID)
	w.U8(m.Charges)
	w.U8(m.BattleCharges)
	for _, x := range m.Weights {
		w.F32(x)
	}
}

func (m *PoolEntry) Decode(r *wire.Reader) error {
	m.PowerupID = r.String(MaxContentID)
	m.Charges = r.U8()
	m.BattleCharges = r.U8()
	for i := range m.Weights {
		m.Weights[i] = r.FiniteF32()
	}
	return r.Err()
}

// Special is a slot's special powerup (RaceLoaded.specials).
type Special struct {
	Slot      uint8
	PowerupID string
}

// ItemRules are the item rules of the loaded game version.
type ItemRules struct {
	ReplacesHeld bool
	RouletteMs   uint16
}

// RaceLoaded is notify 528.
type RaceLoaded struct {
	RaceID    uint64
	TrackHash string
	Laps      uint8
	Boxes     []ItemBoxInfo
	Pool      []PoolEntry
	Specials  []Special
	Rules     ItemRules
	Effects   []string
}

func (m *RaceLoaded) Encode(w *wire.Writer) {
	w.U64(m.RaceID)
	m.EncodeContent(w)
}

// EncodeContent writes every field but raceId: the part racers must agree on.
func (m *RaceLoaded) EncodeContent(w *wire.Writer) {
	w.String(m.TrackHash)
	w.U8(m.Laps)
	w.Count(len(m.Boxes))
	for i := range m.Boxes {
		m.Boxes[i].Encode(w)
	}
	w.Count(len(m.Pool))
	for i := range m.Pool {
		m.Pool[i].Encode(w)
	}
	w.Count(len(m.Specials))
	for _, s := range m.Specials {
		w.U8(s.Slot)
		w.String(s.PowerupID)
	}
	w.Bool(m.Rules.ReplacesHeld)
	w.U16(m.Rules.RouletteMs)
	w.Count(len(m.Effects))
	for _, e := range m.Effects {
		w.String(e)
	}
}

func (m *RaceLoaded) Decode(r *wire.Reader) error {
	m.RaceID = r.U64()
	m.TrackHash = r.String(MaxHash)
	m.Laps = r.U8()
	m.Boxes = make([]ItemBoxInfo, r.Count(MaxItemBoxes, boxInfoMinSize))
	for i := range m.Boxes {
		m.Boxes[i].Decode(r)
	}
	m.Pool = make([]PoolEntry, r.Count(MaxPool, poolMinSize))
	for i := range m.Pool {
		m.Pool[i].Decode(r)
	}
	m.Specials = make([]Special, r.Count(MaxRacers, specialMinSize))
	for i := range m.Specials {
		m.Specials[i].Slot = r.U8()
		m.Specials[i].PowerupID = r.String(MaxContentID)
	}
	m.Rules.ReplacesHeld = r.Bool()
	m.Rules.RouletteMs = r.U16()
	m.Effects = make([]string, r.Count(MaxEffects, effectMinSize))
	for i := range m.Effects {
		m.Effects[i] = r.String(MaxContentID)
	}
	return r.Err()
}

// ItemPickup is notify 529.
type ItemPickup struct {
	RaceID uint64
	BoxID  uint16
	Tick   uint32
	Pos    Vec3
}

func (m *ItemPickup) Encode(w *wire.Writer) {
	w.U64(m.RaceID)
	w.U16(m.BoxID)
	w.U32(m.Tick)
	writeVec3(w, m.Pos)
}

func (m *ItemPickup) Decode(r *wire.Reader) error {
	m.RaceID = r.U64()
	m.BoxID = r.U16()
	m.Tick = r.U32()
	m.Pos = readVec3(r)
	return r.Err()
}

// ItemUse is notify 530.
type ItemUse struct {
	RaceID     uint64
	UseID      uint32
	Tick       uint32
	Pos        Vec3
	Rot        Quat
	Backwards  bool
	TargetSlot uint8
}

func (m *ItemUse) Encode(w *wire.Writer) {
	w.U64(m.RaceID)
	w.U32(m.UseID)
	w.U32(m.Tick)
	writeVec3(w, m.Pos)
	writeQuat(w, m.Rot)
	w.Bool(m.Backwards)
	w.U8(m.TargetSlot)
}

func (m *ItemUse) Decode(r *wire.Reader) error {
	m.RaceID = r.U64()
	m.UseID = r.U32()
	m.Tick = r.U32()
	m.Pos = readVec3(r)
	m.Rot = readQuat(r)
	m.Backwards = r.Bool()
	m.TargetSlot = r.U8()
	return r.Err()
}

// HitTaken is notify 531.
type HitTaken struct {
	RaceID       uint64
	AttackerSlot uint8
	UseID        uint32 // 0 = track hazard or zone
	EffectID     string
	Tick         uint32
}

func (m *HitTaken) Encode(w *wire.Writer) {
	w.U64(m.RaceID)
	w.U8(m.AttackerSlot)
	w.U32(m.UseID)
	w.String(m.EffectID)
	w.U32(m.Tick)
}

func (m *HitTaken) Decode(r *wire.Reader) error {
	m.RaceID = r.U64()
	m.AttackerSlot = r.U8()
	m.UseID = r.U32()
	m.EffectID = r.String(MaxContentID)
	m.Tick = r.U32()
	return r.Err()
}

// HitSeen is notify 532.
type HitSeen struct {
	RaceID       uint64
	VictimSlot   uint8
	AttackerSlot uint8
	UseID        uint32
	EffectID     string
	Tick         uint32
}

func (m *HitSeen) Encode(w *wire.Writer) {
	w.U64(m.RaceID)
	w.U8(m.VictimSlot)
	w.U8(m.AttackerSlot)
	w.U32(m.UseID)
	w.String(m.EffectID)
	w.U32(m.Tick)
}

func (m *HitSeen) Decode(r *wire.Reader) error {
	m.RaceID = r.U64()
	m.VictimSlot = r.U8()
	m.AttackerSlot = r.U8()
	m.UseID = r.U32()
	m.EffectID = r.String(MaxContentID)
	m.Tick = r.U32()
	return r.Err()
}

// Finished is notify 533.
type Finished struct {
	RaceID   uint64
	FinishMs int32
	LapMs    []int32
	Tick     uint32
	Position uint8 // solo races: place among all karts including AI; 0 otherwise
}

func (m *Finished) Encode(w *wire.Writer) {
	w.U64(m.RaceID)
	w.I32(m.FinishMs)
	writeLapMs(w, m.LapMs)
	w.U32(m.Tick)
	w.U8(m.Position)
}

func (m *Finished) Decode(r *wire.Reader) error {
	m.RaceID = r.U64()
	m.FinishMs = r.I32()
	m.LapMs = readLapMs(r)
	m.Tick = r.U32()
	m.Position = r.U8()
	return r.Err()
}

// CheatReport is notify 534.
type CheatReport struct {
	RaceID      uint64
	SuspectSlot uint8
	Reason      FlagReason
	Tick        uint32
	Detail      string
}

func (m *CheatReport) Encode(w *wire.Writer) {
	w.U64(m.RaceID)
	w.U8(m.SuspectSlot)
	w.U8(uint8(m.Reason))
	w.U32(m.Tick)
	w.String(m.Detail)
}

func (m *CheatReport) Decode(r *wire.Reader) error {
	m.RaceID = r.U64()
	m.SuspectSlot = r.U8()
	m.Reason = FlagReason(r.U8())
	m.Tick = r.U32()
	m.Detail = r.String(MaxCheatDetail)
	return r.Err()
}

// UnknownContent is notify 535: racer Slot uses content (kind, id) the reporter does not have.
type UnknownContent struct {
	RaceID uint64
	Slot   uint8
	Kind   uint8
	ID     string
}

func (m *UnknownContent) Encode(w *wire.Writer) {
	w.U64(m.RaceID)
	w.U8(m.Slot)
	w.U8(m.Kind)
	w.String(m.ID)
}

func (m *UnknownContent) Decode(r *wire.Reader) error {
	m.RaceID = r.U64()
	m.Slot = r.U8()
	m.Kind = r.U8()
	m.ID = r.String(MaxContentID)
	return r.Err()
}

// EliminatedSelf is notify 536 (battles).
type EliminatedSelf struct {
	RaceID       uint64
	AttackerSlot uint8
	Tick         uint32
}

func (m *EliminatedSelf) Encode(w *wire.Writer) { w.U64(m.RaceID); w.U8(m.AttackerSlot); w.U32(m.Tick) }
func (m *EliminatedSelf) Decode(r *wire.Reader) error {
	m.RaceID = r.U64()
	m.AttackerSlot = r.U8()
	m.Tick = r.U32()
	return r.Err()
}

// BridgeTrigger is notify 537.
type BridgeTrigger struct {
	RaceID   uint64
	BridgeID uint16
	Tick     uint32
}

func (m *BridgeTrigger) Encode(w *wire.Writer) { w.U64(m.RaceID); w.U16(m.BridgeID); w.U32(m.Tick) }
func (m *BridgeTrigger) Decode(r *wire.Reader) error {
	m.RaceID = r.U64()
	m.BridgeID = r.U16()
	m.Tick = r.U32()
	return r.Err()
}

// ProjectileGone is notify 538.
type ProjectileGone struct {
	RaceID uint64
	UseID  uint32
	Tick   uint32
}

func (m *ProjectileGone) Encode(w *wire.Writer) { w.U64(m.RaceID); w.U32(m.UseID); w.U32(m.Tick) }
func (m *ProjectileGone) Decode(r *wire.Reader) error {
	m.RaceID = r.U64()
	m.UseID = r.U32()
	m.Tick = r.U32()
	return r.Err()
}
