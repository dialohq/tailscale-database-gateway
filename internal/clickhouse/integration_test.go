//go:build integration

package clickhouse

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	credentialspkg "github.com/dialohq/tailscale-database-gateway/internal/credentials"
)

func TestClickHouseGatewaysEndToEnd(t *testing.T) {
	clickhouse := os.Getenv("CLICKHOUSE_BINARY")
	nativeUpstream := os.Getenv("CLICKHOUSE_TEST_NATIVE_UPSTREAM")
	httpUpstream := os.Getenv("CLICKHOUSE_TEST_HTTP_UPSTREAM")
	if clickhouse == "" || nativeUpstream == "" || httpUpstream == "" {
		t.Fatal("run this test with `./scripts/test-clickhouse.sh`")
	}

	dataDir := t.TempDir()
	credentialsPath := filepath.Join(dataDir, "credentials.json")
	if err := os.WriteFile(credentialsPath, []byte(`{"username":"vault_user","password":"expired-password"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	credentialRoles := credentialspkg.NewFileRoles(map[string]string{"readonly": credentialsPath})
	httpUpstreamURL, err := url.Parse(httpUpstream)
	if err != nil {
		t.Fatal(err)
	}
	gatewayContext, stopGateway := context.WithCancel(context.Background())
	defer stopGateway()
	g := &Gateway{
		config: config{
			upstream:           nativeUpstream,
			httpUpstream:       httpUpstreamURL,
			handshakeTimeout:   5 * time.Second,
			retryAttempts:      3,
			retryDelay:         500 * time.Millisecond,
			httpRetryBodyBytes: 1 << 20,
		},
		Authorize: func(_ context.Context, _ string, role string) (Identity, CredentialsProvider, error) {
			return Identity{LoginName: "local-test@example.com", NodeName: "local-test"}, credentialRoles[role], nil
		},
		Dial:   (&net.Dialer{Timeout: time.Second}).DialContext,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	gatewayDone := make(chan error, 1)
	go func() {
		gatewayDone <- g.Serve(gatewayContext, listener)
	}()

	go func() {
		time.Sleep(200 * time.Millisecond)
		_ = os.WriteFile(credentialsPath, []byte(`{"username":"vault_user","password":"fresh-password"}`), 0o600)
	}()
	clientContext, cancelClient := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancelClient()
	client := exec.CommandContext(
		clientContext,
		clickhouse,
		"client",
		"--host", "127.0.0.1",
		"--port", fmt.Sprint(listener.Addr().(*net.TCPAddr).Port),
		"--user", "readonly",
		"--query", "SELECT currentUser()",
	)
	output, err := client.CombinedOutput()
	if err != nil {
		t.Fatalf("clickhouse-client failed: %v\n%s", err, output)
	}
	if strings.TrimSpace(string(output)) != "vault_user" {
		t.Fatalf("currentUser() = %q, want vault_user", strings.TrimSpace(string(output)))
	}

	if err := os.WriteFile(credentialsPath, []byte(`{"username":"vault_user","password":"expired-password"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	httpListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		gatewayDone <- g.ServeHTTP(gatewayContext, httpListener)
	}()
	go func() {
		time.Sleep(200 * time.Millisecond)
		_ = os.WriteFile(credentialsPath, []byte(`{"username":"vault_user","password":"fresh-password"}`), 0o600)
	}()
	httpRequest, err := http.NewRequest(
		http.MethodPost,
		"http://"+httpListener.Addr().String()+"/?user=readonly&password=ignored",
		strings.NewReader("SELECT currentUser()"),
	)
	if err != nil {
		t.Fatal(err)
	}
	httpRequest.SetBasicAuth("readonly", "ignored")
	httpResponse, err := http.DefaultClient.Do(httpRequest)
	if err != nil {
		t.Fatal(err)
	}
	defer httpResponse.Body.Close()
	httpOutput, err := io.ReadAll(httpResponse.Body)
	if err != nil {
		t.Fatal(err)
	}
	if httpResponse.StatusCode != http.StatusOK {
		t.Fatalf("ClickHouse HTTP status = %d, body = %s", httpResponse.StatusCode, httpOutput)
	}
	if strings.TrimSpace(string(httpOutput)) != "vault_user" {
		t.Fatalf("HTTP currentUser() = %q, want vault_user", strings.TrimSpace(string(httpOutput)))
	}

	stopGateway()
	for range 2 {
		select {
		case err := <-gatewayDone:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(time.Second):
			t.Fatal("gateway did not stop")
		}
	}
}
