package admin

import (
	"context"
	"flag"
	"fmt"
	"text/tabwriter"

	"github.com/mhk-os/server/internal/proto"
	"github.com/mhk-os/server/internal/store"
)

var accountCommand = &Command{
	Name: "account",
	Subs: []*Command{
		{Name: "list", Usage: "[--role player|moderator] [--limit N]", Summary: "list accounts", Run: accountList},
		{Name: "show", Usage: "<username>", Summary: "show one account", Run: accountShow},
		{Name: "set-role", Usage: "<username> player|moderator", Summary: "change the role", Run: accountSetRole},
		{Name: "disable", Usage: "<username> --reason TEXT", Summary: "disable and revoke sessions",
			Run: accountDisable},
		{Name: "enable", Usage: "<username>", Summary: "re-enable a disabled account", Run: accountEnable},
		{Name: "delete", Usage: "<username> --yes", Summary: "delete permanently", Run: accountDelete},
	},
}

var auditCommand = &Command{
	Name: "audit", Usage: "[--limit N]", Summary: "show the moderation audit log", Run: audit,
}

func accountList(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("account list", flag.ContinueOnError)
	roleName := fs.String("role", "", "only accounts with this role")
	limit := fs.Int("limit", 0, "at most N accounts")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if err := exactArgs(pos, 0, "account list [--role player|moderator] [--limit N]"); err != nil {
		return err
	}
	var filter store.AccountFilter
	filter.Limit = *limit
	if *roleName != "" {
		role, err := proto.ParseRole(*roleName)
		if err != nil {
			return usageErr("%v", err)
		}
		filter.Role = &role
	}
	accounts, err := env.Store.ListAccounts(ctx, filter)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(env.Out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tUSERNAME\tROLE\tCOUNTRY\tSTATUS\tCREATED\tLAST LOGIN")
	for i := range accounts {
		a := &accounts[i]
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\t%s\n", a.ID, a.Username, a.Role, countryLabel(a), status(a),
			formatTime(&a.CreatedAt), formatTime(a.LastLoginAt))
	}
	tw.Flush()
	fmt.Fprintf(env.Out, "%d account(s)\n", len(accounts))
	return nil
}

func accountShow(ctx context.Context, env *Env, args []string) error {
	pos, err := parseArgs(flag.NewFlagSet("account show", flag.ContinueOnError), args)
	if err != nil {
		return err
	}
	if err := exactArgs(pos, 1, "account show <username>"); err != nil {
		return err
	}
	a, err := findAccount(ctx, env, pos[0])
	if err != nil {
		return err
	}
	sessions, err := env.Store.CountActiveSessions(ctx, a.ID, store.Now())
	if err != nil {
		return err
	}
	p, err := env.Store.Profile(ctx, a.ID)
	if err != nil {
		return err
	}
	ach, err := env.Store.AchievementSummary(ctx, a.ID)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(env.Out, 0, 4, 2, ' ', 0)
	fmt.Fprintf(tw, "ID\t%d\n", a.ID)
	fmt.Fprintf(tw, "Username\t%s\n", a.Username)
	fmt.Fprintf(tw, "Role\t%s\n", a.Role)
	fmt.Fprintf(tw, "Country\t%s\n", countryLabel(a))
	fmt.Fprintf(tw, "IP country\t%s\n", a.CountryIP)
	fmt.Fprintf(tw, "Registration IP\t%s\n", a.RegistrationIP)
	fmt.Fprintf(tw, "Created\t%s\n", formatTime(&a.CreatedAt))
	fmt.Fprintf(tw, "Last login\t%s\n", formatTime(a.LastLoginAt))
	fmt.Fprintf(tw, "Last active\t%s\n", formatTime(p.LastActive))
	fmt.Fprintf(tw, "Active sessions\t%d\n", sessions)
	fmt.Fprintf(tw, "Listed\t%t\n", p.Listed)
	fmt.Fprintf(tw, "Score\t%d\n", p.Score)
	fmt.Fprintf(tw, "Rank\t%s\n", rankLabel(p.Rank))
	fmt.Fprintf(tw, "Races\t%d (%d wins, %d podiums)\n", p.Races, p.Wins, p.Podiums)
	fmt.Fprintf(tw, "Achievements\t%d unlocked (%d stats, %d set items; last sync %s)\n", ach.Unlocked, ach.Stats,
		ach.SetItems, formatTime(ach.SyncedAt))
	fmt.Fprintf(tw, "Status\t%s\n", status(a))
	if a.Disabled() {
		by := "sysadmin"
		if a.DisabledBy != nil {
			by = fmt.Sprintf("account %d", *a.DisabledBy)
		}
		fmt.Fprintf(tw, "Disabled at\t%s\n", formatTime(a.DisabledAt))
		fmt.Fprintf(tw, "Disabled by\t%s\n", by)
		fmt.Fprintf(tw, "Reason\t%s\n", a.DisabledReason)
	}
	return tw.Flush()
}

func rankLabel(rank uint32) string {
	if rank == 0 {
		return "unranked"
	}
	return fmt.Sprintf("%d", rank)
}

func accountSetRole(ctx context.Context, env *Env, args []string) error {
	pos, err := parseArgs(flag.NewFlagSet("account set-role", flag.ContinueOnError), args)
	if err != nil {
		return err
	}
	if err := exactArgs(pos, 2, "account set-role <username> player|moderator"); err != nil {
		return err
	}
	role, err := proto.ParseRole(pos[1])
	if err != nil {
		return usageErr("%v", err)
	}
	a, err := findAccount(ctx, env, pos[0])
	if err != nil {
		return err
	}
	if a.Role == proto.RoleAnonymous {
		return fmt.Errorf("%s holds the runs of a deleted account and keeps its role", a.Username)
	}
	if a.Role == role {
		fmt.Fprintf(env.Out, "%s is already %s\n", a.Username, role)
		return nil
	}
	act := sysadminAction(store.ActionSetRole, a, fmt.Sprintf("%s -> %s", a.Role, role))
	if err := env.Store.SetRole(ctx, a.ID, role, act); err != nil {
		return err
	}
	fmt.Fprintf(env.Out, "%s is now %s\n", a.Username, role)
	return nil
}

func accountDisable(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("account disable", flag.ContinueOnError)
	reason := fs.String("reason", "", "shown to the player")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if err := exactArgs(pos, 1, "account disable <username> --reason TEXT"); err != nil {
		return err
	}
	if err := checkReason(*reason); err != nil {
		return err
	}
	a, err := findAccount(ctx, env, pos[0])
	if err != nil {
		return err
	}
	if err := env.Store.DisableAccount(ctx, a.ID, sysadminAction(store.ActionDisableAccount, a, *reason)); err != nil {
		return err
	}
	fmt.Fprintf(env.Out, "%s disabled; sessions revoked, live connections drop within the server's check interval\n",
		a.Username)
	return nil
}

func accountEnable(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("account enable", flag.ContinueOnError)
	reason := fs.String("reason", "", "optional note for the audit log")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if err := exactArgs(pos, 1, "account enable <username> [--reason TEXT]"); err != nil {
		return err
	}
	a, err := findAccount(ctx, env, pos[0])
	if err != nil {
		return err
	}
	if !a.Disabled() {
		fmt.Fprintf(env.Out, "%s is not disabled\n", a.Username)
		return nil
	}
	if err := env.Store.EnableAccount(ctx, a.ID, sysadminAction(store.ActionEnableAccount, a, *reason)); err != nil {
		return err
	}
	fmt.Fprintf(env.Out, "%s enabled; the player has to log in again\n", a.Username)
	return nil
}

func accountDelete(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("account delete", flag.ContinueOnError)
	yes := fs.Bool("yes", false, "confirm permanent deletion")
	reason := fs.String("reason", "", "optional note for the audit log")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if err := exactArgs(pos, 1, "account delete <username> --yes"); err != nil {
		return err
	}
	if !*yes {
		return usageErr("deleting %q is permanent; repeat with --yes", pos[0])
	}
	a, err := findAccount(ctx, env, pos[0])
	if err != nil {
		return err
	}
	orphans, err := env.Store.DeleteAccount(ctx, a.ID, sysadminAction(store.ActionDeleteAccount, a, *reason))
	if err != nil {
		return err
	}
	fmt.Fprintf(env.Out, "%s (id %d) deleted with its runs and reports\n", a.Username, a.ID)
	return removeBlobs(env, orphans)
}

func audit(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("audit", flag.ContinueOnError)
	limit := fs.Int("limit", 50, "number of entries")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if err := exactArgs(pos, 0, "audit [--limit N]"); err != nil {
		return err
	}
	if *limit <= 0 {
		return usageErr("--limit must be positive")
	}
	actions, err := env.Store.AuditLog(ctx, *limit)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(env.Out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tTIME\tACTOR\tACTION\tTARGET\tREASON")
	for _, a := range actions {
		actor := "sysadmin"
		if a.Actor != nil {
			actor = fmt.Sprintf("account %d", *a.Actor)
		}
		target := a.TargetRef
		if a.TargetAccount != nil {
			target = fmt.Sprintf("%s (id %d)", a.TargetRef, *a.TargetAccount)
		}
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\n", a.ID, formatTime(&a.CreatedAt), actor, a.Action, target, a.Reason)
	}
	return tw.Flush()
}
