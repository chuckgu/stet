package proxy

import (
	"context"
	"net"

	"github.com/stetdb/stet/internal/scram"
)

// Opened is an upstream session the proxy did not log in to itself.
type Opened struct {
	Conn      net.Conn          // logged in and idle
	PID       uint32            // the server process
	SecretKey []byte            // its cancel key
	Params    map[string]string // the settings the server reported at login
	TxStatus  byte              // from the server's ReadyForQuery
	Unread    []byte            // bytes already read from Conn after that, if any
}

// Login authenticates clients for the proxy and opens their sessions.
type Login interface {
	// Verifier returns what to check account's token against. known false:
	// there is no such account; the verifier is a mock.
	Verifier(account string) (v scram.Verifier, known bool)
	// Open logs in upstream as account and hands the session over. params
	// are the client's startup parameters without user, database and
	// replication. Its error is for the log, never for the client.
	Open(ctx context.Context, account, database string, params map[string]string) (*Opened, error)
}
