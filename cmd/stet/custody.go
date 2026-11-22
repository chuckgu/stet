package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/stetdb/stet/internal/config"
	"github.com/stetdb/stet/internal/custody"
	"github.com/stetdb/stet/internal/safetext"
)

// clientSide is what stet proxy serves its clients with on top of the
// relay (spec-v0.2-server 3, 4).
type clientSide struct {
	tls      *tls.Config    // nil: clients' TLS requests are declined
	login    *custody.Login // nil: authentication is relayed
	accounts []string       // the credentials file's, sorted
	database string         // the database the start's logins go to: upstream's
}

// openClientSide does what stet proxy does at start for its clients' side,
// before the gate is opened: with listen_tls it loads the certificate, and
// with agents.credentials it opens the credentials file and settles the
// agents' accounts (spec-v0.2-server 8, correction V1). An error stops the
// start. With custody it returns a copy of cfg whose AgentRoles are the
// file's accounts, so that the agent roles' checks and the gate see them;
// cfg itself is not changed.
func openClientSide(cfg *config.Config, log *slog.Logger) (*clientSide, *config.Config, error) {
	cs := &clientSide{}
	if cfg.ListenTLS != nil {
		var err error
		if cs.tls, err = certReloader(cfg.ListenTLS.Cert, cfg.ListenTLS.Key, log); err != nil {
			return nil, nil, fmt.Errorf("listen_tls: %w", err)
		}
	}
	if cfg.Credentials == "" {
		return cs, cfg, nil
	}
	store, err := custody.OpenStore(cfg.Credentials, os.Getenv, log)
	if err != nil {
		return nil, nil, fmt.Errorf("agents.credentials: %w", err)
	}
	cs.accounts = store.Accounts()
	if len(cs.accounts) == 0 {
		return nil, nil, errors.New("the credentials file has no account; add one with stet agent add")
	}
	if len(cfg.AgentRoles) > 0 {
		onlyRoles, onlyFile := differences(cfg.AgentRoles, cs.accounts)
		if len(onlyRoles) > 0 || len(onlyFile) > 0 {
			return nil, nil, fmt.Errorf("agents.roles and the credentials file name different accounts: "+
				"only in agents.roles: %q; only in the file: %q", onlyRoles, onlyFile)
		}
	}
	if cs.database, err = upstreamDatabase(cfg.Upstream); err != nil {
		return nil, nil, err
	}
	if cs.login, err = custody.NewLogin(store, cfg.Upstream, cfg.UpstreamTLS, cfg.UpstreamPlainFallback); err != nil {
		return nil, nil, err
	}
	if len(cfg.AgentRoles) == 0 {
		c := *cfg
		c.AgentRoles = cs.accounts
		cfg = &c
	}
	return cs, cfg, nil
}

// differences returns what only a holds and what only b holds, each sorted.
// Names are compared byte for byte.
func differences(a, b []string) (onlyA, onlyB []string) {
	for _, x := range a {
		if !slices.Contains(b, x) {
			onlyA = append(onlyA, x)
		}
	}
	for _, x := range b {
		if !slices.Contains(a, x) {
			onlyB = append(onlyB, x)
		}
	}
	slices.Sort(onlyA)
	slices.Sort(onlyB)
	return onlyA, onlyB
}

// upstreamDatabase is the database the upstream address names, as Stet's own
// connections read it: where Stet's start logs in as each account.
func upstreamDatabase(upstream string) (string, error) {
	pc, err := pgconn.ParseConfig(upstream)
	if err != nil {
		// pgconn's error can quote the address, and so its password.
		return "", errors.New("upstream: the address cannot be read")
	}
	if pc.Database == "" {
		return pc.User, nil // as the server reads a startup packet without one
	}
	return pc.Database, nil
}

// accountCheckTimeout bounds the start's login as one account, and
// accountChecksTimeout all of them (spec-v0.2-server 8). Variables so a test
// can shorten them.
var (
	accountCheckTimeout  = 5 * time.Second
	accountChecksTimeout = 30 * time.Second
)

// checkAccounts logs in upstream once as each of cs's accounts, and warns of
// each that cannot, with the kind of failure (spec-v0.2-server 8). It stops
// nothing: the other accounts are to work, and the logins of that one are
// refused anyway.
func checkAccounts(ctx context.Context, cs *clientSide, log *slog.Logger) {
	all, cancel := context.WithTimeout(ctx, accountChecksTimeout)
	defer cancel()
	for i, account := range cs.accounts {
		if all.Err() != nil {
			if ctx.Err() == nil {
				log.Warn("the start ran out of time to log in as the accounts left, which are not checked",
					"accounts", printable(cs.accounts[i:]))
			}
			return
		}
		one, cancelOne := context.WithTimeout(all, accountCheckTimeout)
		err := cs.login.Check(one, account, cs.database)
		cancelOne()
		if err != nil {
			log.Warn("an account of the credentials file cannot log in to the database: its agent's logins are refused until it can",
				"account", safetext.Printable(account), "err", err)
		}
	}
}

func printable(names []string) []string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = safetext.Printable(n)
	}
	return out
}

// certReloader serves the certificate at certPath and keyPath, and reads
// them again when either file changes; a reload that fails keeps the
// certificate in use and logs the error.
func certReloader(certPath, keyPath string, log *slog.Logger) (*tls.Config, error) {
	r := &certFiles{certPath: certPath, keyPath: keyPath, log: log}
	var err error
	if r.certSeen, err = os.Stat(certPath); err != nil {
		return nil, err
	}
	if r.keySeen, err = os.Stat(keyPath); err != nil {
		return nil, err
	}
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, err
	}
	r.cert = &cert
	if perm := r.keySeen.Mode().Perm(); perm&0o077 != 0 {
		log.Warn(fmt.Sprintf("listen_tls.key %s gives users other than its owner access (mode %04o): whoever can read it "+
			"can pose as stet to the agents; fix: chmod 600 %s", keyPath, perm, keyPath))
	}
	return &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: r.get}, nil
}

// certFiles is the certificate a certReloader serves, and its files as last
// looked at.
type certFiles struct {
	certPath, keyPath string
	log               *slog.Logger

	mu                sync.Mutex
	cert              *tls.Certificate
	certSeen, keySeen os.FileInfo // as last looked at, read or not; nil when it could not be
	failing           bool        // the last attempt failed, and was logged
}

// get is the tls.Config's GetCertificate: the certificate in use, read again
// first when either file looks different from the last time.
func (r *certFiles) get(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	certNow, certErr := os.Stat(r.certPath)
	keyNow, keyErr := os.Stat(r.keyPath)
	if err := errors.Join(certErr, keyErr); err != nil {
		r.certSeen, r.keySeen = nil, nil
		r.fail(err)
		return r.cert, nil
	}
	if unchanged(r.certSeen, certNow) && unchanged(r.keySeen, keyNow) {
		return r.cert, nil
	}
	r.certSeen, r.keySeen = certNow, keyNow
	cert, err := tls.LoadX509KeyPair(r.certPath, r.keyPath)
	if err != nil {
		r.fail(err)
		return r.cert, nil
	}
	r.cert = &cert
	r.failing = false
	r.log.Info("read listen_tls again; serving the new certificate from the next handshake")
	return r.cert, nil
}

// fail logs err once for as long as reading the files fails.
func (r *certFiles) fail(err error) {
	if r.failing {
		return
	}
	r.failing = true
	r.log.Error("could not read listen_tls again; serving the certificate in use", "err", err)
}

// unchanged reports whether a file looked at before (was) still looks the
// same (now): its modification time, size, mode and identity.
func unchanged(was, now os.FileInfo) bool {
	return was != nil && was.ModTime().Equal(now.ModTime()) && was.Size() == now.Size() &&
		was.Mode() == now.Mode() && os.SameFile(was, now)
}
