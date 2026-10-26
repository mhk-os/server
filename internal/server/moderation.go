package server

import (
	"context"
	"errors"
	"fmt"

	"github.com/mhk-os/server/internal/proto"
	"github.com/mhk-os/server/internal/store"
	"github.com/mhk-os/server/internal/wire"
)

// Player-facing messages of the moderation requests.
const (
	msgNotModerator     = "This needs a moderator account."
	msgAccountNotFound  = "This account does not exist."
	msgDisableModerator = "Moderator accounts cannot be disabled by moderators."
)

func (s *Server) registerModeration() {
	s.Handle(proto.TypeModHideRun, true, s.moderator(s.handleModHideRun))
	s.Handle(proto.TypeModUnhideRun, true, s.moderator(s.handleModUnhideRun))
	s.Handle(proto.TypeModDisableAccount, true, s.moderator(s.handleModDisableAccount))
	s.Handle(proto.TypeModEnableAccount, true, s.moderator(s.handleModEnableAccount))
	s.Handle(proto.TypeModListFlagged, true, s.moderator(s.handleModListFlagged))
	s.Handle(proto.TypeModRunReports, true, s.moderator(s.handleModRunReports))
	s.Handle(proto.TypeModGetAccount, true, s.moderator(s.handleModGetAccount))
}

// moderator wraps a handler that only moderators may call.
func (s *Server) moderator(fn HandlerFunc) HandlerFunc {
	return func(ctx context.Context, c *Conn, r *wire.Reader) (proto.Message, error) {
		if !isModerator(c) {
			return nil, Errorf(proto.StatusForbidden, msgNotModerator)
		}
		return fn(ctx, c, r)
	}
}

// modAction starts an audit entry for the moderator on c.
func modAction(c *Conn, action, reason string) store.Action {
	id, _ := c.AccountID()
	return store.Action{Actor: &id, Action: action, Reason: storedLabel(reason, maxStoredReason), CreatedAt: store.Now()}
}

func runNotFound(err error) error {
	if errors.Is(err, store.ErrNotFound) {
		return Errorf(proto.StatusNotFound, msgRunNotFound)
	}
	return err
}

func (s *Server) handleModHideRun(ctx context.Context, c *Conn, r *wire.Reader) (proto.Message, error) {
	var req proto.ModHideRun
	if err := req.Decode(r); err != nil {
		return nil, err
	}
	if err := s.deps.Runs.HideRun(ctx, req.RunID, modAction(c, store.ActionHideRun, req.Reason)); err != nil {
		return nil, runNotFound(err)
	}
	c.log.Info("run hidden", "run", req.RunID, "reason", req.Reason)
	return nil, nil
}

func (s *Server) handleModUnhideRun(ctx context.Context, c *Conn, r *wire.Reader) (proto.Message, error) {
	var req proto.RunID
	if err := req.Decode(r); err != nil {
		return nil, err
	}
	if err := s.deps.Runs.UnhideRun(ctx, req.RunID, modAction(c, store.ActionUnhideRun, "")); err != nil {
		return nil, runNotFound(err)
	}
	c.log.Info("run unhidden", "run", req.RunID)
	return nil, nil
}

func (s *Server) targetAccount(ctx context.Context, id uint64) (*store.Account, error) {
	a, err := s.deps.Accounts.AccountByID(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, Errorf(proto.StatusNotFound, msgAccountNotFound)
	}
	return a, err
}

func (s *Server) handleModDisableAccount(ctx context.Context, c *Conn, r *wire.Reader) (proto.Message, error) {
	var req proto.ModDisableAccount
	if err := req.Decode(r); err != nil {
		return nil, err
	}
	target, err := s.targetAccount(ctx, req.AccountID)
	if err != nil {
		return nil, err
	}
	if target.Role == proto.RoleModerator {
		return nil, Errorf(proto.StatusForbidden, msgDisableModerator)
	}
	act := modAction(c, store.ActionDisableAccount, req.Reason)
	act.TargetAccount, act.TargetRef = &target.ID, target.Username
	if err := s.deps.Moderation.DisableAccount(ctx, target.ID, act); err != nil {
		return nil, err
	}
	dropped := s.hub.Disconnect(target.ID, proto.StatusAccountDisabled, disabledMessage(act.Reason))
	c.log.Info("account disabled", "account", target.ID, "username", target.Username, "reason", act.Reason,
		"connections", dropped)
	return nil, nil
}

func (s *Server) handleModEnableAccount(ctx context.Context, c *Conn, r *wire.Reader) (proto.Message, error) {
	var req proto.AccountID
	if err := req.Decode(r); err != nil {
		return nil, err
	}
	target, err := s.targetAccount(ctx, req.AccountID)
	if err != nil {
		return nil, err
	}
	act := modAction(c, store.ActionEnableAccount, "")
	act.TargetAccount, act.TargetRef = &target.ID, target.Username
	if err := s.deps.Moderation.EnableAccount(ctx, target.ID, act); err != nil {
		return nil, err
	}
	c.log.Info("account enabled", "account", target.ID, "username", target.Username)
	return nil, nil
}

func (s *Server) handleModListFlagged(ctx context.Context, _ *Conn, r *wire.Reader) (proto.Message, error) {
	var req proto.ModListFlagged
	if err := req.Decode(r); err != nil {
		return nil, err
	}
	if req.Limit < 1 || req.Limit > proto.MaxPage {
		return nil, Errorf(proto.StatusBadRequest, msgBadLimit)
	}
	total, runs, err := s.deps.Runs.FlaggedRuns(ctx, int(req.Offset), int(req.Limit), req.IncludeHidden)
	if err != nil {
		return nil, err
	}
	return &proto.FlaggedRuns{Total: clampU32(total), Runs: runEntries(runs)}, nil
}

func (s *Server) handleModRunReports(ctx context.Context, _ *Conn, r *wire.Reader) (proto.Message, error) {
	var req proto.RunID
	if err := req.Decode(r); err != nil {
		return nil, err
	}
	if _, err := s.deps.Runs.RunByID(ctx, req.RunID); err != nil {
		return nil, runNotFound(err)
	}
	reports, err := s.deps.Runs.RunReports(ctx, req.RunID, proto.MaxRunReports)
	if err != nil {
		return nil, err
	}
	out := &proto.RunReports{Reports: make([]proto.RunReport, len(reports))}
	for i, rep := range reports {
		out.Reports[i] = proto.RunReport{
			Reporter: rep.Reporter, Reason: rep.Reason, AtSeconds: rep.AtSeconds, Detail: rep.Detail,
			Created: rep.CreatedAt, Counted: rep.Counted,
		}
	}
	return out, nil
}

func (s *Server) handleModGetAccount(ctx context.Context, _ *Conn, r *wire.Reader) (proto.Message, error) {
	var req proto.AccountID
	if err := req.Decode(r); err != nil {
		return nil, err
	}
	a, err := s.targetAccount(ctx, req.AccountID)
	if err != nil {
		return nil, err
	}
	st, err := s.deps.Runs.AccountStats(ctx, a.ID)
	if err != nil {
		return nil, fmt.Errorf("account stats: %w", err)
	}
	info := &proto.ModAccountInfo{
		Account: a.Proto(), Disabled: a.Disabled(), DisabledReason: a.DisabledReason,
		Runs: clampU32(st.Runs), HiddenRuns: clampU32(st.HiddenRuns), CheatKicks: clampU32(st.CheatKicks),
		Created: a.CreatedAt,
	}
	if a.LastLoginAt != nil {
		info.LastLogin = *a.LastLoginAt
	}
	return info, nil
}
