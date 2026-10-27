package admin

import (
	"context"
	"flag"
	"fmt"
	"text/tabwriter"
)

var contentCommand = &Command{
	Name: "content",
	Subs: []*Command{
		{Name: "strikes", Usage: "[--account U] [--limit N]",
			Summary: "list content strikes (copies that differ from the catalog majority), newest first",
			Run:     contentStrikes},
	},
}

var kicksCommand = &Command{
	Name: "kicks", Usage: "[--account U] [--limit N]", Summary: "list race kicks for cheating, newest first",
	Run: cheatKicks,
}

var cheatReportsCommand = &Command{
	Name: "cheat-reports", Usage: "[--account U] [--limit N]",
	Summary: "list cheat reports races received (counted or not) by or about an account, newest first",
	Run:     cheatReports,
}

// accountFilterArgs parses [--account U] [--limit N] and resolves the account (0 = everybody).
func accountFilterArgs(ctx context.Context, env *Env, name string, args []string) (uint64, int, error) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	account := fs.String("account", "", "only this username")
	limit := fs.Int("limit", 100, "at most N rows (0 = all)")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return 0, 0, err
	}
	if err := exactArgs(pos, 0, name+" [--account U] [--limit N]"); err != nil {
		return 0, 0, err
	}
	if *limit < 0 {
		return 0, 0, usageErr("--limit must not be negative")
	}
	if *account == "" {
		return 0, *limit, nil
	}
	a, err := findAccount(ctx, env, *account)
	if err != nil {
		return 0, 0, err
	}
	return a.ID, *limit, nil
}

func contentStrikes(ctx context.Context, env *Env, args []string) error {
	id, limit, err := accountFilterArgs(ctx, env, "content strikes", args)
	if err != nil {
		return err
	}
	strikes, err := env.Store.ContentStrikes(ctx, id, limit)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(env.Out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "TIME\tACCOUNT\tPACK\tCOMMIT\tSIM\tHASH\tMAJORITY\tREASON")
	for i := range strikes {
		k := &strikes[i]
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%s\t%s\t%s\n", formatTime(&k.CreatedAt), k.Username, k.PackID,
			orDash(k.Commit), k.ContentVersion, orDash(k.ManifestHash), orDash(k.MajorityHash), k.Reason)
	}
	tw.Flush()
	fmt.Fprintf(env.Out, "%d strike(s)\n", len(strikes))
	return nil
}

func cheatKicks(ctx context.Context, env *Env, args []string) error {
	id, limit, err := accountFilterArgs(ctx, env, "kicks", args)
	if err != nil {
		return err
	}
	kicks, err := env.Store.CheatKicks(ctx, id, limit)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(env.Out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "TIME\tACCOUNT\tLOBBY\tRACE\tREASON\tREPORTERS")
	for i := range kicks {
		k := &kicks[i]
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", formatTime(&k.CreatedAt), k.Username, orDash(k.LobbyRef),
			orDash(k.RaceRef), k.Reason, orDash(k.Reporters))
	}
	tw.Flush()
	fmt.Fprintf(env.Out, "%d kick(s)\n", len(kicks))
	return nil
}

func cheatReports(ctx context.Context, env *Env, args []string) error {
	id, limit, err := accountFilterArgs(ctx, env, "cheat-reports", args)
	if err != nil {
		return err
	}
	reports, err := env.Store.CheatReports(ctx, id, limit)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(env.Out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "TIME\tSUSPECT\tREPORTER\tLOBBY\tRACE\tREASON\tCOUNTED\tDETAIL")
	for i := range reports {
		r := &reports[i]
		counted := "yes"
		if !r.Counted {
			counted = "no: " + r.Note
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", formatTime(&r.CreatedAt), r.Suspect, r.Reporter,
			orDash(r.LobbyRef), orDash(r.RaceRef), r.Reason, counted, orDash(r.Detail))
	}
	tw.Flush()
	fmt.Fprintf(env.Out, "%d report(s)\n", len(reports))
	return nil
}
