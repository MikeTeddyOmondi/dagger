package llmconfig

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeOAuthServer stands in for a provider's token endpoint. It rotates the
// refresh token on every successful grant and rejects a replayed one with
// invalid_grant, mirroring the single-use refresh tokens Anthropic and OpenAI
// issue: replaying a spent token is precisely the permanent logout the refresh
// path has to avoid.
type fakeOAuthServer struct {
	mu sync.Mutex
	// refreshToken is the only refresh token the endpoint accepts.
	refreshToken string
	// grants counts successful refresh grants.
	grants int
	// expiresIn is the expires_in returned with each grant; negative omits the
	// field, as endpoints that don't advertise a lifetime do.
	expiresIn int
	// omitRefreshToken drops refresh_token from the response, which RFC 6749
	// §5.1 permits for endpoints that don't rotate.
	omitRefreshToken bool
	// fail rejects every grant, standing in for a revoked or already-spent
	// token.
	fail bool

	url string
}

func newFakeOAuthServer(t *testing.T, refreshToken string) *fakeOAuthServer {
	t.Helper()
	s := &fakeOAuthServer{refreshToken: refreshToken, expiresIn: 3600}
	srv := httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(srv.Close)
	s.url = srv.URL
	return s
}

// install points the package's token and profile endpoints at this server for
// the duration of the test.
func (s *fakeOAuthServer) install(t *testing.T) {
	t.Helper()
	origToken, origProfile, origOpenAI := oauthTokenURL, oauthProfileURL, openaiTokenURL
	t.Cleanup(func() {
		oauthTokenURL, oauthProfileURL, openaiTokenURL = origToken, origProfile, origOpenAI
	})
	oauthTokenURL = s.url + "/token"
	openaiTokenURL = s.url + "/token"
	oauthProfileURL = s.url + "/profile"
}

func (s *fakeOAuthServer) state() (grants int, refreshToken string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.grants, s.refreshToken
}

func (s *fakeOAuthServer) serve(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/profile":
		// Best-effort subscription lookup; serve a minimal valid response so
		// it never reaches the real network.
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"organization":{"organization_type":"claude_max"}}`)
		return
	case "/token":
	default:
		http.NotFound(w, r)
		return
	}

	// Anthropic posts JSON, OpenAI posts a form; accept both.
	var grantType, refreshToken string
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		var req struct {
			GrantType    string `json:"grant_type"`
			RefreshToken string `json:"refresh_token"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, `{"error":"invalid_request"}`, http.StatusBadRequest)
			return
		}
		grantType, refreshToken = req.GrantType, req.RefreshToken
	} else {
		if err := r.ParseForm(); err != nil {
			http.Error(w, `{"error":"invalid_request"}`, http.StatusBadRequest)
			return
		}
		grantType, refreshToken = r.PostFormValue("grant_type"), r.PostFormValue("refresh_token")
	}
	if grantType != "refresh_token" {
		http.Error(w, `{"error":"unsupported_grant_type"}`, http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail || refreshToken != s.refreshToken {
		http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
		return
	}
	s.grants++
	resp := map[string]any{"access_token": fmt.Sprintf("access-%d", s.grants)}
	if !s.omitRefreshToken {
		s.refreshToken = fmt.Sprintf("rt-%d", s.grants)
		resp["refresh_token"] = s.refreshToken
	}
	if s.expiresIn >= 0 {
		resp["expires_in"] = s.expiresIn
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// useTempConfig points ConfigRoot/ConfigFile at a fresh temp directory and
// seeds the config file with cfg.
func useTempConfig(t *testing.T, cfg *Config) {
	t.Helper()
	origConfigRoot, origConfigFile := ConfigRoot, ConfigFile
	t.Cleanup(func() {
		ConfigRoot, ConfigFile = origConfigRoot, origConfigFile
	})
	ConfigRoot = filepath.Join(t.TempDir(), "dagger")
	ConfigFile = filepath.Join(ConfigRoot, ConfigFileName)
	if cfg != nil {
		if err := cfg.Save(); err != nil {
			t.Fatalf("seeding config failed: %v", err)
		}
	}
}

// expiredOAuthProvider is an OAuth provider whose access token expired long
// ago, so any refresh path will act on it.
func expiredOAuthProvider(refreshToken string) Provider {
	return Provider{
		AuthType:     "oauth",
		AuthToken:    "stale-access",
		RefreshToken: refreshToken,
		TokenExpiry:  1,
		Enabled:      true,
	}
}

// TestRefreshKeepsRefreshTokenWhenOmitted covers the RFC 6749 §5.1 case: the
// refresh response may omit refresh_token when the endpoint doesn't rotate.
// Copying the empty field over the stored one erases the only credential that
// can mint access tokens, so the next refresh reports "no refresh token
// available" and the user is logged out for good.
func TestRefreshKeepsRefreshTokenWhenOmitted(t *testing.T) {
	// Both flows have their own response handling, and both got this wrong.
	for _, provider := range []string{"anthropic", "openai-codex"} {
		t.Run(provider, func(t *testing.T) {
			srv := newFakeOAuthServer(t, "rt-keep")
			srv.omitRefreshToken = true
			srv.install(t)

			useTempConfig(t, &Config{
				LLM: LLMConfig{
					DefaultProvider: provider,
					Providers: map[string]Provider{
						provider: expiredOAuthProvider("rt-keep"),
					},
				},
			})

			token, err := RefreshOAuthProviderIfNeeded(provider)
			if err != nil {
				t.Fatalf("RefreshOAuthProviderIfNeeded() failed: %v", err)
			}
			if token != "access-1" {
				t.Errorf("returned token = %q, want %q", token, "access-1")
			}

			loaded, err := Load()
			if err != nil {
				t.Fatalf("Load() failed: %v", err)
			}
			if got := loaded.LLM.Providers[provider].RefreshToken; got != "rt-keep" {
				t.Errorf("persisted RefreshToken = %q, want the stored %q kept", got, "rt-keep")
			}
		})
	}
}

// Environment used to drive the re-executed test binary in
// TestRefreshOAuthProviderCrossProcess.
const (
	crossProcessEnv        = "DAGGER_TEST_OAUTH_REFRESH_CHILD"
	crossProcessTokenURL   = "DAGGER_TEST_OAUTH_TOKEN_URL"
	crossProcessProfileURL = "DAGGER_TEST_OAUTH_PROFILE_URL"
	crossProcessStartAt    = "DAGGER_TEST_OAUTH_START_AT"
)

// TestRefreshOAuthProviderCrossProcess runs several real dagger-sized
// processes at the refresh at once — a `dagger call` next to a live `dagger
// agent` is enough to hit this. The in-process mutex says nothing about them,
// so only the file lock plus a re-read under it keeps one process from
// spending an already-rotated refresh token and writing its stale snapshot
// back over the winner's.
func TestRefreshOAuthProviderCrossProcess(t *testing.T) {
	srv := newFakeOAuthServer(t, "rt-0")
	srv.install(t)

	configFile := filepath.Join(t.TempDir(), "nested", ConfigFileName)
	origConfigFile := ConfigFile
	t.Cleanup(func() { ConfigFile = origConfigFile })
	ConfigFile = configFile

	cfg := &Config{
		LLM: LLMConfig{
			DefaultProvider: "anthropic",
			Providers: map[string]Provider{
				"anthropic": expiredOAuthProvider("rt-0"),
			},
		},
	}
	if err := cfg.Save(); err != nil {
		t.Fatalf("Save() failed: %v", err)
	}

	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable() failed: %v", err)
	}

	// Children start together rather than as they happen to be scheduled, so
	// they actually race for the lock.
	startAt := time.Now().Add(750 * time.Millisecond)

	const children = 4
	type childResult struct {
		out []byte
		err error
	}
	results := make(chan childResult, children)
	for range children {
		go func() {
			cmd := exec.Command(self, "-test.run=^TestRefreshOAuthProviderChild$", "-test.v")
			cmd.Env = append(os.Environ(),
				crossProcessEnv+"=1",
				"DAGGER_CONFIG="+configFile,
				crossProcessTokenURL+"="+srv.url+"/token",
				crossProcessProfileURL+"="+srv.url+"/profile",
				crossProcessStartAt+"="+strconv.FormatInt(startAt.UnixNano(), 10),
			)
			out, err := cmd.CombinedOutput()
			results <- childResult{out, err}
		}()
	}
	for range children {
		res := <-results
		if res.err != nil {
			t.Errorf("child refresh failed: %v\n%s", res.err, res.out)
		}
	}

	grants, serverRefreshToken := srv.state()
	if grants != 1 {
		t.Errorf("token endpoint granted %d refreshes, want exactly 1", grants)
	}

	loaded, err := Load()
	if err != nil {
		t.Fatalf("Load() after cross-process refresh failed: %v", err)
	}
	provider := loaded.LLM.Providers["anthropic"]
	if provider.RefreshToken != serverRefreshToken {
		t.Errorf("persisted RefreshToken = %q, want the endpoint's current %q", provider.RefreshToken, serverRefreshToken)
	}
	if provider.AuthToken != "access-1" {
		t.Errorf("persisted AuthToken = %q, want %q", provider.AuthToken, "access-1")
	}
}

// TestRefreshOAuthProviderChild is the child half of
// TestRefreshOAuthProviderCrossProcess: it refreshes the provider once and
// fails if that refresh does not come back with a usable token.
func TestRefreshOAuthProviderChild(t *testing.T) {
	if os.Getenv(crossProcessEnv) == "" {
		t.Skip("child process of TestRefreshOAuthProviderCrossProcess")
	}
	oauthTokenURL = os.Getenv(crossProcessTokenURL)
	oauthProfileURL = os.Getenv(crossProcessProfileURL)
	if raw := os.Getenv(crossProcessStartAt); raw != "" {
		nanos, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			t.Fatalf("bad %s: %v", crossProcessStartAt, err)
		}
		time.Sleep(time.Until(time.Unix(0, nanos)))
	}

	token, err := RefreshOAuthProviderIfNeeded("anthropic")
	if err != nil {
		t.Fatalf("RefreshOAuthProviderIfNeeded() failed: %v", err)
	}
	if token == "" {
		t.Fatal("RefreshOAuthProviderIfNeeded() returned no token")
	}
}
