//go:build darwin || linux

package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/manifoldco/promptui"
)

// selectItem holds a display label and the underlying integer ID.
type selectItem struct {
	Label string
	ID    int
}

// selectFromList renders an interactive selector and returns the chosen ID.
// items must be non-empty. Falls back to a plain text prompt when stdout is
// not a terminal (e.g. piped input).
func selectFromList(label string, items []selectItem) (int, error) {
	if len(items) == 0 {
		return 0, fmt.Errorf("no items to select from")
	}

	labels := make([]string, len(items))
	for i, item := range items {
		labels[i] = item.Label
	}

	sel := promptui.Select{
		Label: label,
		Items: labels,
		Size:  10,
	}

	idx, _, err := sel.Run()
	if err != nil {
		return 0, err
	}
	return items[idx].ID, nil
}

// selectOrganization fetches the organizations list and prompts the user to pick one.
func selectOrganization() (int, error) {
	data, err := sendWSRequest("list_organizations", nil)
	if err != nil {
		return 0, fmt.Errorf("failed to fetch organizations: %w", err)
	}

	var resp struct {
		Organizations []struct {
			ID   int    `json:"id"`
			Name string `json:"name"`
		} `json:"organizations"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return 0, fmt.Errorf("failed to parse organizations: %w", err)
	}
	if len(resp.Organizations) == 0 {
		fmt.Fprintln(os.Stderr, "greenlight: no organizations found — create one first")
		os.Exit(1)
	}

	items := make([]selectItem, len(resp.Organizations))
	for i, o := range resp.Organizations {
		items[i] = selectItem{Label: fmt.Sprintf("%d: %s", o.ID, o.Name), ID: o.ID}
	}
	return selectFromList("Organization", items)
}

// selectAgentJobDescription fetches job descriptions for the given org and prompts the user.
func selectAgentJobDescription(orgID int) (int, error) {
	payload := map[string]interface{}{}
	if orgID != 0 {
		payload["organization_id"] = orgID
	}
	data, err := sendWSRequest("list_agent_job_descriptions", payload)
	if err != nil {
		return 0, fmt.Errorf("failed to fetch job descriptions: %w", err)
	}

	var resp struct {
		AgentJobDescriptions []struct {
			ID    int    `json:"id"`
			Title string `json:"title"`
		} `json:"agent_job_descriptions"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return 0, fmt.Errorf("failed to parse job descriptions: %w", err)
	}
	if len(resp.AgentJobDescriptions) == 0 {
		fmt.Fprintln(os.Stderr, "greenlight: no job descriptions found — create one first")
		os.Exit(1)
	}

	items := make([]selectItem, len(resp.AgentJobDescriptions))
	for i, j := range resp.AgentJobDescriptions {
		items[i] = selectItem{Label: fmt.Sprintf("%d: %s", j.ID, j.Title), ID: j.ID}
	}
	return selectFromList("Agent job description", items)
}

// selectWorkingDirectory fetches working directories for the given org and prompts the user.
func selectWorkingDirectory(orgID int) (int, error) {
	payload := map[string]interface{}{}
	if orgID != 0 {
		payload["organization_id"] = orgID
	}
	data, err := sendWSRequest("list_working_directories", payload)
	if err != nil {
		return 0, fmt.Errorf("failed to fetch working directories: %w", err)
	}

	var resp struct {
		WorkingDirectories []struct {
			ID            int    `json:"id"`
			DirectoryPath string `json:"directory_path"`
		} `json:"working_directories"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return 0, fmt.Errorf("failed to parse working directories: %w", err)
	}
	if len(resp.WorkingDirectories) == 0 {
		fmt.Fprintln(os.Stderr, "greenlight: no working directories found — create one first")
		os.Exit(1)
	}

	items := make([]selectItem, len(resp.WorkingDirectories))
	for i, w := range resp.WorkingDirectories {
		items[i] = selectItem{Label: fmt.Sprintf("%d: %s", w.ID, w.DirectoryPath), ID: w.ID}
	}
	return selectFromList("Working directory", items)
}

// selectOrganizationPosition fetches positions for the given org and prompts the user.
func selectOrganizationPosition(orgID int) (int, error) {
	payload := map[string]interface{}{}
	if orgID != 0 {
		payload["organization_id"] = orgID
	}
	data, err := sendWSRequest("list_organization_positions", payload)
	if err != nil {
		return 0, fmt.Errorf("failed to fetch organization positions: %w", err)
	}

	var resp struct {
		OrganizationPositions []struct {
			ID                    int `json:"id"`
			AgentJobDescriptionID int `json:"agent_job_description_id"`
			WorkingDirectoryID    int `json:"working_directory_id"`
		} `json:"organization_positions"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return 0, fmt.Errorf("failed to parse organization positions: %w", err)
	}
	if len(resp.OrganizationPositions) == 0 {
		fmt.Fprintln(os.Stderr, "greenlight: no organization positions found — create one first")
		os.Exit(1)
	}

	items := make([]selectItem, len(resp.OrganizationPositions))
	for i, p := range resp.OrganizationPositions {
		label := fmt.Sprintf("%d: job=%d wd=%d", p.ID, p.AgentJobDescriptionID, p.WorkingDirectoryID)
		items[i] = selectItem{Label: label, ID: p.ID}
	}
	return selectFromList("Organization position", items)
}
