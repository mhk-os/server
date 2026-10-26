package testclient

import (
	"context"

	"github.com/mhk-os/server/internal/proto"
)

// ContentHello announces packs for simVersion (which must equal the Hello's).
func (c *Client) ContentHello(ctx context.Context, sim int32, packs []proto.PackRef) (*proto.ContentHelloResult, error) {
	var res proto.ContentHelloResult
	if err := c.Request(ctx, proto.TypeContentHello, &proto.ContentHello{SimVersion: sim, Packs: packs}, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

func (c *Client) ContentManifests(ctx context.Context, ms []proto.PackManifest) error {
	return c.Request(ctx, proto.TypeContentManifests, &proto.ContentManifests{Manifests: ms}, nil)
}

func (c *Client) ListLobbies(ctx context.Context) ([]proto.LobbySummary, error) {
	var res proto.LobbyList
	if err := c.Request(ctx, proto.TypeListLobbies, proto.Empty{}, &res); err != nil {
		return nil, err
	}
	return res.Lobbies, nil
}

func (c *Client) lobbyState(ctx context.Context, typ proto.Type, req proto.Message) (*proto.LobbyState, error) {
	var st proto.LobbyState
	if err := c.Request(ctx, typ, req, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

func (c *Client) JoinLobby(ctx context.Context, lobbyID uint64, code string) (*proto.LobbyState, error) {
	return c.lobbyState(ctx, proto.TypeJoinLobby, &proto.JoinLobby{LobbyID: lobbyID, Code: code})
}

func (c *Client) QuickJoin(ctx context.Context, category uint8) (*proto.LobbyState, error) {
	return c.lobbyState(ctx, proto.TypeQuickJoin, &proto.QuickJoin{Category: category})
}

func (c *Client) CreateRoom(ctx context.Context, s *proto.RoomSettings) (*proto.LobbyState, error) {
	return c.lobbyState(ctx, proto.TypeCreateRoom, s)
}

func (c *Client) UpdateRoom(ctx context.Context, s *proto.RoomSettings) error {
	return c.Request(ctx, proto.TypeUpdateRoom, s, nil)
}

func (c *Client) LeaveLobby(ctx context.Context) error {
	return c.Request(ctx, proto.TypeLeaveLobby, proto.Empty{}, nil)
}

func (c *Client) SetLoadout(ctx context.Context, characterID, kartID string) error {
	return c.Request(ctx, proto.TypeSetLoadout, &proto.SetLoadout{CharacterID: characterID, KartID: kartID}, nil)
}

func (c *Client) SetReady(ctx context.Context, ready bool) error {
	return c.Request(ctx, proto.TypeSetReady, &proto.SetReady{Ready: ready}, nil)
}

func (c *Client) Vote(ctx context.Context, trackID string) error {
	return c.Request(ctx, proto.TypeVote, &proto.TrackChoice{TrackID: trackID}, nil)
}

func (c *Client) StartRace(ctx context.Context, trackID string) error {
	return c.Request(ctx, proto.TypeStartRace, &proto.TrackChoice{TrackID: trackID}, nil)
}

func (c *Client) EndRaceEarly(ctx context.Context) error {
	return c.Request(ctx, proto.TypeEndRaceEarly, proto.Empty{}, nil)
}

func (c *Client) KickMember(ctx context.Context, accountID uint64, reason string) error {
	return c.Request(ctx, proto.TypeKickMember, &proto.KickMember{AccountID: accountID, Reason: reason}, nil)
}

// WaitLobbyState waits for a LobbyState push that satisfies ok (nil = any), skipping other pushes.
func (c *Client) WaitLobbyState(ctx context.Context, ok func(*proto.LobbyState) bool) (*proto.LobbyState, error) {
	for {
		var st proto.LobbyState
		if err := c.WaitPushMsg(ctx, proto.PushLobbyState, &st); err != nil {
			return nil, err
		}
		if ok == nil || ok(&st) {
			return &st, nil
		}
	}
}
