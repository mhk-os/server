package server

import (
	"context"
	"errors"
	"math"
	"strings"
	"unicode/utf8"

	"github.com/mhk-os/server/internal/geo"
	"github.com/mhk-os/server/internal/ghost"
	"github.com/mhk-os/server/internal/proto"
	"github.com/mhk-os/server/internal/store"
	"github.com/mhk-os/server/internal/wire"
)

// Player-facing messages of the time-trial requests.
const (
	msgRunNotFound     = "This run does not exist or was removed."
	msgBoardNotFound   = "This leaderboard does not exist."
	msgUploadLimit     = "You uploaded too many runs in the last hour. Please try again later."
	msgGhostTaken      = "This ghost was already uploaded by another player."
	msgInvalidGhost    = "Invalid ghost file: "
	msgBadLimit        = "The page size must be 1-100."
	msgBadContinent    = "Unknown continent code."
	msgBadReportReason = "Unknown report reason."
	maxStoredGameBuild = 64  // runes kept of the ghost's gameBuild label
	maxStoredReason    = 255 // runes of a moderator reason the database keeps (the wire allows 256 bytes)
)

func (s *Server) registerTimeTrials() {
	s.Handle(proto.TypeListBoards, false, s.handleListBoards)
	s.Handle(proto.TypeGetLeaderboard, false, s.handleGetLeaderboard)
	s.Handle(proto.TypeUploadRun, true, s.handleUploadRun)
	s.Handle(proto.TypeDownloadRun, false, s.handleDownloadRun)
	s.Handle(proto.TypeReportRun, true, s.handleReportRun)
	s.Handle(proto.TypeMyRuns, true, s.handleMyRuns)
}

// isModerator reports whether the connection is logged in as a moderator.
func isModerator(c *Conn) bool {
	a := c.Account()
	return a != nil && a.Role == proto.RoleModerator
}

func runEntry(v *store.RunView) proto.RunEntry {
	return proto.RunEntry{
		RunID:         v.ID,
		Rank:          v.Rank,
		AccountID:     v.AccountID,
		Username:      v.Username,
		Country:       v.Country,
		FinishMs:      v.FinishMs,
		BestLapMs:     v.BestLapMs,
		LapMs:         v.LapMs,
		KartID:        v.KartID,
		CharacterID:   v.CharacterID,
		GhostID:       v.GhostID,
		Recorded:      v.RecordedAt,
		Uploaded:      v.UploadedAt,
		Flags:         v.Flags,
		SetupMismatch: v.SetupMismatch,
		Hidden:        v.Hidden(),
		FileSize:      v.FileSize,
		BoardID:       v.BoardID,
		TrackID:       v.BoardTrackID,
		EngineClass:   v.EngineClass,
	}
}

func runEntries(views []store.RunView) []proto.RunEntry {
	out := make([]proto.RunEntry, len(views))
	for i := range views {
		out[i] = runEntry(&views[i])
	}
	return out
}

func clampU32(n int) uint32 { return uint32(min(max(n, 0), math.MaxUint32)) }

// visibleRun loads a run for c: hidden runs exist for moderators only.
func (s *Server) visibleRun(ctx context.Context, c *Conn, id uint64) (*store.RunView, error) {
	v, err := s.deps.Runs.RunByID(ctx, id)
	if errors.Is(err, store.ErrNotFound) || err == nil && v.Hidden() && !isModerator(c) {
		return nil, Errorf(proto.StatusNotFound, msgRunNotFound)
	}
	return v, err
}

func (s *Server) handleListBoards(ctx context.Context, _ *Conn, r *wire.Reader) (proto.Message, error) {
	var req proto.ListBoards
	if err := req.Decode(r); err != nil {
		return nil, err
	}
	f := store.BoardFilter{TrackID: req.TrackID, SimVersion: req.SimVersion, Limit: proto.MaxBoards}
	if req.EngineClass != proto.AnyEngineClass {
		f.EngineClass = &req.EngineClass
	}
	boards, err := s.deps.Runs.ListBoards(ctx, f)
	if err != nil {
		return nil, err
	}
	out := &proto.BoardList{Boards: make([]proto.BoardInfo, len(boards))}
	for i, b := range boards {
		out.Boards[i] = proto.BoardInfo{
			BoardID: b.ID, TrackID: b.TrackID, EngineClass: b.EngineClass, Laps: b.Laps, SimVersion: b.SimVersion,
			TrackHash: b.TrackHash, Players: b.Players, BestMs: b.BestMs,
		}
		if b.LastUpload != nil {
			out.Boards[i].LastUpload = *b.LastUpload
		}
	}
	return out, nil
}

func (s *Server) handleGetLeaderboard(ctx context.Context, c *Conn, r *wire.Reader) (proto.Message, error) {
	var req proto.GetLeaderboard
	if err := req.Decode(r); err != nil {
		return nil, err
	}
	if req.Limit < 1 || req.Limit > proto.MaxPage {
		return nil, Errorf(proto.StatusBadRequest, msgBadLimit)
	}
	f := req.Filter
	if f.Country != "" && !geo.ValidCountry(f.Country) {
		return nil, Errorf(proto.StatusBadRequest, msgBadCountry)
	}
	if f.Continent != "" && geo.CountriesIn(f.Continent) == nil {
		return nil, Errorf(proto.StatusBadRequest, msgBadContinent)
	}
	if _, err := s.deps.Runs.BoardByID(ctx, req.BoardID); errors.Is(err, store.ErrNotFound) {
		return nil, Errorf(proto.StatusNotFound, msgBoardNotFound)
	} else if err != nil {
		return nil, err
	}
	mine, _ := c.AccountID()
	page, err := s.deps.Runs.Leaderboard(ctx, store.LeaderboardQuery{
		BoardID: req.BoardID, Offset: int(req.Offset), Limit: int(req.Limit),
		Username: f.Username, Country: f.Country, Continent: f.Continent, KartID: f.KartID,
		CharacterID: f.CharacterID, IncludeHidden: f.IncludeHidden && isModerator(c), Mine: mine,
	})
	if err != nil {
		return nil, err
	}
	out := &proto.Leaderboard{Total: clampU32(page.Total), Entries: runEntries(page.Entries)}
	if page.Mine != nil {
		e := runEntry(page.Mine)
		out.Mine = &e
	}
	return out, nil
}

// storedLabel keeps a free-form label from a ghost within the database column: valid UTF-8, at most n runes.
func storedLabel(s string, n int) string {
	s = strings.ToValidUTF8(s, "?")
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

func (s *Server) handleUploadRun(ctx context.Context, c *Conn, r *wire.Reader) (proto.Message, error) {
	var req proto.UploadRun
	if err := req.Decode(r); err != nil {
		return nil, err
	}
	acct := c.Account()
	if acct == nil {
		return nil, Errorf(proto.StatusNotLoggedIn, "Please log in first.")
	}
	now := store.Now()
	if limit := s.cfg.UploadLimit; limit.N > 0 {
		n, err := s.deps.Runs.CountUploadsSince(ctx, acct.ID, now.Add(-limit.Per))
		if err != nil {
			return nil, err
		}
		if n >= limit.N {
			return nil, Errorf(proto.StatusRateLimited, msgUploadLimit)
		}
	}
	sum, err := ghost.Parse(req.File)
	if err != nil {
		c.log.Info("ghost rejected", "err", err)
		return nil, Errorf(proto.StatusBadRequest, "%s%s.", msgInvalidGhost, err)
	}
	if res, err := s.existingUpload(ctx, acct.ID, sum.ID); res != nil || err != nil {
		return res, err
	}
	hash, err := s.deps.Blobs.Put(req.File)
	if err != nil {
		return nil, err
	}
	run := &store.Run{
		AccountID: acct.ID, GhostID: sum.ID, TrackID: sum.TrackID, TrackPackID: sum.TrackPackID, KartID: sum.KartID,
		CharacterID: sum.CharacterID, EngineClass: uint8(sum.EngineClass), Laps: uint8(sum.Laps),
		SimVersion: sum.SimVersion, TrackHash: sum.TrackHash, SetupHash: sum.SetupHash,
		GameBuild: storedLabel(sum.GameBuild, maxStoredGameBuild), FinishMs: sum.FinishMs, BestLapMs: sum.BestLapMs,
		LapMs: sum.LapMs, FrameCount: sum.FrameCount, RecordedAt: sum.Recorded, UploadedAt: now, Blob: hash,
		FileSize: uint32(len(req.File)),
	}
	if err := s.deps.Runs.CreateRun(ctx, run); errors.Is(err, store.ErrGhostExists) {
		// Uploaded concurrently (two connections of one player, or a thief racing the owner).
		res, err := s.existingUpload(ctx, acct.ID, sum.ID)
		if res == nil && err == nil {
			err = errors.New("ghost reported as existing but not found")
		}
		return res, err
	} else if err != nil {
		return nil, err
	}
	v, err := s.deps.Runs.RunByID(ctx, run.ID)
	if err != nil {
		return nil, err
	}
	c.log.Info("run uploaded", "run", run.ID, "board", run.BoardID, "track", run.TrackID, "finish_ms", run.FinishMs,
		"rank", v.Rank)
	return &proto.UploadResult{Run: runEntry(v), PersonalBest: v.Rank > 0}, nil
}

// existingUpload answers an upload of a ghost id that is already stored: the owner gets the run back as a
// duplicate, anybody else Forbidden. It returns nil, nil when the ghost is new.
func (s *Server) existingUpload(ctx context.Context, accountID uint64, ghostID string) (proto.Message, error) {
	v, err := s.deps.Runs.RunByGhostID(ctx, ghostID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if v.AccountID != accountID {
		return nil, Errorf(proto.StatusForbidden, msgGhostTaken)
	}
	return &proto.UploadResult{Run: runEntry(v), PersonalBest: v.Rank > 0, Duplicate: true}, nil
}

func (s *Server) handleDownloadRun(ctx context.Context, c *Conn, r *wire.Reader) (proto.Message, error) {
	var req proto.RunID
	if err := req.Decode(r); err != nil {
		return nil, err
	}
	v, err := s.visibleRun(ctx, c, req.RunID)
	if err != nil {
		return nil, err
	}
	file, err := s.deps.Blobs.Get(v.Blob)
	if err != nil {
		return nil, err
	}
	return &proto.DownloadResult{Run: runEntry(v), File: file}, nil
}

func (s *Server) handleReportRun(ctx context.Context, c *Conn, r *wire.Reader) (proto.Message, error) {
	var req proto.ReportRun
	if err := req.Decode(r); err != nil {
		return nil, err
	}
	if !req.Reason.Valid() {
		return nil, Errorf(proto.StatusBadRequest, msgBadReportReason)
	}
	reporter, _ := c.AccountID()
	if _, err := s.visibleRun(ctx, c, req.RunID); err != nil {
		return nil, err
	}
	rep := &store.Report{
		RunID: req.RunID, ReporterID: reporter, Reason: req.Reason, AtSeconds: req.AtSeconds,
		Detail: strings.ToValidUTF8(req.Detail, "?"), CreatedAt: store.Now(),
	}
	flags, inserted, err := s.deps.Runs.ReportRun(ctx, rep)
	if errors.Is(err, store.ErrNotFound) {
		return nil, Errorf(proto.StatusNotFound, msgRunNotFound)
	}
	if err != nil {
		return nil, err
	}
	if inserted {
		c.log.Info("run reported", "run", req.RunID, "reason", req.Reason, "at", req.AtSeconds, "counted", rep.Counted,
			"flags", flags)
	}
	return &proto.ReportResult{Flags: flags}, nil
}

func (s *Server) handleMyRuns(ctx context.Context, c *Conn, r *wire.Reader) (proto.Message, error) {
	var req proto.MyRuns
	if err := req.Decode(r); err != nil {
		return nil, err
	}
	id, _ := c.AccountID()
	runs, err := s.deps.Runs.ListRuns(ctx, store.RunFilter{AccountID: id, TrackID: req.TrackID, Limit: proto.MaxMyRuns})
	if err != nil {
		return nil, err
	}
	return &proto.RunList{Runs: runEntries(runs)}, nil
}
