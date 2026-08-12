package core

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// codexTestToken builds a minimal JWT-shaped token carrying the given
// chatgpt_account_id claim, matching what extractChatGPTAccountID parses.
func codexTestToken(t *testing.T, accountID string) string {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": accountID,
		},
	})
	require.NoError(t, err)
	return "hdr." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

// TestCredentialTransportRefreshesPerRequest is the point of the prototype:
// a provider SDK client bakes its credential in at construction, so the only
// way a rotated token reaches the wire without rebuilding the client is for
// the transport to re-authenticate each request.
func TestCredentialTransportRefreshesPerRequest(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	var n atomic.Int64
	endpoint := &LLMEndpoint{
		Provider: Anthropic,
		IsOAuth:  true,
		// The stale snapshot the SDK would have pinned.
		AuthToken: "token-0",
		AuthTokenSource: func(context.Context) (string, error) {
			return "token-" + string(rune('0'+n.Add(1))), nil
		},
	}
	client := endpoint.otelHTTPClient("anthropic")

	for range 3 {
		req, err := http.NewRequest(http.MethodGet, srv.URL, nil)
		require.NoError(t, err)
		// What the SDK baked in at construction time.
		req.Header.Set("Authorization", "Bearer token-0")
		resp, err := client.Do(req)
		require.NoError(t, err)
		resp.Body.Close()
	}

	require.Equal(t, []string{
		"Bearer token-1",
		"Bearer token-2",
		"Bearer token-3",
	}, seen)
}

// TestCredentialTransportLeavesRequestUnmodified locks in the RoundTripper
// contract: the request handed to RoundTrip must not be mutated (the SDK may
// reuse it across its own retries).
func TestCredentialTransportLeavesRequestUnmodified(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	rt := newCredentialTransport(nil, func(context.Context) (string, error) {
		return "fresh", nil
	}, applyBearer)

	req, err := http.NewRequest(http.MethodGet, srv.URL, nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer stale")
	resp, err := rt.RoundTrip(req)
	require.NoError(t, err)
	resp.Body.Close()

	require.Equal(t, "Bearer stale", req.Header.Get("Authorization"))
}

// TestCredentialTransportNoSourceIsPassthrough covers the plain-API-key path
// (`dagger call` in CI): no source, no interception, no behavior change.
func TestCredentialTransportNoSourceIsPassthrough(t *testing.T) {
	base := http.DefaultTransport
	require.Equal(t, base, newCredentialTransport(base, nil, applyBearer))
	require.Nil(t, newCredentialTransport(nil, nil, applyBearer))
	require.Nil(t, staticCredential(""))
	require.Nil(t, cachedCredential(time.Minute, nil))
}

// TestCachedCredentialCollapsesBursts checks the TTL: a streaming turn that
// issues several requests back-to-back resolves the credential once, while a
// long session still re-resolves after the TTL.
func TestCachedCredentialCollapsesBursts(t *testing.T) {
	var calls atomic.Int64
	src := cachedCredential(time.Hour, func(context.Context) (string, error) {
		calls.Add(1)
		return "tok", nil
	})
	for range 5 {
		v, err := src(context.Background())
		require.NoError(t, err)
		require.Equal(t, "tok", v)
	}
	require.Equal(t, int64(1), calls.Load())

	// Zero TTL means every request re-resolves.
	calls.Store(0)
	src = cachedCredential(0, func(context.Context) (string, error) {
		calls.Add(1)
		return "tok", nil
	})
	for range 5 {
		_, err := src(context.Background())
		require.NoError(t, err)
	}
	require.Equal(t, int64(5), calls.Load())
}

// TestCachedCredentialFallsBackToStale: a transient failure to reach the
// client (nested session mid-reconnect) must not fail the LLM request when a
// previously resolved token is still in hand.
func TestCachedCredentialFallsBackToStale(t *testing.T) {
	var fail atomic.Bool
	src := cachedCredential(0, func(context.Context) (string, error) {
		if fail.Load() {
			return "", context.DeadlineExceeded
		}
		return "tok", nil
	})
	v, err := src(context.Background())
	require.NoError(t, err)
	require.Equal(t, "tok", v)

	fail.Store(true)
	v, err = src(context.Background())
	require.NoError(t, err)
	require.Equal(t, "tok", v)
}

// TestCredentialApplierCodex checks that the Codex account-id header, which
// is derived from the token's JWT claims, follows the token when it rotates
// instead of staying pinned from client construction.
func TestCredentialApplierCodex(t *testing.T) {
	endpoint := &LLMEndpoint{Provider: OpenAICodex}
	apply := endpoint.credentialApplier()

	req, err := http.NewRequest(http.MethodGet, "https://chatgpt.com/backend-api/codex/responses", nil)
	require.NoError(t, err)
	apply(req, codexTestToken(t, "acct-abc"))
	require.Equal(t, "acct-abc", req.Header.Get("chatgpt-account-id"))

	apply(req, codexTestToken(t, "acct-xyz"))
	require.Equal(t, "acct-xyz", req.Header.Get("chatgpt-account-id"))
}
