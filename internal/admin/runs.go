package admin

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/mhk-os/server/internal/blobs"
	"github.com/mhk-os/server/internal/store"
)

var runCommand = &Command{
	Name: "run",
	Subs: []*Command{
		{Name: "list", Usage: "[--account U] [--track T] [--limit N]", Summary: "list uploaded runs, newest first",
			Run: runList},
		{Name: "show", Usage: "<id>", Summary: "show one run", Run: runShow},
		{Name: "hide", Usage: "<id> --reason TEXT", Summary: "hide a run from players", Run: runHide},
		{Name: "unhide", Usage: "<id>", Summary: "show a hidden run again", Run: runUnhide},
		{Name: "delete", Usage: "<id> --yes", Summary: "delete a run and its ghost file permanently", Run: runDelete},
		{Name: "reports", Usage: "<id>", Summary: "list the reports on a run", Run: runReports},
	},
}

var boardCommand = &Command{
	Name: "board",
	Subs: []*Command{
		{Name: "list", Usage: "[--track T]", Summary: "list leaderboards", Run: boardList},
	},
}

func parseRunID(s string) (uint64, error) {
	id, err := strconv.ParseUint(s, 10, 64)
	if err != nil || id == 0 {
		return 0, usageErr("run id %q is not a positive number", s)
	}
	return id, nil
}

func findRun(ctx context.Context, env *Env, arg string) (*store.RunView, error) {
	id, err := parseRunID(arg)
	if err != nil {
		return nil, err
	}
	v, err := env.Store.RunByID(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, fmt.Errorf("no run with id %d", id)
	}
	return v, err
}

// formatMs renders a race time as m:ss.mmm.
func formatMs(ms int32) string {
	return fmt.Sprintf("%d:%02d.%03d", ms/60000, ms/1000%60, ms%1000)
}

func runStatus(v *store.RunView) string {
	switch {
	case v.HiddenAt != nil:
		return "hidden"
	case v.AccountDisabled:
		return "hidden (account disabled)"
	}
	return "visible"
}

func runAction(action string, id uint64, reason string) store.Action {
	return store.Action{Action: action, TargetRef: fmt.Sprintf("run %d", id), Reason: reason, CreatedAt: store.Now()}
}

func runList(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("run list", flag.ContinueOnError)
	account := fs.String("account", "", "only runs of this username")
	track := fs.String("track", "", "only runs on this track id")
	limit := fs.Int("limit", 50, "at most N runs")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if err := exactArgs(pos, 0, "run list [--account U] [--track T] [--limit N]"); err != nil {
		return err
	}
	if *limit <= 0 {
		return usageErr("--limit must be positive")
	}
	f := store.RunFilter{TrackID: *track, Limit: *limit}
	if *account != "" {
		a, err := findAccount(ctx, env, *account)
		if err != nil {
			return err
		}
		f.AccountID = a.ID
	}
	runs, err := env.Store.ListRuns(ctx, f)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(env.Out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tUPLOADED\tACCOUNT\tTRACK\tCLASS\tLAPS\tKART\tTIME\tRANK\tFLAGS\tSTATUS\tBOARD")
	for i := range runs {
		v := &runs[i]
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%d\t%d\t%s\t%s\t%d\t%d\t%s\t%d\n", v.ID, formatTime(&v.UploadedAt),
			v.Username, v.TrackID, v.EngineClass, v.Laps, v.KartID, formatMs(v.FinishMs), v.Rank, v.Flags,
			runStatus(v), v.BoardID)
	}
	tw.Flush()
	fmt.Fprintf(env.Out, "%d run(s)\n", len(runs))
	return nil
}

func runShow(ctx context.Context, env *Env, args []string) error {
	pos, err := parseArgs(flag.NewFlagSet("run show", flag.ContinueOnError), args)
	if err != nil {
		return err
	}
	if err := exactArgs(pos, 1, "run show <id>"); err != nil {
		return err
	}
	v, err := findRun(ctx, env, pos[0])
	if err != nil {
		return err
	}
	laps := make([]string, len(v.LapMs))
	for i, ms := range v.LapMs {
		laps[i] = formatMs(ms)
	}
	tw := tabwriter.NewWriter(env.Out, 0, 4, 2, ' ', 0)
	fmt.Fprintf(tw, "ID\t%d\n", v.ID)
	fmt.Fprintf(tw, "Account\t%s (id %d, %s)\n", v.Username, v.AccountID, orDash(v.Country))
	fmt.Fprintf(tw, "Board\t%d: %s, engine class %d, %d laps, content version %d, track hash %s\n", v.BoardID,
		v.BoardTrackID, v.EngineClass, v.Laps, v.ContentVersion, v.TrackHash)
	fmt.Fprintf(tw, "Track pack\t%s\n", v.TrackPackID)
	fmt.Fprintf(tw, "Kart / character\t%s / %s\n", v.KartID, v.CharacterID)
	fmt.Fprintf(tw, "Time\t%s (best lap %s; laps %s)\n", formatMs(v.FinishMs), formatMs(v.BestLapMs),
		strings.Join(laps, ", "))
	fmt.Fprintf(tw, "Rank\t%d\n", v.Rank)
	fmt.Fprintf(tw, "Setup hash\t%s (mismatch: %t)\n", v.SetupHash, v.SetupMismatch)
	fmt.Fprintf(tw, "Ghost id\t%s\n", v.GhostID)
	fmt.Fprintf(tw, "Game build\t%s\n", orDash(v.GameBuild))
	fmt.Fprintf(tw, "Frames\t%d\n", v.FrameCount)
	fmt.Fprintf(tw, "Recorded\t%s\n", formatTime(&v.RecordedAt))
	fmt.Fprintf(tw, "Uploaded\t%s\n", formatTime(&v.UploadedAt))
	fmt.Fprintf(tw, "File\t%d bytes, sha256 %s\n", v.FileSize, v.Blob)
	if env.Blobs != nil {
		fmt.Fprintf(tw, "Path\t%s\n", env.Blobs.Path(v.Blob))
	}
	fmt.Fprintf(tw, "Flags\t%d\n", v.Flags)
	fmt.Fprintf(tw, "Status\t%s\n", runStatus(v))
	if v.HiddenAt != nil {
		by := "sysadmin"
		switch {
		case v.DeletedByOwner:
			by = "the player, deleting their account"
		case v.HiddenBy != nil:
			by = fmt.Sprintf("account %d", *v.HiddenBy)
		}
		fmt.Fprintf(tw, "Hidden at\t%s\n", formatTime(v.HiddenAt))
		fmt.Fprintf(tw, "Hidden by\t%s\n", by)
		fmt.Fprintf(tw, "Reason\t%s\n", v.HiddenReason)
	}
	return tw.Flush()
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func runHide(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("run hide", flag.ContinueOnError)
	reason := fs.String("reason", "", "why the run is hidden (audit log)")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if err := exactArgs(pos, 1, "run hide <id> --reason TEXT"); err != nil {
		return err
	}
	if err := checkReason(*reason); err != nil {
		return err
	}
	v, err := findRun(ctx, env, pos[0])
	if err != nil {
		return err
	}
	if err := env.Store.HideRun(ctx, v.ID, runAction(store.ActionHideRun, v.ID, *reason)); err != nil {
		return err
	}
	fmt.Fprintf(env.Out, "run %d of %s hidden\n", v.ID, v.Username)
	return nil
}

func runUnhide(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("run unhide", flag.ContinueOnError)
	reason := fs.String("reason", "", "optional note for the audit log")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if err := exactArgs(pos, 1, "run unhide <id>"); err != nil {
		return err
	}
	v, err := findRun(ctx, env, pos[0])
	if err != nil {
		return err
	}
	if v.HiddenAt == nil {
		fmt.Fprintf(env.Out, "run %d is not hidden\n", v.ID)
		return nil
	}
	if err := env.Store.UnhideRun(ctx, v.ID, runAction(store.ActionUnhideRun, v.ID, *reason)); err != nil {
		return err
	}
	fmt.Fprintf(env.Out, "run %d of %s visible again\n", v.ID, v.Username)
	return nil
}

func runDelete(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("run delete", flag.ContinueOnError)
	yes := fs.Bool("yes", false, "confirm permanent deletion")
	reason := fs.String("reason", "", "optional note for the audit log")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if err := exactArgs(pos, 1, "run delete <id> --yes"); err != nil {
		return err
	}
	if !*yes {
		return usageErr("deleting run %s is permanent; repeat with --yes", pos[0])
	}
	v, err := findRun(ctx, env, pos[0])
	if err != nil {
		return err
	}
	orphans, err := env.Store.DeleteRun(ctx, v.ID, runAction(store.ActionDeleteRun, v.ID, *reason))
	if err != nil {
		return err
	}
	fmt.Fprintf(env.Out, "run %d of %s deleted\n", v.ID, v.Username)
	return removeBlobs(env, orphans)
}

// removeBlobs deletes ghost files no run references any more.
func removeBlobs(env *Env, orphans []blobs.Hash) error {
	if len(orphans) == 0 {
		return nil
	}
	if env.Blobs == nil {
		return fmt.Errorf("%d ghost file(s) are unreferenced but no data dir is configured", len(orphans))
	}
	var errs []error
	for _, h := range orphans {
		errs = append(errs, env.Blobs.Delete(h))
	}
	if err := errors.Join(errs...); err != nil {
		return err
	}
	fmt.Fprintf(env.Out, "%d ghost file(s) removed\n", len(orphans))
	return nil
}

func runReports(ctx context.Context, env *Env, args []string) error {
	pos, err := parseArgs(flag.NewFlagSet("run reports", flag.ContinueOnError), args)
	if err != nil {
		return err
	}
	if err := exactArgs(pos, 1, "run reports <id>"); err != nil {
		return err
	}
	v, err := findRun(ctx, env, pos[0])
	if err != nil {
		return err
	}
	reports, err := env.Store.RunReports(ctx, v.ID, 10_000)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(env.Out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "TIME\tREPORTER\tREASON\tAT\tCOUNTED\tDETAIL")
	for _, r := range reports {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%.2f s\t%t\t%s\n", formatTime(&r.CreatedAt), r.Reporter, r.Reason, r.AtSeconds,
			r.Counted, r.Detail)
	}
	tw.Flush()
	fmt.Fprintf(env.Out, "%d report(s), %d counted flag(s)\n", len(reports), v.Flags)
	return nil
}

func boardList(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("board list", flag.ContinueOnError)
	track := fs.String("track", "", "only boards of this track id")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if err := exactArgs(pos, 0, "board list [--track T]"); err != nil {
		return err
	}
	boards, err := env.Store.ListBoards(ctx, store.BoardFilter{TrackID: *track})
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(env.Out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tTRACK\tCLASS\tLAPS\tSIM\tTRACK HASH\tPLAYERS\tBEST\tLAST UPLOAD")
	for i := range boards {
		b := &boards[i]
		best := "-"
		if b.BestMs > 0 {
			best = formatMs(b.BestMs)
		}
		fmt.Fprintf(tw, "%d\t%s\t%d\t%d\t%d\t%s\t%d\t%s\t%s\n", b.ID, b.TrackID, b.EngineClass, b.Laps, b.ContentVersion,
			b.TrackHash, b.Players, best, formatTime(b.LastUpload))
	}
	tw.Flush()
	fmt.Fprintf(env.Out, "%d board(s)\n", len(boards))
	return nil
}
