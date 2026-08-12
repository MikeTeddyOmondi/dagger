package core

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dagger/dagger/dagql"
	"github.com/dagger/dagger/engine"
)

// llmEndpointTestServer is the minimal Query.Server an LLM.Endpoint()
// resolution needs: a dagql server to run the `secret(uri:){plaintext}`
// lookups against, and one client identity to load config from (so
// loadLLMRouter's main-client/parent-client layering collapses to a single
// load).
type llmEndpointTestServer struct {
	*mockServer
	srv *dagql.Server
	md  *engine.ClientMetadata
}

func (s *llmEndpointTestServer) Server(context.Context) (*dagql.Server, error) {
	return s.srv, nil
}

func (s *llmEndpointTestServer) MainClientCallerMetadata(context.Context) (*engine.ClientMetadata, error) {
	return s.md, nil
}

func (s *llmEndpointTestServer) NonModuleParentClientMetadata(context.Context) (*engine.ClientMetadata, error) {
	return s.md, nil
}

// llmEnv is a mutable stand-in for the client's environment, so a test can
// rotate ANTHROPIC_AUTH_TOKEN mid-"session" the way the CLI's env refresher
// does when the OAuth access token expires.
type llmEnv struct {
	mu   sync.Mutex
	vars map[string]string
	// reads counts env:// lookups, i.e. how often the engine actually went
	// back to the client for a value.
	reads map[string]int
}

func (e *llmEnv) get(uri string) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.reads[uri]++
	return e.vars[uri]
}

func (e *llmEnv) set(uri, val string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.vars[uri] = val
}

func (e *llmEnv) readCount(uri string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.reads[uri]
}

// newLLMEndpointTestCtx wires a context in which (*LLM).Endpoint resolves for
// real: it routes through loadLLMRouter -> LoadClientConfig -> the mocked
// `secret(uri:){plaintext}` selection, reading from the returned llmEnv.
func newLLMEndpointTestCtx(t *testing.T) (context.Context, *llmEnv) {
	t.Helper()

	env := &llmEnv{
		vars: map[string]string{
			"file://.env":                "",
			"env://ANTHROPIC_AUTH_TOKEN": "token-v1",
			"env://ANTHROPIC_MODEL":      "claude-sonnet-4-5",
		},
		reads: map[string]int{},
	}

	// Mirror the real secret schema's cache semantics (core/schema/secret.go):
	// `secret(uri:)` is PerCallInput and `plaintext` is DoNotCache, so every
	// resolution really does go back to the client — which is what the CLI's
	// env refresher hook relies on.
	srv := newCoreDagqlServerForTest(t, LLMTestQuery{})
	dagql.Fields[LLMTestQuery]{
		dagql.Func("secret", func(_ context.Context, _ LLMTestQuery, args struct {
			URI string
		}) (mockSecret, error) {
			return mockSecret{uri: args.URI}, nil
		}).WithInput(dagql.PerCallInput),
	}.Install(srv)
	dagql.Fields[mockSecret]{
		dagql.Func("plaintext", func(_ context.Context, self mockSecret, _ struct{}) (string, error) {
			return env.get(self.uri), nil
		}).DoNotCache("plaintext is read fresh from the client"),
	}.Install(srv)

	cache, err := dagql.NewCache(t.Context(), "", nil, nil)
	require.NoError(t, err)

	md := &engine.ClientMetadata{ClientID: "llm-test-client", SessionID: "llm-test-session"}
	query := &Query{Server: &llmEndpointTestServer{
		mockServer: &mockServer{clientMetadata: md},
		srv:        srv,
		md:         md,
	}}

	ctx := dagql.ContextWithCache(engine.ContextWithClientMetadata(t.Context(), md), cache)
	return ContextWithQuery(ctx, query), env
}

// TestLLMEndpointMemoizationPinsAuthToken demonstrates the staleness bug:
// (*LLM).Endpoint memoizes the resolved endpoint (core/llm.go), and the
// subscription OAuth bearer token is a field of that endpoint — baked into the
// provider SDK client at construction (newAnthropicClient, core/llm_anthropic.go).
// So the token a conversation authenticates with is fixed the first time
// anything resolves its endpoint, for the rest of that LLM value chain's life,
// no matter how many times the client refreshes the credential afterwards.
func TestLLMEndpointMemoizationPinsAuthToken(t *testing.T) {
	ctx, env := newLLMEndpointTestCtx(t)
	query, err := CurrentQuery(ctx)
	require.NoError(t, err)

	llm, err := query.NewLLM(ctx, "claude-sonnet-4-5", "")
	require.NoError(t, err)

	ep1, err := llm.Endpoint(ctx)
	require.NoError(t, err)
	require.Equal(t, Anthropic, ep1.Provider)
	require.True(t, ep1.IsOAuth)
	assert.Equal(t, "token-v1", ep1.AuthToken)
	reads := env.readCount("env://ANTHROPIC_AUTH_TOKEN")
	assert.Positive(t, reads, "first resolution must read the token from the client")

	// The client refreshes the expired access token (secretprovider's
	// EnvRefresher hook + os.Setenv, engine/client/secretprovider/env.go).
	env.set("env://ANTHROPIC_AUTH_TOKEN", "token-v2")

	// ...but the memoized endpoint never asks again.
	ep2, err := llm.Endpoint(ctx)
	require.NoError(t, err)
	assert.Same(t, ep1, ep2, "endpoint is memoized, not re-derived")
	assert.Equal(t, "token-v1", ep2.AuthToken, "stale token survives the refresh")
	assert.Equal(t, reads, env.readCount("env://ANTHROPIC_AUTH_TOKEN"),
		"no further client round-trip, so the refresher never runs")

	// Clone() copies the endpoint pointer (core/llm.go), and every LLM
	// transition — withPrompt, withResponse, withToolResult, step, loop,
	// fork, the agent runtime's drainMailbox — goes through Clone. So the
	// whole rest of the conversation inherits the pinned token.
	t.Run("clone carries the pinned endpoint", func(t *testing.T) {
		next := llm.WithPrompt("hello").
			WithResponse([]*LLMContentBlock{{Kind: LLMContentText, Text: "hi"}}, LLMTokenUsage{}).
			WithToolResult("call_1", "ok", false).
			Clone()
		epNext, err := next.Endpoint(ctx)
		require.NoError(t, err)
		assert.Same(t, ep1, epNext)
		assert.Equal(t, "token-v1", epNext.AuthToken)
	})

	// Two "workers" forked off the same conversation share ONE endpoint
	// value, so a refresh observed by neither reaches both: parallel agents
	// derived from a chain whose endpoint is already resolved all run on the
	// same pinned token.
	t.Run("parallel forks share the pinned endpoint", func(t *testing.T) {
		w1, w2 := llm.Clone(), llm.Clone()
		ep1a, err := w1.Endpoint(ctx)
		require.NoError(t, err)
		ep2a, err := w2.Endpoint(ctx)
		require.NoError(t, err)
		assert.Same(t, ep1a, ep2a)
		assert.Equal(t, "token-v1", ep1a.AuthToken)
	})

	// Only a fresh LLM (a new llm() call) or an explicit endpoint reset
	// observes the refreshed credential.
	t.Run("fresh LLM sees the refreshed token", func(t *testing.T) {
		fresh, err := query.NewLLM(ctx, "claude-sonnet-4-5", "")
		require.NoError(t, err)
		ep, err := fresh.Endpoint(ctx)
		require.NoError(t, err)
		assert.Equal(t, "token-v2", ep.AuthToken)
	})

	t.Run("withModel/withReasoningEffort reset the endpoint", func(t *testing.T) {
		remodeled := llm.WithModel("claude-sonnet-4-5", "")
		ep, err := remodeled.Endpoint(ctx)
		require.NoError(t, err)
		assert.Equal(t, "token-v2", ep.AuthToken)

		reasoned := llm.WithReasoningEffort("low")
		ep, err = reasoned.Endpoint(ctx)
		require.NoError(t, err)
		assert.Equal(t, "token-v2", ep.AuthToken)
	})
}

// TestLLMEndpointResolutionOnCloneDoesNotPinReceiver pins down the asymmetry
// that decides whether a long session self-heals.
//
// (*LLM).step clones the receiver before resolving the endpoint (core/llm.go),
// so a step's own resolution is memoized on a throwaway value: an LLM that has
// never had its endpoint resolved "persistently" re-derives the router — and
// therefore re-reads env://ANTHROPIC_AUTH_TOKEN through the client, giving the
// CLI's refresher hook its chance — on EVERY step.
//
// Any resolution on the persistent value itself defeats that permanently:
// LLM.model / LLM.provider / LLM.contextWindow / LLM.reasoningEffort
// (core/schema/llm.go) call Endpoint(ctx) straight on the receiver, and Clone
// then carries the memo into every descendant of the conversation.
func TestLLMEndpointResolutionOnCloneDoesNotPinReceiver(t *testing.T) {
	ctx, env := newLLMEndpointTestCtx(t)
	query, err := CurrentQuery(ctx)
	require.NoError(t, err)

	llm, err := query.NewLLM(ctx, "claude-sonnet-4-5", "")
	require.NoError(t, err)

	// The shape of step()/sendQueryWithRetry: resolve on a clone.
	ep, err := llm.Clone().Endpoint(ctx)
	require.NoError(t, err)
	assert.Equal(t, "token-v1", ep.AuthToken)

	env.set("env://ANTHROPIC_AUTH_TOKEN", "token-v2")

	// The next "step" re-derives, so the refreshed token is picked up.
	ep, err = llm.Clone().Endpoint(ctx)
	require.NoError(t, err)
	assert.Equal(t, "token-v2", ep.AuthToken,
		"a step that resolves on a clone must observe a refreshed credential")

	// One resolution on the persistent value ends that: LLM.model and friends
	// do exactly this, and every later clone inherits the memo.
	pinned, err := llm.Endpoint(ctx)
	require.NoError(t, err)
	assert.Equal(t, "token-v2", pinned.AuthToken)

	env.set("env://ANTHROPIC_AUTH_TOKEN", "token-v3")

	ep, err = llm.Clone().Endpoint(ctx)
	require.NoError(t, err)
	assert.Same(t, pinned, ep)
	assert.Equal(t, "token-v2", ep.AuthToken,
		"once the persistent value memoized, every step reuses the pinned token")
}

// TestAnthropicClientBakesAuthToken shows the second half of the pin: even if
// something did refresh LLMEndpoint.AuthToken in place, the request would still
// carry the old bearer token — newAnthropicClient captures it in the SDK
// client's request options at construction time (core/llm_anthropic.go).
// It also covers 401 handling: an expired-token 401 is not retryable, so
// sendQueryWithRetry gives up immediately (which is just as well, since the
// retry would reuse the same baked-in token).
func TestAnthropicClientBakesAuthToken(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		//nolint:errcheck
		w.Write([]byte(`{"type":"error","error":{"type":"authentication_error","message":"OAuth token has expired"}}`))
	}))
	defer ts.Close()

	endpoint := &LLMEndpoint{
		Model:     "claude-sonnet-4-5",
		BaseURL:   ts.URL,
		Provider:  Anthropic,
		AuthToken: "token-v1",
		IsOAuth:   true,
	}
	client := newAnthropicClient(endpoint)

	history := []*LLMMessage{{
		Role:    LLMMessageRoleUser,
		Content: []*LLMContentBlock{{Kind: LLMContentText, Text: "hi"}},
	}}

	_, err := client.SendQuery(t.Context(), history, nil, &LLMCallOpts{})
	require.Error(t, err)

	// An expired-token 401 is not classified as retryable, so
	// sendQueryWithRetry wraps it in backoff.Permanent and the user sees the
	// raw provider error.
	assert.False(t, client.IsRetryable(err),
		"401 authentication_error is not retryable: %v", err)

	// Rotating the token on the endpoint changes nothing: the SDK client
	// already holds the old one.
	endpoint.AuthToken = "token-v2"
	_, err = client.SendQuery(t.Context(), history, nil, &LLMCallOpts{})
	require.Error(t, err)

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, seen, 2)
	assert.Equal(t, "Bearer token-v1", seen[0])
	assert.Equal(t, "Bearer token-v1", seen[1],
		"the bearer token is captured at client construction, not read per request")
}

// TestCodexClientBakesAuthToken is the Codex (ChatGPT subscription) twin of
// TestAnthropicClientBakesAuthToken. Codex is worse off on two counts: the
// token is also used to derive the chatgpt-account-id header, and
// OpenAICodexClient.IsRetryable returns false unconditionally
// (core/llm_openai_codex.go).
func TestCodexClientBakesAuthToken(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		//nolint:errcheck
		w.Write([]byte(`{"detail":"Your authentication token has expired."}`))
	}))
	defer ts.Close()

	endpoint := &LLMEndpoint{
		Model:     "gpt-5.5",
		BaseURL:   ts.URL,
		Provider:  OpenAICodex,
		AuthToken: "token-v1",
		IsOAuth:   true,
	}
	client := newOpenAICodexClient(endpoint)

	history := []*LLMMessage{{
		Role:    LLMMessageRoleUser,
		Content: []*LLMContentBlock{{Kind: LLMContentText, Text: "hi"}},
	}}

	_, err := client.SendQuery(t.Context(), history, nil, &LLMCallOpts{})
	require.Error(t, err)
	assert.False(t, client.IsRetryable(err), "Codex never retries anything")

	endpoint.AuthToken = "token-v2"
	_, err = client.SendQuery(t.Context(), history, nil, &LLMCallOpts{})
	require.Error(t, err)

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, seen, 2)
	assert.Equal(t, "Bearer token-v1", seen[0])
	assert.Equal(t, "Bearer token-v1", seen[1])
}
