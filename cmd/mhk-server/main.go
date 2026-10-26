// Command mhk-server is the online backend of Moorhuhn Kart: Open Source.
//
//	mhk-server serve     run the server
//	mhk-server migrate   apply database migrations
//	mhk-server admin ... sysadmin commands on the database
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/mhk-os/server/internal/admin"
	"github.com/mhk-os/server/internal/auth"
	"github.com/mhk-os/server/internal/blobs"
	"github.com/mhk-os/server/internal/content"
	"github.com/mhk-os/server/internal/geo"
	"github.com/mhk-os/server/internal/lobby"
	"github.com/mhk-os/server/internal/race"
	"github.com/mhk-os/server/internal/server"
	"github.com/mhk-os/server/internal/store"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const usage = `mhk-server %s

Usage:
  mhk-server serve [flags]     run the server
  mhk-server migrate [flags]   apply database migrations and exit
  mhk-server admin [flags] ... sysadmin commands (run "mhk-server admin help")
  mhk-server version           print the version

Every flag falls back to its MHK_* environment variable; run "mhk-server <command> -h" for details.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, usage, version)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch cmd, args := os.Args[1], os.Args[2:]; cmd {
	case "serve":
		err = serve(ctx, args)
	case "migrate":
		err = migrate(ctx, args)
	case "admin":
		err = runAdmin(ctx, args)
	case "version":
		fmt.Println(version)
	case "help", "-h", "--help":
		fmt.Printf(usage, version)
	default:
		fmt.Fprintf(os.Stderr, usage, version)
		err = fmt.Errorf("unknown command %q", cmd)
	}
	if err != nil {
		var ue *admin.UsageError
		if errors.As(err, &ue) {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(2)
		}
		if !errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	}
}

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return def
}

func envBool(key string, def bool) bool {
	if v, ok := os.LookupEnv(key); ok {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
}

func dbFlag(fs *flag.FlagSet) *string {
	return fs.String("db", env("MHK_DB_DSN", ""),
		"MariaDB DSN, e.g. mhk:mhk@tcp(localhost:3307)/mhk?parseTime=true (MHK_DB_DSN)")
}

func dataDirFlag(fs *flag.FlagSet) *string {
	return fs.String("data-dir", env("MHK_DATA_DIR", "./data"),
		"directory for server data; ghost files live in its ghosts/ (MHK_DATA_DIR)")
}

func openStore(ctx context.Context, dsn string) (*store.Store, error) {
	if dsn == "" {
		return nil, errors.New("no database configured: set MHK_DB_DSN or -db")
	}
	return store.Open(ctx, dsn)
}

func newLogger(level string) (*slog.Logger, error) {
	var l slog.Level
	if err := l.UnmarshalText([]byte(level)); err != nil {
		return nil, fmt.Errorf("log level: %w", err)
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: l})), nil
}

func serve(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	listen := fs.String("listen", env("MHK_LISTEN", ":7350"), "TCP listen address (MHK_LISTEN)")
	udpListen := fs.String("udp-listen", env("MHK_UDP_LISTEN", ":7350"), "UDP realtime listen address (MHK_UDP_LISTEN)")
	dsn := dbFlag(fs)
	var ts tlsSettings
	fs.StringVar(&ts.certFile, "tls-cert", env("MHK_TLS_CERT", ""), "TLS certificate chain PEM, reloaded on change (MHK_TLS_CERT)")
	fs.StringVar(&ts.keyFile, "tls-key", env("MHK_TLS_KEY", ""), "TLS private key PEM (MHK_TLS_KEY)")
	fs.StringVar(&ts.acmeDomain, "acme-domain", env("MHK_ACME_DOMAIN", ""), "obtain a Let's Encrypt certificate for this domain (MHK_ACME_DOMAIN)")
	fs.StringVar(&ts.acmeCache, "acme-cache", env("MHK_ACME_CACHE", ""), "directory for ACME account and certificates (MHK_ACME_CACHE)")
	fs.StringVar(&ts.acmeHTTP, "acme-http", env("MHK_ACME_HTTP", ":80"), "HTTP-01 challenge listen address (MHK_ACME_HTTP)")
	geoPath := fs.String("geoip", env("MHK_GEOIP", ""), "ISO country .mmdb database, optional (MHK_GEOIP)")
	dataDir := dataDirFlag(fs)
	autoMigrate := fs.Bool("auto-migrate", envBool("MHK_AUTO_MIGRATE", true), "apply migrations at startup (MHK_AUTO_MIGRATE)")
	logLevel := fs.String("log-level", env("MHK_LOG_LEVEL", "info"), "debug, info, warn or error (MHK_LOG_LEVEL)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	log, err := newLogger(*logLevel)
	if err != nil {
		return err
	}
	slog.SetDefault(log)

	ghosts, err := blobs.Open(*dataDir)
	if err != nil {
		return fmt.Errorf("data dir: %w", err)
	}
	st, err := openStore(ctx, *dsn)
	if err != nil {
		return err
	}
	defer st.Close()
	if *autoMigrate {
		if err := st.Migrate(ctx, log); err != nil {
			return err
		}
	}
	var geoDB *geo.DB
	if *geoPath != "" {
		if geoDB, err = geo.Open(*geoPath); err != nil {
			return err
		}
		defer geoDB.Close()
	} else {
		log.Warn("no GeoIP database configured (MHK_GEOIP); countries will be unknown until players choose one")
	}

	tlsConfig, err := buildTLS(ctx, ts, log)
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		return err
	}
	if tlsConfig != nil {
		ln = tls.NewListener(ln, tlsConfig)
	} else if !isLoopback(ln.Addr()) {
		log.Warn("serving plaintext on a non-loopback address; clients refuse plaintext except on local networks",
			"addr", ln.Addr().String())
	}
	udpAddr, err := net.ResolveUDPAddr("udp", *udpListen)
	if err != nil {
		ln.Close()
		return fmt.Errorf("udp listen address: %w", err)
	}
	udp, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		ln.Close()
		return err
	}
	udpPort := uint16(udp.LocalAddr().(*net.UDPAddr).Port)

	svc := auth.NewService(st, st, auth.NewHasher(auth.DefaultParams, auth.DefaultHashConcurrency), auth.DefaultLimits)
	srv := server.New(server.Config{Version: version, UDPPort: udpPort}, server.Deps{
		Log:        log,
		Auth:       svc,
		Accounts:   st,
		Sessions:   st,
		Moderation: st,
		Runs:       st,
		Blobs:      ghosts,
		Geo:        geoDB,
	})
	catalog := content.New(st, log)
	catalog.Register(srv)
	races := race.NewEngine(race.Config{Log: log, Hub: srv.Hub(), Kicks: st, UDP: udp, Catalog: catalog})
	defer races.Close()
	races.Register(srv)
	lobbies := lobby.New(lobby.Config{Races: races, Catalog: catalog, Pusher: srv.Hub(), Audit: st, Log: log})
	defer lobbies.Close()
	lobbies.Register(srv)
	go pruneSessions(ctx, st, log)
	log.Info("mhk-server listening", "version", version, "addr", ln.Addr().String(), "tls", tlsConfig != nil,
		"udp", udp.LocalAddr().String())
	err = srv.Serve(ctx, ln)
	log.Info("mhk-server stopped")
	return err
}

func isLoopback(addr net.Addr) bool {
	ap, err := netip.ParseAddrPort(addr.String())
	return err == nil && ap.Addr().IsLoopback()
}

// pruneSessions deletes sessions that expired or were revoked more than 30 days ago, once an hour.
func pruneSessions(ctx context.Context, st *store.Store, log *slog.Logger) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			n, err := st.DeleteExpiredSessions(ctx, store.Now().Add(-30*24*time.Hour))
			if err != nil {
				log.Error("pruning sessions failed", "err", err)
			} else if n > 0 {
				log.Info("pruned sessions", "count", n)
			}
		}
	}
}

func migrate(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("migrate", flag.ContinueOnError)
	dsn := dbFlag(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	st, err := openStore(ctx, *dsn)
	if err != nil {
		return err
	}
	defer st.Close()
	if err := st.Migrate(ctx, slog.Default()); err != nil {
		return err
	}
	fmt.Println("database is up to date")
	return nil
}

func runAdmin(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("admin", flag.ContinueOnError)
	dsn := dbFlag(fs)
	dataDir := dataDirFlag(fs)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage: mhk-server admin [-db DSN] [-data-dir DIR] <command> ...")
		fs.PrintDefaults()
		admin.Root.PrintUsage(fs.Output(), "admin")
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() == 0 || fs.Arg(0) == "help" {
		fs.Usage()
		return nil
	}
	st, err := openStore(ctx, *dsn)
	if err != nil {
		return err
	}
	defer st.Close()
	ghosts, err := blobs.Open(*dataDir)
	if err != nil {
		return fmt.Errorf("data dir: %w", err)
	}
	return admin.Run(ctx, &admin.Env{Store: st, Blobs: ghosts, Out: os.Stdout}, fs.Args())
}
