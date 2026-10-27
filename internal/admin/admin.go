// Package admin is the sysadmin command line (`mhk-server admin ...`). It works on the database directly, so it
// needs no running server; the server notices disables and revocations within its revalidation interval.
// Every mutating command writes the moderation audit log with a NULL actor (= sysadmin).
package admin

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/mhk-os/server/internal/blobs"
	"github.com/mhk-os/server/internal/proto"
	"github.com/mhk-os/server/internal/store"
)

// Store is what the CLI needs from the database.
type Store interface {
	store.Accounts
	store.Moderation
	store.Runs
	store.Profiles
	ContentStrikes(ctx context.Context, accountID uint64, limit int) ([]store.ContentStrike, error)
	CheatKicks(ctx context.Context, accountID uint64, limit int) ([]store.CheatKickView, error)
}

// Env is passed to every command.
type Env struct {
	Store Store
	Blobs *blobs.Store // ghost files, removed when deletes leave them unreferenced
	Out   io.Writer
}

// Command is a node of the command tree: either Run or Subs is set. Adding a command means adding a node.
type Command struct {
	Name    string
	Usage   string // arguments, e.g. "<username> --reason TEXT"
	Summary string
	Run     func(ctx context.Context, env *Env, args []string) error
	Subs    []*Command
}

// UsageError is a mistake in the command line; the caller prints usage.
type UsageError struct{ Msg string }

func (e *UsageError) Error() string { return e.Msg }

func usageErr(format string, args ...any) error {
	return &UsageError{Msg: fmt.Sprintf(format, args...)}
}

// Root is the command tree. Lobbies live in the server's memory, so the CLI has no lobby commands.
var Root = &Command{
	Name: "admin",
	Subs: []*Command{accountCommand, runCommand, boardCommand, auditCommand, contentCommand, kicksCommand},
}

// Run executes args (without the leading "admin") against the tree.
func Run(ctx context.Context, env *Env, args []string) error {
	return Root.dispatch(ctx, env, args, "admin")
}

func (c *Command) dispatch(ctx context.Context, env *Env, args []string, path string) error {
	if c.Run != nil {
		return c.Run(ctx, env, args)
	}
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		c.PrintUsage(env.Out, path)
		if len(args) == 0 {
			return usageErr("missing command")
		}
		return nil
	}
	for _, sub := range c.Subs {
		if sub.Name == args[0] {
			return sub.dispatch(ctx, env, args[1:], path+" "+sub.Name)
		}
	}
	c.PrintUsage(env.Out, path)
	return usageErr("unknown command %q", args[0])
}

// PrintUsage lists the leaf commands below c.
func (c *Command) PrintUsage(w io.Writer, path string) {
	fmt.Fprintf(w, "Usage:\n")
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	var walk func(c *Command, path string)
	walk = func(c *Command, path string) {
		if c.Run != nil {
			fmt.Fprintf(tw, "  mhk-server %s %s\t%s\n", path, c.Usage, c.Summary)
			return
		}
		for _, sub := range c.Subs {
			walk(sub, path+" "+sub.Name)
		}
	}
	walk(c, path)
	tw.Flush()
}

// parseArgs parses flags anywhere among the arguments (Go's flag package stops at the first positional one),
// so `account disable bob --reason x` and `account disable --reason x bob` both work.
func parseArgs(fs *flag.FlagSet, args []string) ([]string, error) {
	fs.SetOutput(io.Discard)
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, usageErr("%v", err)
		}
		args = fs.Args()
		if len(args) == 0 {
			return positional, nil
		}
		positional = append(positional, args[0])
		args = args[1:]
	}
}

func exactArgs(pos []string, n int, usage string) error {
	if len(pos) != n {
		return usageErr("usage: %s", usage)
	}
	return nil
}

func sysadminAction(action string, target *store.Account, reason string) store.Action {
	id := target.ID
	return store.Action{
		Action:        action,
		TargetAccount: &id,
		TargetRef:     target.Username,
		Reason:        reason,
		CreatedAt:     store.Now(),
	}
}

func findAccount(ctx context.Context, env *Env, username string) (*store.Account, error) {
	a, err := env.Store.AccountByUsername(ctx, username)
	if errors.Is(err, store.ErrNotFound) {
		return nil, fmt.Errorf("no account named %q", username)
	}
	return a, err
}

func formatTime(t *time.Time) string {
	if t == nil {
		return "-"
	}
	return t.UTC().Format("2006-01-02 15:04:05Z")
}

func countryLabel(a *store.Account) string {
	country, fromIP := a.Country()
	if country == "" {
		country = "--"
	}
	if fromIP {
		return country + " (ip)"
	}
	return country + " (chosen)"
}

func status(a *store.Account) string {
	if a.Disabled() {
		return "disabled"
	}
	return "active"
}

// maxReason keeps reasons within the Disconnect message limit so the player sees them complete.
const maxReason = proto.MaxDisconnectText

func checkReason(reason string) error {
	if strings.TrimSpace(reason) == "" {
		return usageErr("--reason is required")
	}
	if len(reason) > maxReason {
		return usageErr("--reason is longer than %d bytes", maxReason)
	}
	return nil
}
