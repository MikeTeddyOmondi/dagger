package llmconfig

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

const (
	// Claude Code OAuth configuration
	oauthClientID    = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	oauthAuthorize   = "https://claude.ai/oauth/authorize"
	oauthRedirectURI = "https://console.anthropic.com/oauth/code/callback"
	oauthScopes      = "org:create_api_key user:profile user:inference"
)

// vars (not consts) so tests can point them at a local server, mirroring the
// ConfigRoot/ConfigFile override pattern.
var (
	oauthTokenURL   = "https://console.anthropic.com/v1/oauth/token" //nolint:gosec // OAuth token endpoint URL, not a credential
	oauthProfileURL = "https://api.anthropic.com/api/oauth/profile"
)

// OAuthTokenResponse represents the token endpoint response.
type OAuthTokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
}

// oauthHTTPTimeout bounds every call to a provider's OAuth endpoints. Refresh
// runs inside the client's GetSecret handler while the engine blocks on that
// RPC, so an unresponsive token endpoint would otherwise wedge the whole
// session with nothing to cancel.
const oauthHTTPTimeout = 10 * time.Second

var oauthHTTPClient = &http.Client{Timeout: oauthHTTPTimeout}

// postOAuthToken posts body to an OAuth token endpoint and decodes the JSON
// response into out. what names the operation in error messages, e.g. "token
// refresh". The request is bound to ctx and to oauthHTTPTimeout, whichever
// comes first.
func postOAuthToken(ctx context.Context, url, what, contentType string, body io.Reader, out any) error {
	ctx, cancel := context.WithTimeout(ctx, oauthHTTPTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", contentType)

	resp, err := oauthHTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("%s request failed: %w", what, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s failed (HTTP %d): %s", what, resp.StatusCode, string(respBody))
	}

	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("failed to decode %s response: %w", what, err)
	}
	return nil
}

// GenerateOAuthURL generates a PKCE-protected OAuth authorization URL.
// Returns the URL the user should visit and the PKCE verifier for later use.
func GenerateOAuthURL() (authURL, verifier string, err error) {
	verifier, challenge, err := generatePKCE()
	if err != nil {
		return "", "", fmt.Errorf("generate PKCE: %w", err)
	}

	params := url.Values{
		"code":                  {"true"},
		"client_id":             {oauthClientID},
		"response_type":         {"code"},
		"redirect_uri":          {oauthRedirectURI},
		"scope":                 {oauthScopes},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"state":                 {verifier},
	}

	return oauthAuthorize + "?" + params.Encode(), verifier, nil
}

// ExchangeOAuthCode exchanges an authorization code for tokens.
// The authCode should be in the format "code#state" as provided by the callback.
func ExchangeOAuthCode(ctx context.Context, authCode, verifier string) (*Provider, error) {
	// The auth code may include a state suffix separated by #
	code := authCode
	state := ""
	for i := len(authCode) - 1; i >= 0; i-- {
		if authCode[i] == '#' {
			code = authCode[:i]
			state = authCode[i+1:]
			break
		}
	}

	body, err := json.Marshal(map[string]string{
		"grant_type":    "authorization_code",
		"client_id":     oauthClientID,
		"code":          code,
		"state":         state,
		"redirect_uri":  oauthRedirectURI,
		"code_verifier": verifier,
	})
	if err != nil {
		return nil, err
	}

	var tokenResp OAuthTokenResponse
	if err := postOAuthToken(ctx, oauthTokenURL, "token exchange", "application/json", bytes.NewReader(body), &tokenResp); err != nil {
		return nil, err
	}

	// Subtract 5 minutes from expiry for safety margin
	expiryMs := time.Now().UnixMilli() + int64(tokenResp.ExpiresIn)*1000 - 5*60*1000

	provider := &Provider{
		AuthType:     "oauth",
		AuthToken:    tokenResp.AccessToken,
		RefreshToken: tokenResp.RefreshToken,
		TokenExpiry:  expiryMs,
		Enabled:      true,
	}

	// Fetch subscription type from profile (best-effort)
	if subType, err := FetchSubscriptionType(ctx, tokenResp.AccessToken); err == nil {
		provider.SubscriptionType = subType
	}

	return provider, nil
}

// RefreshOAuthToken refreshes an expired OAuth token.
func RefreshOAuthToken(ctx context.Context, provider *Provider) (*Provider, error) {
	if provider.RefreshToken == "" {
		return nil, fmt.Errorf("no refresh token available")
	}

	body, err := json.Marshal(map[string]string{
		"grant_type":    "refresh_token",
		"client_id":     oauthClientID,
		"refresh_token": provider.RefreshToken,
	})
	if err != nil {
		return nil, err
	}

	var tokenResp OAuthTokenResponse
	if err := postOAuthToken(ctx, oauthTokenURL, "token refresh", "application/json", bytes.NewReader(body), &tokenResp); err != nil {
		return nil, err
	}

	expiryMs := time.Now().UnixMilli() + int64(tokenResp.ExpiresIn)*1000 - 5*60*1000

	updated := *provider
	updated.AuthToken = tokenResp.AccessToken
	// RFC 6749 §5.1 makes refresh_token optional in a refresh response;
	// endpoints that don't rotate simply omit it. Overwriting unconditionally
	// would erase the only credential that can mint new access tokens, leaving
	// "no refresh token available" forever.
	if tokenResp.RefreshToken != "" {
		updated.RefreshToken = tokenResp.RefreshToken
	}
	updated.TokenExpiry = expiryMs

	// Refresh subscription type (best-effort)
	if subType, err := FetchSubscriptionType(ctx, tokenResp.AccessToken); err == nil {
		updated.SubscriptionType = subType
	}

	return &updated, nil
}

// IsTokenExpired checks if the OAuth token has expired.
func IsTokenExpired(provider *Provider) bool {
	if provider.TokenExpiry == 0 {
		return true
	}
	return time.Now().UnixMilli() >= provider.TokenExpiry
}

// FetchSubscriptionType queries the Anthropic OAuth profile endpoint to
// determine the user's subscription type (pro, max, team, enterprise).
func FetchSubscriptionType(ctx context.Context, accessToken string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, oauthHTTPTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, oauthProfileURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := oauthHTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("profile request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("profile request returned HTTP %d", resp.StatusCode)
	}

	var profile struct {
		Organization struct {
			OrganizationType string `json:"organization_type"`
		} `json:"organization"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&profile); err != nil {
		return "", fmt.Errorf("failed to decode profile: %w", err)
	}

	// Map organization_type to friendly subscription name
	switch profile.Organization.OrganizationType {
	case "claude_pro":
		return "pro", nil
	case "claude_max":
		return "max", nil
	case "claude_team":
		return "team", nil
	case "claude_enterprise":
		return "enterprise", nil
	default:
		return "", nil
	}
}

// SubscriptionLabel returns a human-readable label for a subscription type.
func SubscriptionLabel(subType string) string {
	switch subType {
	case "pro":
		return "Claude Pro"
	case "max":
		return "Claude Max"
	case "team":
		return "Claude Team"
	case "enterprise":
		return "Claude Enterprise"
	case "chatgpt":
		return "ChatGPT"
	default:
		return ""
	}
}

// generatePKCE generates a PKCE verifier and challenge pair.
func generatePKCE() (verifier, challenge string, err error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", "", err
	}
	verifier = base64.RawURLEncoding.EncodeToString(buf)

	hash := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(hash[:])

	return verifier, challenge, nil
}
