// Package tidal implements the Tidal API client, including OAuth2 device
// authorization flow and authenticated HTTP requests.
//
// Tidal uses OAuth2 Device Authorization Grant (RFC 8628) but with non-standard
// camelCase JSON keys in the device code response (e.g. "deviceCode" instead of
// "device_code"). The initial device code request is therefore done manually,
// while the token polling phase uses the standard oauth2 library.
package tidal

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/pkg/browser"
	"golang.org/x/oauth2"
)

const (
	ClientID     = "fX2JxdmntZWK0ixT"
	ClientSecret = "1Nn9AfDAjxrgJFJbKNWLeAyKGVGmINuXPPLHVXAvxAg="
	AuthURL      = "https://auth.tidal.com/v1/oauth2"
	BaseURL      = "https://api.tidal.com/v1"
	BaseURLV2    = "https://openapi.tidal.com/v2"
)

// Client holds the OAuth2 configuration and authenticated session state for
// making Tidal API requests.
type Client struct {
	Session   *Session
	Oauth     *oauth2.Config
	Transport http.RoundTripper // optional; overrides the base transport (used in tests)
}

// Session contains the persisted OAuth2 token and user metadata needed for
// all API calls (UserID for user-specific endpoints, CountryCode for content
// availability filtering).
type Session struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	TokenType    string    `json:"token_type"`
	Expiry       time.Time `json:"expiry"`
	UserID       int       `json:"user_id"`
	CountryCode  string    `json:"country_code"`
}

// NewClient creates a Tidal API client with the OAuth2 configuration
// pre-populated. Call AuthenticateInteractive to obtain a session, or set
// Client.Session directly from a persisted token.
func NewClient() *Client {
	return &Client{
		Oauth: &oauth2.Config{
			ClientID:     ClientID,
			ClientSecret: ClientSecret,
			Endpoint: oauth2.Endpoint{
				AuthURL:       AuthURL + "/device_authorization",
				DeviceAuthURL: AuthURL + "/device_authorization",
				TokenURL:      AuthURL + "/token",
			},
			Scopes: []string{"r_usr", "w_usr", "w_sub"},
		},
	}
}

// AuthenticateInteractive runs the OAuth2 device code flow in the terminal.
// It displays a QR code and user code, opens the verification URL in the
// default browser, and polls for token completion.
func (c *Client) AuthenticateInteractive(ctx context.Context) (*Session, error) {
	fmt.Println("Initiating Tidal Login...")

	httpClient := &http.Client{Timeout: 10 * time.Second}

	// Step 1: Request device code manually — Tidal's response uses camelCase
	// JSON keys (e.g. "deviceCode") instead of the RFC 8628 snake_case
	// ("device_code"), so oauth2.Config.DeviceAuth cannot parse it directly.
	data := url.Values{}
	data.Set("client_id", ClientID)
	data.Set("scope", "r_usr w_usr w_sub")

	resp, err := httpClient.PostForm(c.Oauth.Endpoint.AuthURL, data)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	var da struct {
		DeviceCode      string `json:"deviceCode"`      // NOT device_code (Tidal non-standard)
		UserCode        string `json:"userCode"`         // NOT user_code
		VerificationURI string `json:"verificationUri"`  // NOT verification_uri
		Interval        int    `json:"interval"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&da); err != nil {
		return nil, err
	}

	verifyURL := "https://" + da.VerificationURI + "?user_code=" + url.QueryEscape(da.UserCode)

	fmt.Println("Scan the QR code or open the link below to log in:")
	printLoginPrompt(verifyURL, da.UserCode)

	_ = browser.OpenURL(verifyURL)

	// Step 2: Poll for token using the standard oauth2 library. The token
	// endpoint uses standard JSON keys, so DeviceAccessToken works here.
	standardDA := &oauth2.DeviceAuthResponse{
		DeviceCode:      da.DeviceCode,
		UserCode:        da.UserCode,
		VerificationURI: da.VerificationURI,
		Interval:        int64(da.Interval),
	}

	token, err := c.Oauth.DeviceAccessToken(ctx, standardDA)
	if err != nil {
		return nil, err
	}

	// Step 3: Fetch session info to get UserID and CountryCode. These are
	// required for every subsequent API call.
	req, err := http.NewRequestWithContext(ctx, "GET", BaseURL+"/sessions", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token.AccessToken)

	sResp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch session info: %w", err)
	}
	defer func() { _ = sResp.Body.Close() }()

	var sessionInfo struct {
		UserID      int    `json:"userId"`
		CountryCode string `json:"countryCode"`
	}
	if err := json.NewDecoder(sResp.Body).Decode(&sessionInfo); err != nil {
		// Fallback: extract from token extras if session endpoint fails.
		if user, ok := token.Extra("user").(map[string]interface{}); ok {
			if id, ok := user["id"].(float64); ok {
				sessionInfo.UserID = int(id)
			}
		}
		if cc, ok := token.Extra("countryCode").(string); ok {
			sessionInfo.CountryCode = cc
		}
	}

	c.Session = &Session{
		AccessToken:  token.AccessToken,
		RefreshToken: token.RefreshToken,
		TokenType:    token.TokenType,
		Expiry:       token.Expiry,
		UserID:       sessionInfo.UserID,
		CountryCode:  sessionInfo.CountryCode,
	}

	if c.Session.CountryCode == "" {
		c.Session.CountryCode = "US"
	}

	fmt.Printf("Successfully authenticated! (User: %d, Country: %s)\n", c.Session.UserID, c.Session.CountryCode)
	return c.Session, nil
}

// TokenSource wraps the session into an oauth2.TokenSource that handles
// automatic token refresh.
func (c *Client) TokenSource(ctx context.Context) oauth2.TokenSource {
	t := &oauth2.Token{
		AccessToken:  c.Session.AccessToken,
		RefreshToken: c.Session.RefreshToken,
		TokenType:    c.Session.TokenType,
		Expiry:       c.Session.Expiry,
	}
	return c.Oauth.TokenSource(ctx, t)
}

// RevokeToken revokes the given token via the Tidal OAuth2 revocation endpoint.
// Errors are logged but not fatal — the caller should still delete the local
// session regardless.
func (c *Client) RevokeToken(ctx context.Context, token string) error {
	data := url.Values{}
	data.Set("token", token)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, AuthURL+"/revoke",
		strings.NewReader(data.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(ClientID, ClientSecret)

	httpClient := &http.Client{Timeout: 10 * time.Second}
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("revoke returned HTTP %d", resp.StatusCode)
	}
	return nil
}

// GetAuthClient returns an *http.Client with the OAuth2 authorization header
// injected into every request. If a custom Transport is set (for testing), it
// is used as the base transport with the oauth2 layer on top.
func (c *Client) GetAuthClient(ctx context.Context) *http.Client {
	if c.Transport != nil {
		return &http.Client{
			Transport: &oauth2.Transport{
				Source: c.TokenSource(ctx),
				Base:   c.Transport,
			},
		}
	}
	return oauth2.NewClient(ctx, c.TokenSource(ctx))
}
