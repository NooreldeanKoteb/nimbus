package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultGitHubScopes covers cloning the state repo and creating it on first run.
var DefaultGitHubScopes = []string{"repo", "read:user"}

// GitHub implements Provider via the OAuth 2.0 device authorization grant
// (RFC 8628). No gh CLI, no personal access token, no browser redirect: the
// user types a short code on github.com and we poll for the result.
type GitHub struct {
	ClientID string
	Scopes   []string

	// BaseURL and APIURL are overridden in tests. Empty means production.
	BaseURL string
	APIURL  string
	HTTP    *http.Client
}

func (g *GitHub) Name() string { return "github" }

func (g *GitHub) base() string {
	if g.BaseURL != "" {
		return strings.TrimSuffix(g.BaseURL, "/")
	}
	return "https://github.com"
}

func (g *GitHub) api() string {
	if g.APIURL != "" {
		return strings.TrimSuffix(g.APIURL, "/")
	}
	return "https://api.github.com"
}

func (g *GitHub) client() *http.Client {
	if g.HTTP != nil {
		return g.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// deviceCodeResponse is the first leg of the device flow.
type deviceCodeResponse struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri"`
	ExpiresIn       int    `json:"expires_in"`
	Interval        int    `json:"interval"`
}

// tokenResponse doubles as both success and error payload; GitHub returns HTTP
// 200 with an "error" field for the pending case, so status alone is not enough.
type tokenResponse struct {
	AccessToken string `json:"access_token"`
	Scope       string `json:"scope"`
	Error       string `json:"error"`
	Description string `json:"error_description"`
	Interval    int    `json:"interval"`
}

// Login runs the device flow to completion, blocking until the user approves,
// the code expires, or ctx is cancelled.
func (g *GitHub) Login(ctx context.Context, prompt PromptFunc) (*Identity, error) {
	if g.ClientID == "" {
		return nil, fmt.Errorf("github: no client id configured")
	}

	dev, err := g.requestDeviceCode(ctx)
	if err != nil {
		return nil, err
	}

	interval := time.Duration(dev.Interval) * time.Second
	if interval <= 0 {
		interval = 5 * time.Second
	}

	if prompt != nil {
		prompt(Verification{
			UserCode:        dev.UserCode,
			VerificationURI: dev.VerificationURI,
			ExpiresIn:       time.Duration(dev.ExpiresIn) * time.Second,
			Interval:        interval,
		})
	}

	tok, scopes, err := g.poll(ctx, dev.DeviceCode, interval)
	if err != nil {
		return nil, err
	}

	login, err := g.currentLogin(ctx, tok)
	if err != nil {
		return nil, err
	}

	return &Identity{
		Provider:  g.Name(),
		Login:     login,
		Token:     tok,
		Scopes:    scopes,
		CreatedAt: time.Now().UTC(),
	}, nil
}

func (g *GitHub) requestDeviceCode(ctx context.Context) (*deviceCodeResponse, error) {
	scopes := g.Scopes
	if len(scopes) == 0 {
		scopes = DefaultGitHubScopes
	}

	form := url.Values{
		"client_id": {g.ClientID},
		"scope":     {strings.Join(scopes, " ")},
	}

	var out deviceCodeResponse
	if err := g.postForm(ctx, g.base()+"/login/device/code", form, &out); err != nil {
		return nil, fmt.Errorf("request device code: %w", err)
	}
	if out.DeviceCode == "" || out.UserCode == "" {
		return nil, fmt.Errorf("request device code: incomplete response from provider")
	}
	return &out, nil
}

// poll waits for the user to approve. Per RFC 8628 the server may ask us to
// back off via slow_down, which permanently increases the interval.
func (g *GitHub) poll(ctx context.Context, deviceCode string, interval time.Duration) (string, []string, error) {
	form := url.Values{
		"client_id":   {g.ClientID},
		"device_code": {deviceCode},
		"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
	}

	timer := time.NewTimer(interval)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return "", nil, ctx.Err()
		case <-timer.C:
		}

		var out tokenResponse
		if err := g.postForm(ctx, g.base()+"/login/oauth/access_token", form, &out); err != nil {
			return "", nil, fmt.Errorf("poll for token: %w", err)
		}

		switch out.Error {
		case "":
			if out.AccessToken == "" {
				return "", nil, fmt.Errorf("poll for token: empty token in success response")
			}
			return out.AccessToken, splitScopes(out.Scope), nil
		case "authorization_pending":
			// Expected: user has not finished approving yet.
		case "slow_down":
			// Server-mandated backoff. Honor its interval when supplied.
			if out.Interval > 0 {
				interval = time.Duration(out.Interval) * time.Second
			} else {
				interval += 5 * time.Second
			}
		case "expired_token":
			return "", nil, ErrExpired
		case "access_denied":
			return "", nil, ErrDenied
		default:
			desc := out.Description
			if desc == "" {
				desc = out.Error
			}
			return "", nil, fmt.Errorf("poll for token: %s", desc)
		}

		timer.Reset(interval)
	}
}

// TokenPageURL returns a GitHub token-creation page with the scopes nimbus
// needs already selected, so the user only has to click Generate.
//
// This is the path that needs no registered OAuth app, which makes it the
// fastest way to a working login on a fresh install.
func (g *GitHub) TokenPageURL() string {
	scopes := g.Scopes
	if len(scopes) == 0 {
		scopes = DefaultGitHubScopes
	}
	return fmt.Sprintf("%s/settings/tokens/new?scopes=%s&description=%s",
		g.base(),
		url.QueryEscape(strings.Join(scopes, ",")),
		url.QueryEscape("Nimbus — device setup and state repo"))
}

// LoginWithToken adopts a token the user created by hand, verifying it works
// and identifying the account behind it before storing anything.
func (g *GitHub) LoginWithToken(ctx context.Context, token string) (*Identity, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, errors.New("no token provided")
	}

	login, err := g.currentLogin(ctx, token)
	if err != nil {
		if errors.Is(err, ErrNotLoggedIn) {
			return nil, errors.New("github rejected that token (check it was copied in full and has not expired)")
		}
		return nil, err
	}

	return &Identity{
		Provider:  g.Name(),
		Login:     login,
		Token:     token,
		Scopes:    g.tokenScopes(ctx, token),
		CreatedAt: time.Now().UTC(),
	}, nil
}

// tokenScopes reads the granted scopes from the response header. Fine-grained
// tokens report none, which is not an error — they carry permissions instead.
func (g *GitHub) tokenScopes(ctx context.Context, token string) []string {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.api()+"/user", nil)
	if err != nil {
		return nil
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := g.client().Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	return splitScopes(resp.Header.Get("X-OAuth-Scopes"))
}

// Validate confirms a stored token still authenticates.
func (g *GitHub) Validate(ctx context.Context, id *Identity) error {
	if id == nil || id.Token == "" {
		return ErrNotLoggedIn
	}
	_, err := g.currentLogin(ctx, id.Token)
	return err
}

func (g *GitHub) currentLogin(ctx context.Context, token string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.api()+"/user", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := g.client().Do(req)
	if err != nil {
		return "", fmt.Errorf("identify user: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		return "", ErrNotLoggedIn
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("identify user: unexpected status %s", resp.Status)
	}

	var body struct {
		Login string `json:"login"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", fmt.Errorf("identify user: %w", err)
	}
	if body.Login == "" {
		return "", fmt.Errorf("identify user: provider returned no login")
	}
	return body.Login, nil
}

// postForm posts url-encoded values and decodes a JSON reply. GitHub defaults
// to form-encoded responses, so the Accept header is required.
func (g *GitHub) postForm(ctx context.Context, endpoint string, form url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := g.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	// 400 carries a meaningful OAuth error body, so decode before judging status.
	if resp.StatusCode >= 500 {
		return fmt.Errorf("unexpected status %s", resp.Status)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

func splitScopes(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	// GitHub separates granted scopes with commas, sometimes with spaces.
	parts := strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' })
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
