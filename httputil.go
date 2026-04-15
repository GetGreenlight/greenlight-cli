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
func registerUser(baseURL, email string) (userID, orgID string, err error) {
	payload := map[string]string{"email": email}
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
// the server allocated (or found) for this host's terminal.
func registerHost(baseURL, userID, hostID, hostname string) (string, error) {
	payload := map[string]string{
		"user_id": userID,
		"host_id": hostID,
	}
	if hostname != "" {
		payload["hostname"] = hostname
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("failed to encode request: %w", err)
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Post(baseURL+"/hosts/register", "application/json", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("host registration request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == 404 {
		return "", fmt.Errorf("unknown user (register first with 'greenlight register <email>')")
	}
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("host registration failed (HTTP %d)", resp.StatusCode)
	}

	var result struct {
		IODeviceID string `json:"io_device_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("failed to decode response: %w", err)
	}
	return result.IODeviceID, nil
}

// enrollSession registers a session with the server and blocks until the user
// approves it on their phone. Returns an error if rejected or timed out.
func enrollSession(baseURL, deviceID, sessionID, project, agent, cwd, hostname string) error {
	payload := map[string]string{
		"device_id":  deviceID,
		"session_id": sessionID,
	}
	if project != "" {
		payload["project"] = project
	}
	if agent != "" {
		payload["agent"] = agent
	}
	if cwd != "" {
		payload["cwd"] = cwd
	}
	if hostname != "" {
		payload["hostname"] = hostname
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
