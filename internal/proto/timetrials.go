package proto

import (
	"time"

	"github.com/mhk-os/server/internal/wire"
)

// Field and list limits of the time-trial and moderation messages (docs/protocol.md, phase 2).
const (
	MaxTrackID        = 96
	MaxContentID      = 96 // kartId, characterId
	MaxHash           = 32 // trackHash, ghost id
	MaxContinent      = 2
	MaxLapTimes       = 99
	MaxGhostFile      = 8 << 20
	MaxReportDetail   = 512
	MaxModReason      = 256
	MaxBoards         = 200
	MaxPage           = 100 // limit of every paged request (leaderboards, run pages, player lists)
	MaxMyRuns         = 500
	MaxRunReports     = 500
	AnyEngineClass    = 255
	AnyContentVersion = 0
	boardInfoMinSize  = 8 + 1 + 1 + 1 + 4 + 1 + 4 + 4 + 8
	runEntryMinSize   = 8 + 4 + 8 + 1 + 1 + 4 + 4 + 1 + 1 + 1 + 1 + 8 + 8 + 4 + 1 + 1 + 4 + 8 + 1 + 1
	runReportMinSize  = 1 + 1 + 4 + 1 + 8 + 1
)

// optTime writes "0 when none" times: the zero time.Time encodes as 0.
func optTime(w *wire.Writer, t time.Time) {
	if t.IsZero() {
		w.I64(0)
		return
	}
	w.Time(t)
}

// readOptTime is the inverse of optTime: 0 decodes as the zero time.Time.
func readOptTime(r *wire.Reader) time.Time {
	t := r.Time()
	if t.UnixMilli() == 0 {
		return time.Time{}
	}
	return t
}

// ListBoards is request 128. EngineClass AnyEngineClass and ContentVersion AnyContentVersion match every board.
type ListBoards struct {
	TrackID        string
	EngineClass    uint8
	ContentVersion int32
}

func (m *ListBoards) Encode(w *wire.Writer) {
	w.String(m.TrackID)
	w.U8(m.EngineClass)
	w.I32(m.ContentVersion)
}

func (m *ListBoards) Decode(r *wire.Reader) error {
	m.TrackID = r.String(MaxTrackID)
	m.EngineClass = r.U8()
	m.ContentVersion = r.I32()
	return r.Err()
}

// BoardInfo describes one leaderboard. Players, BestMs and LastUpload count visible runs only.
type BoardInfo struct {
	BoardID        uint64
	TrackID        string
	EngineClass    uint8
	Laps           uint8
	ContentVersion int32
	TrackHash      string
	Players        uint32
	BestMs         int32
	LastUpload     time.Time // zero = none
}

func (m *BoardInfo) Encode(w *wire.Writer) {
	w.U64(m.BoardID)
	w.String(m.TrackID)
	w.U8(m.EngineClass)
	w.U8(m.Laps)
	w.I32(m.ContentVersion)
	w.String(m.TrackHash)
	w.U32(m.Players)
	w.I32(m.BestMs)
	optTime(w, m.LastUpload)
}

func (m *BoardInfo) Decode(r *wire.Reader) error {
	m.BoardID = r.U64()
	m.TrackID = r.String(MaxTrackID)
	m.EngineClass = r.U8()
	m.Laps = r.U8()
	m.ContentVersion = r.I32()
	m.TrackHash = r.String(MaxHash)
	m.Players = r.U32()
	m.BestMs = r.I32()
	m.LastUpload = readOptTime(r)
	return r.Err()
}

// BoardList is the response to ListBoards.
type BoardList struct {
	Boards []BoardInfo
}

func (m *BoardList) Encode(w *wire.Writer) {
	w.Count(len(m.Boards))
	for i := range m.Boards {
		m.Boards[i].Encode(w)
	}
}

func (m *BoardList) Decode(r *wire.Reader) error {
	m.Boards = make([]BoardInfo, r.Count(MaxBoards, boardInfoMinSize))
	for i := range m.Boards {
		m.Boards[i].Decode(r)
	}
	return r.Err()
}

// RunEntry is one uploaded run as clients see it.
type RunEntry struct {
	RunID         uint64
	Rank          uint32 // 0 = not ranked
	AccountID     uint64
	Username      string
	Country       string
	FinishMs      int32
	BestLapMs     int32
	LapMs         []int32
	KartID        string
	CharacterID   string
	GhostID       string
	Recorded      time.Time
	Uploaded      time.Time
	Flags         uint32
	SetupMismatch bool
	Hidden        bool
	FileSize      uint32
	BoardID       uint64
	TrackID       string // the board's track
	EngineClass   uint8  // the board's engine class
}

func (m *RunEntry) Encode(w *wire.Writer) {
	w.U64(m.RunID)
	w.U32(m.Rank)
	w.U64(m.AccountID)
	w.String(m.Username)
	w.String(m.Country)
	w.I32(m.FinishMs)
	w.I32(m.BestLapMs)
	w.Count(len(m.LapMs))
	for _, ms := range m.LapMs {
		w.I32(ms)
	}
	w.String(m.KartID)
	w.String(m.CharacterID)
	w.String(m.GhostID)
	w.Time(m.Recorded)
	w.Time(m.Uploaded)
	w.U32(m.Flags)
	w.Bool(m.SetupMismatch)
	w.Bool(m.Hidden)
	w.U32(m.FileSize)
	w.U64(m.BoardID)
	w.String(m.TrackID)
	w.U8(m.EngineClass)
}

func (m *RunEntry) Decode(r *wire.Reader) error {
	m.RunID = r.U64()
	m.Rank = r.U32()
	m.AccountID = r.U64()
	m.Username = r.String(MaxAccountName)
	m.Country = r.String(MaxCountry)
	m.FinishMs = r.I32()
	m.BestLapMs = r.I32()
	m.LapMs = make([]int32, r.Count(MaxLapTimes, 4))
	for i := range m.LapMs {
		m.LapMs[i] = r.I32()
	}
	m.KartID = r.String(MaxContentID)
	m.CharacterID = r.String(MaxContentID)
	m.GhostID = r.String(MaxHash)
	m.Recorded = r.Time()
	m.Uploaded = r.Time()
	m.Flags = r.U32()
	m.SetupMismatch = r.Bool()
	m.Hidden = r.Bool()
	m.FileSize = r.U32()
	m.BoardID = r.U64()
	m.TrackID = r.String(MaxTrackID)
	m.EngineClass = r.U8()
	return r.Err()
}

func encodeRuns(w *wire.Writer, runs []RunEntry) {
	w.Count(len(runs))
	for i := range runs {
		runs[i].Encode(w)
	}
}

func decodeRuns(r *wire.Reader, max int) []RunEntry {
	runs := make([]RunEntry, r.Count(max, runEntryMinSize))
	for i := range runs {
		runs[i].Decode(r)
	}
	return runs
}

// LeaderboardFilter narrows GetLeaderboard; empty strings match everything.
type LeaderboardFilter struct {
	Username      string
	Country       string
	Continent     string
	KartID        string
	CharacterID   string
	IncludeHidden bool // honoured for moderators only
}

func (m *LeaderboardFilter) Encode(w *wire.Writer) {
	w.String(m.Username)
	w.String(m.Country)
	w.String(m.Continent)
	w.String(m.KartID)
	w.String(m.CharacterID)
	w.Bool(m.IncludeHidden)
}

func (m *LeaderboardFilter) Decode(r *wire.Reader) error {
	m.Username = r.String(MaxAccountName)
	m.Country = r.String(MaxCountry)
	m.Continent = r.String(MaxContinent)
	m.KartID = r.String(MaxContentID)
	m.CharacterID = r.String(MaxContentID)
	m.IncludeHidden = r.Bool()
	return r.Err()
}

// GetLeaderboard is request 129.
type GetLeaderboard struct {
	BoardID uint64
	Offset  uint32
	Limit   uint16
	Filter  LeaderboardFilter
}

func (m *GetLeaderboard) Encode(w *wire.Writer) {
	w.U64(m.BoardID)
	w.U32(m.Offset)
	w.U16(m.Limit)
	m.Filter.Encode(w)
}

func (m *GetLeaderboard) Decode(r *wire.Reader) error {
	m.BoardID = r.U64()
	m.Offset = r.U32()
	m.Limit = r.U16()
	return m.Filter.Decode(r)
}

// Leaderboard is the response to GetLeaderboard. Mine is nil when the caller has no matching row (or is logged out).
type Leaderboard struct {
	Total   uint32
	Entries []RunEntry
	Mine    *RunEntry
}

func (m *Leaderboard) Encode(w *wire.Writer) {
	w.U32(m.Total)
	encodeRuns(w, m.Entries)
	w.Bool(m.Mine != nil)
	if m.Mine != nil {
		m.Mine.Encode(w)
	}
}

func (m *Leaderboard) Decode(r *wire.Reader) error {
	m.Total = r.U32()
	m.Entries = decodeRuns(r, MaxPage)
	m.Mine = nil
	if r.Bool() {
		m.Mine = &RunEntry{}
		return m.Mine.Decode(r)
	}
	return r.Err()
}

// UploadRun is request 130: a complete .mhkghost file.
type UploadRun struct {
	File []byte
}

func (m *UploadRun) Encode(w *wire.Writer)       { w.Raw(m.File) }
func (m *UploadRun) Decode(r *wire.Reader) error { m.File = r.Raw(MaxGhostFile); return r.Err() }

// UploadResult is the response to UploadRun.
type UploadResult struct {
	Run          RunEntry
	PersonalBest bool
	Duplicate    bool
}

func (m *UploadResult) Encode(w *wire.Writer) {
	m.Run.Encode(w)
	w.Bool(m.PersonalBest)
	w.Bool(m.Duplicate)
}

func (m *UploadResult) Decode(r *wire.Reader) error {
	m.Run.Decode(r)
	m.PersonalBest = r.Bool()
	m.Duplicate = r.Bool()
	return r.Err()
}

// RunID is the body of DownloadRun (131), ModUnhideRun (65) and ModRunReports (69).
type RunID struct {
	RunID uint64
}

func (m *RunID) Encode(w *wire.Writer)       { w.U64(m.RunID) }
func (m *RunID) Decode(r *wire.Reader) error { m.RunID = r.U64(); return r.Err() }

// DownloadResult is the response to DownloadRun.
type DownloadResult struct {
	Run  RunEntry
	File []byte
}

func (m *DownloadResult) Encode(w *wire.Writer) {
	m.Run.Encode(w)
	w.Raw(m.File)
}

func (m *DownloadResult) Decode(r *wire.Reader) error {
	m.Run.Decode(r)
	m.File = r.Raw(MaxGhostFile)
	return r.Err()
}

// ReportRun is request 132.
type ReportRun struct {
	RunID     uint64
	Reason    FlagReason
	AtSeconds float32
	Detail    string
}

func (m *ReportRun) Encode(w *wire.Writer) {
	w.U64(m.RunID)
	w.U8(uint8(m.Reason))
	w.F32(m.AtSeconds)
	w.String(m.Detail)
}

func (m *ReportRun) Decode(r *wire.Reader) error {
	m.RunID = r.U64()
	m.Reason = FlagReason(r.U8())
	m.AtSeconds = r.FiniteF32()
	m.Detail = r.String(MaxReportDetail)
	return r.Err()
}

// ReportResult is the response to ReportRun: the run's current flag count.
type ReportResult struct {
	Flags uint32
}

func (m *ReportResult) Encode(w *wire.Writer)       { w.U32(m.Flags) }
func (m *ReportResult) Decode(r *wire.Reader) error { m.Flags = r.U32(); return r.Err() }

// MyRuns is request 133.
type MyRuns struct {
	TrackID string
}

func (m *MyRuns) Encode(w *wire.Writer)       { w.String(m.TrackID) }
func (m *MyRuns) Decode(r *wire.Reader) error { m.TrackID = r.String(MaxTrackID); return r.Err() }

// RunList is the response to MyRuns.
type RunList struct {
	Runs []RunEntry
}

func (m *RunList) Encode(w *wire.Writer)       { encodeRuns(w, m.Runs) }
func (m *RunList) Decode(r *wire.Reader) error { m.Runs = decodeRuns(r, MaxMyRuns); return r.Err() }

// PlayerRuns is request 134.
type PlayerRuns struct {
	AccountID uint64
	Offset    uint32
	Limit     uint16
}

func (m *PlayerRuns) Encode(w *wire.Writer) {
	w.U64(m.AccountID)
	w.U32(m.Offset)
	w.U16(m.Limit)
}

func (m *PlayerRuns) Decode(r *wire.Reader) error {
	m.AccountID = r.U64()
	m.Offset = r.U32()
	m.Limit = r.U16()
	return r.Err()
}

// ModHideRun is request 64.
type ModHideRun struct {
	RunID  uint64
	Reason string
}

func (m *ModHideRun) Encode(w *wire.Writer) {
	w.U64(m.RunID)
	w.String(m.Reason)
}

func (m *ModHideRun) Decode(r *wire.Reader) error {
	m.RunID = r.U64()
	m.Reason = r.String(MaxModReason)
	return r.Err()
}

// ModDisableAccount is request 66.
type ModDisableAccount struct {
	AccountID uint64
	Reason    string
}

func (m *ModDisableAccount) Encode(w *wire.Writer) {
	w.U64(m.AccountID)
	w.String(m.Reason)
}

func (m *ModDisableAccount) Decode(r *wire.Reader) error {
	m.AccountID = r.U64()
	m.Reason = r.String(MaxModReason)
	return r.Err()
}

// AccountID is the body of ModEnableAccount (67) and ModGetAccount (70).
type AccountID struct {
	AccountID uint64
}

func (m *AccountID) Encode(w *wire.Writer)       { w.U64(m.AccountID) }
func (m *AccountID) Decode(r *wire.Reader) error { m.AccountID = r.U64(); return r.Err() }

// ModListFlagged is request 68.
type ModListFlagged struct {
	Offset        uint32
	Limit         uint16
	IncludeHidden bool
}

func (m *ModListFlagged) Encode(w *wire.Writer) {
	w.U32(m.Offset)
	w.U16(m.Limit)
	w.Bool(m.IncludeHidden)
}

func (m *ModListFlagged) Decode(r *wire.Reader) error {
	m.Offset = r.U32()
	m.Limit = r.U16()
	m.IncludeHidden = r.Bool()
	return r.Err()
}

// ModListHidden is request 71; BoardID 0 lists every board.
type ModListHidden struct {
	BoardID uint64
	Offset  uint32
	Limit   uint16
}

func (m *ModListHidden) Encode(w *wire.Writer) {
	w.U64(m.BoardID)
	w.U32(m.Offset)
	w.U16(m.Limit)
}

func (m *ModListHidden) Decode(r *wire.Reader) error {
	m.BoardID = r.U64()
	m.Offset = r.U32()
	m.Limit = r.U16()
	return r.Err()
}

// RunPage is one page of runs and the total: the response to ModListFlagged, ModListHidden and PlayerRuns.
type RunPage struct {
	Total uint32
	Runs  []RunEntry
}

func (m *RunPage) Encode(w *wire.Writer) {
	w.U32(m.Total)
	encodeRuns(w, m.Runs)
}

func (m *RunPage) Decode(r *wire.Reader) error {
	m.Total = r.U32()
	m.Runs = decodeRuns(r, MaxPage)
	return r.Err()
}

// RunReport is one report on a run, as moderators see it.
type RunReport struct {
	Reporter  string
	Reason    FlagReason
	AtSeconds float32
	Detail    string
	Created   time.Time
	Counted   bool
}

func (m *RunReport) Encode(w *wire.Writer) {
	w.String(m.Reporter)
	w.U8(uint8(m.Reason))
	w.F32(m.AtSeconds)
	w.String(m.Detail)
	w.Time(m.Created)
	w.Bool(m.Counted)
}

func (m *RunReport) Decode(r *wire.Reader) error {
	m.Reporter = r.String(MaxAccountName)
	m.Reason = FlagReason(r.U8())
	m.AtSeconds = r.FiniteF32()
	m.Detail = r.String(MaxReportDetail)
	m.Created = r.Time()
	m.Counted = r.Bool()
	return r.Err()
}

// RunReports is the response to ModRunReports.
type RunReports struct {
	Reports []RunReport
}

func (m *RunReports) Encode(w *wire.Writer) {
	w.Count(len(m.Reports))
	for i := range m.Reports {
		m.Reports[i].Encode(w)
	}
}

func (m *RunReports) Decode(r *wire.Reader) error {
	m.Reports = make([]RunReport, r.Count(MaxRunReports, runReportMinSize))
	for i := range m.Reports {
		m.Reports[i].Decode(r)
	}
	return r.Err()
}

// ModAccountInfo is the response to ModGetAccount.
type ModAccountInfo struct {
	Account        Account
	Disabled       bool
	DisabledReason string
	Runs           uint32
	HiddenRuns     uint32
	CheatKicks     uint32
	Created        time.Time
	LastLogin      time.Time // zero = never
}

func (m *ModAccountInfo) Encode(w *wire.Writer) {
	m.Account.Encode(w)
	w.Bool(m.Disabled)
	w.String(m.DisabledReason)
	w.U32(m.Runs)
	w.U32(m.HiddenRuns)
	w.U32(m.CheatKicks)
	w.Time(m.Created)
	optTime(w, m.LastLogin)
}

func (m *ModAccountInfo) Decode(r *wire.Reader) error {
	m.Account.Decode(r)
	m.Disabled = r.Bool()
	m.DisabledReason = r.String(MaxModReason)
	m.Runs = r.U32()
	m.HiddenRuns = r.U32()
	m.CheatKicks = r.U32()
	m.Created = r.Time()
	m.LastLogin = readOptTime(r)
	return r.Err()
}
