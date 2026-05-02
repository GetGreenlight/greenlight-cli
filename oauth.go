//go:build darwin || linux

package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// oauthProvider mirrors the server's OAuthProviderInfo (subset we need).
type oauthProvider struct {
	ID                  string `json:"id"`
	TokenURL            string `json:"token_url"`
	ClientID            string `json:"client_id"`
	ClientSecret        string `json:"client_secret"`
	AccessTokenLifetime *int   `json:"access_token_lifetime"`
	TokenAuthMethod     string `json:"token_auth_method"`
}

var (
	oauthCacheMu sync.Mutex
	oauthCache   []oauthProvider
	oauthCacheAt time.Time
)

// fetchProviders returns the OAuth provider registry, cached for an hour.
func fetchProviders() ([]oauthProvider, error) {
	oauthCacheMu.Lock()
	defer oauthCacheMu.Unlock()
	if oauthCache != nil && time.Since(oauthCacheAt) < time.Hour {
		return oauthCache, nil
	}
	raw, err := daemonWSRequest("list_oauth_providers", map[string]interface{}{}, 30*time.Second)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Providers []oauthProvider `json:"providers"`
		Error     string          `json:"error"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, err
	}
	if resp.Error != "" {
		return nil, fmt.Errorf("%s", resp.Error)
	}
	oauthCache = resp.Providers
	oauthCacheAt = time.Now()
	return oauthCache, nil
}

// refreshAccessToken refreshes an expired OAuth access token by exchanging
// its companion refresh token at the provider's token endpoint, then storing
// and returning the new plaintext access token.
//
// The key is shaped like ${PROVIDER}_ACCESS_TOKEN and the refresh token is
// stored under ${PROVIDER}_REFRESH_TOKEN.
func refreshAccessToken(accessKey string) ([]byte, error) {
	prefix := strings.TrimSuffix(accessKey, "_ACCESS_TOKEN")
	if prefix == accessKey {
		return nil, fmt.Errorf("not an access token key")
	}
	providerID := strings.ToLower(prefix)
	refreshKey := prefix + "_REFRESH_TOKEN"

	// Get refresh token (encrypted) from server.
	refreshRaw, err := daemonWSRequest("secrets_get", map[string]interface{}{
		"key": refreshKey,
	}, 30*time.Second)
	if err != nil {
		return nil, err
	}
	var rresp struct {
		Ciphertext string `json:"ciphertext"`
		Error      string `json:"error"`
	}
	if err := json.Unmarshal(refreshRaw, &rresp); err != nil {
		return nil, err
	}
	if rresp.Error == "not_found" {
		return nil, fmt.Errorf("no refresh token (%s) found", refreshKey)
	}
	if rresp.Error != "" && rresp.Error != "expired" {
		return nil, fmt.Errorf("%s", rresp.Error)
	}
	if rresp.Ciphertext == "" {
		return nil, fmt.Errorf("empty refresh ciphertext")
	}

	priv, err := loadPrivateKey()
	if err != nil {
		return nil, fmt.Errorf("load key: %w", err)
	}
	blob, err := base64.StdEncoding.DecodeString(rresp.Ciphertext)
	if err != nil {
		return nil, err
	}
	refreshToken, err := decryptSecret(priv, blob)
	if err != nil {
		return nil, fmt.Errorf("decrypt refresh token: %w", err)
	}

	// Look up provider config.
	providers, err := fetchProviders()
	if err != nil {
		return nil, fmt.Errorf("fetch providers: %w", err)
	}
	var prov *oauthProvider
	for i := range providers {
		if providers[i].ID == providerID {
			prov = &providers[i]
			break
		}
	}
	if prov == nil {
		return nil, fmt.Errorf("unknown provider %q", providerID)
	}

	// Build refresh request.
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {string(refreshToken)},
	}
	useBasic := prov.TokenAuthMethod == "basic"
	if !useBasic {
		form.Set("client_id", prov.ClientID)
		if prov.ClientSecret != "" {
			form.Set("client_secret", prov.ClientSecret)
		}
	}

	httpReq, err := http.NewRequest("POST", prov.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	httpReq.Header.Set("Accept", "application/json")
	if useBasic {
		creds := prov.ClientID + ":" + prov.ClientSecret
		httpReq.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(creds)))
	}

	client := &http.Client{Timeout: 30 * time.Second}
	httpResp, err := client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer httpResp.Body.Close()
	body, _ := io.ReadAll(httpResp.Body)
	if httpResp.StatusCode != 200 {
		return nil, fmt.Errorf("token endpoint returned HTTP %d: %s", httpResp.StatusCode, string(body))
	}
	var tok struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &tok); err != nil {
		return nil, err
	}
	if tok.AccessToken == "" {
		return nil, fmt.Errorf("no access_token in refresh response")
	}

	// Store the new access token.
	expiresIn := tok.ExpiresIn
	if expiresIn == 0 && prov.AccessTokenLifetime != nil {
		expiresIn = *prov.AccessTokenLifetime
	}
	putBody := map[string]interface{}{
		"key":        accessKey,
		"ciphertext": "",
	}
	newCT, err := encryptSecret(priv.PublicKey(), []byte(tok.AccessToken))
	if err != nil {
		return nil, err
	}
	putBody["ciphertext"] = base64.StdEncoding.EncodeToString(newCT)
	if expiresIn > 0 {
		putBody["expires_at"] = time.Now().UTC().Add(time.Duration(expiresIn) * time.Second).Format(time.RFC3339)
	}
	if _, err := daemonWSRequest("secrets_put", putBody, 30*time.Second); err != nil {
		return nil, fmt.Errorf("store new access token: %w", err)
	}

	// If the provider rotated the refresh token, store the new one too.
	if tok.RefreshToken != "" && tok.RefreshToken != string(refreshToken) {
		newRefCT, err := encryptSecret(priv.PublicKey(), []byte(tok.RefreshToken))
		if err == nil {
			daemonWSRequest("secrets_put", map[string]interface{}{
				"key":        refreshKey,
				"ciphertext": base64.StdEncoding.EncodeToString(newRefCT),
			}, 30*time.Second)
		}
	}

	return []byte(tok.AccessToken), nil
}
