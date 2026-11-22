package custody

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"maps"
	"net"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stetdb/stet/internal/proxy"
	"github.com/stetdb/stet/internal/safetext"
	"github.com/stetdb/stet/internal/scram"
)

// Login is the proxy.Login of a credentials store.
type Login struct {
	store *Store
	mock  []byte // the store's mock secret
	up    *upstream
}

var _ proxy.Login = (*Login)(nil)

// NewLogin returns the Login of store over upstream, the config's upstream
// URL, reached with tlsConfig (nil: plaintext) and plainFallback as the
// proxy's own leg is (config.UpstreamTLS, UpstreamPlainFallback).
func NewLogin(store *Store, upstream string, tlsConfig *tls.Config, plainFallback bool) (*Login, error) {
	up, err := newUpstream(upstream, tlsConfig, plainFallback)
	if err != nil {
		return nil, err
	}
	return &Login{store: store, mock: store.MockSecret(), up: up}, nil
}

// Verifier returns account's token verifier, or for an account that is not
// in the store (never was, or removed since) the mock of its name.
func (l *Login) Verifier(account string) (v scram.Verifier, known bool) {
	if e, ok := l.store.Lookup(account); ok {
		return e.Token, true
	}
	return scram.Mock(account, l.mock), false
}

// Open logs in upstream as account with its stored password and hands the
// session over. It applies no time limit of its own: ctx should carry one.
func (l *Login) Open(ctx context.Context, account, database string, params map[string]string) (*proxy.Opened, error) {
	e, ok := l.store.Lookup(account)
	if !ok {
		return nil, errNoAccount
	}
	cfg, err := l.up.config(account, e.Password, database, params)
	if err != nil {
		return nil, err
	}
	return hijack(ctx, cfg)
}

// Check logs in as account and closes the session: what stet proxy does at
// start and stet agent before it writes. Its error carries no secret. It
// applies no time limit of its own: ctx should carry one.
func (l *Login) Check(ctx context.Context, account, database string) error {
	e, ok := l.store.Lookup(account)
	if !ok {
		return errNoAccount
	}
	return l.up.check(ctx, account, e.Password, database)
}

// TryPassword is Check with a password that is not in the store yet. It
// applies no time limit of its own: ctx should carry one.
func TryPassword(ctx context.Context, upstream string, tlsConfig *tls.Config, plainFallback bool, account, password, database string) error {
	up, err := newUpstream(upstream, tlsConfig, plainFallback)
	if err != nil {
		return err
	}
	return up.check(ctx, account, password, database)
}

var (
	errNoAccount  = errors.New("the account is not in the credentials file")
	errNoPassword = errors.New("the account's password is empty; not logging in")
)

// upstream is how to log in to the database for an account.
type upstream struct {
	base          *pgconn.Config // the address as Stet's own pool reads it; copied for each login
	tls           *tls.Config    // with no client certificate; nil: plaintext
	plainFallback bool
}

func newUpstream(address string, tlsConfig *tls.Config, plainFallback bool) (*upstream, error) {
	// Read as Stet's own pool reads it (cmd/stet's openPool), so that pgx's
	// and pgxpool's own keys (pool_max_conns, statement_cache_capacity, …)
	// are taken out and not sent as startup parameters.
	pool, err := pgxpool.ParseConfig(address)
	if err != nil {
		// pgx's error can quote the address, and so its password.
		return nil, errors.New("the upstream address cannot be read")
	}
	base := &pool.ConnConfig.Config
	if tlsConfig != nil {
		// A client certificate is for Stet's own connections only.
		tlsConfig = tlsConfig.Clone()
		tlsConfig.Certificates = nil
		tlsConfig.GetClientCertificate = nil
	}
	return &upstream{base: base, tls: tlsConfig, plainFallback: plainFallback && tlsConfig != nil}, nil
}

// config is what to log in with as account: the address's host, port and
// other settings, with account, password and database, params as the
// startup parameters, TLS as the proxy's leg has it, and protocol 3.0. The
// password is the one given and no other: pgconn's own from the environment
// or a password file is replaced, and an empty one is refused.
func (u *upstream) config(account, password, database string, params map[string]string) (*pgconn.Config, error) {
	if password == "" {
		return nil, errNoPassword
	}
	cfg := u.base.Copy()
	cfg.User = account
	cfg.Password = password
	cfg.Database = database
	cfg.RuntimeParams = maps.Clone(params)
	if cfg.RuntimeParams == nil {
		cfg.RuntimeParams = map[string]string{}
	}
	// Never from params: the account and database are the ones given, and a
	// replication connection is not a session.
	delete(cfg.RuntimeParams, "user")
	delete(cfg.RuntimeParams, "database")
	delete(cfg.RuntimeParams, "replication")
	cfg.TLSConfig = u.tls
	cfg.Fallbacks = nil
	if u.plainFallback {
		cfg.Fallbacks = []*pgconn.FallbackConfig{{Host: cfg.Host, Port: cfg.Port}}
	}
	// Stet answers its clients as protocol 3.0. A 3.2 session, which the
	// address or PGMAXPROTOCOLVERSION could ask for, would hand a 3.0 client
	// a cancel key longer than it can read.
	cfg.MinProtocolVersion = "3.0"
	cfg.MaxProtocolVersion = "3.0"
	cfg.OnNotice = nil // notices during the login are dropped
	cfg.OnNotification = nil
	return cfg, nil
}

// check logs in as account and closes the session. The session carries the
// run-time parameters the address names (application_name, options), as
// Stet's own pool reads them from it; the settings openPool adds on top are
// not sent.
func (u *upstream) check(ctx context.Context, account, password, database string) error {
	cfg, err := u.config(account, password, database, u.base.RuntimeParams)
	if err != nil {
		return err
	}
	pc, err := pgconn.ConnectConfig(ctx, cfg)
	if err != nil {
		return describe(err)
	}
	pc.Close(ctx)
	return nil
}

// hijack logs in with cfg and takes the session over from pgconn. Hijack is
// outside pgconn's compatibility promise: this is the one place that uses it.
func hijack(ctx context.Context, cfg *pgconn.Config) (*proxy.Opened, error) {
	var read *lastRead
	cfg.BuildFrontend = func(r io.Reader, w io.Writer) *pgproto3.Frontend {
		read = &lastRead{r: r} // one per attempt; the last is the session's
		return pgproto3.NewFrontend(read, w)
	}
	pc, err := pgconn.ConnectConfig(ctx, cfg)
	if err != nil {
		return nil, describe(err)
	}
	if err := pc.SyncConn(ctx); err != nil {
		pc.Close(ctx)
		return nil, describe(err)
	}
	hc, err := pc.Hijack()
	if err != nil {
		pc.Close(ctx)
		return nil, describe(err)
	}
	unread, ok := read.last(hc.Frontend.ReadBufferLen())
	if !ok {
		hc.Conn.Close()
		return nil, &loginError{kind: kindOther}
	}
	return &proxy.Opened{
		Conn:      hc.Conn,
		PID:       hc.PID,
		SecretKey: bytes.Clone(hc.SecretKey),
		Params:    maps.Clone(hc.ParameterStatuses),
		TxStatus:  hc.TxStatus,
		Unread:    unread,
	}, nil
}

// lastRead passes reads through and keeps the latest bytes read. A
// pgproto3.Frontend reads ahead into its buffer and hands messages out in
// order, so what it has not handed out is always the last of what it read.
type lastRead struct {
	r    io.Reader
	tail []byte
}

// keepRead is how many of the latest bytes lastRead keeps at least: far more
// than pgconn leaves unread at the end of a login, which is nothing.
const keepRead = 64 << 10

func (l *lastRead) Read(p []byte) (int, error) {
	n, err := l.r.Read(p)
	l.tail = append(l.tail, p[:n]...)
	if len(l.tail) > 2*keepRead {
		l.tail = append(l.tail[:0], l.tail[len(l.tail)-keepRead:]...)
	}
	return n, err
}

// last returns a copy of the last n bytes read. ok false: fewer are kept.
func (l *lastRead) last(n int) (b []byte, ok bool) {
	switch {
	case n == 0:
		return nil, true
	case n > len(l.tail):
		return nil, false
	}
	return bytes.Clone(l.tail[len(l.tail)-n:]), true
}

// The kinds of failed login.
const (
	kindWrongPassword = "wrong password"
	kindUnreachable   = "cannot reach the server"
	kindCertificate   = "certificate"
	kindRefusedTLS    = "refused TLS"
	kindOther         = "other"
)

// loginError is why a login upstream failed, in words that quote neither
// the address nor a secret: the kind of failure, and the server's own error
// when it sent one. It wraps nothing: pgconn's errors quote the address, and
// its ConnectError holds the config, password included.
type loginError struct {
	kind    string
	code    string // the server's SQLSTATE; "" when it sent no error
	message string // the server's message, printable
}

func (e *loginError) Error() string {
	s := "upstream login: " + e.kind
	if e.code != "" {
		s += ": " + e.message + " (SQLSTATE " + e.code + ")"
	}
	return s
}

// describe is pgconn's err as a loginError.
func describe(err error) *loginError {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		e := &loginError{kind: kindOther, code: safetext.Printable(pgErr.Code), message: safetext.Printable(pgErr.Message)}
		if pgErr.Code == "28P01" {
			e.kind = kindWrongPassword
		}
		return e
	}
	switch {
	case strings.Contains(err.Error(), "server refused TLS connection"):
		// pgconn's words for an N; it has no error type for it. Should they
		// change, TestLoginErrorsCarryNoSecrets/RefusedTLS and
		// TestOpenFallsBackToPlaintextOnlyWhenAllowed/NotAllowed fail.
		return &loginError{kind: kindRefusedTLS}
	case badCertificate(err):
		return &loginError{kind: kindCertificate}
	case unreachable(err):
		return &loginError{kind: kindUnreachable}
	}
	return &loginError{kind: kindOther}
}

// badCertificate: the server's certificate did not verify.
func badCertificate(err error) bool {
	var (
		verify    *tls.CertificateVerificationError
		authority x509.UnknownAuthorityError
		hostname  x509.HostnameError
		invalid   x509.CertificateInvalidError
	)
	return errors.As(err, &verify) || errors.As(err, &authority) || errors.As(err, &hostname) || errors.As(err, &invalid)
}

// unreachable: the name did not resolve, the connection was not taken, or
// the server did not answer in time.
func unreachable(err error) bool {
	var (
		dns *net.DNSError
		op  *net.OpError
	)
	return errors.As(err, &dns) ||
		(errors.As(err, &op) && op.Op == "dial") ||
		pgconn.Timeout(err) || errors.Is(err, context.DeadlineExceeded)
}
