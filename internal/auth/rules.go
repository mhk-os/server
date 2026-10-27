// Package auth implements account rules, password hashing, session tokens, rate limits and the
// register/login/resume flows on top of internal/store.
package auth

import (
	"strings"
	"unicode/utf8"
)

const (
	MinUsername = 3
	MaxUsername = 20
	MinPassword = 8
	MaxPassword = 128
)

var reservedUsernames = map[string]bool{
	"admin": true, "administrator": true, "moderator": true, "mod": true, "system": true, "server": true,
	"root": true, "mhk": true, "mhkos": true, "mhk-os": true, "anonymous": true,
}

// anonymousPrefix starts the usernames of anonymous accounts (store.DeleteOwnAccount); players cannot take it.
const anonymousPrefix = "anon-"

// RuleError explains to the player why a username or password was refused.
type RuleError struct {
	Password bool // false = username rule
	Msg      string
}

func (e *RuleError) Error() string { return e.Msg }

func isAlnum(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

// ValidateUsername applies the username rules of docs/protocol.md.
func ValidateUsername(name string) error {
	fail := func(msg string) error { return &RuleError{Msg: msg} }
	if len(name) < MinUsername || len(name) > MaxUsername {
		return fail("Usernames have 3 to 20 characters.")
	}
	for i := range len(name) {
		c := name[i]
		if !isAlnum(c) && c != '_' && c != '.' && c != '-' {
			return fail("Usernames may only contain letters A-Z, digits, '_', '.' and '-'.")
		}
	}
	if !isAlnum(name[0]) || !isAlnum(name[len(name)-1]) {
		return fail("Usernames must start and end with a letter or digit.")
	}
	if lower := strings.ToLower(name); reservedUsernames[lower] || strings.HasPrefix(lower, anonymousPrefix) {
		return fail("This username is reserved.")
	}
	return nil
}

// ValidatePassword applies the password rules; the length counts Unicode code points.
func ValidatePassword(password, username string) error {
	fail := func(msg string) error { return &RuleError{Password: true, Msg: msg} }
	n := utf8.RuneCountInString(password)
	if n < MinPassword || n > MaxPassword {
		return fail("Passwords have 8 to 128 characters.")
	}
	if strings.EqualFold(password, username) {
		return fail("The password must not be the username.")
	}
	return nil
}
