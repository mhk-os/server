// Package proto defines the MHK:OS protocol constants and messages on top of internal/wire (docs/protocol.md).
package proto

import "fmt"

const (
	// Magic opens every Hello: the bytes "MHKO" read as a little-endian u32.
	Magic uint32 = 0x4F4B484D
	// Version is the protocol revision; bump it only for incompatible changes.
	Version uint16 = 1
	// DefaultPort serves both the TCP control connection and (phase 3) UDP realtime.
	DefaultPort = 7350
)

// Kind is the first header byte of a frame.
type Kind uint8

const (
	KindRequest  Kind = 0
	KindResponse Kind = 1
	KindPush     Kind = 2
	KindNotify   Kind = 3
)

// Type identifies a message. Requests and pushes use separate numbering; the frame kind tells them apart.
type Type uint16

// Request types.
const (
	TypeHello      Type = 1
	TypePing       Type = 2
	TypeRegister   Type = 16
	TypeLogin      Type = 17
	TypeResume     Type = 18
	TypeLogout     Type = 19
	TypeSetCountry Type = 20
)

// Push types.
const (
	PushDisconnect Type = 1
)

// Range is an inclusive block of message types reserved for one area.
type Range struct{ First, Last Type }

// Contains reports whether t lies in the range.
func (r Range) Contains(t Type) bool { return t >= r.First && t <= r.Last }

// Type ranges; later phases allocate their messages inside these.
var (
	RangeConnection  = Range{1, 15}
	RangeAccounts    = Range{16, 63}
	RangeModeration  = Range{64, 127}
	RangeTimeTrials  = Range{128, 255}
	RangeLobbies     = Range{256, 511}
	RangeRaceControl = Range{512, 767}
)

// Status is the first field of every response body.
type Status uint16

const (
	StatusOk                 Status = 0
	StatusBadRequest         Status = 1
	StatusNotLoggedIn        Status = 2
	StatusInvalidCredentials Status = 3
	StatusUsernameTaken      Status = 4
	StatusInvalidUsername    Status = 5
	StatusInvalidPassword    Status = 6
	StatusRateLimited        Status = 7
	StatusAccountDisabled    Status = 8
	StatusVersionMismatch    Status = 9
	StatusNotFound           Status = 10
	StatusForbidden          Status = 11
	StatusTooLarge           Status = 12
	StatusServerError        Status = 13
	StatusSessionInvalid     Status = 14
	StatusUnsupported        Status = 15
)

var statusNames = [...]string{
	"Ok", "BadRequest", "NotLoggedIn", "InvalidCredentials", "UsernameTaken", "InvalidUsername", "InvalidPassword",
	"RateLimited", "AccountDisabled", "VersionMismatch", "NotFound", "Forbidden", "TooLarge", "ServerError",
	"SessionInvalid", "Unsupported",
}

func (s Status) String() string {
	if int(s) < len(statusNames) {
		return statusNames[s]
	}
	return fmt.Sprintf("Status(%d)", uint16(s))
}

// Role is an account's privilege level.
type Role uint8

const (
	RolePlayer    Role = 0
	RoleModerator Role = 1
)

func (r Role) String() string {
	switch r {
	case RolePlayer:
		return "player"
	case RoleModerator:
		return "moderator"
	}
	return fmt.Sprintf("Role(%d)", uint8(r))
}

// ParseRole accepts the names String returns.
func ParseRole(s string) (Role, error) {
	switch s {
	case "player":
		return RolePlayer, nil
	case "moderator":
		return RoleModerator, nil
	}
	return 0, fmt.Errorf("unknown role %q (want player or moderator)", s)
}
