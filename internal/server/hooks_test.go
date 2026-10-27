package server_test

import (
	"context"
	"log/slog"
	"net"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/mhk-os/server/internal/auth"
	"github.com/mhk-os/server/internal/blobs"
	"github.com/mhk-os/server/internal/server"
	"github.com/mhk-os/server/internal/testclient"
)

// TestConnectionHooks checks OnAccountGone (only the account's last connection, also on logout) and OnClose.
func TestConnectionHooks(t *testing.T) {
	st := openStore(t)
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	svc := auth.NewService(st, st, auth.NewHasher(auth.DefaultParams, auth.DefaultHashConcurrency), generous)
	ghosts, err := blobs.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	srv := server.New(server.Config{Version: "test", ConnLimit: auth.Rate{N: 1000, Per: time.Minute}},
		server.Deps{Log: log, Auth: svc, Accounts: st, Sessions: st, Moderation: st, Runs: st, Profiles: st,
			Blobs: ghosts})
	var (
		mu     sync.Mutex
		gone   []uint64
		closed int
	)
	srv.OnAccountGone(func(id uint64) {
		mu.Lock()
		gone = append(gone, id)
		mu.Unlock()
	})
	srv.OnClose(func(*server.Conn) {
		mu.Lock()
		closed++
		mu.Unlock()
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	snapshot := func() ([]uint64, int) {
		mu.Lock()
		defer mu.Unlock()
		return append([]uint64(nil), gone...), closed
	}
	waitFor := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatal(what)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}

	rctx := ctxT(t)
	name := uniqueName("hk")
	c1, err := testclient.Connect(rctx, ln.Addr().String(), nil)
	if err != nil {
		t.Fatal(err)
	}
	s, err := c1.Register(rctx, name, password)
	if err != nil {
		t.Fatal(err)
	}
	c2, err := testclient.Connect(rctx, ln.Addr().String(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c2.Login(rctx, name, password); err != nil {
		t.Fatal(err)
	}

	// The account is still online on c2: no OnAccountGone.
	c1.Close()
	waitFor("OnClose not called", func() bool { _, n := snapshot(); return n == 1 })
	if g, _ := snapshot(); len(g) != 0 {
		t.Fatalf("gone after the first of two connections: %v", g)
	}
	// Logging out the last connection is the account going away, though the connection stays.
	if err := c2.Logout(rctx); err != nil {
		t.Fatal(err)
	}
	if g, n := snapshot(); len(g) != 1 || g[0] != s.Account.ID || n != 1 {
		t.Fatalf("after logout: gone %v, closed %d", g, n)
	}
	c2.Close()
	waitFor("OnClose not called for the logged-out connection", func() bool { _, n := snapshot(); return n == 2 })
	if g, _ := snapshot(); len(g) != 1 {
		t.Fatalf("gone twice: %v", g)
	}
}
