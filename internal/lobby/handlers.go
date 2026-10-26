package lobby

import (
	"context"

	"github.com/mhk-os/server/internal/proto"
	"github.com/mhk-os/server/internal/server"
	"github.com/mhk-os/server/internal/wire"
)

// Register adds the lobby request handlers (258–269) and leaves the lobby when an account's last connection goes.
func (m *Manager) Register(srv *server.Server) {
	h := func(typ proto.Type, fn func(ctx context.Context, p Player, r *wire.Reader) (proto.Message, error)) {
		srv.Handle(typ, true, func(ctx context.Context, c *server.Conn, r *wire.Reader) (proto.Message, error) {
			p, err := m.player(c)
			if err != nil {
				return nil, err
			}
			return fn(ctx, p, r)
		})
	}
	h(proto.TypeListLobbies, m.handleList)
	h(proto.TypeJoinLobby, m.handleJoin)
	h(proto.TypeQuickJoin, m.handleQuickJoin)
	h(proto.TypeCreateRoom, m.handleCreateRoom)
	h(proto.TypeUpdateRoom, m.handleUpdateRoom)
	h(proto.TypeLeaveLobby, m.handleLeave)
	h(proto.TypeSetLoadout, m.handleSetLoadout)
	h(proto.TypeSetReady, m.handleSetReady)
	h(proto.TypeVote, m.handleVote)
	h(proto.TypeStartRace, m.handleStartRace)
	h(proto.TypeEndRaceEarly, m.handleEndRaceEarly)
	h(proto.TypeKickMember, m.handleKick)
	srv.OnAccountGone(m.Leave)
}

// player identifies the caller; lobby requests need a completed ContentHello on this connection.
func (m *Manager) player(c *server.Conn) (Player, error) {
	a := c.Account()
	if a == nil {
		return Player{}, server.Errorf(proto.StatusNotLoggedIn, "Please log in first.")
	}
	if !m.cfg.Catalog.Ready(c, a.ID) {
		return Player{}, errNotAnnounced
	}
	country, _ := a.Country()
	return Player{AccountID: a.ID, Username: a.Username, Country: country, Moderator: a.Role == proto.RoleModerator},
		nil
}

func (m *Manager) handleList(_ context.Context, p Player, r *wire.Reader) (proto.Message, error) {
	if err := (proto.Empty{}).Decode(r); err != nil {
		return nil, err
	}
	list, err := m.List(p)
	if err != nil {
		return nil, err
	}
	return &proto.LobbyList{Lobbies: list}, nil
}

func (m *Manager) handleJoin(_ context.Context, p Player, r *wire.Reader) (proto.Message, error) {
	var req proto.JoinLobby
	if err := req.Decode(r); err != nil {
		return nil, err
	}
	return m.Join(p, req.LobbyID, req.Code)
}

func (m *Manager) handleQuickJoin(_ context.Context, p Player, r *wire.Reader) (proto.Message, error) {
	var req proto.QuickJoin
	if err := req.Decode(r); err != nil {
		return nil, err
	}
	return m.QuickJoin(p, req.Category)
}

func (m *Manager) handleCreateRoom(_ context.Context, p Player, r *wire.Reader) (proto.Message, error) {
	var req proto.RoomSettings
	if err := req.Decode(r); err != nil {
		return nil, err
	}
	return m.CreateRoom(p, req)
}

func (m *Manager) handleUpdateRoom(_ context.Context, p Player, r *wire.Reader) (proto.Message, error) {
	var req proto.RoomSettings
	if err := req.Decode(r); err != nil {
		return nil, err
	}
	return nil, m.UpdateRoom(p, req)
}

func (m *Manager) handleLeave(_ context.Context, p Player, r *wire.Reader) (proto.Message, error) {
	if err := (proto.Empty{}).Decode(r); err != nil {
		return nil, err
	}
	m.Leave(p.AccountID)
	return nil, nil
}

func (m *Manager) handleSetLoadout(_ context.Context, p Player, r *wire.Reader) (proto.Message, error) {
	var req proto.SetLoadout
	if err := req.Decode(r); err != nil {
		return nil, err
	}
	return nil, m.SetLoadout(p, req.CharacterID, req.KartID)
}

func (m *Manager) handleSetReady(_ context.Context, p Player, r *wire.Reader) (proto.Message, error) {
	var req proto.SetReady
	if err := req.Decode(r); err != nil {
		return nil, err
	}
	return nil, m.SetReady(p, req.Ready)
}

func (m *Manager) handleVote(_ context.Context, p Player, r *wire.Reader) (proto.Message, error) {
	var req proto.TrackChoice
	if err := req.Decode(r); err != nil {
		return nil, err
	}
	return nil, m.Vote(p, req.TrackID)
}

func (m *Manager) handleStartRace(_ context.Context, p Player, r *wire.Reader) (proto.Message, error) {
	var req proto.TrackChoice
	if err := req.Decode(r); err != nil {
		return nil, err
	}
	return nil, m.StartRace(p, req.TrackID)
}

func (m *Manager) handleEndRaceEarly(_ context.Context, p Player, r *wire.Reader) (proto.Message, error) {
	if err := (proto.Empty{}).Decode(r); err != nil {
		return nil, err
	}
	return nil, m.EndRaceEarly(p)
}

func (m *Manager) handleKick(ctx context.Context, p Player, r *wire.Reader) (proto.Message, error) {
	var req proto.KickMember
	if err := req.Decode(r); err != nil {
		return nil, err
	}
	return nil, m.Kick(ctx, p, req.AccountID, req.Reason)
}
