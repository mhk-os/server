package testclient

import (
	"context"

	"github.com/mhk-os/server/internal/proto"
)

func (c *Client) SyncAchievements(ctx context.Context, req *proto.SyncAchievements) (*proto.AchievementState,
	error) {
	var resp proto.AchievementState
	if err := c.Request(ctx, proto.TypeSyncAchievements, req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (c *Client) AchievementShares(ctx context.Context, ids ...string) (*proto.AchievementShares, error) {
	var resp proto.AchievementShares
	if err := c.Request(ctx, proto.TypeAchievementShares, &proto.AchievementSharesRequest{IDs: ids}, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (c *Client) DeleteAccount(ctx context.Context, password string, deleteRuns bool) error {
	return c.Request(ctx, proto.TypeDeleteAccount, &proto.DeleteAccount{Password: password, DeleteRuns: deleteRuns},
		nil)
}

func (c *Client) RunHiddenReason(ctx context.Context, runID uint64) (*proto.RunHiddenInfo, error) {
	var resp proto.RunHiddenInfo
	if err := c.Request(ctx, proto.TypeRunHiddenReason, &proto.RunID{RunID: runID}, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}
