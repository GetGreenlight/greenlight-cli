//go:build darwin || linux

package main

import (
	"bufio"
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

// =============================================================================
// selectOrganization
// =============================================================================

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
		fmt.Println("No organizations found. Let's create one.")
		return createOrganizationInteractive()
	}

	return selectFromList("Organization", func() []selectItem {
		items := make([]selectItem, len(resp.Organizations))
		for i, o := range resp.Organizations {
			items[i] = selectItem{Label: fmt.Sprintf("%d: %s", o.ID, o.Name), ID: o.ID}
		}
		return items
	}())
}

func createOrganizationInteractive() (int, error) {
	reader := bufio.NewReader(os.Stdin)
	name := promptLine(reader, "Name: ")
	if name == "" {
		return 0, fmt.Errorf("name required")
	}
	email := promptLine(reader, "Recovery email (optional): ")

	payload := map[string]interface{}{"name": name}
	if email != "" {
		payload["recovery_email"] = email
	}
	data, err := sendWSRequest("create_organization", payload)
	if err != nil {
		return 0, fmt.Errorf("failed to create organization: %w", err)
	}

	var resp struct {
		Organization struct {
			ID int `json:"id"`
		} `json:"organization"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return 0, fmt.Errorf("failed to parse response: %w", err)
	}
	if resp.Error != "" {
		return 0, fmt.Errorf("create organization: %s", resp.Error)
	}
	fmt.Printf("Created organization %d.\n", resp.Organization.ID)
	return resp.Organization.ID, nil
}

// =============================================================================
// selectAgentJobDescription
// =============================================================================

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
		fmt.Println("No job descriptions found. Let's create one.")
		return createAgentJobDescriptionInteractive(orgID)
	}

	items := make([]selectItem, len(resp.AgentJobDescriptions))
	for i, j := range resp.AgentJobDescriptions {
		items[i] = selectItem{Label: fmt.Sprintf("%d: %s", j.ID, j.Title), ID: j.ID}
	}
	items = append(items, selectItem{Label: "→ Create new…", ID: 0})

	id, err := selectFromList("Agent job description", items)
	if err != nil {
		return 0, err
	}
	if id == 0 {
		return createAgentJobDescriptionInteractive(orgID)
	}
	return id, nil
}

func createAgentJobDescriptionInteractive(orgID int) (int, error) {
	if orgID == 0 {
		orgID = workingOrgID()
	}
	if orgID == 0 {
		return 0, fmt.Errorf("no working organization set (run 'greenlight organization organization use --id <ID>')")
	}

	reader := bufio.NewReader(os.Stdin)
	title := promptLine(reader, "Title: ")
	if title == "" {
		return 0, fmt.Errorf("title required")
	}
	mandate := promptLine(reader, "Mandate (optional): ")

	payload := map[string]interface{}{
		"organization_id": orgID,
		"title":           title,
		"priority":        5,
	}
	if mandate != "" {
		payload["mandate"] = mandate
	}
	data, err := sendWSRequest("create_agent_job_description", payload)
	if err != nil {
		return 0, fmt.Errorf("failed to create job description: %w", err)
	}

	var resp struct {
		AgentJobDescription struct {
			ID int `json:"id"`
		} `json:"agent_job_description"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return 0, fmt.Errorf("failed to parse response: %w", err)
	}
	if resp.Error != "" {
		return 0, fmt.Errorf("create job description: %s", resp.Error)
	}
	fmt.Printf("Created job description %d.\n", resp.AgentJobDescription.ID)
	return resp.AgentJobDescription.ID, nil
}

// =============================================================================
// selectWorkingDirectory
// =============================================================================

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
			Hostname      string `json:"hostname"`
			DirectoryPath string `json:"directory_path"`
		} `json:"working_directories"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return 0, fmt.Errorf("failed to parse working directories: %w", err)
	}

	if len(resp.WorkingDirectories) == 0 {
		fmt.Println("No working directories found. Let's create one.")
		return createWorkingDirectoryInteractive(orgID)
	}

	items := make([]selectItem, len(resp.WorkingDirectories))
	for i, w := range resp.WorkingDirectories {
		label := w.Hostname + ":" + w.DirectoryPath
		if w.Hostname == "" && w.DirectoryPath == "" {
			label = fmt.Sprintf("id=%d", w.ID)
		}
		items[i] = selectItem{Label: fmt.Sprintf("%d: %s", w.ID, label), ID: w.ID}
	}
	items = append(items, selectItem{Label: "→ Create new…", ID: 0})

	id, err := selectFromList("Working directory", items)
	if err != nil {
		return 0, err
	}
	if id == 0 {
		return createWorkingDirectoryInteractive(orgID)
	}
	return id, nil
}

func createWorkingDirectoryInteractive(orgID int) (int, error) {
	if orgID == 0 {
		orgID = workingOrgID()
	}
	if orgID == 0 {
		return 0, fmt.Errorf("no working organization set (run 'greenlight organization organization use --id <ID>')")
	}

	hostID := readConfigValue("host_id")
	if hostID == "" {
		return 0, fmt.Errorf("host ID not found — run 'greenlight daemon start' to enroll this host")
	}

	cwd, _ := os.Getwd()
	reader := bufio.NewReader(os.Stdin)
	dir := promptWithDefault(reader, "Directory path", cwd)

	payload := map[string]interface{}{
		"organization_id": orgID,
		"host_id":         hostID,
	}
	if dir != "" {
		payload["directory_path"] = dir
	}
	data, err := sendWSRequest("create_working_directory", payload)
	if err != nil {
		return 0, fmt.Errorf("failed to create working directory: %w", err)
	}

	var resp struct {
		WorkingDirectory struct {
			ID int `json:"id"`
		} `json:"working_directory"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return 0, fmt.Errorf("failed to parse response: %w", err)
	}
	if resp.Error != "" {
		return 0, fmt.Errorf("create working directory: %s", resp.Error)
	}
	fmt.Printf("Created working directory %d.\n", resp.WorkingDirectory.ID)
	return resp.WorkingDirectory.ID, nil
}

// =============================================================================
// selectOrganizationPosition
// =============================================================================

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
			ID                    int    `json:"id"`
			Name                  string `json:"name"`
			WorkingDirectoryID    int    `json:"working_directory_id"`
			AgentJobDescriptionID *int   `json:"agent_job_description_id"`
		} `json:"organization_positions"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return 0, fmt.Errorf("failed to parse organization positions: %w", err)
	}

	if len(resp.OrganizationPositions) == 0 {
		fmt.Println("No organization positions found. Let's create one.")
		return createOrganizationPositionInteractive(orgID)
	}

	items := make([]selectItem, len(resp.OrganizationPositions))
	for i, p := range resp.OrganizationPositions {
		var label string
		if p.Name != "" {
			label = p.Name
		} else {
			label = fmt.Sprintf("wd=%d", p.WorkingDirectoryID)
			if p.AgentJobDescriptionID != nil {
				label += fmt.Sprintf(" job=%d", *p.AgentJobDescriptionID)
			}
		}
		items[i] = selectItem{Label: fmt.Sprintf("%d: %s", p.ID, label), ID: p.ID}
	}
	items = append(items, selectItem{Label: "→ Create new…", ID: 0})

	id, err := selectFromList("Organization position", items)
	if err != nil {
		return 0, err
	}
	if id == 0 {
		return createOrganizationPositionInteractive(orgID)
	}
	return id, nil
}

func createOrganizationPositionInteractive(orgID int) (int, error) {
	if orgID == 0 {
		orgID = workingOrgID()
	}
	if orgID == 0 {
		return 0, fmt.Errorf("no working organization set (run 'greenlight organization organization use --id <ID>')")
	}

	jobID, err := selectAgentJobDescription(orgID)
	if err != nil {
		return 0, err
	}

	wdID, err := selectWorkingDirectory(orgID)
	if err != nil {
		return 0, err
	}

	reader := bufio.NewReader(os.Stdin)
	name := promptLine(reader, "Name (optional): ")

	payload := map[string]interface{}{
		"organization_id":          orgID,
		"agent_job_description_id": jobID,
		"working_directory_id":     wdID,
	}
	if name != "" {
		payload["name"] = name
	}
	data, err := sendWSRequest("create_organization_position", payload)
	if err != nil {
		return 0, fmt.Errorf("failed to create organization position: %w", err)
	}

	var resp struct {
		OrganizationPosition struct {
			ID int `json:"id"`
		} `json:"organization_position"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return 0, fmt.Errorf("failed to parse response: %w", err)
	}
	if resp.Error != "" {
		return 0, fmt.Errorf("create organization position: %s", resp.Error)
	}
	fmt.Printf("Created organization position %d.\n", resp.OrganizationPosition.ID)
	return resp.OrganizationPosition.ID, nil
}
