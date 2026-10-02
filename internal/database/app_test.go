package database

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/dialohq/tailscale-database-gateway/internal/backend"
	"github.com/dialohq/tailscale-database-gateway/internal/tailnet"

	gatewayconfig "github.com/dialohq/tailscale-database-gateway/internal/config"
)

func TestConfigureDestinations(t *testing.T) {
	configured, err := configure(map[string]gatewayconfig.Destination{
		"wax":       {Backend: "clickhouse", NativePort: 9000, NativeUpstream: "clickhouse:9000", HTTPPort: 8123, HTTPUpstream: "http://clickhouse:8123", Capability: "example.com/cap/clickhouse", CredentialFiles: map[string]string{"readonly": "/readonly.json"}},
		"app-db-ro": {Backend: "postgres", Port: 5432, Upstream: "postgresql://db-ro:5432/postgres", Capability: "example.com/cap/clickhouse", CredentialFiles: map[string]string{"readonly": "/readonly.json"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	ports := make(map[uint16]bool)
	for _, destination := range configured {
		ports[destination.ListenPort()] = true
	}
	if len(configured) != 3 || !ports[9000] || !ports[8123] || !ports[5432] {
		t.Fatalf("configured ports = %#v", ports)
	}
}

func TestConfigureDestinationsRejectsInvalidDocuments(t *testing.T) {
	for name, destinations := range map[string]map[string]gatewayconfig.Destination{
		"empty": {},
		"invalid name": {
			"Wax": {Backend: "clickhouse"},
		},
		"unsupported backend": {
			"wax": {Backend: "mysql"},
		},
		"invalid capability": {
			"wax": {Backend: "clickhouse", NativePort: 9000, NativeUpstream: "clickhouse:9000", Capability: " example.com/cap/clickhouse", CredentialFiles: map[string]string{"readonly": "/readonly.json"}},
		},
		"backend field mismatch": {
			"wax": {Backend: "clickhouse", Port: 9000, Capability: "example.com/cap/clickhouse", CredentialFiles: map[string]string{"readonly": "/readonly.json"}},
		},
		"port collision": {
			"wax":       {Backend: "clickhouse", NativePort: 5432, NativeUpstream: "clickhouse:9000", Capability: "example.com/cap/clickhouse", CredentialFiles: map[string]string{"readonly": "/readonly.json"}},
			"app-db-ro": {Backend: "postgres", Port: 5432, Upstream: "postgresql://db-ro:5432/postgres", Capability: "example.com/cap/postgres-ro", CredentialFiles: map[string]string{"readonly": "/readonly.json"}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := configure(destinations); err == nil {
				t.Fatal("invalid configuration was accepted")
			}
		})
	}
}

func TestServeRegistersListenersBeforeServing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ports := []uint16{5432, 5433, 8123, 9000}
		var registering atomic.Bool
		var registered, started, stopped atomic.Int32
		allStarted := make(chan struct{})
		serveError := errors.New("backend stopped")
		listeners := make(map[uint16]*testListener)
		var backends []backend.Backend
		for _, port := range ports {
			listeners[port] = &testListener{}
			backends = append(backends, testBackend{port: port, serve: func(ctx context.Context, listener net.Listener) error {
				defer stopped.Add(1)
				if registered.Load() != int32(len(ports)) {
					t.Error("serving began before all listeners were registered")
				}
				if listener != listeners[port] {
					t.Errorf("wrong listener for port %d", port)
				}
				if started.Add(1) == int32(len(ports)) {
					close(allStarted)
				}
				if port == ports[0] {
					<-allStarted
					return serveError
				}
				<-ctx.Done()
				return ctx.Err()
			}})
		}
		err := serve(context.Background(), backends, func(port uint16) (net.Listener, error) {
			if registering.Swap(true) {
				t.Error("concurrent listener registration")
			}
			time.Sleep(time.Millisecond)
			registered.Add(1)
			registering.Store(false)
			return listeners[port], nil
		}, nil, slog.Default())
		if !errors.Is(err, serveError) {
			t.Fatalf("serve error = %v", err)
		}
		if stopped.Load() != int32(len(ports)) {
			t.Error("serve returned before all backends stopped")
		}
		for port, listener := range listeners {
			if !listener.closed.Load() {
				t.Errorf("listener on port %d was not closed", port)
			}
		}
	})
}

func TestServeClosesListenersOnRegistrationFailure(t *testing.T) {
	listenError := errors.New("registration failed")
	listener := &testListener{}
	var started atomic.Bool
	var attempted []uint16
	var backends []backend.Backend
	for _, port := range []uint16{5432, 5433, 8123} {
		backends = append(backends, testBackend{port: port, serve: func(context.Context, net.Listener) error {
			started.Store(true)
			return nil
		}})
	}
	err := serve(context.Background(), backends, func(port uint16) (net.Listener, error) {
		attempted = append(attempted, port)
		if port == 5433 {
			return nil, listenError
		}
		return listener, nil
	}, nil, slog.Default())
	if !errors.Is(err, listenError) {
		t.Fatalf("serve error = %v", err)
	}
	if len(attempted) != 2 || attempted[0] != 5432 || attempted[1] != 5433 {
		t.Errorf("registration attempts = %v", attempted)
	}
	if started.Load() {
		t.Error("backend started despite incomplete registration")
	}
	if !listener.closed.Load() {
		t.Error("previously registered listener was not closed")
	}
}

type testBackend struct {
	port  uint16
	serve func(context.Context, net.Listener) error
}

func (b testBackend) ListenPort() uint16 { return b.port }
func (b testBackend) Serve(ctx context.Context, listener net.Listener, _ *tailnet.Network, _ *slog.Logger) error {
	return b.serve(ctx, listener)
}

type testListener struct {
	net.Listener
	closed atomic.Bool
}

func (l *testListener) Close() error {
	l.closed.Store(true)
	return nil
}
