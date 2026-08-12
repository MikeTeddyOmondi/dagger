package core

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// PROTOTYPE (exploration): per-request credential resolution for LLM
// providers. See the design notes in the accompanying report — this file
// exists to prove out the "MINIMAL" option: an LLMEndpoint carries a
// credential *source* instead of (only) a pinned string, and the endpoint's
// HTTP client re-asks it on every provider request.

// credentialRefreshTTL bounds how often a credential source actually goes
// back to its origin. Resolving an OAuth bearer token round-trips to the
// client's session (env:// -> secret provider -> the CLI's OAuth refresher),
// which is cheap relative to an LLM call but not free, and a streaming turn
// can issue several requests back-to-back. A short TTL keeps a long session
// from ever sending a stale token while collapsing bursts.
const credentialRefreshTTL = 30 * time.Second

// CredentialSource yields the credential to authenticate a provider API
// request with, at the moment the request is made. It is the anti-snapshot:
// a subscription OAuth access token lives ~1h and is rotated behind the
// scenes, so anything that captures the string once (an SDK client built
// with option.WithAuthToken, a memoized *LLMEndpoint) pins a value that will
// expire mid-session.
type CredentialSource func(ctx context.Context) (string, error)

// staticCredential adapts a fixed credential (a plain API key, or a token
// supplied by a caller with no way to refresh it) to a CredentialSource.
func staticCredential(token string) CredentialSource {
	if token == "" {
		return nil
	}
	return func(context.Context) (string, error) { return token, nil }
}

// cachedCredential wraps src with a TTL cache and single-flight, so
// concurrent requests (parallel agents sharing one endpoint, or a retry
// storm) collapse onto one resolution. A nil src yields a nil source, so
// callers can pass an unset slot through unconditionally.
func cachedCredential(ttl time.Duration, src CredentialSource) CredentialSource {
	if src == nil {
		return nil
	}
	var (
		mu       sync.Mutex
		cached   string
		cachedAt time.Time
	)
	return func(ctx context.Context) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		if cached != "" && time.Since(cachedAt) < ttl {
			return cached, nil
		}
		token, err := src(ctx)
		if err != nil {
			if cached != "" {
				// Prefer a stale-but-working token over failing the request:
				// the client may be momentarily unreachable (a nested client
				// whose parent session is mid-reconnect), and the token we
				// have is very likely still valid.
				return cached, nil
			}
			return "", err
		}
		if token != "" {
			cached, cachedAt = token, time.Now()
		}
		return token, nil
	}
}

// applyCredential writes a resolved credential onto an outgoing request.
// Providers differ in more than the header name: Codex also derives a
// required account-id header from the token's JWT claims, so that has to be
// recomputed whenever the token rotates rather than pinned at client
// construction.
type applyCredential func(req *http.Request, token string)

func applyBearer(req *http.Request, token string) {
	req.Header.Set("Authorization", "Bearer "+token)
}

func applyCodexBearer(req *http.Request, token string) {
	applyBearer(req, token)
	if accountID := extractChatGPTAccountID(token); accountID != "" {
		req.Header.Set("chatgpt-account-id", accountID)
	}
}

// credentialTransport re-authenticates every request from a CredentialSource,
// overwriting whatever the SDK baked in at construction. This is the single
// choke point that makes credential freshness a property of the transport
// rather than of each provider client: all four providers are built with
// option.WithHTTPClient(endpoint.otelHTTPClient(...)).
type credentialTransport struct {
	base  http.RoundTripper
	src   CredentialSource
	apply applyCredential
}

func newCredentialTransport(base http.RoundTripper, src CredentialSource, apply applyCredential) http.RoundTripper {
	if src == nil {
		return base
	}
	if base == nil {
		base = http.DefaultTransport
	}
	if apply == nil {
		apply = applyBearer
	}
	return &credentialTransport{base: base, src: src, apply: apply}
}

func (t *credentialTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	token, err := t.src(req.Context())
	if err != nil {
		return nil, fmt.Errorf("resolve LLM credential: %w", err)
	}
	if token != "" {
		// RoundTrippers must not mutate the request they are handed.
		req = req.Clone(req.Context())
		t.apply(req, token)
	}
	return t.base.RoundTrip(req)
}

// credentialApplier returns how this endpoint's provider carries its
// credential on the wire.
func (endpoint *LLMEndpoint) credentialApplier() applyCredential {
	if endpoint.Provider == OpenAICodex {
		return applyCodexBearer
	}
	return applyBearer
}
