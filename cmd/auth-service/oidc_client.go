package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// OIDCDiscoveryResponse holds standard OpenID Connect configuration metadata.
type OIDCDiscoveryResponse struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	JwksURI               string `json:"jwks_uri"`
	UserinfoEndpoint      string `json:"userinfo_endpoint"`
	EndSessionEndpoint    string `json:"end_session_endpoint"`
	RevocationEndpoint    string `json:"revocation_endpoint"`
}

// OIDCTokenResponse represents the OAuth2 / OIDC token issuance response.
type OIDCTokenResponse struct {
	AccessToken      string `json:"access_token"`
	RefreshToken     string `json:"refresh_token"`
	TokenType        string `json:"token_type"`
	ExpiresIn        int64  `json:"expires_in"`
	RefreshExpiresIn int64  `json:"refresh_expires_in"`
	Scope            string `json:"scope"`
	Error            string `json:"error,omitempty"`
	ErrorDescription string `json:"error_description,omitempty"`
}

// OIDCClient encapsulates discovery, token issuance, and revocation calls against an external OIDC provider like Keycloak.
type OIDCClient struct {
	issuerURL    string
	clientID     string
	clientSecret string
	httpClient   *http.Client
	config       *OIDCDiscoveryResponse
}

// NewOIDCClient initializes an OIDC client and fetches provider configuration.
func NewOIDCClient(ctx context.Context, issuerURL, clientID, clientSecret string, explicitJWKS string) (*OIDCClient, error) {
	cleanIssuer := strings.TrimRight(issuerURL, "/")
	client := &OIDCClient{
		issuerURL:    cleanIssuer,
		clientID:     clientID,
		clientSecret: clientSecret,
		httpClient:   &http.Client{Timeout: 10 * time.Second},
	}

	// Fetch discovery document
	discoveryURL := cleanIssuer + "/.well-known/openid-configuration"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, discoveryURL, nil)
	if err != nil {
		return nil, fmt.Errorf("oidc: create discovery request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := client.httpClient.Do(req)
	if err != nil {
		// If discovery fails at startup (e.g. Keycloak starting up or explicit JWKS provided), allow manual fallback
		if explicitJWKS != "" {
			client.config = &OIDCDiscoveryResponse{
				Issuer:  cleanIssuer,
				JwksURI: explicitJWKS,
			}
			return client, nil
		}
		return nil, fmt.Errorf("oidc: discovery failed from %s: %w", discoveryURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		if explicitJWKS != "" {
			client.config = &OIDCDiscoveryResponse{
				Issuer:  cleanIssuer,
				JwksURI: explicitJWKS,
			}
			return client, nil
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("oidc: discovery returned status %d: %s", resp.StatusCode, string(body))
	}

	var conf OIDCDiscoveryResponse
	if err := json.NewDecoder(resp.Body).Decode(&conf); err != nil {
		return nil, fmt.Errorf("oidc: parse discovery response: %w", err)
	}

	if explicitJWKS != "" {
		conf.JwksURI = explicitJWKS
	}
	client.config = &conf
	return client, nil
}

// JwksURI returns the discovered or configured JWKS URI.
func (c *OIDCClient) JwksURI() string {
	if c.config != nil {
		return c.config.JwksURI
	}
	return ""
}

// Issuer returns the issuer URL.
func (c *OIDCClient) Issuer() string {
	if c.config != nil && c.config.Issuer != "" {
		return c.config.Issuer
	}
	return c.issuerURL
}

// AuthenticatePassword performs Direct Access Grant (ROPC flow) against Keycloak token endpoint.
func (c *OIDCClient) AuthenticatePassword(ctx context.Context, username, password string, scopes []string) (*OIDCTokenResponse, error) {
	if c.config == nil || c.config.TokenEndpoint == "" {
		return nil, fmt.Errorf("oidc: token endpoint not available")
	}

	form := url.Values{}
	form.Set("grant_type", "password")
	form.Set("client_id", c.clientID)
	if c.clientSecret != "" {
		form.Set("client_secret", c.clientSecret)
	}
	form.Set("username", username)
	form.Set("password", password)

	scopeStr := "openid"
	if len(scopes) > 0 {
		scopeStr = "openid " + strings.Join(scopes, " ")
	}
	form.Set("scope", scopeStr)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.config.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("oidc: create login request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("oidc: login request failed: %w", err)
	}
	defer resp.Body.Close()

	var tokenResp OIDCTokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
		return nil, fmt.Errorf("oidc: parse token response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		msg := tokenResp.ErrorDescription
		if msg == "" {
			msg = tokenResp.Error
		}
		if msg == "" {
			msg = fmt.Sprintf("status %d", resp.StatusCode)
		}
		return nil, fmt.Errorf("oidc: authentication rejected: %s", msg)
	}

	return &tokenResp, nil
}

// RefreshToken exchanges an existing refresh token with Keycloak's token endpoint.
func (c *OIDCClient) RefreshToken(ctx context.Context, refreshToken string) (*OIDCTokenResponse, error) {
	if c.config == nil || c.config.TokenEndpoint == "" {
		return nil, fmt.Errorf("oidc: token endpoint not available")
	}

	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("client_id", c.clientID)
	if c.clientSecret != "" {
		form.Set("client_secret", c.clientSecret)
	}
	form.Set("refresh_token", refreshToken)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.config.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("oidc: create refresh request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("oidc: refresh request failed: %w", err)
	}
	defer resp.Body.Close()

	var tokenResp OIDCTokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
		return nil, fmt.Errorf("oidc: parse refresh response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("oidc: refresh failed: %s", tokenResp.ErrorDescription)
	}

	return &tokenResp, nil
}
