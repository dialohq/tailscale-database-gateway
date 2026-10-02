package proxy

import (
	"context"
	"io"
	"net"
)

// Proxy copies data in both directions until a connection closes or the context is canceled.
func Proxy(ctx context.Context, first, second net.Conn) {
	finished := make(chan struct{}, 2)
	stop := context.AfterFunc(ctx, func() {
		_ = first.Close()
		_ = second.Close()
	})
	defer stop()
	copyConnection := func(destination, source net.Conn) {
		_, _ = io.Copy(destination, source)
		finished <- struct{}{}
	}
	go copyConnection(second, first)
	go copyConnection(first, second)
	<-finished
	_ = first.Close()
	_ = second.Close()
	<-finished
}
