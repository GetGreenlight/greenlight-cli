//go:build darwin || linux

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/mdp/qrterminal/v3"
)

// runPair initiates interactive pairing: displays a QR code and 6-digit code,
// polls until the mobile app claims it, then saves the device ID.
func runPair(baseURL string) (string, error) {
	// Create a pairing code
	body, _ := json.Marshal(map[string]string{})
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Post(baseURL+"/pair/create", "application/json", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("failed to create pairing code: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return "", fmt.Errorf("server returned HTTP %d", resp.StatusCode)
	}

	var result struct {
		Code      string `json:"code"`
		Token     string `json:"token"`
		ExpiresIn int    `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("failed to decode response: %w", err)
	}

	// Build the QR URL (includes both token for web redirect and code for direct app use)
	qrURL := fmt.Sprintf("https://aigreenlight.app/pair?t=%s&code=%s", url.QueryEscape(result.Token), result.Code)

	// Display the QR code and numeric code
	fmt.Fprintf(os.Stderr, "\n  Pair your phone with Greenlight\n\n")
	qrterminal.GenerateHalfBlock(qrURL, qrterminal.L, os.Stderr)
	fmt.Fprintf(os.Stderr, "\n  Scan the QR code, or enter this code in the app:\n\n")
	fmt.Fprintf(os.Stderr, "    %s %s %s   %s %s %s\n\n",
		string(result.Code[0]), string(result.Code[1]), string(result.Code[2]),
		string(result.Code[3]), string(result.Code[4]), string(result.Code[5]))

	// Poll until claimed or expired
	deadline := time.Now().Add(time.Duration(result.ExpiresIn) * time.Second)
	for time.Now().Before(deadline) {
		remaining := time.Until(deadline).Truncate(time.Second)
		fmt.Fprintf(os.Stderr, "\r  Waiting for phone... (expires in %d:%02d)  ",
			int(remaining.Minutes()), int(remaining.Seconds())%60)

		deviceID, done, err := pollPairing(baseURL, result.Token)
		if err != nil {
			return "", fmt.Errorf("polling failed: %w", err)
		}
		if done {
			fmt.Fprintf(os.Stderr, "\r  Paired!                                    \n\n")
			// Save to config
			if err := writeConfigValue("device_id", deviceID); err != nil {
				fmt.Fprintf(os.Stderr, "  Warning: could not save device ID to config: %v\n", err)
			}
			return deviceID, nil
		}
	}

	fmt.Fprintf(os.Stderr, "\r  Pairing code expired.                      \n\n")
	return "", fmt.Errorf("pairing code expired")
}

// pollPairing long-polls the server for pairing completion.
// Returns (deviceID, true, nil) if claimed, ("", false, nil) if still pending.
func pollPairing(baseURL, token string) (string, bool, error) {
	client := &http.Client{Timeout: 35 * time.Second}
	resp, err := client.Get(baseURL + "/pair/poll?token=" + url.QueryEscape(token))
	if err != nil {
		return "", false, err
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case 200:
		var result struct {
			DeviceID string `json:"device_id"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			return "", false, err
		}
		return result.DeviceID, true, nil
	case 202:
		return "", false, nil // still pending
	case 404:
		return "", false, fmt.Errorf("pairing code expired or invalid")
	default:
		return "", false, fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
}

// runPairCommand handles the `greenlight pair` subcommand.
func runPairCommand(args []string) {
	baseURL, err := serverBaseURL()
	if err != nil {
		fmt.Fprintf(os.Stderr, "greenlight: %v\n", err)
		os.Exit(1)
	}

	deviceID, err := runPair(baseURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "greenlight: pairing failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "Registered device %s\n", deviceID)
}
