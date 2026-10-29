package proto

import (
	"time"

	"github.com/mhk-os/server/internal/wire"
)

// Content and lobby requests (docs/protocol.md, "Multiplayer (phase 3)").
const (
	TypeContentHello     Type = 256
	TypeContentManifests Type = 257
	TypeListLobbies      Type = 258
	TypeJoinLobby        Type = 259
	TypeQuickJoin        Type = 260
	TypeCreateRoom       Type = 261
	TypeUpdateRoom       Type = 262
	TypeLeaveLobby       Type = 263
	TypeSetLoadout       Type = 264
	TypeSetReady         Type = 265
	TypeVote             Type = 266
	TypeStartRace        Type = 267
	TypeEndRaceEarly     Type = 268
	TypeKickMember       Type = 269
)

// Lobby pushes, and RaceResults from the race-control push range (the lobby computes the points).
const (
	PushLobbyState          Type = 256
	PushLobbyLeft           Type = 257
	PushSoloRaceJoinWaiting Type = 258
	PushRaceResults         Type = 523
)

// Field and list limits of the content and lobby messages.
const (
	MaxPackID         = 64
	MaxPackCommit     = 40
	MaxManifestHash   = 32
	MaxFingerprint    = 32
	MaxAnnouncedPacks = 64
	MaxManifestItems  = 4096
	MaxStrikeMessage  = 256
	MaxRoomName       = 32
	MaxLobbyName      = 64
	MaxRoomPacks      = 32
	MaxLobbies        = 500
	MaxLobbyMembers   = 50
	MaxRoomCode       = 8
	MaxRaceRows       = 50
	MaxAIDrivers      = 49 // LobbyState.aiDrivers: every kart but one human
	MaxCustomTracks   = 35 // RoomSettings.trackIds of a custom-list championship
	MaxKickReason     = 256
	MaxLobbyMessage   = 256

	packRefMinSize       = 1 + 1 + 1
	strikeMinSize        = 1 + 1
	manifestItemMinSize  = 1 + 1 + 1
	packManifestMinSize  = 1 + 1 + 4 + 1 + 1
	lobbySummaryMinSize  = 8 + 1 + 1 + 2 + 1 + 1 + 1 + 1 + 1 + 1 + 1 + 4 + 1 + 1 + 1 + 1
	lobbyMemberMinSize   = 8 + 1 + 1 + 1 + 1 + 1 + 1 + 1 + 1 + 2 + 4 + 8 + 1
	lobbyVoteMinSize     = 8 + 1
	raceResultRowMinSize = 1 + 8 + 1 + 1 + 4 + 4 + 4 + 1 + 1 + 1
	aiDriverMinSize      = 1 + 1 + 4
)

// Manifest item kinds.
const (
	ContentTrack     uint8 = 1 // race track
	ContentKart      uint8 = 2
	ContentCharacter uint8 = 3
	ContentPowerup   uint8 = 4
	ContentEffect    uint8 = 5
	ContentCup       uint8 = 6
	ContentArena     uint8 = 7 // battle arena: a track with the arena layout
)

// Room modes (RoomSettings.mode, LobbySummary.mode).
const (
	RoomRaceSet      uint8 = 0
	RoomBattleSet    uint8 = 1
	RoomChampionship uint8 = 2
)

// Lobby kinds.
const (
	LobbyPublic  uint8 = 0
	LobbyPrivate uint8 = 1
)

// Lobby phases (LobbySummary.phase).
const (
	PhaseWaiting    uint8 = 0
	PhaseVoting     uint8 = 1
	PhaseLoading    uint8 = 2
	PhaseCountdown  uint8 = 3
	PhaseRacing     uint8 = 4
	PhaseResults    uint8 = 5
	PhaseSetResults uint8 = 6
)

// Track pick modes (RoomSettings.trackPick).
const (
	PickOwner uint8 = 0
	PickVote  uint8 = 1
)

// LobbyLeft reasons.
const (
	LeftSelf        uint8 = 0
	LeftOwnerKick   uint8 = 1
	LeftModKick     uint8 = 2
	LeftLobbyClosed uint8 = 3
)

func encodeStrings(w *wire.Writer, list []string) {
	w.Count(len(list))
	for _, s := range list {
		w.String(s)
	}
}

func decodeStrings(r *wire.Reader, maxCount, maxLen int) []string {
	list := make([]string, r.Count(maxCount, 1))
	for i := range list {
		list[i] = r.String(maxLen)
	}
	return list
}

// PackRef is one announced pack of ContentHello.
type PackRef struct {
	PackID       string
	Commit       string
	ManifestHash string
}

func (m *PackRef) Encode(w *wire.Writer) {
	w.String(m.PackID)
	w.String(m.Commit)
	w.String(m.ManifestHash)
}

func (m *PackRef) Decode(r *wire.Reader) error {
	m.PackID = r.String(MaxPackID)
	m.Commit = r.String(MaxPackCommit)
	m.ManifestHash = r.String(MaxManifestHash)
	return r.Err()
}

// ContentHello is request 256: the content packs the client has enabled.
type ContentHello struct {
	ContentVersion int32
	Packs          []PackRef
}

func (m *ContentHello) Encode(w *wire.Writer) {
	w.I32(m.ContentVersion)
	w.Count(len(m.Packs))
	for i := range m.Packs {
		m.Packs[i].Encode(w)
	}
}

func (m *ContentHello) Decode(r *wire.Reader) error {
	m.ContentVersion = r.I32()
	m.Packs = make([]PackRef, r.Count(MaxAnnouncedPacks, packRefMinSize))
	for i := range m.Packs {
		m.Packs[i].Decode(r)
	}
	return r.Err()
}

// ContentStrike names a pack whose announced copy differs from the catalog majority.
type ContentStrike struct {
	PackID  string
	Message string
}

// ContentHelloResult is the response to ContentHello.
type ContentHelloResult struct {
	Missing []string // manifest hashes the server wants (ContentManifests)
	Strikes []ContentStrike
}

func (m *ContentHelloResult) Encode(w *wire.Writer) {
	encodeStrings(w, m.Missing)
	w.Count(len(m.Strikes))
	for _, s := range m.Strikes {
		w.String(s.PackID)
		w.String(s.Message)
	}
}

func (m *ContentHelloResult) Decode(r *wire.Reader) error {
	m.Missing = decodeStrings(r, MaxAnnouncedPacks, MaxManifestHash)
	m.Strikes = make([]ContentStrike, r.Count(MaxAnnouncedPacks, strikeMinSize))
	for i := range m.Strikes {
		m.Strikes[i].PackID = r.String(MaxPackID)
		m.Strikes[i].Message = r.String(MaxStrikeMessage)
	}
	return r.Err()
}

// ManifestItem is one gameplay item a pack registers.
type ManifestItem struct {
	Kind        uint8
	ID          string
	Fingerprint string
}

// PackManifest describes the gameplay content of one pack version.
type PackManifest struct {
	PackID         string
	Commit         string
	ContentVersion int32
	ManifestHash   string
	Items          []ManifestItem
}

func (m *PackManifest) Encode(w *wire.Writer) {
	w.String(m.PackID)
	w.String(m.Commit)
	w.I32(m.ContentVersion)
	w.String(m.ManifestHash)
	EncodeManifestItems(w, m.Items)
}

func (m *PackManifest) Decode(r *wire.Reader) error {
	m.PackID = r.String(MaxPackID)
	m.Commit = r.String(MaxPackCommit)
	m.ContentVersion = r.I32()
	m.ManifestHash = r.String(MaxManifestHash)
	m.Items = DecodeManifestItems(r)
	return r.Err()
}

// EncodeManifestItems writes list<(kind, id, fingerprint)>; the server also stores manifests in this form.
func EncodeManifestItems(w *wire.Writer, items []ManifestItem) {
	w.Count(len(items))
	for _, it := range items {
		w.U8(it.Kind)
		w.String(it.ID)
		w.String(it.Fingerprint)
	}
}

// DecodeManifestItems reads what EncodeManifestItems writes.
func DecodeManifestItems(r *wire.Reader) []ManifestItem {
	items := make([]ManifestItem, r.Count(MaxManifestItems, manifestItemMinSize))
	for i := range items {
		items[i].Kind = r.U8()
		items[i].ID = r.String(MaxContentID)
		items[i].Fingerprint = r.String(MaxFingerprint)
	}
	return items
}

// ContentManifests is request 257: the manifests ContentHello asked for.
type ContentManifests struct {
	Manifests []PackManifest
}

func (m *ContentManifests) Encode(w *wire.Writer) {
	w.Count(len(m.Manifests))
	for i := range m.Manifests {
		m.Manifests[i].Encode(w)
	}
}

func (m *ContentManifests) Decode(r *wire.Reader) error {
	m.Manifests = make([]PackManifest, r.Count(MaxAnnouncedPacks, packManifestMinSize))
	for i := range m.Manifests {
		m.Manifests[i].Decode(r)
	}
	return r.Err()
}

// RoomSettings configures a private room (and describes the effective settings of public lobbies).
type RoomSettings struct {
	Name        string
	Mode        uint8
	Packs       []string
	Anticheat   bool
	EngineClass uint8
	Laps        uint8
	Items       bool
	ItemOdds    []ItemOdds
	MaxAI       uint8 // most AI karts in a race, 0 = no AI, at most MaxPlayers − 1
	MaxPlayers  uint8
	TrackPick   uint8
	RacesPerSet uint8
	CupID       string
	TrackIDs    []string // championship without a cup: the custom track list in race order
}

func (m *RoomSettings) Encode(w *wire.Writer) {
	w.String(m.Name)
	w.U8(m.Mode)
	encodeStrings(w, m.Packs)
	w.Bool(m.Anticheat)
	w.U8(m.EngineClass)
	w.U8(m.Laps)
	w.Bool(m.Items)
	EncodeItemOdds(w, m.ItemOdds)
	w.U8(m.MaxAI)
	w.U8(m.MaxPlayers)
	w.U8(m.TrackPick)
	w.U8(m.RacesPerSet)
	w.String(m.CupID)
	encodeStrings(w, m.TrackIDs)
}

func (m *RoomSettings) Decode(r *wire.Reader) error {
	m.Name = r.String(MaxRoomName)
	m.Mode = r.U8()
	m.Packs = decodeStrings(r, MaxRoomPacks, MaxPackID)
	m.Anticheat = r.Bool()
	m.EngineClass = r.U8()
	m.Laps = r.U8()
	m.Items = r.Bool()
	m.ItemOdds = DecodeItemOdds(r)
	m.MaxAI = r.U8()
	m.MaxPlayers = r.U8()
	m.TrackPick = r.U8()
	m.RacesPerSet = r.U8()
	m.CupID = r.String(MaxContentID)
	m.TrackIDs = decodeStrings(r, MaxCustomTracks, MaxTrackID)
	return r.Err()
}

// LobbySummary is one lobby in ListLobbies and the head of LobbyState.
type LobbySummary struct {
	LobbyID        uint64
	Kind           uint8
	Category       uint8
	Instance       uint16
	Name           string
	Mode           uint8
	Anticheat      bool
	Packs          []string
	Players        uint8
	MaxPlayers     uint8
	Phase          uint8
	ContentVersion int32
	EngineClass    uint8
	SetRace        uint8 // as LobbyState.SetRace
	SetSize        uint8
	MaxAI          uint8 // as RoomSettings.MaxAI
}

func (m *LobbySummary) Encode(w *wire.Writer) {
	w.U64(m.LobbyID)
	w.U8(m.Kind)
	w.U8(m.Category)
	w.U16(m.Instance)
	w.String(m.Name)
	w.U8(m.Mode)
	w.Bool(m.Anticheat)
	encodeStrings(w, m.Packs)
	w.U8(m.Players)
	w.U8(m.MaxPlayers)
	w.U8(m.Phase)
	w.I32(m.ContentVersion)
	w.U8(m.EngineClass)
	w.U8(m.SetRace)
	w.U8(m.SetSize)
	w.U8(m.MaxAI)
}

func (m *LobbySummary) Decode(r *wire.Reader) error {
	m.LobbyID = r.U64()
	m.Kind = r.U8()
	m.Category = r.U8()
	m.Instance = r.U16()
	m.Name = r.String(MaxLobbyName)
	m.Mode = r.U8()
	m.Anticheat = r.Bool()
	m.Packs = decodeStrings(r, MaxRoomPacks, MaxPackID)
	m.Players = r.U8()
	m.MaxPlayers = r.U8()
	m.Phase = r.U8()
	m.ContentVersion = r.I32()
	m.EngineClass = r.U8()
	m.SetRace = r.U8()
	m.SetSize = r.U8()
	m.MaxAI = r.U8()
	return r.Err()
}

// LobbyList is the response to ListLobbies.
type LobbyList struct {
	Lobbies []LobbySummary
}

func (m *LobbyList) Encode(w *wire.Writer) {
	w.Count(len(m.Lobbies))
	for i := range m.Lobbies {
		m.Lobbies[i].Encode(w)
	}
}

func (m *LobbyList) Decode(r *wire.Reader) error {
	m.Lobbies = make([]LobbySummary, r.Count(MaxLobbies, lobbySummaryMinSize))
	for i := range m.Lobbies {
		m.Lobbies[i].Decode(r)
	}
	return r.Err()
}

// LobbyMember is one player in LobbyState.
type LobbyMember struct {
	AccountID   uint64
	Username    string
	Country     string
	CharacterID string
	KartID      string
	Ready       bool
	Owner       bool
	Moderator   bool
	InRace      bool
	PingMs      uint16
	SetPoints   int32
	Joined      time.Time
	Platform    uint8 // Platform* constant of the member's device
}

func (m *LobbyMember) Encode(w *wire.Writer) {
	w.U64(m.AccountID)
	w.String(m.Username)
	w.String(m.Country)
	w.String(m.CharacterID)
	w.String(m.KartID)
	w.Bool(m.Ready)
	w.Bool(m.Owner)
	w.Bool(m.Moderator)
	w.Bool(m.InRace)
	w.U16(m.PingMs)
	w.I32(m.SetPoints)
	w.Time(m.Joined)
	w.U8(m.Platform)
}

func (m *LobbyMember) Decode(r *wire.Reader) error {
	m.AccountID = r.U64()
	m.Username = r.String(MaxAccountName)
	m.Country = r.String(MaxCountry)
	m.CharacterID = r.String(MaxContentID)
	m.KartID = r.String(MaxContentID)
	m.Ready = r.Bool()
	m.Owner = r.Bool()
	m.Moderator = r.Bool()
	m.InRace = r.Bool()
	m.PingMs = r.U16()
	m.SetPoints = r.I32()
	m.Joined = r.Time()
	m.Platform = r.U8()
	return r.Err()
}

// LobbyVote is one member's track vote; TrackID "" = Random.
type LobbyVote struct {
	AccountID uint64
	TrackID   string
}

// RaceResultRow is one racer's result with the points the lobby gave. AI rows have AccountID 0 and Username "".
type RaceResultRow struct {
	Slot        uint8
	AccountID   uint64
	Username    string
	Position    uint8
	FinishMs    int32
	Points      int32
	SetPoints   int32
	Outcome     uint8
	AI          bool
	CharacterID string
}

func (m *RaceResultRow) Encode(w *wire.Writer) {
	w.U8(m.Slot)
	w.U64(m.AccountID)
	w.String(m.Username)
	w.U8(m.Position)
	w.I32(m.FinishMs)
	w.I32(m.Points)
	w.I32(m.SetPoints)
	w.U8(m.Outcome)
	w.Bool(m.AI)
	w.String(m.CharacterID)
}

func (m *RaceResultRow) Decode(r *wire.Reader) error {
	m.Slot = r.U8()
	m.AccountID = r.U64()
	m.Username = r.String(MaxAccountName)
	m.Position = r.U8()
	m.FinishMs = r.I32()
	m.Points = r.I32()
	m.SetPoints = r.I32()
	m.Outcome = r.U8()
	m.AI = r.Bool()
	m.CharacterID = r.String(MaxContentID)
	return r.Err()
}

func encodeRows(w *wire.Writer, rows []RaceResultRow) {
	w.Count(len(rows))
	for i := range rows {
		rows[i].Encode(w)
	}
}

func decodeRows(r *wire.Reader, max int) []RaceResultRow {
	rows := make([]RaceResultRow, r.Count(max, raceResultRowMinSize))
	for i := range rows {
		rows[i].Decode(r)
	}
	return rows
}

// AIDriver is one AI driver of the lobby's current set (LobbyState.aiDrivers, in grid order).
type AIDriver struct {
	CharacterID string
	KartID      string
	SetPoints   int32
}

// LobbyState is push 256 and the answer of JoinLobby, QuickJoin and CreateRoom.
type LobbyState struct {
	Summary     LobbySummary
	Code        string
	Settings    RoomSettings
	Members     []LobbyMember
	PhaseEnds   time.Time // zero = no timer
	SetRace     uint8
	SetSize     uint8
	Votes       []LobbyVote
	TrackID     string
	RaceID      uint64
	LastResults []RaceResultRow
	AIDrivers   []AIDriver
}

func (m *LobbyState) Encode(w *wire.Writer) {
	m.Summary.Encode(w)
	w.String(m.Code)
	m.Settings.Encode(w)
	w.Count(len(m.Members))
	for i := range m.Members {
		m.Members[i].Encode(w)
	}
	optTime(w, m.PhaseEnds)
	w.U8(m.SetRace)
	w.U8(m.SetSize)
	w.Count(len(m.Votes))
	for _, v := range m.Votes {
		w.U64(v.AccountID)
		w.String(v.TrackID)
	}
	w.String(m.TrackID)
	w.U64(m.RaceID)
	encodeRows(w, m.LastResults)
	w.Count(len(m.AIDrivers))
	for _, d := range m.AIDrivers {
		w.String(d.CharacterID)
		w.String(d.KartID)
		w.I32(d.SetPoints)
	}
}

func (m *LobbyState) Decode(r *wire.Reader) error {
	m.Summary.Decode(r)
	m.Code = r.String(MaxRoomCode)
	m.Settings.Decode(r)
	m.Members = make([]LobbyMember, r.Count(MaxLobbyMembers, lobbyMemberMinSize))
	for i := range m.Members {
		m.Members[i].Decode(r)
	}
	m.PhaseEnds = readOptTime(r)
	m.SetRace = r.U8()
	m.SetSize = r.U8()
	m.Votes = make([]LobbyVote, r.Count(MaxLobbyMembers, lobbyVoteMinSize))
	for i := range m.Votes {
		m.Votes[i].AccountID = r.U64()
		m.Votes[i].TrackID = r.String(MaxTrackID)
	}
	m.TrackID = r.String(MaxTrackID)
	m.RaceID = r.U64()
	m.LastResults = decodeRows(r, MaxRaceRows)
	m.AIDrivers = make([]AIDriver, r.Count(MaxAIDrivers, aiDriverMinSize))
	for i := range m.AIDrivers {
		m.AIDrivers[i].CharacterID = r.String(MaxContentID)
		m.AIDrivers[i].KartID = r.String(MaxContentID)
		m.AIDrivers[i].SetPoints = r.I32()
	}
	return r.Err()
}

// JoinLobby is request 259; a non-empty Code wins over LobbyID.
type JoinLobby struct {
	LobbyID uint64
	Code    string
}

func (m *JoinLobby) Encode(w *wire.Writer) {
	w.U64(m.LobbyID)
	w.String(m.Code)
}

func (m *JoinLobby) Decode(r *wire.Reader) error {
	m.LobbyID = r.U64()
	m.Code = r.String(MaxRoomCode)
	return r.Err()
}

// QuickJoin is request 260.
type QuickJoin struct {
	Category uint8
}

func (m *QuickJoin) Encode(w *wire.Writer)       { w.U8(m.Category) }
func (m *QuickJoin) Decode(r *wire.Reader) error { m.Category = r.U8(); return r.Err() }

// SetLoadout is request 264; "" clears a choice.
type SetLoadout struct {
	CharacterID string
	KartID      string
}

func (m *SetLoadout) Encode(w *wire.Writer) {
	w.String(m.CharacterID)
	w.String(m.KartID)
}

func (m *SetLoadout) Decode(r *wire.Reader) error {
	m.CharacterID = r.String(MaxContentID)
	m.KartID = r.String(MaxContentID)
	return r.Err()
}

// SetReady is request 265.
type SetReady struct {
	Ready bool
}

func (m *SetReady) Encode(w *wire.Writer)       { w.Bool(m.Ready) }
func (m *SetReady) Decode(r *wire.Reader) error { m.Ready = r.Bool(); return r.Err() }

// TrackChoice is the body of Vote (266) and StartRace (267); "" = Random (Vote) or none (StartRace).
type TrackChoice struct {
	TrackID string
}

func (m *TrackChoice) Encode(w *wire.Writer)       { w.String(m.TrackID) }
func (m *TrackChoice) Decode(r *wire.Reader) error { m.TrackID = r.String(MaxTrackID); return r.Err() }

// KickMember is request 269.
type KickMember struct {
	AccountID uint64
	Reason    string
}

func (m *KickMember) Encode(w *wire.Writer) {
	w.U64(m.AccountID)
	w.String(m.Reason)
}

func (m *KickMember) Decode(r *wire.Reader) error {
	m.AccountID = r.U64()
	m.Reason = r.String(MaxKickReason)
	return r.Err()
}

// LobbyLeft is push 257: the player is no longer in the lobby.
type LobbyLeft struct {
	Reason  uint8
	Message string
}

func (m *LobbyLeft) Encode(w *wire.Writer) {
	w.U8(m.Reason)
	w.String(m.Message)
}

func (m *LobbyLeft) Decode(r *wire.Reader) error {
	m.Reason = r.U8()
	m.Message = r.String(MaxLobbyMessage)
	return r.Err()
}

// SoloRaceJoinWaiting is push 258 to the only human of a race: Username joined the lobby and waits.
type SoloRaceJoinWaiting struct {
	Username string
}

func (m *SoloRaceJoinWaiting) Encode(w *wire.Writer) { w.String(m.Username) }
func (m *SoloRaceJoinWaiting) Decode(r *wire.Reader) error {
	m.Username = r.String(MaxAccountName)
	return r.Err()
}

// RaceResults is push 523: the results of a finished race with the points of the lobby.
type RaceResults struct {
	RaceID  uint64
	Rows    []RaceResultRow
	SetDone bool
}

func (m *RaceResults) Encode(w *wire.Writer) {
	w.U64(m.RaceID)
	encodeRows(w, m.Rows)
	w.Bool(m.SetDone)
}

func (m *RaceResults) Decode(r *wire.Reader) error {
	m.RaceID = r.U64()
	m.Rows = decodeRows(r, MaxRaceRows)
	m.SetDone = r.Bool()
	return r.Err()
}
