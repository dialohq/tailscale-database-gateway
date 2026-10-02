package postgres

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dialohq/tailscale-database-gateway/internal/credentials"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"
)

type connectorFunc func(context.Context, ConnectRequest) (*Upstream, error)

func (function connectorFunc) Connect(ctx context.Context, request ConnectRequest) (*Upstream, error) {
	return function(ctx, request)
}

type connectionWithAddress struct {
	net.Conn
	remote net.Addr
}

func (connection connectionWithAddress) RemoteAddr() net.Addr { return connection.remote }

func TestGatewayRewritesAuthenticationAndProxies(t *testing.T) {
	clientGateway, client := net.Pipe()
	upstreamGateway, upstreamServer := net.Pipe()
	defer client.Close()
	defer upstreamServer.Close()
	var request ConnectRequest
	gateway := &Gateway{
		HandshakeTimeout: time.Second, RetryAttempts: 1,
		Authorize: func(_ context.Context, _, role string) (credentials.Identity, credentials.Provider, error) {
			if role != "admin" {
				t.Fatalf("role=%q", role)
			}
			return credentials.Identity{LoginName: "alice@example.com", NodeName: "laptop"}, func(context.Context, credentials.Identity, bool) (credentials.Credentials, error) {
				return credentials.Credentials{Username: "vault-user", Password: "secret"}, nil
			}, nil
		},
		Connector: connectorFunc(func(_ context.Context, got ConnectRequest) (*Upstream, error) {
			request = got
			return &Upstream{Conn: connectionWithAddress{upstreamGateway, &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 5432}}, PID: 7, SecretKey: []byte{1, 2, 3, 4}, TxStatus: 'I'}, nil
		}),
		CancelDialer: &net.Dialer{}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	done := make(chan struct{})
	go func() { gateway.handle(context.Background(), clientGateway); close(done) }()
	startup, _ := (&pgproto3.StartupMessage{ProtocolVersion: pgproto3.ProtocolVersion30, Parameters: map[string]string{"user": "admin", "database": "app", "application_name": "spoofed", "options": "-c role=postgres"}}).Encode(nil)
	_, _ = client.Write(startup)
	frontend := pgproto3.NewFrontend(client, client)
	for {
		message, err := frontend.Receive()
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := message.(*pgproto3.ReadyForQuery); ok {
			break
		}
	}
	if request.Username != "vault-user" || request.Database != "app" || request.RuntimeParameters["application_name"] != "tailscale:alice@example.com@laptop" {
		t.Fatalf("upstream request = %#v", request)
	}
	if _, exists := request.RuntimeParameters["options"]; exists {
		t.Fatal("unsafe options forwarded")
	}
	query, _ := (&pgproto3.Query{String: "select 1"}).Encode(nil)
	go func() { _, _ = client.Write(query) }()
	got := make([]byte, len(query))
	_, _ = io.ReadFull(upstreamServer, got)
	if !bytes.Equal(got, query) {
		t.Fatal("query was not transparently proxied")
	}
	_ = client.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("gateway did not stop")
	}
}

func TestConnectRetriesRejectedCredentials(t *testing.T) {
	var loads, finishes atomic.Int32
	gateway := &Gateway{HandshakeTimeout: time.Second, RetryAttempts: 2, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	gateway.Connector = connectorFunc(func(context.Context, ConnectRequest) (*Upstream, error) {
		if loads.Load() == 1 {
			return nil, &pgconn.PgError{Code: "28P01"}
		}
		return &Upstream{}, nil
	})
	provider := func(context.Context, credentials.Identity, bool) (credentials.Credentials, error) {
		loads.Add(1)
		return credentials.Credentials{Username: "u", Password: "p", Complete: func(rejected bool) error {
			if rejected {
				finishes.Add(1)
			}
			return nil
		}}, nil
	}
	loaded, _, _, err := gateway.connect(context.Background(), credentials.Identity{}, "readonly", "app", &pgproto3.StartupMessage{ProtocolVersion: pgproto3.ProtocolVersion30, Parameters: map[string]string{}}, provider)
	if err != nil {
		t.Fatal(err)
	}
	if loads.Load() != 2 || finishes.Load() != 1 || loaded.Username != "u" {
		t.Fatalf("loads=%d finishes=%d", loads.Load(), finishes.Load())
	}
}
