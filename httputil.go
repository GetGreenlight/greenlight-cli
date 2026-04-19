//go:build darwin || linux

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// serverBaseURL derives the HTTPS base URL from the build-time wsURL.
// e.g. "wss://api.aigreenlight.app/ws/relay" → "https://api.aigreenlight.app"
func serverBaseURL() (string, error) {
	if wsURL == "" {
		return "", fmt.Errorf("no relay server URL configured")
	}
	u, err := url.Parse(wsURL)
	if err != nil {
		return "", fmt.Errorf("bad relay URL: %w", err)
	}
	scheme := "https"
	if u.Scheme == "ws" {
		scheme = "http"
	}
	return fmt.Sprintf("%s://%s", scheme, u.Host), nil
}

// registerUser registers a user by email and returns the user_id and the
// organization_id of the user's primary organization (auto-created on first
// registration).
func registerUser(baseURL, email, name, orgName string) (userID, orgID string, err error) {
	payload := map[string]string{"email": email, "name": name, "org_name": orgName}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", "", fmt.Errorf("failed to encode request: %w", err)
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Post(baseURL+"/users/register", "application/json", bytes.NewReader(body))
	if err != nil {
		return "", "", fmt.Errorf("registration request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return "", "", fmt.Errorf("registration failed (HTTP %d)", resp.StatusCode)
	}

	var result struct {
		UserID         string `json:"user_id"`
		OrganizationID string `json:"organization_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", "", fmt.Errorf("failed to decode response: %w", err)
	}
	if result.UserID == "" {
		return "", "", fmt.Errorf("server returned empty user_id")
	}
	return result.UserID, result.OrganizationID, nil
}

// registerHost registers a host (daemon) for a user. Returns the io_device_id
// and the plaintext secret the server allocated for this host's terminal.
// A fresh secret is minted on every call — re-running `greenlight register`
// doubles as credential rotation.
func registerHost(baseURL, userID, hostID, hostname string) (ioDeviceID, secret string, err error) {
	payload := map[string]string{
		"user_id": userID,
		"host_id": hostID,
	}
	if hostname != "" {
		payload["hostname"] = hostname
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", "", fmt.Errorf("failed to encode request: %w", err)
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Post(baseURL+"/hosts/register", "application/json", bytes.NewReader(body))
	if err != nil {
		return "", "", fmt.Errorf("host registration request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == 404 {
		return "", "", fmt.Errorf("unknown user (register first with 'greenlight register <email>')")
	}
	if resp.StatusCode != 200 {
		return "", "", fmt.Errorf("host registration failed (HTTP %d)", resp.StatusCode)
	}

	var result struct {
		IODeviceID     string `json:"io_device_id"`
		IODeviceSecret string `json:"io_device_secret"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", "", fmt.Errorf("failed to decode response: %w", err)
	}
	if result.IODeviceID == "" || result.IODeviceSecret == "" {
		return "", "", fmt.Errorf("server returned empty io_device credentials")
	}
	return result.IODeviceID, result.IODeviceSecret, nil
}

