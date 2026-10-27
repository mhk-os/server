package server

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/mhk-os/server/internal/auth"
	"github.com/mhk-os/server/internal/geo"
	"github.com/mhk-os/server/internal/proto"
	"github.com/mhk-os/server/internal/store"
	"github.com/mhk-os/server/internal/wire"
)

// Player-facing messages.
const (
	msgInvalidCredentials = "Wrong username or password."
	msgUsernameTaken      = "This username is already taken."
	msgRateLimited        = "Too many attempts. Please try again later."
	msgSessionInvalid     = "Your session has ended. Please log in again."
	msgAccountDisabled    = "This account has been disabled."
	msgBadCountry         = "Unknown country code."
	msgWrongPassword      = "Wrong password."
	msgAccountDeleted     = "This account was deleted."
)

// auditAccountDeleted is the audit log reason of DeleteAccount; the store appends what happened to the runs.
const auditAccountDeleted = "account deleted by the player"

// StatusError is an expected request failure with a status and a message for the player.
type StatusError struct {
	Status  proto.Status
	Message string
}

func (e *StatusError) Error() string { return fmt.Sprintf("%s: %s", e.Status, e.Message) }

// Errorf returns a *StatusError.
func Errorf(status proto.Status, format string, args ...any) error {
	return &StatusError{Status: status, Message: fmt.Sprintf(format, args...)}
}

func disabledMessage(reason string) string {
	if reason == "" {
		return msgAccountDisabled
	}
	return reason
}

// authError maps auth flow errors to statuses; unknown errors pass through as server errors.
func authError(err error) error {
	var (
		rule     *auth.RuleError
		disabled *auth.DisabledError
	)
	switch {
	case errors.As(err, &rule) && rule.Password:
		return Errorf(proto.StatusInvalidPassword, "%s", rule.Msg)
	case errors.As(err, &rule):
		return Errorf(proto.StatusInvalidUsername, "%s", rule.Msg)
	case errors.As(err, &disabled):
		return Errorf(proto.StatusAccountDisabled, "%s", disabledMessage(disabled.Reason))
	case errors.Is(err, auth.ErrInvalidCredentials):
		return Errorf(proto.StatusInvalidCredentials, msgInvalidCredentials)
	case errors.Is(err, auth.ErrUsernameTaken):
		return Errorf(proto.StatusUsernameTaken, msgUsernameTaken)
	case errors.Is(err, auth.ErrRateLimited):
		return Errorf(proto.StatusRateLimited, msgRateLimited)
	case errors.Is(err, auth.ErrSessionInvalid):
		return Errorf(proto.StatusSessionInvalid, msgSessionInvalid)
	}
	return err
}

func (s *Server) registerConnection() {
	s.Handle(proto.TypeHello, false, func(context.Context, *Conn, *wire.Reader) (proto.Message, error) {
		return nil, Errorf(proto.StatusBadRequest, "Hello was already sent.")
	})
	s.Handle(proto.TypePing, false, func(_ context.Context, _ *Conn, r *wire.Reader) (proto.Message, error) {
		var req proto.Ping
		if err := req.Decode(r); err != nil {
			return nil, err
		}
		return &proto.Pong{ClientTime: req.ClientTime, ServerTime: time.Now()}, nil
	})
}

func (s *Server) registerAccounts() {
	s.Handle(proto.TypeRegister, false, s.handleRegister)
	s.Handle(proto.TypeLogin, false, s.handleLogin)
	s.Handle(proto.TypeResume, false, s.handleResume)
	s.Handle(proto.TypeLogout, true, s.handleLogout)
	s.Handle(proto.TypeSetCountry, true, s.handleSetCountry)
	s.Handle(proto.TypeDeleteAccount, true, s.handleDeleteAccount)
}

// loggedIn binds c to the session and records the account's activity.
func (s *Server) loggedIn(ctx context.Context, c *Conn, l *auth.Login, how string) (proto.Message, error) {
	if err := s.deps.Accounts.TouchActive(ctx, l.Account.ID, store.Now()); err != nil {
		return nil, err
	}
	c.login(l.Account, l.Hash)
	c.log.Info(how, "account", l.Account.ID, "username", l.Account.Username)
	return &proto.Session{Token: l.Token, Expires: l.Expires, Account: l.Account.Proto()}, nil
}

func (s *Server) handleRegister(ctx context.Context, c *Conn, r *wire.Reader) (proto.Message, error) {
	var req proto.Credentials
	if err := req.Decode(r); err != nil {
		return nil, err
	}
	l, err := s.deps.Auth.Register(ctx, c.IP(), s.deps.Geo.Country(c.IP()), req.Username, req.Password)
	if err != nil {
		return nil, authError(err)
	}
	return s.loggedIn(ctx, c, l, "registered")
}

func (s *Server) handleLogin(ctx context.Context, c *Conn, r *wire.Reader) (proto.Message, error) {
	var req proto.Credentials
	if err := req.Decode(r); err != nil {
		return nil, err
	}
	l, err := s.deps.Auth.Login(ctx, c.IP(), req.Username, req.Password)
	if err != nil {
		return nil, authError(err)
	}
	return s.loggedIn(ctx, c, l, "logged in")
}

func (s *Server) handleResume(ctx context.Context, c *Conn, r *wire.Reader) (proto.Message, error) {
	var req proto.Resume
	if err := req.Decode(r); err != nil {
		return nil, err
	}
	l, err := s.deps.Auth.Resume(ctx, c.IP(), req.Token)
	if err != nil {
		return nil, authError(err)
	}
	return s.loggedIn(ctx, c, l, "resumed")
}

func (s *Server) handleLogout(ctx context.Context, c *Conn, r *wire.Reader) (proto.Message, error) {
	if err := (proto.Empty{}).Decode(r); err != nil {
		return nil, err
	}
	hash, ok := c.sessionHash()
	if !ok {
		return nil, Errorf(proto.StatusNotLoggedIn, "Please log in first.")
	}
	if err := s.deps.Auth.Logout(ctx, hash); err != nil {
		return nil, err
	}
	c.log.Info("logged out")
	c.logout()
	return nil, nil
}

func (s *Server) handleSetCountry(ctx context.Context, c *Conn, r *wire.Reader) (proto.Message, error) {
	var req proto.SetCountry
	if err := req.Decode(r); err != nil {
		return nil, err
	}
	var country *string
	if req.Country != "" {
		if !geo.ValidCountry(req.Country) {
			return nil, Errorf(proto.StatusBadRequest, msgBadCountry)
		}
		country = &req.Country
	}
	id, ok := c.AccountID()
	if !ok {
		return nil, Errorf(proto.StatusNotLoggedIn, "Please log in first.")
	}
	if err := s.deps.Accounts.SetCountryUser(ctx, id, country); err != nil {
		return nil, err
	}
	acct, err := s.deps.Accounts.AccountByID(ctx, id)
	if err != nil {
		return nil, err
	}
	c.updateAccount(acct)
	a := acct.Proto()
	return &a, nil
}

// handleDeleteAccount deletes the logged-in account after checking its password. Its runs stay ranked under a new
// anonymous account (hidden with deleteRuns); its other connections are dropped, this one stays open, logged out.
func (s *Server) handleDeleteAccount(ctx context.Context, c *Conn, r *wire.Reader) (proto.Message, error) {
	var req proto.DeleteAccount
	if err := req.Decode(r); err != nil {
		return nil, err
	}
	acct := c.Account()
	if acct == nil {
		return nil, Errorf(proto.StatusNotLoggedIn, "Please log in first.")
	}
	checked, err := s.deps.Auth.CheckCredentials(ctx, c.IP(), acct.Username, req.Password)
	switch {
	case errors.Is(err, auth.ErrInvalidCredentials):
		return nil, Errorf(proto.StatusInvalidCredentials, msgWrongPassword)
	case err != nil:
		return nil, authError(err)
	case checked.ID != acct.ID:
		// The account was deleted elsewhere and its name taken again.
		return nil, Errorf(proto.StatusSessionInvalid, msgSessionInvalid)
	}
	act := store.Action{Actor: &acct.ID, Action: store.ActionDeleteAccount, TargetAccount: &acct.ID,
		TargetRef: acct.Username, Reason: auditAccountDeleted, CreatedAt: store.Now()}
	anon, err := s.deps.Accounts.DeleteOwnAccount(ctx, acct.ID, req.DeleteRuns, act)
	if errors.Is(err, store.ErrNotFound) {
		return nil, Errorf(proto.StatusSessionInvalid, msgSessionInvalid)
	}
	if err != nil {
		return nil, err
	}
	c.logout()
	dropped := s.hub.Disconnect(acct.ID, proto.StatusSessionInvalid, msgAccountDeleted)
	c.log.Info("account deleted by the player", "account", acct.ID, "username", acct.Username, "runs_to", anon,
		"runs_hidden", req.DeleteRuns, "connections", dropped)
	return nil, nil
}
