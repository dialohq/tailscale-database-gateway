package clickhouse

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"strings"
	"sync"
	"time"

	"github.com/dialohq/tailscale-database-gateway/internal/service"
)

const authenticationFailedCode = "516"

var errHTTPForbidden = errors.New("ClickHouse HTTP request is forbidden")

func (g *Gateway) ServeHTTP(ctx context.Context, listener net.Listener) error {
	proxy := httputil.NewSingleHostReverseProxy(g.httpUpstream)
	proxy.Transport = &credentialTransport{
		base:             http.DefaultTransport,
		authorize:        g.Authorize,
		authorizeTimeout: g.handshakeTimeout,
		retryAttempts:    max(g.retryAttempts, 1),
		retryDelay:       g.retryDelay,
		retryBodyBytes:   g.httpRetryBodyBytes,
		logger:           g.Logger,
	}
	proxy.FlushInterval = -1
	proxy.ErrorHandler = func(response http.ResponseWriter, request *http.Request, err error) {
		if errors.Is(err, errHTTPForbidden) {
			g.Logger.Warn("rejected ClickHouse HTTP request", "remote", request.RemoteAddr, "error", err)
			http.Error(response, "forbidden", http.StatusForbidden)
			return
		}
		g.Logger.Warn("ClickHouse HTTP proxy failed", "remote", request.RemoteAddr, "error", err)
		http.Error(response, "ClickHouse gateway error", http.StatusBadGateway)
	}

	server := &http.Server{
		Handler:           proxy,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       90 * time.Second,
	}
	go func() {
		<-ctx.Done()
		_ = server.Close()
	}()

	if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve ClickHouse HTTP: %w", err)
	}
	return nil
}

type credentialTransport struct {
	base             http.RoundTripper
	authorize        func(context.Context, string, string) (Identity, CredentialsProvider, error)
	authorizeTimeout time.Duration
	retryAttempts    int
	retryDelay       time.Duration
	retryBodyBytes   int64
	logger           *slog.Logger
}

func (t *credentialTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	role, err := requestedHTTPRole(request)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errHTTPForbidden, err)
	}
	authorizeCtx, cancelAuthorize := context.WithTimeout(request.Context(), t.authorizeTimeout)
	identity, provider, err := t.authorize(authorizeCtx, request.RemoteAddr, role)
	cancelAuthorize()
	if err != nil {
		return nil, fmt.Errorf("%w: authorize Tailscale identity: %v", errHTTPForbidden, err)
	}
	stripHTTPClientCredentials(request)
	body, replayable, err := readReplayableBody(request, t.retryBodyBytes)
	if err != nil {
		return nil, err
	}

	for attempt := 1; ; attempt++ {
		credentials, err := provider(request.Context(), identity, true)
		if err != nil {
			return nil, err
		}
		outgoing := request.Clone(request.Context())
		if replayable {
			outgoing.Body = io.NopCloser(bytes.NewReader(body))
			outgoing.ContentLength = int64(len(body))
		} else {
			outgoing.Body = request.Body
		}
		outgoing.SetBasicAuth(credentials.Username, credentials.Password)

		response, err := t.base.RoundTrip(outgoing)
		if err != nil {
			credentials.Finish(t.logger, "ClickHouse HTTP", role, false)
			return nil, err
		}
		authenticationFailed := isHTTPAuthenticationFailure(response)
		if authenticationFailed && attempt < t.retryAttempts && replayable {
			t.logger.Info(
				"upstream rejected ClickHouse HTTP credentials; reloading and retrying",
				"login", identity.LoginName,
				"node", identity.NodeName,
				"role", role,
				"attempt", attempt,
			)
			_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
			_ = response.Body.Close()
			credentials.Finish(t.logger, "ClickHouse HTTP", role, true)
			if !service.Wait(request.Context(), t.retryDelay) {
				return nil, request.Context().Err()
			}
			continue
		}
		if !authenticationFailed {
			t.logger.Info(
				"authorized ClickHouse HTTP request",
				"login", identity.LoginName,
				"node", identity.NodeName,
				"role", role,
				"method", request.Method,
				"path", request.URL.Path,
			)
		}
		release := sync.OnceFunc(func() {
			credentials.Finish(t.logger, "ClickHouse HTTP", role, authenticationFailed)
		})
		response.Body = &credentialResponseBody{
			ReadCloser: response.Body,
			release:    release,
		}
		return response, nil
	}
}

func requestedHTTPRole(request *http.Request) (string, error) {
	var roles []string
	if username, _, ok := request.BasicAuth(); ok {
		roles = append(roles, username)
	}
	roles = append(roles, request.Header.Values("X-ClickHouse-User")...)
	for name, values := range request.URL.Query() {
		if strings.EqualFold(name, "user") {
			roles = append(roles, values...)
		}
	}
	if len(roles) == 0 || roles[0] == "" {
		return "", fmt.Errorf("ClickHouse HTTP request must select a role")
	}
	for _, role := range roles[1:] {
		if role == "" || role != roles[0] {
			return "", fmt.Errorf("ClickHouse HTTP request has conflicting roles")
		}
	}
	return roles[0], nil
}

type credentialResponseBody struct {
	io.ReadCloser
	release func()
}

func (b *credentialResponseBody) Read(buffer []byte) (int, error) {
	read, err := b.ReadCloser.Read(buffer)
	if err == io.EOF {
		b.release()
	}
	return read, err
}

func (b *credentialResponseBody) Close() error {
	err := b.ReadCloser.Close()
	b.release()
	return err
}

func readReplayableBody(request *http.Request, limit int64) ([]byte, bool, error) {
	if request.Body == nil || request.Body == http.NoBody {
		return nil, true, nil
	}
	if request.ContentLength < 0 || request.ContentLength > limit {
		return nil, false, nil
	}
	body, err := io.ReadAll(io.LimitReader(request.Body, limit+1))
	if err != nil {
		return nil, false, fmt.Errorf("buffer ClickHouse HTTP request: %w", err)
	}
	_ = request.Body.Close()
	if int64(len(body)) > limit {
		return nil, false, fmt.Errorf("ClickHouse HTTP request exceeded declared content length")
	}
	return body, true, nil
}

func isHTTPAuthenticationFailure(response *http.Response) bool {
	return response.StatusCode == http.StatusUnauthorized ||
		strings.TrimSpace(response.Header.Get("X-ClickHouse-Exception-Code")) == authenticationFailedCode
}

func stripHTTPClientCredentials(request *http.Request) {
	for _, header := range []string{"Authorization", "Proxy-Authorization", "X-ClickHouse-User", "X-ClickHouse-Key"} {
		request.Header.Del(header)
	}
	request.Host = ""
	request.Trailer = nil
	query := request.URL.Query()
	for key := range query {
		if strings.EqualFold(key, "user") || strings.EqualFold(key, "password") {
			query.Del(key)
		}
	}
	request.URL.RawQuery = query.Encode()
}
