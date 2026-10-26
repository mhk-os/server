package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"time"

	"golang.org/x/crypto/acme/autocert"
)

// certReloader serves a certificate from files and reloads it when either file changes, so renewing the
// certificate (e.g. by certbot) needs no restart. Files are checked at most every checkEvery.
type certReloader struct {
	certFile, keyFile string
	log               *slog.Logger

	mu        sync.Mutex
	cert      *tls.Certificate
	certMod   time.Time
	keyMod    time.Time
	lastCheck time.Time
}

const checkEvery = 10 * time.Second

func newCertReloader(certFile, keyFile string, log *slog.Logger) (*certReloader, error) {
	r := &certReloader{certFile: certFile, keyFile: keyFile, log: log}
	if err := r.reload(); err != nil {
		return nil, err
	}
	return r, nil
}

func modTime(path string) (time.Time, error) {
	st, err := os.Stat(path)
	if err != nil {
		return time.Time{}, err
	}
	return st.ModTime(), nil
}

// reload loads the pair; callers hold mu (or own r exclusively).
func (r *certReloader) reload() error {
	certMod, err := modTime(r.certFile)
	if err != nil {
		return err
	}
	keyMod, err := modTime(r.keyFile)
	if err != nil {
		return err
	}
	cert, err := tls.LoadX509KeyPair(r.certFile, r.keyFile)
	if err != nil {
		return fmt.Errorf("load TLS certificate: %w", err)
	}
	r.cert, r.certMod, r.keyMod = &cert, certMod, keyMod
	return nil
}

func (r *certReloader) getCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if now := time.Now(); now.Sub(r.lastCheck) >= checkEvery {
		r.lastCheck = now
		certMod, err1 := modTime(r.certFile)
		keyMod, err2 := modTime(r.keyFile)
		if err1 == nil && err2 == nil && (!certMod.Equal(r.certMod) || !keyMod.Equal(r.keyMod)) {
			// A half-written pair fails to load; keep serving the old one and retry on the next check.
			if err := r.reload(); err != nil {
				r.log.Warn("TLS certificate reload failed, keeping the previous one", "err", err)
			} else {
				r.log.Info("TLS certificate reloaded")
			}
		}
	}
	return r.cert, nil
}

// tlsSettings selects the TLS source.
type tlsSettings struct {
	certFile, keyFile               string
	acmeDomain, acmeCache, acmeHTTP string
}

// buildTLS returns nil for plaintext. With ACME it also starts the HTTP-01 challenge listener, stopped with ctx.
func buildTLS(ctx context.Context, s tlsSettings, log *slog.Logger) (*tls.Config, error) {
	files := s.certFile != "" || s.keyFile != ""
	acme := s.acmeDomain != ""
	switch {
	case files && acme:
		return nil, errors.New("set either a TLS certificate (MHK_TLS_CERT/MHK_TLS_KEY) or MHK_ACME_DOMAIN, not both")
	case files:
		if s.certFile == "" || s.keyFile == "" {
			return nil, errors.New("MHK_TLS_CERT and MHK_TLS_KEY must be set together")
		}
		r, err := newCertReloader(s.certFile, s.keyFile, log)
		if err != nil {
			return nil, err
		}
		return &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: r.getCertificate}, nil
	case acme:
		if s.acmeCache == "" {
			return nil, errors.New("MHK_ACME_CACHE is required with MHK_ACME_DOMAIN")
		}
		m := &autocert.Manager{
			Prompt:     autocert.AcceptTOS,
			HostPolicy: autocert.HostWhitelist(s.acmeDomain),
			Cache:      autocert.DirCache(s.acmeCache),
		}
		srv := &http.Server{Addr: s.acmeHTTP, Handler: m.HTTPHandler(nil), ReadHeaderTimeout: 10 * time.Second}
		go func() {
			log.Info("ACME HTTP-01 listener", "addr", s.acmeHTTP, "domain", s.acmeDomain)
			if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Error("ACME HTTP listener failed", "err", err)
			}
		}()
		context.AfterFunc(ctx, func() { srv.Close() })
		return &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: m.GetCertificate}, nil
	}
	return nil, nil
}
