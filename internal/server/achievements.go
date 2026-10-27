package server

import (
	"context"
	"errors"

	"github.com/mhk-os/server/internal/proto"
	"github.com/mhk-os/server/internal/store"
	"github.com/mhk-os/server/internal/wire"
)

// Player-facing messages of the achievement requests.
const (
	msgBadStatID        = "Invalid achievement or stat id."
	msgBadUnlockTime    = "Invalid unlock time."
	msgAchievementLimit = "This account holds too much achievement data."
)

func (s *Server) registerAchievements() {
	s.Handle(proto.TypeSyncAchievements, true, s.handleSyncAchievements)
	s.Handle(proto.TypeAchievementShares, false, s.handleAchievementShares)
}

// syncInput validates a SyncAchievements request and converts it for the store. Unlock times in the future become
// now. A request that alone exceeds an account cap is refused before touching the database.
func syncInput(req *proto.SyncAchievements) (*store.AchievementData, error) {
	badID := Errorf(proto.StatusBadRequest, msgBadStatID)
	in := &store.AchievementData{}
	for _, c := range req.Counters {
		if !proto.ValidID(c.Stat, proto.MaxStatID) {
			return nil, badID
		}
		in.Counters = append(in.Counters, store.StatCount{Stat: c.Stat, Value: uint64(c.Value)})
	}
	for _, m := range req.Maxima {
		if !proto.ValidID(m.Stat, proto.MaxStatID) {
			return nil, badID
		}
		in.Maxima = append(in.Maxima, store.StatCount{Stat: m.Stat, Value: uint64(m.Value)})
	}
	items := 0
	for _, set := range req.Sets {
		if !proto.ValidID(set.Stat, proto.MaxStatID) {
			return nil, badID
		}
		for _, item := range set.Items {
			if !proto.ValidID(item, proto.MaxSetItem) {
				return nil, badID
			}
		}
		items += len(set.Items)
		in.Sets = append(in.Sets, store.StatItems{Stat: set.Stat, Items: set.Items})
	}
	if items > proto.MaxAccountSetItems {
		return nil, Errorf(proto.StatusBadRequest, msgAchievementLimit)
	}
	now := store.Now()
	for _, u := range req.Unlocked {
		if !proto.ValidID(u.ID, proto.MaxAchievementID) {
			return nil, badID
		}
		if u.At.UnixMilli() <= 0 {
			return nil, Errorf(proto.StatusBadRequest, msgBadUnlockTime)
		}
		at := u.At.UTC()
		if at.After(now) {
			at = now
		}
		in.Unlocked = append(in.Unlocked, store.Unlock{ID: u.ID, At: at})
	}
	return in, nil
}

func (s *Server) handleSyncAchievements(ctx context.Context, c *Conn, r *wire.Reader) (proto.Message, error) {
	var req proto.SyncAchievements
	if err := req.Decode(r); err != nil {
		return nil, err
	}
	in, err := syncInput(&req)
	if err != nil {
		return nil, err
	}
	id, _ := c.AccountID()
	data, err := s.deps.Achievements.SyncAchievements(ctx, id, in, store.Now())
	switch {
	case errors.Is(err, store.ErrAchievementLimit):
		return nil, Errorf(proto.StatusBadRequest, msgAchievementLimit)
	case errors.Is(err, store.ErrNotFound):
		return nil, Errorf(proto.StatusSessionInvalid, msgSessionInvalid)
	case err != nil:
		return nil, err
	}
	out := &proto.AchievementState{
		Counters: make([]proto.StatTotal, len(data.Counters)),
		Maxima:   make([]proto.StatValue, len(data.Maxima)),
		Sets:     make([]proto.StatSet, len(data.Sets)),
		Unlocked: make([]proto.Unlock, len(data.Unlocked)),
	}
	for i, c := range data.Counters {
		out.Counters[i] = proto.StatTotal{Stat: c.Stat, Value: c.Value}
	}
	for i, m := range data.Maxima {
		out.Maxima[i] = proto.StatValue{Stat: m.Stat, Value: uint32(m.Value)}
	}
	for i, set := range data.Sets {
		out.Sets[i] = proto.StatSet{Stat: set.Stat, Items: set.Items}
	}
	for i, u := range data.Unlocked {
		out.Unlocked[i] = proto.Unlock{ID: u.ID, At: u.At}
	}
	return out, nil
}

func (s *Server) handleAchievementShares(ctx context.Context, _ *Conn, r *wire.Reader) (proto.Message, error) {
	var req proto.AchievementSharesRequest
	if err := req.Decode(r); err != nil {
		return nil, err
	}
	players, shares, err := s.deps.Achievements.AchievementShares(ctx, req.IDs, proto.MaxShareIDs)
	if err != nil {
		return nil, err
	}
	out := &proto.AchievementShares{Players: clampU32(players), Shares: make([]proto.AchievementShare, len(shares))}
	for i, sh := range shares {
		out.Shares[i] = proto.AchievementShare{ID: sh.ID, Unlocked: clampU32(sh.Unlocked)}
	}
	return out, nil
}
