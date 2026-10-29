package proto

import (
	"time"

	"github.com/mhk-os/server/internal/wire"
)

// Field limits from docs/protocol.md.
const (
	MaxGameCommit     = 40
	MaxPlatform       = 32
	MaxServerVersion  = 64
	MaxUsernameField  = 64
	MaxPasswordField  = 512
	MaxToken          = 128
	MaxAccountName    = 20
	MaxCountry        = 2
	MaxMessage        = wire.DefaultMaxString
	MaxDisconnectText = wire.DefaultMaxString
)

// Message is a body that encodes and decodes itself. Decode reads every field and returns the first error;
// trailing bytes are ignored because bodies are append-only.
type Message interface {
	Encode(w *wire.Writer)
	Decode(r *wire.Reader) error
}

// Empty is the body of requests and responses without fields (Logout).
type Empty struct{}

func (Empty) Encode(*wire.Writer)         {}
func (Empty) Decode(r *wire.Reader) error { return r.Err() }

// Hello is request 1, the mandatory first request of a connection.
type Hello struct {
	Magic          uint32
	Protocol       uint16
	GameCommit     string
	GameBuild      int32
	ContentVersion int32
	ModAPI         int32
	Platform       string
}

func (m *Hello) Encode(w *wire.Writer) {
	w.U32(m.Magic)
	w.U16(m.Protocol)
	w.String(m.GameCommit)
	w.I32(m.GameBuild)
	w.I32(m.ContentVersion)
	w.I32(m.ModAPI)
	w.String(m.Platform)
}

func (m *Hello) Decode(r *wire.Reader) error {
	m.Magic = r.U32()
	m.Protocol = r.U16()
	m.GameCommit = r.String(MaxGameCommit)
	m.GameBuild = r.I32()
	m.ContentVersion = r.I32()
	m.ModAPI = r.I32()
	m.Platform = r.String(MaxPlatform)
	return r.Err()
}

// Player devices (LobbyMember.platform, RaceSlot.platform).
const (
	PlatformUnknown uint8 = 0
	PlatformWindows uint8 = 1
	PlatformMacOS   uint8 = 2
	PlatformLinux   uint8 = 3
	PlatformAndroid uint8 = 4
	PlatformIOS     uint8 = 5
)

// PlatformOf maps Hello.platform (a Unity RuntimePlatform name, editors included) to a Platform* constant.
func PlatformOf(name string) uint8 {
	switch name {
	case "WindowsPlayer", "WindowsEditor":
		return PlatformWindows
	case "OSXPlayer", "OSXEditor":
		return PlatformMacOS
	case "LinuxPlayer", "LinuxEditor":
		return PlatformLinux
	case "Android":
		return PlatformAndroid
	case "IPhonePlayer":
		return PlatformIOS
	}
	return PlatformUnknown
}

// HelloOk is the response to Hello.
type HelloOk struct {
	Protocol      uint16
	ServerVersion string
	UDPPort       uint16
	ServerTime    time.Time
}

func (m *HelloOk) Encode(w *wire.Writer) {
	w.U16(m.Protocol)
	w.String(m.ServerVersion)
	w.U16(m.UDPPort)
	w.Time(m.ServerTime)
}

func (m *HelloOk) Decode(r *wire.Reader) error {
	m.Protocol = r.U16()
	m.ServerVersion = r.String(MaxServerVersion)
	m.UDPPort = r.U16()
	m.ServerTime = r.Time()
	return r.Err()
}

// Ping is request 2.
type Ping struct {
	ClientTime time.Time
}

func (m *Ping) Encode(w *wire.Writer)       { w.Time(m.ClientTime) }
func (m *Ping) Decode(r *wire.Reader) error { m.ClientTime = r.Time(); return r.Err() }

// Pong is the response to Ping.
type Pong struct {
	ClientTime time.Time
	ServerTime time.Time
}

func (m *Pong) Encode(w *wire.Writer) {
	w.Time(m.ClientTime)
	w.Time(m.ServerTime)
}

func (m *Pong) Decode(r *wire.Reader) error {
	m.ClientTime = r.Time()
	m.ServerTime = r.Time()
	return r.Err()
}

// Credentials is the body of Register (16) and Login (17).
type Credentials struct {
	Username string
	Password string
}

func (m *Credentials) Encode(w *wire.Writer) {
	w.String(m.Username)
	w.String(m.Password)
}

func (m *Credentials) Decode(r *wire.Reader) error {
	m.Username = r.String(MaxUsernameField)
	m.Password = r.String(MaxPasswordField)
	return r.Err()
}

// Resume is request 18.
type Resume struct {
	Token string
}

func (m *Resume) Encode(w *wire.Writer)       { w.String(m.Token) }
func (m *Resume) Decode(r *wire.Reader) error { m.Token = r.String(MaxToken); return r.Err() }

// SetCountry is request 20; Country is "" or an upper-case ISO 3166-1 alpha-2 code.
type SetCountry struct {
	Country string
}

func (m *SetCountry) Encode(w *wire.Writer)       { w.String(m.Country) }
func (m *SetCountry) Decode(r *wire.Reader) error { m.Country = r.String(MaxCountry); return r.Err() }

// ChangeUsername is request 29; the response is the renamed Account.
type ChangeUsername struct {
	Username string
}

func (m *ChangeUsername) Encode(w *wire.Writer) { w.String(m.Username) }
func (m *ChangeUsername) Decode(r *wire.Reader) error {
	m.Username = r.String(MaxUsernameField)
	return r.Err()
}

// Account is the public view of an account.
type Account struct {
	ID            uint64
	Username      string
	Country       string
	CountryFromIP bool
	Role          Role
	Created       time.Time
}

func (m *Account) Encode(w *wire.Writer) {
	w.U64(m.ID)
	w.String(m.Username)
	w.String(m.Country)
	w.Bool(m.CountryFromIP)
	w.U8(uint8(m.Role))
	w.Time(m.Created)
}

func (m *Account) Decode(r *wire.Reader) error {
	m.ID = r.U64()
	m.Username = r.String(MaxAccountName)
	m.Country = r.String(MaxCountry)
	m.CountryFromIP = r.Bool()
	m.Role = Role(r.U8())
	m.Created = r.Time()
	return r.Err()
}

// Session is the response to Register, Login and Resume.
type Session struct {
	Token   string
	Expires time.Time
	Account Account
}

func (m *Session) Encode(w *wire.Writer) {
	w.String(m.Token)
	w.Time(m.Expires)
	m.Account.Encode(w)
}

func (m *Session) Decode(r *wire.Reader) error {
	m.Token = r.String(MaxToken)
	m.Expires = r.Time()
	return m.Account.Decode(r)
}

// DeleteAccount is request 26.
type DeleteAccount struct {
	Password   string
	DeleteRuns bool // hide the anonymized runs too
}

func (m *DeleteAccount) Encode(w *wire.Writer) {
	w.String(m.Password)
	w.Bool(m.DeleteRuns)
}

func (m *DeleteAccount) Decode(r *wire.Reader) error {
	m.Password = r.String(MaxPasswordField)
	m.DeleteRuns = r.Bool()
	return r.Err()
}

// Disconnect is push 1: the server is about to close the connection.
type Disconnect struct {
	Status  Status
	Message string
}

func (m *Disconnect) Encode(w *wire.Writer) {
	w.U16(uint16(m.Status))
	w.String(m.Message)
}

func (m *Disconnect) Decode(r *wire.Reader) error {
	m.Status = Status(r.U16())
	m.Message = r.String(MaxDisconnectText)
	return r.Err()
}

// EncodeOK returns a success response body: status Ok followed by the payload (nil for none).
func EncodeOK(payload Message) []byte {
	w := wire.NewWriter(64)
	w.U16(uint16(StatusOk))
	if payload != nil {
		payload.Encode(w)
	}
	return w.Bytes()
}

// EncodeError returns an error response body: status followed by the player-facing message.
func EncodeError(status Status, message string) []byte {
	w := wire.NewWriter(3 + len(message))
	w.U16(uint16(status))
	w.String(message)
	return w.Bytes()
}

// Encode returns the bytes of a push, notify or request body.
func Encode(m Message) []byte {
	w := wire.NewWriter(64)
	m.Encode(w)
	return w.Bytes()
}
