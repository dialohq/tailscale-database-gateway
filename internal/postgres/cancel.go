package postgres

import (
	"context"
	"log/slog"
	"sync"

	"github.com/jackc/pgx/v5/pgproto3"
)

type cancelKey struct {
	processID uint32
	secret    string
	clientIP  string
}

type cancellationRegistry struct {
	mu      sync.Mutex
	targets map[cancelKey]string
}

func (g *Gateway) handleCancel(ctx context.Context, clientIP string, request *pgproto3.CancelRequest, logger *slog.Logger) {
	key := cancelKey{request.ProcessID, string(request.SecretKey), clientIP}
	g.cancellations.mu.Lock()
	address, ok := g.cancellations.targets[key]
	g.cancellations.mu.Unlock()
	if !ok {
		logger.Warn("rejected PostgreSQL cancellation request")
		return
	}
	connection, err := g.CancelDialer.DialContext(ctx, "tcp", address)
	if err != nil {
		logger.Warn("failed to connect for PostgreSQL cancellation request", "error", err)
		return
	}
	defer connection.Close()
	encoded, _ := request.Encode(nil)
	if _, err := connection.Write(encoded); err != nil {
		logger.Warn("failed to forward PostgreSQL cancellation request", "error", err)
	}
}

func (registry *cancellationRegistry) register(upstream *Upstream, clientIP string) bool {
	key := cancelKey{upstream.PID, string(upstream.SecretKey), clientIP}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if registry.targets == nil {
		registry.targets = make(map[cancelKey]string)
	}
	if _, exists := registry.targets[key]; exists {
		return false
	}
	registry.targets[key] = upstream.Conn.RemoteAddr().String()
	return true
}

func (registry *cancellationRegistry) unregister(upstream *Upstream, clientIP string) {
	registry.mu.Lock()
	delete(registry.targets, cancelKey{upstream.PID, string(upstream.SecretKey), clientIP})
	registry.mu.Unlock()
}
