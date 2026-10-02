package clickhouse

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"
)

func credentialsFunc(load func(context.Context, Identity) (Credentials, error)) CredentialsProvider {
	return func(ctx context.Context, identity Identity, _ bool) (Credentials, error) {
		return load(ctx, identity)
	}
}

type queueDialer struct {
	mu          sync.Mutex
	connections []net.Conn
}

func (d *queueDialer) DialContext(context.Context, string, string) (net.Conn, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.connections) == 0 {
		return nil, errors.New("no fake upstream connection")
	}
	connection := d.connections[0]
	d.connections = d.connections[1:]
	return connection, nil
}

func TestGatewayRewritesCredentialsAndPreservesTraffic(t *testing.T) {
	client, gatewayClient := net.Pipe()
	gatewayUpstream, upstream := net.Pipe()

	g := testGateway(&queueDialer{connections: []net.Conn{gatewayUpstream}})
	provider := credentialsFunc(func(_ context.Context, _ Identity) (Credentials, error) {
		return Credentials{Username: "vault-user", Password: "vault-password"}, nil
	})
	g.Authorize = func(_ context.Context, _ string, role string) (Identity, CredentialsProvider, error) {
		if role != "admin" {
			t.Fatalf("authorized role = %q", role)
		}
		return Identity{LoginName: "alice@example.com"}, provider, nil
	}
	done := make(chan struct{})
	go func() {
		g.handle(context.Background(), gatewayClient)
		close(done)
	}()

	original := clientHello{
		clientName: "ClickHouse client",
		major:      26,
		minor:      7,
		revision:   54481,
		database:   "analytics",
	}
	wireHello, err := original.encode("admin", "")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		_, _ = client.Write(wireHello)
	}()

	expected, err := original.encode("vault-user", "vault-password")
	if err != nil {
		t.Fatal(err)
	}
	rewritten := make([]byte, len(expected))
	if _, err := io.ReadFull(upstream, rewritten); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(rewritten, expected) {
		t.Fatalf("rewritten hello = %q, want %q", rewritten, expected)
	}

	go func() {
		writeUvarint(upstream, serverHelloPacket)
		_, _ = upstream.Write([]byte("server hello remainder"))
	}()
	serverReply := make([]byte, 1+len("server hello remainder"))
	if _, err := io.ReadFull(client, serverReply); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(serverReply, append([]byte{0}, []byte("server hello remainder")...)) {
		t.Fatalf("server reply = %q", serverReply)
	}

	go func() {
		_, _ = client.Write([]byte("query bytes"))
	}()
	query := make([]byte, len("query bytes"))
	if _, err := io.ReadFull(upstream, query); err != nil {
		t.Fatal(err)
	}
	if string(query) != "query bytes" {
		t.Fatalf("query = %q", query)
	}

	_ = client.Close()
	_ = upstream.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("gateway did not stop")
	}
}

func TestGatewayRejectsUnauthorizedRoleBeforeLoadingCredentials(t *testing.T) {
	client, gatewayClient := net.Pipe()
	defer client.Close()
	g := testGateway(&queueDialer{})
	loaded := false
	provider := credentialsFunc(func(context.Context, Identity) (Credentials, error) {
		loaded = true
		return Credentials{}, nil
	})
	g.Authorize = func(_ context.Context, _ string, role string) (Identity, CredentialsProvider, error) {
		return Identity{}, provider, errors.New("role " + role + " is not granted")
	}
	done := make(chan struct{})
	go func() {
		g.handle(context.Background(), gatewayClient)
		close(done)
	}()
	hello, err := (clientHello{clientName: "client", revision: 54481}).encode("admin", "")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = client.Write(hello)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("gateway did not reject unauthorized role")
	}
	if loaded {
		t.Fatal("unauthorized role loaded credentials")
	}
}

func TestGatewayReloadsCredentialsAfterHandshakeException(t *testing.T) {
	client, gatewayClient := net.Pipe()
	firstGatewayUpstream, firstUpstream := net.Pipe()
	secondGatewayUpstream, secondUpstream := net.Pipe()
	dialer := &queueDialer{connections: []net.Conn{firstGatewayUpstream, secondGatewayUpstream}}

	loads := 0
	releases := 0
	g := testGateway(dialer)
	g.retryAttempts = 2
	g.retryDelay = 0
	provider := credentialsFunc(func(_ context.Context, _ Identity) (Credentials, error) {
		loads++
		if loads == 1 {
			return Credentials{Username: "expired", Password: "old", Complete: func(bool) error {
				releases++
				return nil
			}}, nil
		}
		return Credentials{Username: "fresh", Password: "new", Complete: func(bool) error {
			releases++
			return nil
		}}, nil
	})
	g.Authorize = func(_ context.Context, _ string, role string) (Identity, CredentialsProvider, error) {
		if role != "readonly" {
			t.Fatalf("requested role = %q", role)
		}
		return Identity{LoginName: "alice@example.com", NodeName: "alice-laptop"}, provider, nil
	}
	done := make(chan struct{})
	go func() {
		g.handle(context.Background(), gatewayClient)
		close(done)
	}()

	hello, err := (clientHello{clientName: "client", revision: 54481, database: "default"}).encode("readonly", "ignored")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		_, _ = client.Write(hello)
	}()

	firstReceived, err := readClientHello(firstUpstream)
	if err != nil {
		t.Fatal(err)
	}
	if firstReceived.username != "expired" {
		t.Fatalf("first username = %q", firstReceived.username)
	}
	writeUvarint(firstUpstream, serverExceptionPacket)

	secondReceived, err := readClientHello(secondUpstream)
	if err != nil {
		t.Fatal(err)
	}
	if secondReceived.username != "fresh" {
		t.Fatalf("second username = %q", secondReceived.username)
	}
	go writeUvarint(secondUpstream, serverHelloPacket)

	packet, err := readUvarint(client)
	if err != nil {
		t.Fatal(err)
	}
	if packet != serverHelloPacket {
		t.Fatalf("client received packet %d, want hello", packet)
	}
	if loads != 2 {
		t.Fatalf("credential loads = %d, want 2", loads)
	}
	if releases != 1 {
		t.Fatalf("credential releases before session close = %d, want 1", releases)
	}

	_ = client.Close()
	_ = firstUpstream.Close()
	_ = secondUpstream.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("gateway did not stop")
	}
	if releases != 2 {
		t.Fatalf("credential releases = %d, want 2", releases)
	}
}

func TestReadClientHelloRejectsOversizedString(t *testing.T) {
	var wire bytes.Buffer
	writeUvarint(&wire, clientHelloPacket)
	writeUvarint(&wire, maxHandshakeString+1)
	if _, err := readClientHello(&wire); err == nil {
		t.Fatal("expected oversized string error")
	}
}

func testGateway(dialer *queueDialer) *Gateway {
	return &Gateway{
		config: config{
			upstream:         "fake:9000",
			handshakeTimeout: time.Second,
			retryAttempts:    1,
		},
		Authorize: func(context.Context, string, string) (Identity, CredentialsProvider, error) {
			provider := credentialsFunc(func(context.Context, Identity) (Credentials, error) {
				return Credentials{Username: "vault-user", Password: "vault-password"}, nil
			})
			return Identity{LoginName: "alice@example.com", NodeName: "alice-laptop"}, provider, nil
		},
		Dial:   dialer.DialContext,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}
