//go:build darwin || linux

package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"

	"github.com/manifoldco/promptui"
)

// selectItem holds a display label and the underlying ID. The ID type is
// generic so we can share the picker between INTEGER PKs (harnesses,
// organization_positions) and TEXT/UUID PKs (organizations, agents, etc.).
type selectItem[T any] struct {
	Label string
	ID    T
}

// selectFromList renders an interactive selector and returns the chosen ID.
func selectFromList[T any](label string, items []selectItem[T]) (T, error) {
	var zero T
	if len(items) == 0 {
		return zero, fmt.Errorf("no items to select from")
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
		return zero, err
	}
	return items[idx].ID, nil
}

// =============================================================================
// selectHarness — harnesses.id is still INTEGER
// =============================================================================

func selectHarness() (int, error) {
	data, err := sendWSRequest("list_harnesses", nil)
	if err != nil {
		return 0, fmt.Errorf("failed to fetch harnesses: %w", err)
	}

	var resp struct {
		Harnesses []struct {
			ID   int    `json:"id"`
			Name string `json:"name"`
		} `json:"harnesses"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return 0, fmt.Errorf("failed to parse harnesses: %w", err)
	}

	if len(resp.Harnesses) == 0 {
		return 0, fmt.Errorf("no harnesses found — seed the database first")
	}

	items := make([]selectItem[int], len(resp.Harnesses))
	for i, h := range resp.Harnesses {
		items[i] = selectItem[int]{Label: h.Name, ID: h.ID}
	}
	return selectFromList("Harness", items)
}

// =============================================================================
// selectAIBrainModel
// =============================================================================

func selectAIBrainModel(orgID string) (string, error) {
	payload := map[string]interface{}{}
	if orgID != "" {
		payload["organization_id"] = orgID
	}
	data, err := sendWSRequest("list_ai_brain_models", payload)
	if err != nil {
		return "", fmt.Errorf("failed to fetch AI brain models: %w", err)
	}

	var resp struct {
		AIBrainModels []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"ai_brain_models"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", fmt.Errorf("failed to parse AI brain models: %w", err)
	}

	if len(resp.AIBrainModels) == 0 {
		return "", fmt.Errorf("no AI brain models found — seed the database first")
	}

	items := make([]selectItem[string], len(resp.AIBrainModels))
	for i, m := range resp.AIBrainModels {
		items[i] = selectItem[string]{Label: m.Name, ID: m.ID}
	}
	return selectFromList("AI brain model", items)
}

// =============================================================================
// selectUserOrganization — picker scoped to one human user's memberships
// =============================================================================

// selectUserOrganization fetches every organization the given user belongs to
// (joined via organization_human_users) and renders an interactive picker. A
// "→ Create new…" row at the bottom drops the user into the create flow,
// which both creates the org and adds the user as its owner in one step.
func selectUserOrganization(userID string) (string, error) {
	if userID == "" {
		return "", fmt.Errorf("user_id required")
	}
	data, err := sendWSRequest("list_organizations", map[string]interface{}{"for_human_user_id": userID})
	if err != nil {
		return "", fmt.Errorf("failed to fetch organizations: %w", err)
	}

	var resp struct {
		Organizations []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"organizations"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", fmt.Errorf("failed to parse organizations: %w", err)
	}
	if resp.Error != "" {
		return "", fmt.Errorf("list organizations: %s", resp.Error)
	}

	if len(resp.Organizations) == 0 {
		fmt.Println("You don't belong to any organizations yet. Let's create one.")
		return createUserOrganizationInteractive(userID)
	}

	items := make([]selectItem[string], len(resp.Organizations))
	for i, o := range resp.Organizations {
		items[i] = selectItem[string]{Label: o.Name, ID: o.ID}
	}
	items = append(items, selectItem[string]{Label: "→ Create new…", ID: sentinelCreateNew})

	id, err := selectFromList("Organization", items)
	if err != nil {
		return "", err
	}
	if id == sentinelCreateNew {
		return createUserOrganizationInteractive(userID)
	}
	return id, nil
}

// createUserOrganizationInteractive prompts for an org name, creates the row,
// and adds the given user as its owner.
func createUserOrganizationInteractive(userID string) (string, error) {
	if userID == "" {
		return "", fmt.Errorf("user_id required")
	}
	reader := bufio.NewReader(os.Stdin)
	name := promptLine(reader, "Name: ")
	if name == "" {
		return "", fmt.Errorf("name required")
	}

	createData, err := sendWSRequest("create_organization", map[string]interface{}{"name": name})
	if err != nil {
		return "", fmt.Errorf("failed to create organization: %w", err)
	}
	var createResp struct {
		Organization struct {
			ID string `json:"id"`
		} `json:"organization"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(createData, &createResp); err != nil {
		return "", fmt.Errorf("failed to parse create response: %w", err)
	}
	if createResp.Error != "" {
		return "", fmt.Errorf("create organization: %s", createResp.Error)
	}
	orgID := createResp.Organization.ID

	addData, err := sendWSRequest("add_organization_member", map[string]interface{}{
		"organization_id": orgID,
		"human_user_id":   userID,
		"role":            "owner",
	})
	if err != nil {
		return "", fmt.Errorf("created organization %s but failed to add owner membership: %w", orgID, err)
	}
	var addResp struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(addData, &addResp); err == nil && addResp.Error != "" {
		return "", fmt.Errorf("created organization %s but failed to add owner membership: %s", orgID, addResp.Error)
	}

	fmt.Printf("Created organization %s.\n", orgID)
	return orgID, nil
}

// =============================================================================
// selectAgentJobDescription
// =============================================================================

// sentinelCreateNew is returned by the optional/required selectors to indicate
// that the user picked the "→ Create new…" option. It can never collide with a
// real UUID since UUIDs are 36 chars long with dashes in fixed positions.
const sentinelCreateNew = "__create_new__"

func selectAgentJobDescription(orgID string) (string, error) {
	payload := map[string]interface{}{}
	if orgID != "" {
		payload["organization_id"] = orgID
	}
	data, err := sendWSRequest("list_agent_job_descriptions", payload)
	if err != nil {
		return "", fmt.Errorf("failed to fetch job descriptions: %w", err)
	}

	var resp struct {
		AgentJobDescriptions []struct {
			ID    string `json:"id"`
			Title string `json:"title"`
		} `json:"agent_job_descriptions"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", fmt.Errorf("failed to parse job descriptions: %w", err)
	}

	if len(resp.AgentJobDescriptions) == 0 {
		fmt.Println("No job descriptions found. Let's create one.")
		return createAgentJobDescriptionInteractive(orgID)
	}

	items := make([]selectItem[string], len(resp.AgentJobDescriptions))
	for i, j := range resp.AgentJobDescriptions {
		items[i] = selectItem[string]{Label: j.Title, ID: j.ID}
	}
	items = append(items, selectItem[string]{Label: "→ Create new…", ID: sentinelCreateNew})

	id, err := selectFromList("Agent job description", items)
	if err != nil {
		return "", err
	}
	if id == sentinelCreateNew {
		return createAgentJobDescriptionInteractive(orgID)
	}
	return id, nil
}

// selectOptionalAgentJobDescription is like selectAgentJobDescription but adds a
// "→ Skip" option. Returns "" when the user chooses to skip.
func selectOptionalAgentJobDescription(orgID string) (string, error) {
	payload := map[string]interface{}{}
	if orgID != "" {
		payload["organization_id"] = orgID
	}
	data, err := sendWSRequest("list_agent_job_descriptions", payload)
	if err != nil {
		return "", fmt.Errorf("failed to fetch job descriptions: %w", err)
	}

	var resp struct {
		AgentJobDescriptions []struct {
			ID    string `json:"id"`
			Title string `json:"title"`
		} `json:"agent_job_descriptions"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", fmt.Errorf("failed to parse job descriptions: %w", err)
	}

	items := make([]selectItem[string], len(resp.AgentJobDescriptions))
	for i, j := range resp.AgentJobDescriptions {
		items[i] = selectItem[string]{Label: j.Title, ID: j.ID}
	}
	items = append(items, selectItem[string]{Label: "→ Create new…", ID: sentinelCreateNew})
	items = append(items, selectItem[string]{Label: "→ Skip (none)", ID: ""})

	id, err := selectFromList("Agent job description (optional)", items)
	if err != nil {
		return "", err
	}
	if id == sentinelCreateNew {
		return createAgentJobDescriptionInteractive(orgID)
	}
	return id, nil // "" = skip
}

func createAgentJobDescriptionInteractive(orgID string) (string, error) {
	if orgID == "" {
		orgID = workingOrgID()
	}
	if orgID == "" {
		return "", fmt.Errorf("no working organization set (run 'greenlight org org use --id <ID>')")
	}

	reader := bufio.NewReader(os.Stdin)
	title := promptLine(reader, "Title: ")
	if title == "" {
		return "", fmt.Errorf("title required")
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
		return "", fmt.Errorf("failed to create job description: %w", err)
	}

	var resp struct {
		AgentJobDescription struct {
			ID string `json:"id"`
		} `json:"agent_job_description"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", fmt.Errorf("failed to parse response: %w", err)
	}
	if resp.Error != "" {
		return "", fmt.Errorf("create job description: %s", resp.Error)
	}
	fmt.Printf("Created job description %s.\n", resp.AgentJobDescription.ID)
	return resp.AgentJobDescription.ID, nil
}

// =============================================================================
// selectWorkingDirectory
// =============================================================================

func selectWorkingDirectory(orgID string) (string, error) {
	return selectWorkingDirectoryForPosition(orgID, "")
}

// selectWorkingDirectoryForPosition is like selectWorkingDirectory, but when
// the user chooses "Create new…" the path prompt defaults to a subdirectory
// named after the (snake_cased) position — so a position "Head Gardener"
// defaults its wd to $HOME/greenlight_agents/head_gardener.
func selectWorkingDirectoryForPosition(orgID, positionName string) (string, error) {
	payload := map[string]interface{}{}
	if orgID != "" {
		payload["organization_id"] = orgID
	}
	data, err := sendWSRequest("list_working_directories", payload)
	if err != nil {
		return "", fmt.Errorf("failed to fetch working directories: %w", err)
	}

	var resp struct {
		WorkingDirectories []struct {
			ID            string `json:"id"`
			Hostname      string `json:"hostname"`
			DirectoryPath string `json:"directory_path"`
		} `json:"working_directories"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", fmt.Errorf("failed to parse working directories: %w", err)
	}

	if len(resp.WorkingDirectories) == 0 {
		fmt.Println("No working directories found. Let's create one.")
		return createWorkingDirectoryInteractive(orgID, positionName)
	}

	items := make([]selectItem[string], len(resp.WorkingDirectories))
	for i, w := range resp.WorkingDirectories {
		label := w.Hostname + ":" + w.DirectoryPath
		if w.Hostname == "" && w.DirectoryPath == "" {
			label = "(unnamed)"
		}
		items[i] = selectItem[string]{Label: label, ID: w.ID}
	}
	items = append(items, selectItem[string]{Label: "→ Create new…", ID: sentinelCreateNew})

	id, err := selectFromList("Working directory", items)
	if err != nil {
		return "", err
	}
	if id == sentinelCreateNew {
		return createWorkingDirectoryInteractive(orgID, positionName)
	}
	return id, nil
}

func createWorkingDirectoryInteractive(orgID, positionName string) (string, error) {
	if orgID == "" {
		orgID = workingOrgID()
	}
	if orgID == "" {
		return "", fmt.Errorf("no working organization set (run 'greenlight org org use --id <ID>')")
	}

	hostID := readConfigValue("host_id")
	if hostID == "" {
		return "", fmt.Errorf("host ID not found — run 'greenlight daemon start' to enroll this host")
	}

	reader := bufio.NewReader(os.Stdin)
	dir := promptWithDefault(reader, "Directory path", defaultAgentWorkingDirFor(positionName))

	payload := map[string]interface{}{
		"organization_id": orgID,
		"host_id":         hostID,
		"directory_path":  dir,
	}
	data, err := sendWSRequest("create_working_directory", payload)
	if err != nil {
		return "", fmt.Errorf("failed to create working directory: %w", err)
	}

	var resp struct {
		WorkingDirectory struct {
			ID string `json:"id"`
		} `json:"working_directory"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", fmt.Errorf("failed to parse response: %w", err)
	}
	if resp.Error != "" {
		return "", fmt.Errorf("create working directory: %s", resp.Error)
	}
	fmt.Printf("Created working directory %s.\n", resp.WorkingDirectory.ID)
	return resp.WorkingDirectory.ID, nil
}

// =============================================================================
// selectOrganizationPosition
// =============================================================================

func selectOrganizationPosition(orgID string) (string, error) {
	payload := map[string]interface{}{}
	if orgID != "" {
		payload["organization_id"] = orgID
	}
	data, err := sendWSRequest("list_organization_positions", payload)
	if err != nil {
		return "", fmt.Errorf("failed to fetch organization positions: %w", err)
	}

	var resp struct {
		OrganizationPositions []struct {
			ID                    string `json:"id"`
			Name                  string `json:"name"`
			WorkingDirectoryID    string `json:"working_directory_id"`
			AgentJobDescriptionID string `json:"agent_job_description_id"`
		} `json:"organization_positions"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", fmt.Errorf("failed to parse organization positions: %w", err)
	}

	if len(resp.OrganizationPositions) == 0 {
		fmt.Println("No organization positions found. Let's create one.")
		return createOrganizationPositionInteractive(orgID)
	}

	items := make([]selectItem[string], len(resp.OrganizationPositions))
	for i, p := range resp.OrganizationPositions {
		label := p.Name
		if label == "" {
			label = "(unnamed position)"
		}
		items[i] = selectItem[string]{Label: label, ID: p.ID}
	}
	items = append(items, selectItem[string]{Label: "→ Create new…", ID: sentinelCreateNew})

	id, err := selectFromList("Organization position", items)
	if err != nil {
		return "", err
	}
	if id == sentinelCreateNew {
		return createOrganizationPositionInteractive(orgID)
	}
	return id, nil
}

func createOrganizationPositionInteractive(orgID string) (string, error) {
	if orgID == "" {
		orgID = workingOrgID()
	}
	if orgID == "" {
		return "", fmt.Errorf("no working organization set (run 'greenlight org org use --id <ID>')")
	}

	reader := bufio.NewReader(os.Stdin)
	rawName := promptLine(reader, "Name: ")
	if rawName == "" {
		return "", fmt.Errorf("name required")
	}
	name := toSnakeCase(rawName)
	if name == "" {
		return "", fmt.Errorf("name must contain at least one alphanumeric character")
	}
	if name != rawName {
		fmt.Printf("Position name stored as %q (snake_case).\n", name)
	}

	wdID, err := selectWorkingDirectoryForPosition(orgID, name)
	if err != nil {
		return "", err
	}

	jobID, err := selectOptionalAgentJobDescription(orgID)
	if err != nil {
		return "", err
	}

	payload := map[string]interface{}{
		"organization_id":      orgID,
		"working_directory_id": wdID,
	}
	if name != "" {
		payload["name"] = name
	}
	if jobID != "" {
		payload["agent_job_description_id"] = jobID
	}
	data, err := sendWSRequest("create_organization_position", payload)
	if err != nil {
		return "", fmt.Errorf("failed to create organization position: %w", err)
	}

	var resp struct {
		OrganizationPosition struct {
			ID string `json:"id"`
		} `json:"organization_position"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", fmt.Errorf("failed to parse response: %w", err)
	}
	if resp.Error != "" {
		return "", fmt.Errorf("create organization position: %s", resp.Error)
	}
	fmt.Printf("Created organization position %s.\n", resp.OrganizationPosition.ID)
	return resp.OrganizationPosition.ID, nil
}
