package proto

import (
	"bytes"
	"reflect"
	"testing"
	"time"

	"github.com/mhk-os/server/internal/wire"
)

var when = time.UnixMilli(1_761_000_000_123).UTC()

func messages() []Message {
	list := []Message{
		&Hello{Magic: Magic, Protocol: Version, GameCommit: "abc123", GameBuild: 42, ContentVersion: 3, ModAPI: 1,
			Platform: "WindowsPlayer"},
		&HelloOk{Protocol: Version, ServerVersion: "v1.0.0", UDPPort: 7350, ServerTime: when},
		&Ping{ClientTime: when},
		&Pong{ClientTime: when, ServerTime: when.Add(time.Second)},
		&Credentials{Username: "Hans_Huhn", Password: "pässwörd!"},
		&Resume{Token: "tok"},
		&SetCountry{Country: "DE"},
		&Account{ID: 7, Username: "Hans", Country: "SK", CountryFromIP: true, Role: RoleModerator, Created: when},
		&Session{Token: "t", Expires: when, Account: Account{ID: 1, Username: "abc", Created: when}},
		&Disconnect{Status: StatusAccountDisabled, Message: "cheating"},
	}
	list = append(append(list, timeTrialMessages()...), profileMessages()...)
	return append(list, achievementMessages()...)
}

func TestRoundTrips(t *testing.T) {
	for _, m := range messages() {
		b := Encode(m)
		got := reflect.New(reflect.TypeOf(m).Elem()).Interface().(Message)
		if err := got.Decode(wire.NewReader(b)); err != nil {
			t.Fatalf("%T: %v", m, err)
		}
		if !reflect.DeepEqual(got, m) {
			t.Fatalf("%T: got %+v, want %+v", m, got, m)
		}
		// Every strict prefix must fail cleanly, never panic.
		for n := range len(b) {
			if err := got.Decode(wire.NewReader(b[:n])); err == nil {
				t.Fatalf("%T: prefix of %d/%d bytes decoded", m, n, len(b))
			}
		}
		// Trailing bytes are ignored (append-only bodies).
		if err := got.Decode(wire.NewReader(append(b, 1, 2, 3))); err != nil {
			t.Fatalf("%T with trailing bytes: %v", m, err)
		}
	}
}

// TestHelloBytes pins the exact encoding so the C# codec can assert the same bytes.
func TestHelloBytes(t *testing.T) {
	h := &Hello{Magic: Magic, Protocol: 1, GameCommit: "ab", GameBuild: 2, ContentVersion: 3, ModAPI: 4, Platform: "W"}
	want := []byte{
		'M', 'H', 'K', 'O', // magic
		0x01, 0x00, // protocol
		0x02, 'a', 'b', // gameCommit
		0x02, 0, 0, 0, // gameBuild
		0x03, 0, 0, 0, // contentVersion
		0x04, 0, 0, 0, // modApi
		0x01, 'W', // platform
	}
	if got := Encode(h); !bytes.Equal(got, want) {
		t.Fatalf("got % x\nwant % x", got, want)
	}
	frame := wire.AppendFrame(nil, uint8(KindRequest), uint16(TypeHello), 1, want)
	if !bytes.Equal(frame[:11], []byte{30, 0, 0, 0, 0, 1, 0, 1, 0, 0, 0}) {
		t.Fatalf("frame header % x", frame[:11])
	}
}

func TestResponses(t *testing.T) {
	ok := EncodeOK(&Ping{ClientTime: when})
	if ok[0] != 0 || ok[1] != 0 || len(ok) != 10 {
		t.Fatalf("ok body % x", ok)
	}
	if b := EncodeOK(nil); !bytes.Equal(b, []byte{0, 0}) {
		t.Fatalf("empty ok % x", b)
	}
	e := EncodeError(StatusUsernameTaken, "taken")
	if !bytes.Equal(e, []byte{4, 0, 5, 't', 'a', 'k', 'e', 'n'}) {
		t.Fatalf("error body % x", e)
	}
}

func TestLimits(t *testing.T) {
	w := wire.NewWriter(0)
	w.String(string(bytes.Repeat([]byte{'a'}, MaxUsernameField+1)))
	w.String("password")
	var c Credentials
	if err := c.Decode(wire.NewReader(w.Bytes())); err == nil {
		t.Fatal("over-long username accepted")
	}
	var s SetCountry
	if err := s.Decode(wire.NewReader([]byte{3, 'D', 'E', 'U'})); err == nil {
		t.Fatal("3-byte country accepted")
	}
	var a Account
	b := Encode(&Account{Username: "x"})
	b[8+1+1+1] = 2 // countryFromIp after id(8) + username(2) + country(1)
	if err := a.Decode(wire.NewReader(b)); err == nil {
		t.Fatal("bool 2 accepted")
	}
}

func TestRanges(t *testing.T) {
	for _, typ := range []Type{TypeHello, TypePing} {
		if !RangeConnection.Contains(typ) {
			t.Errorf("%d not in connection range", typ)
		}
	}
	for _, typ := range []Type{TypeRegister, TypeLogin, TypeResume, TypeLogout, TypeSetCountry} {
		if !RangeAccounts.Contains(typ) {
			t.Errorf("%d not in accounts range", typ)
		}
	}
	if StatusUnsupported.String() != "Unsupported" || Status(99).String() != "Status(99)" {
		t.Error("status names")
	}
}
