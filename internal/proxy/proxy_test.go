package proxy_test

import (
	"context"
	"io"
	"net"
	"testing"
	"time"

	"github.com/dialohq/tailscale-database-gateway/internal/proxy"
)

const testTimeout = time.Second

func TestProxyCopiesBidirectionally(t *testing.T) {
	left, right, _, stopped := startProxy(t)

	assertCopied(t, left, right, []byte("left to right"))
	assertCopied(t, right, left, []byte("right to left"))

	_ = left.Close()
	waitForProxy(t, stopped)
}

func TestProxyClosesOtherConnectionWhenOneConnectionCloses(t *testing.T) {
	left, right, _, stopped := startProxy(t)

	_ = left.Close()
	waitForProxy(t, stopped)
	assertEOF(t, right)
}

func TestProxyStopsWhenContextIsCanceled(t *testing.T) {
	left, right, cancel, stopped := startProxy(t)

	cancel()
	waitForProxy(t, stopped)
	assertEOF(t, left)
	assertEOF(t, right)
}

func startProxy(t *testing.T) (net.Conn, net.Conn, context.CancelFunc, <-chan struct{}) {
	t.Helper()

	left, first := net.Pipe()
	second, right := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		proxy.Proxy(ctx, first, second)
		close(stopped)
	}()

	t.Cleanup(func() {
		cancel()
		_ = left.Close()
		_ = right.Close()
		waitForProxy(t, stopped)
	})

	return left, right, cancel, stopped
}

func assertCopied(t *testing.T, source, destination net.Conn, payload []byte) {
	t.Helper()

	writeResult := make(chan error, 1)
	go func() {
		_ = source.SetWriteDeadline(time.Now().Add(testTimeout))
		_, err := source.Write(payload)
		writeResult <- err
	}()

	_ = destination.SetReadDeadline(time.Now().Add(testTimeout))
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(destination, got); err != nil {
		t.Fatalf("read proxied data: %v", err)
	}
	if err := <-writeResult; err != nil {
		t.Fatalf("write source data: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("proxied data = %q, want %q", got, payload)
	}
}

func assertEOF(t *testing.T, connection net.Conn) {
	t.Helper()

	_ = connection.SetReadDeadline(time.Now().Add(testTimeout))
	buffer := make([]byte, 1)
	if _, err := connection.Read(buffer); err != io.EOF {
		t.Fatalf("read error = %v, want EOF", err)
	}
}

func waitForProxy(t *testing.T, stopped <-chan struct{}) {
	t.Helper()

	select {
	case <-stopped:
	case <-time.After(testTimeout):
		t.Fatal("proxy did not stop")
	}
}
