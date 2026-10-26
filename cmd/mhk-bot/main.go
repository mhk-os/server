// Command mhk-bot is a development and load-test player: it joins a lobby and races by replaying a ghost, so the
// game can be tested against remote karts.
//
//	mhk-bot -manifests packs.json -ghost run.mhkghost -user bot -pass secret -quick 1 [-n 3]
//	mhk-bot dump-manifests -dsn DSN -sim N -out packs.json mhk2 mhk3 ...
//
// Bots know nothing about the track, so their RaceLoaded carries a hash unique to each bot: with one human (or
// bots only) nobody has a majority, the tie keeps every racer and the race uses the lowest slot's content. With two
// or more humans the humans form the majority and the bots leave as desync, so use bots with a single human.
// Bots never pick up or use items; they drive the ghost's path whatever the track and finish at the ghost's time.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/mhk-os/server/internal/ghost"
	"github.com/mhk-os/server/internal/store"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var err error
	if len(os.Args) > 1 && os.Args[1] == "dump-manifests" {
		err = dump(ctx, os.Args[2:])
	} else {
		err = run(ctx, os.Args[1:])
	}
	if err != nil && !errors.Is(err, flag.ErrHelp) {
		fmt.Fprintln(os.Stderr, "mhk-bot:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("mhk-bot", flag.ContinueOnError)
	server := fs.String("server", "tcp://localhost:7350", "server URI: tcp://host:port or tls://host:port")
	user := fs.String("user", "", "username; with -n > 1 the bots are <user>1 … <user>N (registered when missing)")
	pass := fs.String("pass", "", "password")
	join := fs.String("join", "", "private room code to join")
	quick := fs.Int("quick", 0, "public lobby category to quick-join (1-10)")
	character := fs.String("character", "", "character id (default: the ghost's)")
	kart := fs.String("kart", "", "kart id (default: the ghost's)")
	ghostPath := fs.String("ghost", "", ".mhkghost file to replay in every race")
	manifests := fs.String("manifests", "", "pack manifest file (mhk-bot dump-manifests)")
	n := fs.Int("n", 1, "number of bots")
	keepAlive := fs.Duration("keepalive", 15*time.Second, "TCP Ping interval (the server drops connections silent for 45 s)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *user == "" || *pass == "" || *ghostPath == "" || *manifests == "" || (*join == "") == (*quick == 0) ||
		*n < 1 || *quick < 0 || *quick > 255 || *keepAlive <= 0 {
		fs.Usage()
		return errors.New("need -user, -pass, -ghost, -manifests and exactly one of -join or -quick")
	}
	addr, host, tlsConf, err := parseServer(*server)
	if err != nil {
		return err
	}
	file, err := os.ReadFile(*ghostPath)
	if err != nil {
		return err
	}
	g, frames, err := ghost.ParseFrames(file)
	if err != nil {
		return fmt.Errorf("%s: %w", *ghostPath, err)
	}
	mf, err := loadManifests(*manifests)
	if err != nil {
		return err
	}
	cfg := &botConfig{addr: addr, host: host, tls: tlsConf, pass: *pass, join: strings.ToUpper(*join),
		quick: uint8(*quick), character: *character, kart: *kart, trackID: g.TrackID, replay: newReplay(g, frames),
		manifests: mf, keepAlive: *keepAlive}
	if cfg.character == "" {
		cfg.character = g.CharacterID
	}
	if cfg.kart == "" {
		cfg.kart = g.KartID
	}
	var wg sync.WaitGroup
	for i := range *n {
		name := *user
		if *n > 1 {
			name += strconv.Itoa(i + 1)
		}
		b := &bot{cfg: cfg, name: name, log: slog.Default().With("bot", name)}
		wg.Go(func() {
			if err := b.run(ctx); err != nil {
				b.log.Error("bot stopped", "err", err)
			}
		})
	}
	wg.Wait()
	return nil
}

// parseServer accepts tcp://host:port, tls://host:port and bare host:port (plaintext: a development tool).
func parseServer(uri string) (addr, host string, conf *tls.Config, err error) {
	addr = uri
	if rest, ok := strings.CutPrefix(uri, "tls://"); ok {
		addr = rest
		conf = &tls.Config{MinVersion: tls.VersionTLS12}
	} else {
		addr = strings.TrimPrefix(uri, "tcp://")
	}
	host, _, err = net.SplitHostPort(addr)
	if err != nil {
		return "", "", nil, fmt.Errorf("server %q: %w", uri, err)
	}
	if conf != nil {
		conf.ServerName = host
	}
	return addr, host, conf, nil
}

func dump(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("dump-manifests", flag.ContinueOnError)
	dsn := fs.String("dsn", os.Getenv("MHK_DB_DSN"), "MySQL DSN of the server (MHK_DB_DSN)")
	sim := fs.Int("sim", 0, "simVersion of the manifests")
	out := fs.String("out", "manifests.json", "file to write")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *dsn == "" || *sim == 0 || fs.NArg() == 0 {
		fs.Usage()
		return errors.New("need -dsn, -sim and at least one pack id")
	}
	st, err := store.Open(ctx, *dsn)
	if err != nil {
		return err
	}
	defer st.Close()
	if err := dumpManifests(ctx, st, int32(*sim), fs.Args(), *out); err != nil {
		return err
	}
	fmt.Printf("wrote %d packs to %s\n", fs.NArg(), *out)
	return nil
}
