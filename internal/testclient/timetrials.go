package testclient

import (
	"context"

	"github.com/mhk-os/server/internal/proto"
)

func (c *Client) ListBoards(ctx context.Context, trackID string, engineClass uint8, simVersion int32) ([]proto.BoardInfo, error) {
	var resp proto.BoardList
	err := c.Request(ctx, proto.TypeListBoards,
		&proto.ListBoards{TrackID: trackID, EngineClass: engineClass, SimVersion: simVersion}, &resp)
	return resp.Boards, err
}

func (c *Client) GetLeaderboard(ctx context.Context, req *proto.GetLeaderboard) (*proto.Leaderboard, error) {
	var resp proto.Leaderboard
	if err := c.Request(ctx, proto.TypeGetLeaderboard, req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (c *Client) UploadRun(ctx context.Context, file []byte) (*proto.UploadResult, error) {
	var resp proto.UploadResult
	if err := c.Request(ctx, proto.TypeUploadRun, &proto.UploadRun{File: file}, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (c *Client) DownloadRun(ctx context.Context, runID uint64) (*proto.DownloadResult, error) {
	var resp proto.DownloadResult
	if err := c.Request(ctx, proto.TypeDownloadRun, &proto.RunID{RunID: runID}, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (c *Client) ReportRun(ctx context.Context, req *proto.ReportRun) (uint32, error) {
	var resp proto.ReportResult
	err := c.Request(ctx, proto.TypeReportRun, req, &resp)
	return resp.Flags, err
}

func (c *Client) MyRuns(ctx context.Context, trackID string) ([]proto.RunEntry, error) {
	var resp proto.RunList
	err := c.Request(ctx, proto.TypeMyRuns, &proto.MyRuns{TrackID: trackID}, &resp)
	return resp.Runs, err
}

func (c *Client) ModHideRun(ctx context.Context, runID uint64, reason string) error {
	return c.Request(ctx, proto.TypeModHideRun, &proto.ModHideRun{RunID: runID, Reason: reason}, nil)
}

func (c *Client) ModUnhideRun(ctx context.Context, runID uint64) error {
	return c.Request(ctx, proto.TypeModUnhideRun, &proto.RunID{RunID: runID}, nil)
}

func (c *Client) ModDisableAccount(ctx context.Context, accountID uint64, reason string) error {
	return c.Request(ctx, proto.TypeModDisableAccount, &proto.ModDisableAccount{AccountID: accountID, Reason: reason}, nil)
}

func (c *Client) ModEnableAccount(ctx context.Context, accountID uint64) error {
	return c.Request(ctx, proto.TypeModEnableAccount, &proto.AccountID{AccountID: accountID}, nil)
}

func (c *Client) ModListFlagged(ctx context.Context, offset uint32, limit uint16, includeHidden bool) (*proto.RunPage, error) {
	var resp proto.RunPage
	err := c.Request(ctx, proto.TypeModListFlagged,
		&proto.ModListFlagged{Offset: offset, Limit: limit, IncludeHidden: includeHidden}, &resp)
	return &resp, err
}

func (c *Client) ModListHidden(ctx context.Context, boardID uint64, offset uint32, limit uint16) (*proto.RunPage, error) {
	var resp proto.RunPage
	err := c.Request(ctx, proto.TypeModListHidden,
		&proto.ModListHidden{BoardID: boardID, Offset: offset, Limit: limit}, &resp)
	return &resp, err
}

func (c *Client) PlayerRuns(ctx context.Context, accountID uint64, offset uint32, limit uint16) (*proto.RunPage, error) {
	var resp proto.RunPage
	err := c.Request(ctx, proto.TypePlayerRuns,
		&proto.PlayerRuns{AccountID: accountID, Offset: offset, Limit: limit}, &resp)
	return &resp, err
}

func (c *Client) ModRunReports(ctx context.Context, runID uint64) ([]proto.RunReport, error) {
	var resp proto.RunReports
	err := c.Request(ctx, proto.TypeModRunReports, &proto.RunID{RunID: runID}, &resp)
	return resp.Reports, err
}

func (c *Client) ModGetAccount(ctx context.Context, accountID uint64) (*proto.ModAccountInfo, error) {
	var resp proto.ModAccountInfo
	if err := c.Request(ctx, proto.TypeModGetAccount, &proto.AccountID{AccountID: accountID}, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}
