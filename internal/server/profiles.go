package server

import (
	"context"
	"errors"

	"github.com/mhk-os/server/internal/proto"
	"github.com/mhk-os/server/internal/store"
	"github.com/mhk-os/server/internal/wire"
)

const msgBadSort = "Unknown sort order."

func (s *Server) registerProfiles() {
	s.Handle(proto.TypeSetProfileListed, true, s.handleSetProfileListed)
	s.Handle(proto.TypeListPlayers, false, s.handleListPlayers)
	s.Handle(proto.TypeGetProfiles, false, s.handleGetProfiles)
	s.Handle(proto.TypeGetProfile, false, s.handleGetProfile)
	s.Handle(proto.TypeScoreLeaderboard, false, s.handleScoreLeaderboard)
}

// profileAccount loads an account whose runs are asked for: disabled and anonymous accounts exist for moderators
// only.
func (s *Server) profileAccount(ctx context.Context, id uint64, moderator bool) (*store.Account, error) {
	a, err := s.deps.Accounts.AccountByID(ctx, id)
	if errors.Is(err, store.ErrNotFound) ||
		err == nil && (a.Disabled() || a.Role == proto.RoleAnonymous) && !moderator {
		return nil, Errorf(proto.StatusNotFound, msgAccountNotFound)
	}
	return a, err
}

// summary adds online, which only the hub knows.
func (s *Server) summary(p *store.PlayerSummary) proto.PlayerSummary {
	out := proto.PlayerSummary{
		AccountID: p.AccountID, Username: p.Username, Country: p.Country, Role: p.Role, Created: p.CreatedAt,
		Online: s.hub.Online(p.AccountID), Score: p.Score, Rank: p.Rank,
	}
	if p.LastActive != nil {
		out.LastActive = *p.LastActive
	}
	return out
}

func (s *Server) summaries(players []store.PlayerSummary) []proto.PlayerSummary {
	out := make([]proto.PlayerSummary, len(players))
	for i := range players {
		out[i] = s.summary(&players[i])
	}
	return out
}

func (s *Server) handleSetProfileListed(ctx context.Context, c *Conn, r *wire.Reader) (proto.Message, error) {
	var req proto.SetProfileListed
	if err := req.Decode(r); err != nil {
		return nil, err
	}
	id, _ := c.AccountID()
	return nil, s.deps.Profiles.SetProfileListed(ctx, id, req.Listed)
}

func (s *Server) handleListPlayers(ctx context.Context, _ *Conn, r *wire.Reader) (proto.Message, error) {
	var req proto.ListPlayers
	if err := req.Decode(r); err != nil {
		return nil, err
	}
	if req.Limit < 1 || req.Limit > proto.MaxPage {
		return nil, Errorf(proto.StatusBadRequest, msgBadLimit)
	}
	if req.Sort != proto.PlayerSortNewest && req.Sort != proto.PlayerSortName {
		return nil, Errorf(proto.StatusBadRequest, msgBadSort)
	}
	total, players, err := s.deps.Profiles.ListPlayers(ctx, store.PlayerQuery{
		Offset: int(req.Offset), Limit: int(req.Limit), Search: req.Search, ByName: req.Sort == proto.PlayerSortName,
	})
	if err != nil {
		return nil, err
	}
	return &proto.PlayerPage{Total: clampU32(total), Players: s.summaries(players)}, nil
}

func (s *Server) handleGetProfiles(ctx context.Context, c *Conn, r *wire.Reader) (proto.Message, error) {
	var req proto.GetProfiles
	if err := req.Decode(r); err != nil {
		return nil, err
	}
	players, err := s.deps.Profiles.PlayerSummaries(ctx, req.AccountIDs, isModerator(c))
	if err != nil {
		return nil, err
	}
	return &proto.PlayerSummaries{Players: s.summaries(players)}, nil
}

func (s *Server) handleGetProfile(ctx context.Context, c *Conn, r *wire.Reader) (proto.Message, error) {
	var req proto.AccountID
	if err := req.Decode(r); err != nil {
		return nil, err
	}
	mod := isModerator(c)
	p, err := s.deps.Profiles.Profile(ctx, req.AccountID)
	if errors.Is(err, store.ErrNotFound) || err == nil && (p.Disabled && !mod || p.Role == proto.RoleAnonymous) {
		return nil, Errorf(proto.StatusNotFound, msgAccountNotFound)
	}
	if err != nil {
		return nil, err
	}
	out := &proto.PlayerProfile{
		Player: s.summary(&p.PlayerSummary), Listed: p.Listed, Races: p.Races, Wins: p.Wins, Podiums: p.Podiums,
		TTRuns: p.TTRuns, TTBoards: p.TTBoards, TTBestRank: p.TTBestRank,
	}
	if mod {
		out.Disabled, out.DisabledReason = p.Disabled, p.DisabledReason
		out.CheatKicks, out.ContentStrikes, out.HiddenRuns, out.FlaggedRuns = p.CheatKicks, p.ContentStrikes,
			p.HiddenRuns, p.FlaggedRuns
	}
	return out, nil
}

func (s *Server) handleScoreLeaderboard(ctx context.Context, c *Conn, r *wire.Reader) (proto.Message, error) {
	var req proto.ScoreLeaderboard
	if err := req.Decode(r); err != nil {
		return nil, err
	}
	if req.Limit < 1 || req.Limit > proto.MaxPage {
		return nil, Errorf(proto.StatusBadRequest, msgBadLimit)
	}
	total, players, err := s.deps.Profiles.ScoreLeaderboard(ctx, int(req.Offset), int(req.Limit), req.Search)
	if err != nil {
		return nil, err
	}
	out := &proto.ScoreRanking{Total: clampU32(total), Players: s.summaries(players)}
	if id, ok := c.AccountID(); ok {
		mine, err := s.deps.Profiles.PlayerSummaries(ctx, []uint64{id}, false)
		if err != nil {
			return nil, err
		}
		if len(mine) == 1 && mine[0].Rank > 0 {
			p := s.summary(&mine[0])
			out.Mine = &p
		}
	}
	return out, nil
}
