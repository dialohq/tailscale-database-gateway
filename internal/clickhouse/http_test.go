package clickhouse

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestHTTPTransportRewritesCredentialsAndRetriesAuthentication(t *testing.T) {
	var upstreamAttempts atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		attempt := upstreamAttempts.Add(1)
		if request.Header.Get("X-Request-ID") != "request-123" || request.Header.Get("Cookie") != "session=value" || request.Header.Get("Content-Type") != "application/sql" {
			t.Errorf("arbitrary client headers were not preserved: %#v", request.Header)
		}
		if request.Host == "gateway.internal" {
			t.Error("client host reached upstream")
		}
		if request.URL.Query().Has("user") || request.URL.Query().Has("password") {
			t.Error("client URL credentials reached upstream")
		}
		if request.Header.Get("X-ClickHouse-User") != "" || request.Header.Get("X-ClickHouse-Key") != "" || request.Header.Get("Proxy-Authorization") != "" {
			t.Error("client ClickHouse headers reached upstream")
		}
		username, password, ok := request.BasicAuth()
		if !ok || username != "vault-user" {
			t.Errorf("basic auth = %q/%q/%v", username, password, ok)
		}
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Error(err)
		}
		if string(body) != "SELECT currentUser()" {
			t.Errorf("body = %q", body)
		}
		if attempt == 1 {
			if password != "expired-password" {
				t.Errorf("first password = %q", password)
			}
			response.Header().Set("X-ClickHouse-Exception-Code", authenticationFailedCode)
			response.WriteHeader(http.StatusForbidden)
			return
		}
		if password != "fresh-password" {
			t.Errorf("second password = %q", password)
		}
		_, _ = io.WriteString(response, "vault-user\n")
	}))
	defer upstream.Close()

	var credentialLoads atomic.Int32
	var credentialReleases atomic.Int32
	transport := testHTTPTransport()
	provider := credentialsFunc(func(_ context.Context, _ Identity) (Credentials, error) {
		if credentialLoads.Add(1) == 1 {
			return Credentials{Username: "vault-user", Password: "expired-password", Complete: func(bool) error {
				credentialReleases.Add(1)
				return nil
			}}, nil
		}
		return Credentials{Username: "vault-user", Password: "fresh-password", Complete: func(bool) error {
			credentialReleases.Add(1)
			return nil
		}}, nil
	})
	transport.authorize = func(_ context.Context, _ string, role string) (Identity, CredentialsProvider, error) {
		if role != "admin" {
			t.Fatalf("authorized role = %q", role)
		}
		return Identity{LoginName: "alice@example.com"}, provider, nil
	}

	request, err := http.NewRequest(http.MethodPost, upstream.URL+"/?user=admin&password=attacker", strings.NewReader("SELECT currentUser()"))
	if err != nil {
		t.Fatal(err)
	}
	request.SetBasicAuth("admin", "attacker")
	request.Header.Set("X-ClickHouse-User", "admin")
	request.Header.Set("X-ClickHouse-Key", "attacker")
	request.Header.Set("Proxy-Authorization", "Basic attacker")
	request.Header.Set("X-Request-ID", "request-123")
	request.Header.Set("Cookie", "session=value")
	request.Header.Set("Content-Type", "application/sql")
	request.Host = "gateway.internal"
	response, err := transport.RoundTrip(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || string(body) != "vault-user\n" {
		t.Fatalf("response = %d %q", response.StatusCode, body)
	}
	if upstreamAttempts.Load() != 2 || credentialLoads.Load() != 2 {
		t.Fatalf("attempts = %d, loads = %d", upstreamAttempts.Load(), credentialLoads.Load())
	}
	if credentialReleases.Load() != 2 {
		t.Fatalf("credential releases = %d, want 2", credentialReleases.Load())
	}
}

func TestHTTPTransportLoadsReusableCredentials(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		_, _ = response.Write([]byte("ok"))
	}))
	defer upstream.Close()

	transport := testHTTPTransport()
	var reusableLoads atomic.Int32
	provider := CredentialsProvider(func(_ context.Context, _ Identity, reusable bool) (Credentials, error) {
		if !reusable {
			t.Fatal("HTTP loaded connection-scoped credentials")
		}
		reusableLoads.Add(1)
		return Credentials{Username: "vault-user", Password: "vault-password"}, nil
	})
	transport.authorize = func(context.Context, string, string) (Identity, CredentialsProvider, error) {
		return Identity{}, provider, nil
	}
	request := mustRequest(t, upstream.URL+"/?user=readonly")
	response, err := transport.RoundTrip(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if reusableLoads.Load() != 1 {
		t.Fatalf("reusable credential loads = %d, want 1", reusableLoads.Load())
	}
}

func TestHTTPTransportDoesNotRetryStreamingBody(t *testing.T) {
	var attempts atomic.Int32
	var invalidations atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		attempts.Add(1)
		_, _ = io.Copy(io.Discard, request.Body)
		response.Header().Set("X-ClickHouse-Exception-Code", authenticationFailedCode)
		response.WriteHeader(http.StatusUnauthorized)
	}))
	defer upstream.Close()

	transport := testHTTPTransport()
	provider := credentialsFunc(func(context.Context, Identity) (Credentials, error) {
		return Credentials{
			Username: "vault-user",
			Password: "vault-password",
			Complete: func(rejected bool) error {
				if rejected {
					invalidations.Add(1)
				}
				return nil
			},
		}, nil
	})
	transport.authorize = func(context.Context, string, string) (Identity, CredentialsProvider, error) {
		return Identity{}, provider, nil
	}

	request, err := http.NewRequest(http.MethodPost, upstream.URL+"/?user=readonly", io.NopCloser(strings.NewReader("streamed query")))
	if err != nil {
		t.Fatal(err)
	}
	request.ContentLength = -1
	response, err := transport.RoundTrip(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d", response.StatusCode)
	}
	if attempts.Load() != 1 {
		t.Fatalf("attempts = %d, want 1", attempts.Load())
	}
	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if invalidations.Load() != 1 {
		t.Fatalf("credential invalidations = %d, want 1", invalidations.Load())
	}
}

func TestHTTPTransportRejectsUnknownIdentityBeforeUpstream(t *testing.T) {
	var upstreamRequests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		upstreamRequests.Add(1)
	}))
	defer upstream.Close()

	transport := testHTTPTransport()
	transport.authorize = func(context.Context, string, string) (Identity, CredentialsProvider, error) {
		return Identity{}, nil, errors.New("no ClickHouse grant")
	}
	request, err := http.NewRequest(http.MethodGet, upstream.URL+"/?user=readonly", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = transport.RoundTrip(request)
	if !errors.Is(err, errHTTPForbidden) {
		t.Fatalf("error = %v, want forbidden", err)
	}
	if upstreamRequests.Load() != 0 {
		t.Fatal("rejected request reached upstream")
	}
}

func TestHTTPTransportRejectsInvalidRoleBeforeUpstream(t *testing.T) {
	var upstreamRequests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		upstreamRequests.Add(1)
	}))
	defer upstream.Close()

	transport := testHTTPTransport()
	requests := []*http.Request{
		mustRequest(t, upstream.URL+"/"),
		mustRequest(t, upstream.URL+"/?user=admin"),
		mustRequest(t, upstream.URL+"/?user=readonly"),
	}
	requests[2].SetBasicAuth("admin", "ignored")
	for _, request := range requests {
		_, err := transport.RoundTrip(request)
		if !errors.Is(err, errHTTPForbidden) {
			t.Errorf("request %q error = %v, want forbidden", request.URL, err)
		}
	}
	if upstreamRequests.Load() != 0 {
		t.Fatal("rejected role reached upstream")
	}
}

func testHTTPTransport() *credentialTransport {
	return &credentialTransport{
		base: http.DefaultTransport,
		authorize: func(_ context.Context, _ string, role string) (Identity, CredentialsProvider, error) {
			if role != "readonly" {
				return Identity{}, nil, errors.New("role is not granted")
			}
			provider := credentialsFunc(func(context.Context, Identity) (Credentials, error) {
				return Credentials{Username: "vault-user", Password: "vault-password"}, nil
			})
			return Identity{LoginName: "alice@example.com", NodeName: "alice-laptop"}, provider, nil
		},
		authorizeTimeout: time.Second,
		retryAttempts:    2,
		retryBodyBytes:   1 << 20,
		logger:           slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func mustRequest(t *testing.T, target string) *http.Request {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	return request
}
