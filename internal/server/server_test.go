package server_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"log/slog"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/mhk-os/server/internal/admin"
	"github.com/mhk-os/server/internal/auth"
	"github.com/mhk-os/server/internal/proto"
	"github.com/mhk-os/server/internal/server"
	"github.com/mhk-os/server/internal/store"
	"github.com/mhk-os/server/internal/testclient"
	"github.com/mhk-os/server/internal/wire"
)

// Integration tests need a MySQL database: MHK_TEST_DSN=mhk:mhk@tcp(localhost:3307)/mhk_test?parseTime=true
// (docker compose up -d mysql creates mhk_test). They create uniquely named accounts and leave them in place.

const password = "kart-racer-2026"

var generous = auth.Limits{
	Register:         auth.Rate{N: 1000, Per: time.Hour},
	LoginFailIP:      auth.Rate{N: 1000, Per: time.Hour},
	LoginFailAccount: auth.Rate{N: 1000, Per: time.Hour},
}

type testServer struct {
	addr string
	st   *store.Store
	stop context.CancelFunc
	done chan error
}

func openStore(t *testing.T) *store.Store {
	t.Helper()
	dsn := os.Getenv("MHK_TEST_DSN")
	if dsn == "" {
		t.Skip("MHK_TEST_DSN not set")
	}
	ctx := context.Background()
	st, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.Migrate(ctx, nil); err != nil {
		t.Fatal(err)
	}
	return st
}

func start(t *testing.T, cfg server.Config, limits auth.Limits) *testServer {
	t.Helper()
	st := openStore(t)
	if cfg.RevalidateInterval == 0 {
		cfg.RevalidateInterval = 100 * time.Millisecond
	}
	if cfg.ConnLimit == (auth.Rate{}) {
		cfg.ConnLimit = auth.Rate{N: 1000, Per: time.Minute}
	}
	cfg.Version = "test"
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	svc := auth.NewService(st, st, auth.NewHasher(auth.DefaultParams, auth.DefaultHashConcurrency), limits)
	srv := server.New(cfg, server.Deps{Log: log, Auth: svc, Accounts: st, Sessions: st})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	ts := &testServer{addr: ln.Addr().String(), st: st, stop: cancel, done: make(chan error, 1)}
	go func() { ts.done <- srv.Serve(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		<-ts.done
	})
	return ts
}

func ctxT(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func connect(t *testing.T, ts *testServer) *testclient.Client {
	t.Helper()
	c, err := testclient.Connect(ctxT(t), ts.addr, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func uniqueName(prefix string) string {
	b := make([]byte, 5)
	rand.Read(b)
	return prefix + hex.EncodeToString(b)
}

func wantStatus(t *testing.T, err error, want proto.Status) {
	t.Helper()
	if got := testclient.StatusOf(err); got != want {
		t.Fatalf("status %s (%v), want %s", got, err, want)
	}
}

func waitClosed(t *testing.T, c *testclient.Client) {
	t.Helper()
	select {
	case <-c.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("connection not closed")
	}
}

func runAdmin(t *testing.T, st *store.Store, args ...string) string {
	t.Helper()
	var out bytes.Buffer
	if err := admin.Run(ctxT(t), &admin.Env{Store: st, Out: &out}, args); err != nil {
		t.Fatalf("admin %v: %v\n%s", args, err, out.String())
	}
	return out.String()
}

func TestAccountFlow(t *testing.T) {
	ts := start(t, server.Config{}, generous)
	ctx := ctxT(t)
	name := uniqueName("Flow_")
	c := connect(t, ts)

	reg, err := c.Register(ctx, name, password)
	if err != nil {
		t.Fatal(err)
	}
	a := reg.Account
	if a.Username != name || a.Role != proto.RolePlayer || a.Country != "" || !a.CountryFromIP || a.ID == 0 {
		t.Fatalf("account %+v", a)
	}
	if len(reg.Token) != 43 || time.Until(reg.Expires) < 89*24*time.Hour {
		t.Fatalf("session token %d chars, expires %v", len(reg.Token), reg.Expires)
	}

	login, err := connect(t, ts).Login(ctx, strings.ToUpper(name), password)
	if err != nil {
		t.Fatal(err)
	}
	if login.Account.ID != a.ID || login.Account.Username != name || login.Token == reg.Token {
		t.Fatalf("login %+v", login)
	}

	c2 := connect(t, ts)
	res, err := c2.Resume(ctx, login.Token)
	if err != nil {
		t.Fatal(err)
	}
	if res.Token != login.Token || res.Account.ID != a.ID || !res.Expires.After(login.Expires.Add(-time.Second)) {
		t.Fatalf("resume %+v", res)
	}
	if err := c2.Logout(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := c2.SetCountry(ctx, "DE"); testclient.StatusOf(err) != proto.StatusNotLoggedIn {
		t.Fatalf("after logout: %v", err)
	}
	if _, err := c2.Ping(ctx); err != nil {
		t.Fatalf("connection should stay open after logout: %v", err)
	}
	_, err = connect(t, ts).Resume(ctx, login.Token)
	wantStatus(t, err, proto.StatusSessionInvalid)
	_, err = connect(t, ts).Resume(ctx, "not-a-token")
	wantStatus(t, err, proto.StatusSessionInvalid)
	// The registration session is unaffected.
	if _, err := connect(t, ts).Resume(ctx, reg.Token); err != nil {
		t.Fatal(err)
	}
}

func TestDuplicateUsername(t *testing.T) {
	ts := start(t, server.Config{}, generous)
	ctx := ctxT(t)
	name := uniqueName("dup")
	if _, err := connect(t, ts).Register(ctx, name, password); err != nil {
		t.Fatal(err)
	}
	_, err := connect(t, ts).Register(ctx, strings.ToUpper(name), password)
	wantStatus(t, err, proto.StatusUsernameTaken)
}

func TestInvalidRegistration(t *testing.T) {
	ts := start(t, server.Config{}, generous)
	ctx := ctxT(t)
	c := connect(t, ts)
	for _, name := range []string{"ab", "_abc", "abc.", "has space", "Admin", "mhk-os", strings.Repeat("x", 21), "ünï"} {
		_, err := c.Register(ctx, name, password)
		wantStatus(t, err, proto.StatusInvalidUsername)
	}
	name := uniqueName("pw")
	for _, pw := range []string{"short", strings.Repeat("p", 129), strings.ToUpper(name)} {
		_, err := c.Register(ctx, name, pw)
		wantStatus(t, err, proto.StatusInvalidPassword)
	}
}

func TestWrongPassword(t *testing.T) {
	ts := start(t, server.Config{}, generous)
	ctx := ctxT(t)
	name := uniqueName("wrong")
	if _, err := connect(t, ts).Register(ctx, name, password); err != nil {
		t.Fatal(err)
	}
	c := connect(t, ts)
	_, err := c.Login(ctx, name, password+"x")
	wantStatus(t, err, proto.StatusInvalidCredentials)
	_, err = c.Login(ctx, uniqueName("nobody"), password)
	wantStatus(t, err, proto.StatusInvalidCredentials)
}

func TestRateLimits(t *testing.T) {
	limits := auth.Limits{
		Register:         auth.Rate{N: 2, Per: time.Hour},
		LoginFailIP:      auth.Rate{N: 100, Per: time.Hour},
		LoginFailAccount: auth.Rate{N: 3, Per: time.Hour},
	}
	ts := start(t, server.Config{}, limits)
	ctx := ctxT(t)
	c := connect(t, ts)
	name := uniqueName("rate")
	if _, err := c.Register(ctx, name, password); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Register(ctx, uniqueName("rate"), password); err != nil {
		t.Fatal(err)
	}
	_, err := c.Register(ctx, uniqueName("rate"), password)
	wantStatus(t, err, proto.StatusRateLimited)

	for range 3 {
		_, err := c.Login(ctx, name, "wrong-password")
		wantStatus(t, err, proto.StatusInvalidCredentials)
	}
	// Blocked per username regardless of case, even with the right password.
	_, err = c.Login(ctx, strings.ToUpper(name), password)
	wantStatus(t, err, proto.StatusRateLimited)

	t.Run("per IP", func(t *testing.T) {
		ts := start(t, server.Config{}, auth.Limits{
			Register:         generous.Register,
			LoginFailIP:      auth.Rate{N: 2, Per: time.Hour},
			LoginFailAccount: generous.LoginFailAccount,
		})
		c := connect(t, ts)
		for range 2 {
			_, err := c.Login(ctx, uniqueName("ghost"), password)
			wantStatus(t, err, proto.StatusInvalidCredentials)
		}
		_, err := c.Login(ctx, uniqueName("ghost"), password)
		wantStatus(t, err, proto.StatusRateLimited)
	})

	t.Run("connections", func(t *testing.T) {
		ts := start(t, server.Config{ConnLimit: auth.Rate{N: 2, Per: time.Minute}}, generous)
		connect(t, ts)
		connect(t, ts)
		if c, err := testclient.Connect(ctx, ts.addr, nil); err == nil {
			c.Close()
			t.Fatal("third connection within a minute accepted")
		}
	})
}

func TestSetCountry(t *testing.T) {
	ts := start(t, server.Config{}, generous)
	ctx := ctxT(t)
	c := connect(t, ts)
	_, err := c.SetCountry(ctx, "DE")
	wantStatus(t, err, proto.StatusNotLoggedIn)

	reg, err := c.Register(ctx, uniqueName("geo"), password)
	if err != nil {
		t.Fatal(err)
	}
	a, err := c.SetCountry(ctx, "SK")
	if err != nil {
		t.Fatal(err)
	}
	if a.Country != "SK" || a.CountryFromIP || a.ID != reg.Account.ID {
		t.Fatalf("account %+v", a)
	}
	for _, bad := range []string{"sk", "XX", "XK", "D"} {
		_, err := c.SetCountry(ctx, bad)
		wantStatus(t, err, proto.StatusBadRequest)
	}
	res, err := connect(t, ts).Resume(ctx, reg.Token)
	if err != nil || res.Account.Country != "SK" {
		t.Fatalf("persisted country %+v, %v", res, err)
	}
	a, err = c.SetCountry(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if a.Country != "" || !a.CountryFromIP {
		t.Fatalf("reset %+v", a)
	}
}

func TestDisableByCLI(t *testing.T) {
	ts := start(t, server.Config{}, generous)
	ctx := ctxT(t)
	name := uniqueName("bad")
	c := connect(t, ts)
	reg, err := c.Register(ctx, name, password)
	if err != nil {
		t.Fatal(err)
	}
	bystander := connect(t, ts)
	if _, err := bystander.Register(ctx, uniqueName("good"), password); err != nil {
		t.Fatal(err)
	}

	runAdmin(t, ts.st, "account", "disable", strings.ToLower(name), "--reason", "Cheating in races")
	d, err := c.WaitDisconnect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if d.Status != proto.StatusAccountDisabled || d.Message != "Cheating in races" {
		t.Fatalf("disconnect %+v", d)
	}
	waitClosed(t, c)
	if _, err := bystander.Ping(ctx); err != nil {
		t.Fatalf("other player affected: %v", err)
	}

	_, err = connect(t, ts).Login(ctx, name, password)
	wantStatus(t, err, proto.StatusAccountDisabled)
	_, err = connect(t, ts).Resume(ctx, reg.Token)
	wantStatus(t, err, proto.StatusSessionInvalid) // disabling revoked the session
	out := runAdmin(t, ts.st, "account", "show", name)
	if !strings.Contains(out, "disabled") || !strings.Contains(out, "Cheating in races") {
		t.Fatalf("show:\n%s", out)
	}

	runAdmin(t, ts.st, "account", "enable", name)
	if _, err := connect(t, ts).Login(ctx, name, password); err != nil {
		t.Fatalf("login after enable: %v", err)
	}

	runAdmin(t, ts.st, "account", "set-role", name, "moderator")
	l, err := connect(t, ts).Login(ctx, name, password)
	if err != nil || l.Account.Role != proto.RoleModerator {
		t.Fatalf("role after set-role: %+v %v", l, err)
	}
	if out := runAdmin(t, ts.st, "account", "list", "--role", "moderator"); !strings.Contains(out, name) {
		t.Fatalf("moderator list:\n%s", out)
	}

	live := connect(t, ts)
	if _, err := live.Login(ctx, name, password); err != nil {
		t.Fatal(err)
	}
	runAdmin(t, ts.st, "account", "delete", name, "--yes")
	d, err = live.WaitDisconnect(ctx)
	if err != nil || d.Status != proto.StatusSessionInvalid {
		t.Fatalf("after delete: %+v %v", d, err)
	}
	_, err = connect(t, ts).Login(ctx, name, password)
	wantStatus(t, err, proto.StatusInvalidCredentials)

	audit := runAdmin(t, ts.st, "audit", "--limit", "200")
	for _, action := range []string{store.ActionDisableAccount, store.ActionEnableAccount, store.ActionSetRole,
		store.ActionDeleteAccount} {
		if !strings.Contains(audit, action) {
			t.Errorf("audit log lacks %s", action)
		}
	}
	var ue *admin.UsageError
	if err := admin.Run(ctx, &admin.Env{Store: ts.st, Out: &bytes.Buffer{}},
		[]string{"account", "disable", name}); !errors.As(err, &ue) {
		t.Fatalf("disable without reason: %v", err)
	}
}

func TestLogoutElsewhereRevokesLiveConnection(t *testing.T) {
	ts := start(t, server.Config{}, generous)
	ctx := ctxT(t)
	a := connect(t, ts)
	reg, err := a.Register(ctx, uniqueName("two"), password)
	if err != nil {
		t.Fatal(err)
	}
	b := connect(t, ts)
	if _, err := b.Resume(ctx, reg.Token); err != nil {
		t.Fatal(err)
	}
	if err := b.Logout(ctx); err != nil {
		t.Fatal(err)
	}
	d, err := a.WaitDisconnect(ctx)
	if err != nil || d.Status != proto.StatusSessionInvalid {
		t.Fatalf("%+v %v", d, err)
	}
	if _, err := b.Ping(ctx); err != nil {
		t.Fatalf("logged-out connection dropped: %v", err)
	}
}

func TestHelloFirst(t *testing.T) {
	ts := start(t, server.Config{}, generous)
	ctx := ctxT(t)
	for _, typ := range []proto.Type{proto.TypePing, proto.TypeLogin, 999} {
		c, err := testclient.Dial(ctx, ts.addr, nil)
		if err != nil {
			t.Fatal(err)
		}
		_, err = c.RequestRaw(ctx, typ, proto.Encode(&proto.Ping{ClientTime: time.Now()}))
		if !errors.Is(err, testclient.ErrClosed) {
			t.Fatalf("type %d before Hello: %v", typ, err)
		}
		waitClosed(t, c)
	}

	c, err := testclient.Dial(ctx, ts.addr, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	h := testclient.DefaultHello()
	h.Magic = 0x12345678
	if _, err := c.Hello(ctx, h); !errors.Is(err, testclient.ErrClosed) {
		t.Fatalf("bad magic: %v", err)
	}

	c2, err := testclient.Dial(ctx, ts.addr, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	h = testclient.DefaultHello()
	h.Protocol = proto.Version + 1
	_, err = c2.Hello(ctx, h)
	wantStatus(t, err, proto.StatusVersionMismatch)
	waitClosed(t, c2)

	c3 := connect(t, ts)
	_, err = c3.Hello(ctx, testclient.DefaultHello())
	wantStatus(t, err, proto.StatusBadRequest)
}

func TestHelloOk(t *testing.T) {
	ts := start(t, server.Config{}, generous)
	ctx := ctxT(t)
	c, err := testclient.Dial(ctx, ts.addr, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ok, err := c.Hello(ctx, testclient.DefaultHello())
	if err != nil {
		t.Fatal(err)
	}
	if ok.Protocol != proto.Version || ok.ServerVersion != "test" || ok.UDPPort != 0 ||
		time.Since(ok.ServerTime).Abs() > time.Minute {
		t.Fatalf("%+v", ok)
	}
	pong, err := c.Ping(ctx)
	if err != nil || pong.ServerTime.IsZero() {
		t.Fatalf("%+v %v", pong, err)
	}
}

func rawFrameHeader(length uint32, typ proto.Type, id uint32) []byte {
	b := binary.LittleEndian.AppendUint32(nil, length)
	b = append(b, uint8(proto.KindRequest))
	b = binary.LittleEndian.AppendUint16(b, uint16(typ))
	return binary.LittleEndian.AppendUint32(b, id)
}

func TestFrameLimits(t *testing.T) {
	ts := start(t, server.Config{}, generous)
	ctx := ctxT(t)

	c := connect(t, ts)
	if err := c.WriteRaw(rawFrameHeader(wire.MaxFramePreLogin+1, proto.TypePing, 99)); err != nil {
		t.Fatal(err)
	}
	d, err := c.WaitDisconnect(ctx)
	if err != nil || d.Status != proto.StatusTooLarge {
		t.Fatalf("%+v %v", d, err)
	}
	waitClosed(t, c)

	// After login the limit is 16 MiB: a 100 KiB request is read and answered.
	c = connect(t, ts)
	if _, err := c.Register(ctx, uniqueName("big"), password); err != nil {
		t.Fatal(err)
	}
	_, err = c.RequestRaw(ctx, 999, make([]byte, 100<<10))
	wantStatus(t, err, proto.StatusUnsupported)
}

func TestUnsupportedAndMalformed(t *testing.T) {
	ts := start(t, server.Config{MaxMalformed: 3}, generous)
	ctx := ctxT(t)
	c := connect(t, ts)
	_, err := c.RequestRaw(ctx, 999, nil)
	wantStatus(t, err, proto.StatusUnsupported)
	_, err = c.RequestRaw(ctx, proto.RangeTimeTrials.First, nil)
	wantStatus(t, err, proto.StatusUnsupported)
	if _, err := c.Ping(ctx); err != nil {
		t.Fatalf("connection dropped after Unsupported: %v", err)
	}

	_, err = c.RequestRaw(ctx, proto.TypeLogin, []byte{10, 'a'}) // string beyond the body
	wantStatus(t, err, proto.StatusBadRequest)
	_, err = c.RequestRaw(ctx, proto.TypeLogin, []byte{2, 0xC3, 0x28, 0}) // invalid UTF-8
	wantStatus(t, err, proto.StatusBadRequest)
	_, err = c.RequestRaw(ctx, proto.TypePing, []byte{1})
	wantStatus(t, err, proto.StatusBadRequest)
	d, err := c.WaitDisconnect(ctx)
	if err != nil || d.Status != proto.StatusBadRequest {
		t.Fatalf("after repeated malformed requests: %+v %v", d, err)
	}
	waitClosed(t, c)
}

func TestShutdownNotifiesClients(t *testing.T) {
	ts := start(t, server.Config{}, generous)
	ctx := ctxT(t)
	c := connect(t, ts)
	ts.stop()
	d, err := c.WaitDisconnect(ctx)
	if err != nil || d.Status != proto.StatusServerError || d.Message != "Server is restarting" {
		t.Fatalf("%+v %v", d, err)
	}
	waitClosed(t, c)
	select {
	case err := <-ts.done:
		ts.done <- err
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not return")
	}
}
