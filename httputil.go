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
// e.g. "wss://permit.dnmfarrell.com/ws/relay" → "https://permit.dnmfarrell.com"
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

// enrollSession registers a session with the server and blocks until the user
// approves it on their phone. Returns an error if rejected or timed out.
func enrollSession(baseURL, deviceID, sessionID, project string) error {
	payload := map[string]string{
		"device_id":  deviceID,
		"session_id": sessionID,
	}
	if project != "" {
		payload["project"] = project
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to encode request: %w", err)
	}

	client := &http.Client{Timeout: 65 * time.Second}
	resp, err := client.Post(baseURL+"/session/enroll", "application/json", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("enrollment request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Errorf("enrollment rejected (HTTP %d)", resp.StatusCode)
	}

	var result struct {
		Approved bool   `json:"approved"`
		Message  string `json:"message"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("failed to decode response: %w", err)
	}
	if !result.Approved {
		if result.Message != "" {
			return fmt.Errorf("session enrollment %s", result.Message)
		}
		return fmt.Errorf("session enrollment rejected")
	}
	return nil
}

// postJSON sends a JSON POST request and returns the response.
func postJSON(url string, payload interface{}, timeout time.Duration) (*http.Response, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("failed to encode payload: %w", err)
	}
	client := &http.Client{Timeout: timeout}
	return client.Post(url, "application/json", bytes.NewReader(body))
}

// postRawJSON sends a pre-encoded JSON body as a POST request.
func postRawJSON(url string, body []byte, timeout time.Duration) (*http.Response, error) {
	client := &http.Client{Timeout: timeout}
	return client.Post(url, "application/json", bytes.NewReader(body))
}
