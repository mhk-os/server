package proto

import (
	"fmt"
	"time"

	"github.com/mhk-os/server/internal/wire"
)

// Limits of the profile messages (docs/protocol.md, "Profiles and score").
const (
	MaxProfileIDs              = 100 // GetProfiles
	playerSummaryMinSize       = 8 + 1 + 1 + 1 + 8 + 8 + 1 + 4 + 4
	PlayerSortNewest     uint8 = 0 // ListPlayers: newest registration first
	PlayerSortName       uint8 = 1 // ListPlayers: username A–Z
)

// PlaytimeStat is the game's time played counter (seconds in the foreground, AchievementStats.PlaytimeSeconds),
// the one achievement stat the server reads: PlayerSummary.Playtime.
const PlaytimeStat = "playtime.s"

// SetProfileListed is request 21.
type SetProfileListed struct {
	Listed bool
}

func (m *SetProfileListed) Encode(w *wire.Writer)       { w.Bool(m.Listed) }
func (m *SetProfileListed) Decode(r *wire.Reader) error { m.Listed = r.Bool(); return r.Err() }

// ListPlayers is request 22.
type ListPlayers struct {
	Offset uint32
	Limit  uint16
	Search string // case-insensitive username substring, "" = all
	Sort   uint8  // PlayerSort*
}

func (m *ListPlayers) Encode(w *wire.Writer) {
	w.U32(m.Offset)
	w.U16(m.Limit)
	w.String(m.Search)
	w.U8(m.Sort)
}

func (m *ListPlayers) Decode(r *wire.Reader) error {
	m.Offset = r.U32()
	m.Limit = r.U16()
	m.Search = r.String(MaxAccountName)
	m.Sort = r.U8()
	return r.Err()
}

// PlayerSummary is an account's public mini-profile.
type PlayerSummary struct {
	AccountID  uint64
	Username   string
	Country    string
	Role       Role
	Created    time.Time
	LastActive time.Time // zero = never
	Online     bool
	Score      uint32
	Rank       uint32 // 0 = unranked
	// Playtime is the account's PlaytimeStat total in seconds (0 = none synced). It is not part of the summary's
	// own encoding: the message carrying summaries appends them all at its end (encodePlaytimes), so readers of
	// older bodies keep working.
	Playtime uint64
}

func (m *PlayerSummary) Encode(w *wire.Writer) {
	w.U64(m.AccountID)
	w.String(m.Username)
	w.String(m.Country)
	w.U8(uint8(m.Role))
	w.Time(m.Created)
	optTime(w, m.LastActive)
	w.Bool(m.Online)
	w.U32(m.Score)
	w.U32(m.Rank)
}

func (m *PlayerSummary) Decode(r *wire.Reader) error {
	m.AccountID = r.U64()
	m.Username = r.String(MaxAccountName)
	m.Country = r.String(MaxCountry)
	m.Role = Role(r.U8())
	m.Created = r.Time()
	m.LastActive = readOptTime(r)
	m.Online = r.Bool()
	m.Score = r.U32()
	m.Rank = r.U32()
	m.Playtime = 0 // decodePlaytimes of the carrying message fills it
	return r.Err()
}

func encodePlayers(w *wire.Writer, players []PlayerSummary) {
	w.Count(len(players))
	for i := range players {
		players[i].Encode(w)
	}
}

func decodePlayers(r *wire.Reader, max int) []PlayerSummary {
	players := make([]PlayerSummary, r.Count(max, playerSummaryMinSize))
	for i := range players {
		players[i].Decode(r)
	}
	return players
}

// encodePlaytimes ends a message carrying summaries: list<u64> with the Playtime of every summary of the message in
// order (players, then mine when present).
func encodePlaytimes(w *wire.Writer, players []PlayerSummary, mine *PlayerSummary) {
	n := len(players)
	if mine != nil {
		n++
	}
	w.Count(n)
	for i := range players {
		w.U64(players[i].Playtime)
	}
	if mine != nil {
		w.U64(mine.Playtime)
	}
}

// decodePlaytimes reads what encodePlaytimes wrote. A body that ends before it (an older server) leaves every
// Playtime 0; a list of another length than the message's summaries is invalid.
func decodePlaytimes(r *wire.Reader, players []PlayerSummary, mine *PlayerSummary) {
	if r.Err() != nil || r.Remaining() == 0 {
		return
	}
	n := len(players)
	if mine != nil {
		n++
	}
	if got := r.Count(n, 8); r.Err() == nil && got != n {
		r.Fail(fmt.Errorf("%w: %d playtimes for %d players", wire.ErrInvalid, got, n))
		return
	}
	for i := range players {
		players[i].Playtime = r.U64()
	}
	if mine != nil {
		mine.Playtime = r.U64()
	}
}

// PlayerPage is the response to ListPlayers.
type PlayerPage struct {
	Total   uint32
	Players []PlayerSummary
}

func (m *PlayerPage) Encode(w *wire.Writer) {
	w.U32(m.Total)
	encodePlayers(w, m.Players)
	encodePlaytimes(w, m.Players, nil)
}

func (m *PlayerPage) Decode(r *wire.Reader) error {
	m.Total = r.U32()
	m.Players = decodePlayers(r, MaxPage)
	decodePlaytimes(r, m.Players, nil)
	return r.Err()
}

// GetProfiles is request 23.
type GetProfiles struct {
	AccountIDs []uint64
}

func (m *GetProfiles) Encode(w *wire.Writer) {
	w.Count(len(m.AccountIDs))
	for _, id := range m.AccountIDs {
		w.U64(id)
	}
}

func (m *GetProfiles) Decode(r *wire.Reader) error {
	m.AccountIDs = make([]uint64, r.Count(MaxProfileIDs, 8))
	for i := range m.AccountIDs {
		m.AccountIDs[i] = r.U64()
	}
	return r.Err()
}

// PlayerSummaries is the response to GetProfiles.
type PlayerSummaries struct {
	Players []PlayerSummary
}

func (m *PlayerSummaries) Encode(w *wire.Writer) {
	encodePlayers(w, m.Players)
	encodePlaytimes(w, m.Players, nil)
}

func (m *PlayerSummaries) Decode(r *wire.Reader) error {
	m.Players = decodePlayers(r, MaxProfileIDs)
	decodePlaytimes(r, m.Players, nil)
	return r.Err()
}

// PlayerProfile is the response to GetProfile (request 24, body AccountID). The fields from Disabled on are filled
// for moderators only.
type PlayerProfile struct {
	Player         PlayerSummary
	Listed         bool
	Races          uint32
	Wins           uint32
	Podiums        uint32
	TTRuns         uint32
	TTBoards       uint32
	TTBestRank     uint32 // 0 = none
	Disabled       bool
	DisabledReason string
	CheatKicks     uint32
	HiddenRuns     uint32
	FlaggedRuns    uint32
}

func (m *PlayerProfile) Encode(w *wire.Writer) {
	m.Player.Encode(w)
	w.Bool(m.Listed)
	w.U32(m.Races)
	w.U32(m.Wins)
	w.U32(m.Podiums)
	w.U32(m.TTRuns)
	w.U32(m.TTBoards)
	w.U32(m.TTBestRank)
	w.Bool(m.Disabled)
	w.String(m.DisabledReason)
	w.U32(m.CheatKicks)
	w.U32(m.HiddenRuns)
	w.U32(m.FlaggedRuns)
	encodePlaytimes(w, nil, &m.Player)
}

func (m *PlayerProfile) Decode(r *wire.Reader) error {
	m.Player.Decode(r)
	m.Listed = r.Bool()
	m.Races = r.U32()
	m.Wins = r.U32()
	m.Podiums = r.U32()
	m.TTRuns = r.U32()
	m.TTBoards = r.U32()
	m.TTBestRank = r.U32()
	m.Disabled = r.Bool()
	m.DisabledReason = r.String(MaxModReason)
	m.CheatKicks = r.U32()
	m.HiddenRuns = r.U32()
	m.FlaggedRuns = r.U32()
	decodePlaytimes(r, nil, &m.Player)
	return r.Err()
}

// ScoreLeaderboard is request 25.
type ScoreLeaderboard struct {
	Offset uint32
	Limit  uint16
	Search string
}

func (m *ScoreLeaderboard) Encode(w *wire.Writer) {
	w.U32(m.Offset)
	w.U16(m.Limit)
	w.String(m.Search)
}

func (m *ScoreLeaderboard) Decode(r *wire.Reader) error {
	m.Offset = r.U32()
	m.Limit = r.U16()
	m.Search = r.String(MaxAccountName)
	return r.Err()
}

// ScoreRanking is the response to ScoreLeaderboard. Mine is nil when the caller is logged out or unranked.
type ScoreRanking struct {
	Total   uint32
	Players []PlayerSummary
	Mine    *PlayerSummary
}

func (m *ScoreRanking) Encode(w *wire.Writer) {
	w.U32(m.Total)
	encodePlayers(w, m.Players)
	w.Bool(m.Mine != nil)
	if m.Mine != nil {
		m.Mine.Encode(w)
	}
	encodePlaytimes(w, m.Players, m.Mine)
}

func (m *ScoreRanking) Decode(r *wire.Reader) error {
	m.Total = r.U32()
	m.Players = decodePlayers(r, MaxPage)
	m.Mine = nil
	if r.Bool() {
		m.Mine = &PlayerSummary{}
		m.Mine.Decode(r)
	}
	decodePlaytimes(r, m.Players, m.Mine)
	return r.Err()
}
