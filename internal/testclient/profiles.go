package testclient

import (
	"context"

	"github.com/mhk-os/server/internal/proto"
)

func (c *Client) SetProfileListed(ctx context.Context, listed bool) error {
	return c.Request(ctx, proto.TypeSetProfileListed, &proto.SetProfileListed{Listed: listed}, nil)
}

func (c *Client) ListPlayers(ctx context.Context, req *proto.ListPlayers) (*proto.PlayerPage, error) {
	var resp proto.PlayerPage
	err := c.Request(ctx, proto.TypeListPlayers, req, &resp)
	return &resp, err
}

func (c *Client) GetProfiles(ctx context.Context, ids ...uint64) ([]proto.PlayerSummary, error) {
	var resp proto.PlayerSummaries
	err := c.Request(ctx, proto.TypeGetProfiles, &proto.GetProfiles{AccountIDs: ids}, &resp)
	return resp.Players, err
}

func (c *Client) GetProfile(ctx context.Context, accountID uint64) (*proto.PlayerProfile, error) {
	var resp proto.PlayerProfile
	err := c.Request(ctx, proto.TypeGetProfile, &proto.AccountID{AccountID: accountID}, &resp)
	return &resp, err
}

func (c *Client) ScoreLeaderboard(ctx context.Context, req *proto.ScoreLeaderboard) (*proto.ScoreRanking, error) {
	var resp proto.ScoreRanking
	err := c.Request(ctx, proto.TypeScoreLeaderboard, req, &resp)
	return &resp, err
}
