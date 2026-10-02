//go:build integration

package postgres

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dialohq/tailscale-database-gateway/internal/credentials"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgconn/ctxwatch"
)

func TestPostgresGatewayEndToEnd(t *testing.T) {
	upstreamURL := os.Getenv("POSTGRES_TEST_UPSTREAM")
	adminURL := os.Getenv("POSTGRES_TEST_ADMIN")
	if upstreamURL == "" || adminURL == "" {
		t.Fatal("run this test with `./scripts/test-postgres.sh`")
	}
	root := t.TempDir()
	credentialsPath := filepath.Join(root, "credentials.json")
	writeCredentials := func(password string) {
		t.Helper()
		contents := fmt.Sprintf(`{"username":"gateway_login","password":%q}`, password)
		if err := os.WriteFile(credentialsPath, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeCredentials("initial-password")

	connector, err := NewPGXConnector(upstreamURL)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	g := &Gateway{
		HandshakeTimeout: 5 * time.Second,
		RetryAttempts:    5,
		RetryDelay:       200 * time.Millisecond,
		Authorize: func(context.Context, string, string) (credentials.Identity, credentials.Provider, error) {
			return credentials.Identity{LoginName: "alice@example.com", NodeName: "alice-laptop"}, credentials.NewFileRoles(map[string]string{"readonly": credentialsPath})["readonly"], nil
		},
		Connector:    connector,
		CancelDialer: &net.Dialer{Timeout: time.Second},
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	serveErrors := make(chan error, 1)
	go func() { serveErrors <- g.Serve(ctx, listener) }()
	t.Cleanup(func() {
		cancel()
		if err := <-serveErrors; err != nil {
			t.Errorf("serve gateway: %v", err)
		}
	})

	gatewayURL := fmt.Sprintf("postgresql://readonly@%s/gateway_db?sslmode=prefer&application_name=spoofed", listener.Addr())
	clientConfig, err := pgx.ParseConfig(gatewayURL)
	if err != nil {
		t.Fatal(err)
	}
	clientConfig.BuildContextWatcherHandler = func(connection *pgconn.PgConn) ctxwatch.Handler {
		return &pgconn.CancelRequestContextWatcherHandler{Conn: connection, DeadlineDelay: 5 * time.Second}
	}
	queryContext, cancelQuery := context.WithTimeout(ctx, 5*time.Second)
	defer cancelQuery()
	client, err := pgx.ConnectConfig(queryContext, clientConfig)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close(context.Background()) })
	var backendPID int32
	var user, application, value string
	if err := client.QueryRow(queryContext, "SELECT pg_backend_pid(), current_user, current_setting('application_name'), value FROM protected").Scan(&backendPID, &user, &application, &value); err != nil {
		t.Fatal(err)
	}
	if user != "gateway_login" || application != "tailscale:alice@example.com@alice-laptop" || value != "visible" {
		t.Fatalf("query result = %q, %q, %q", user, application, value)
	}
	cancelContext, cancelSlowQuery := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancelSlowQuery()
	_, err = client.Exec(cancelContext, "SELECT pg_sleep(10)")
	var postgresError *pgconn.PgError
	if !errors.As(err, &postgresError) || postgresError.Code != "57014" {
		t.Fatalf("canceled query error = %v", err)
	}
	if err := client.Ping(queryContext); err != nil {
		t.Fatalf("connection was unusable after cancellation: %v", err)
	}
	admin, err := pgx.Connect(queryContext, adminURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close(context.Background()) })
	if err := client.Close(queryContext); err != nil {
		t.Fatal(err)
	}
	for {
		var connected bool
		if err := admin.QueryRow(queryContext, "SELECT EXISTS (SELECT FROM pg_stat_activity WHERE pid = $1)", backendPID).Scan(&connected); err != nil {
			t.Fatal(err)
		}
		if !connected {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
}
