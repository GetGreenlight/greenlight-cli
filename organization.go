//go:build darwin || linux

package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"time"
)

// sendWSRequest connects to the daemon IPC socket and sends a ws_request,
// returning the raw JSON response from the server.
func sendWSRequest(msgType string, data map[string]interface{}) (json.RawMessage, error) {
	conn, err := net.DialTimeout("unix", daemonSockPath(), 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("cannot connect to daemon: %v\nRun 'greenlight daemon start' first", err)
	}
	defer conn.Close()

	var payload json.RawMessage
	if len(data) > 0 {
		payload, err = json.Marshal(data)
		if err != nil {
			return nil, err
		}
	}

	req := ipcRequest{
		Type:      "ws_request",
		WSMsgType: msgType,
		WSData:    payload,
	}
	reqBytes, _ := json.Marshal(req)
	reqBytes = append(reqBytes, '\n')
	if _, err := conn.Write(reqBytes); err != nil {
		return nil, fmt.Errorf("failed to send request: %v", err)
	}

	reader := bufio.NewReader(conn)
	line, err := reader.ReadBytes('\n')
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %v", err)
	}

	var resp ipcResponse
	if err := json.Unmarshal(line, &resp); err != nil {
		return nil, fmt.Errorf("invalid daemon response: %v", err)
	}
	if resp.Type == "error" {
		return nil, fmt.Errorf("%s", resp.Message)
	}
	return resp.Data, nil
}

// printJSON pretty-prints a JSON value to stdout.
func printJSON(v json.RawMessage) {
	var m interface{}
	if json.Unmarshal(v, &m) == nil {
		if pretty, err := json.MarshalIndent(m, "", "  "); err == nil {
			fmt.Println(string(pretty))
			return
		}
	}
	fmt.Println(string(v))
}

// workingOrgID returns the organization_id from ~/.greenlight/config, or 0 if not set.
func workingOrgID() int {
	v := readConfigValue("organization_id")
	if v == "" {
		return 0
	}
	var id int
	fmt.Sscanf(v, "%d", &id)
	return id
}

// =============================================================================
// runOrganization — entry point
// =============================================================================

func runOrganization(args []string) {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		printOrganizationUsage()
		os.Exit(0)
	}
	switch args[0] {
	case "organization":
		runOrganizationOrg(args[1:])
	case "wd":
		runOrganizationWD(args[1:])
	case "job_description":
		runOrganizationJob(args[1:])
	case "position":
		runOrganizationPos(args[1:])
	case "agent":
		runOrganizationAgent(args[1:])
	case "ai_model":
		runOrganizationModel(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "greenlight organization: unknown entity %q\nRun 'greenlight organization --help' for usage.\n", args[0])
		os.Exit(1)
	}
}

func printOrganizationUsage() {
	fmt.Fprintf(os.Stderr, `Usage: greenlight organization <entity> <command> [flags]

Entities:
  organization     Organizations
  wd               Working directories
  job_description  Agent job descriptions
  position         Organization positions
  agent            AI agent instances
  ai_model         AI brain models (read-only)

Run 'greenlight organization <entity> --help' for details.
`)
}

// =============================================================================
// org — organizations
// =============================================================================

func runOrganizationOrg(args []string) {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		fmt.Fprintf(os.Stderr, "Usage: greenlight organization organization <list|get|create|update|delete|use>\n")
		os.Exit(0)
	}
	switch args[0] {
	case "list":
		data, err := sendWSRequest("list_organizations", nil)
		if err != nil {
			fmt.Fprintf(os.Stderr, "greenlight: %v\n", err)
			os.Exit(1)
		}
		printJSON(data)
	case "get":
		fs := flag.NewFlagSet("org get", flag.ExitOnError)
		id := fs.Int("id", 0, "Organization ID")
		fs.Parse(args[1:])
		if *id == 0 {
			fmt.Fprintf(os.Stderr, "greenlight: --id required\n")
			os.Exit(1)
		}
		data, err := sendWSRequest("get_organization", map[string]interface{}{"id": *id})
		if err != nil {
			fmt.Fprintf(os.Stderr, "greenlight: %v\n", err)
			os.Exit(1)
		}
		printJSON(data)
	case "create":
		fs := flag.NewFlagSet("org create", flag.ExitOnError)
		name := fs.String("name", "", "Organization name")
		email := fs.String("email", "", "Recovery email (optional)")
		fs.Parse(args[1:])
		if *name == "" {
			reader := bufio.NewReader(os.Stdin)
			*name = promptLine(reader, "Name: ")
		}
		if *name == "" {
			fmt.Fprintf(os.Stderr, "greenlight: name required\n")
			os.Exit(1)
		}
		payload := map[string]interface{}{"name": *name}
		if *email != "" {
			payload["recovery_email"] = *email
		}
		data, err := sendWSRequest("create_organization", payload)
		if err != nil {
			fmt.Fprintf(os.Stderr, "greenlight: %v\n", err)
			os.Exit(1)
		}
		printJSON(data)
	case "update":
		fs := flag.NewFlagSet("org update", flag.ExitOnError)
		id := fs.Int("id", 0, "Organization ID")
		name := fs.String("name", "", "New name")
		email := fs.String("email", "", "Recovery email")
		fs.Parse(args[1:])
		if *id == 0 || *name == "" {
			fmt.Fprintf(os.Stderr, "greenlight: --id and --name required\n")
			os.Exit(1)
		}
		payload := map[string]interface{}{"id": *id, "name": *name}
		if *email != "" {
			payload["recovery_email"] = *email
		}
		data, err := sendWSRequest("update_organization", payload)
		if err != nil {
			fmt.Fprintf(os.Stderr, "greenlight: %v\n", err)
			os.Exit(1)
		}
		printJSON(data)
	case "delete":
		fs := flag.NewFlagSet("org delete", flag.ExitOnError)
		id := fs.Int("id", 0, "Organization ID")
		fs.Parse(args[1:])
		if *id == 0 {
			fmt.Fprintf(os.Stderr, "greenlight: --id required\n")
			os.Exit(1)
		}
		data, err := sendWSRequest("delete_organization", map[string]interface{}{"id": *id})
		if err != nil {
			fmt.Fprintf(os.Stderr, "greenlight: %v\n", err)
			os.Exit(1)
		}
		printJSON(data)
	case "use":
		fs := flag.NewFlagSet("org use", flag.ExitOnError)
		id := fs.Int("id", 0, "Organization ID to set as working organization")
		fs.Parse(args[1:])
		if *id == 0 {
			fmt.Fprintf(os.Stderr, "greenlight: --id required\n")
			os.Exit(1)
		}
		if err := writeConfigValue("organization_id", fmt.Sprintf("%d", *id)); err != nil {
			fmt.Fprintf(os.Stderr, "greenlight: failed to save organization_id: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Working organization set to %d.\n", *id)
	default:
		fmt.Fprintf(os.Stderr, "greenlight organization organization: unknown command %q\n", args[0])
		os.Exit(1)
	}
}

// =============================================================================
// wd — working directories
// =============================================================================

func runOrganizationWD(args []string) {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		fmt.Fprintf(os.Stderr, "Usage: greenlight organization wd <list|get|create|update|delete>\n")
		os.Exit(0)
	}
	switch args[0] {
	case "list":
		fs := flag.NewFlagSet("wd list", flag.ExitOnError)
		orgID := fs.Int("org", 0, "Organization ID (defaults to working org from config)")
		fs.Parse(args[1:])
		if *orgID == 0 {
			*orgID = workingOrgID()
		}
		if *orgID == 0 {
			fmt.Fprintf(os.Stderr, "greenlight: --org required (or set with 'greenlight organization organization use --id <ID>')\n")
			os.Exit(1)
		}
		data, err := sendWSRequest("list_working_directories", map[string]interface{}{"organization_id": *orgID})
		if err != nil {
			fmt.Fprintf(os.Stderr, "greenlight: %v\n", err)
			os.Exit(1)
		}
		printJSON(data)
	case "get":
		fs := flag.NewFlagSet("wd get", flag.ExitOnError)
		id := fs.Int("id", 0, "Working directory ID")
		fs.Parse(args[1:])
		if *id == 0 {
			fmt.Fprintf(os.Stderr, "greenlight: --id required\n")
			os.Exit(1)
		}
		data, err := sendWSRequest("get_working_directory", map[string]interface{}{"id": *id})
		if err != nil {
			fmt.Fprintf(os.Stderr, "greenlight: %v\n", err)
			os.Exit(1)
		}
		printJSON(data)
	case "create":
		fs := flag.NewFlagSet("wd create", flag.ExitOnError)
		hostID := fs.String("host-id", "", "Host ID (defaults to daemon's host)")
		dir := fs.String("dir", "", "Directory path (defaults to cwd)")
		fs.Parse(args[1:])

		orgID := workingOrgID()
		if orgID == 0 {
			fmt.Fprintf(os.Stderr, "greenlight: no working organization set (run 'greenlight organization organization use --id <ID>')\n")
			os.Exit(1)
		}

		reader := bufio.NewReader(os.Stdin)

		if *hostID == "" {
			*hostID = readConfigValue("host_id")
		}
		if *hostID == "" {
			*hostID = promptLine(reader, "Host ID: ")
		}
		if *hostID == "" {
			fmt.Fprintf(os.Stderr, "greenlight: host ID required (run 'greenlight daemon start' to enroll)\n")
			os.Exit(1)
		}
		if *dir == "" {
			cwd, _ := os.Getwd()
			*dir = promptWithDefault(reader, "Directory path", cwd)
		}

		payload := map[string]interface{}{
			"organization_id": orgID,
			"host_id":         *hostID,
			"directory_path":  *dir,
		}
		data, err := sendWSRequest("create_working_directory", payload)
		if err != nil {
			fmt.Fprintf(os.Stderr, "greenlight: %v\n", err)
			os.Exit(1)
		}
		printJSON(data)
	case "update":
		fs := flag.NewFlagSet("wd update", flag.ExitOnError)
		id := fs.Int("id", 0, "Working directory ID")
		hostID := fs.String("host-id", "", "New host ID")
		dir := fs.String("dir", "", "New directory path")
		fs.Parse(args[1:])
		if *id == 0 {
			fmt.Fprintf(os.Stderr, "greenlight: --id required\n")
			os.Exit(1)
		}
		payload := map[string]interface{}{"id": *id}
		if *hostID != "" {
			payload["host_id"] = *hostID
		}
		if *dir != "" {
			payload["directory_path"] = *dir
		}
		data, err := sendWSRequest("update_working_directory", payload)
		if err != nil {
			fmt.Fprintf(os.Stderr, "greenlight: %v\n", err)
			os.Exit(1)
		}
		printJSON(data)
	case "delete":
		fs := flag.NewFlagSet("wd delete", flag.ExitOnError)
		id := fs.Int("id", 0, "Working directory ID")
		fs.Parse(args[1:])
		if *id == 0 {
			fmt.Fprintf(os.Stderr, "greenlight: --id required\n")
			os.Exit(1)
		}
		data, err := sendWSRequest("delete_working_directory", map[string]interface{}{"id": *id})
		if err != nil {
			fmt.Fprintf(os.Stderr, "greenlight: %v\n", err)
			os.Exit(1)
		}
		printJSON(data)
	default:
		fmt.Fprintf(os.Stderr, "greenlight organization wd: unknown command %q\n", args[0])
		os.Exit(1)
	}
}

// =============================================================================
// job — agent job descriptions
// =============================================================================

func runOrganizationJob(args []string) {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		fmt.Fprintf(os.Stderr, "Usage: greenlight organization job_description <list|get|create|update|delete>\n")
		os.Exit(0)
	}
	switch args[0] {
	case "list":
		fs := flag.NewFlagSet("job list", flag.ExitOnError)
		orgID := fs.Int("org", 0, "Organization ID (defaults to working org from config)")
		fs.Parse(args[1:])
		if *orgID == 0 {
			*orgID = workingOrgID()
		}
		if *orgID == 0 {
			fmt.Fprintf(os.Stderr, "greenlight: --org required (or set with 'greenlight organization organization use --id <ID>')\n")
			os.Exit(1)
		}
		data, err := sendWSRequest("list_agent_job_descriptions", map[string]interface{}{"organization_id": *orgID})
		if err != nil {
			fmt.Fprintf(os.Stderr, "greenlight: %v\n", err)
			os.Exit(1)
		}
		printJSON(data)
	case "get":
		fs := flag.NewFlagSet("job get", flag.ExitOnError)
		id := fs.Int("id", 0, "Job description ID")
		fs.Parse(args[1:])
		if *id == 0 {
			fmt.Fprintf(os.Stderr, "greenlight: --id required\n")
			os.Exit(1)
		}
		data, err := sendWSRequest("get_agent_job_description", map[string]interface{}{"id": *id})
		if err != nil {
			fmt.Fprintf(os.Stderr, "greenlight: %v\n", err)
			os.Exit(1)
		}
		printJSON(data)
	case "create":
		fs := flag.NewFlagSet("job create", flag.ExitOnError)
		title := fs.String("title", "", "Job title (e.g. \"Gardener\")")
		mandate := fs.String("mandate", "", "Mandate / responsibilities (markdown)")
		requiredSkills := fs.String("required-skills", "", "Required skills, tools, or knowledge (markdown)")
		priority := fs.Int("priority", 5, "Priority 1-10 (1=highest)")
		fs.Parse(args[1:])

		orgID := workingOrgID()
		if orgID == 0 {
			fmt.Fprintf(os.Stderr, "greenlight: no working organization set (run 'greenlight organization organization use --id <ID>')\n")
			os.Exit(1)
		}

		reader := bufio.NewReader(os.Stdin)
		if *title == "" {
			*title = promptLine(reader, "Title: ")
		}
		if *title == "" {
			fmt.Fprintf(os.Stderr, "greenlight: title required\n")
			os.Exit(1)
		}

		payload := map[string]interface{}{
			"organization_id": orgID,
			"title":           *title,
			"priority":        *priority,
		}
		if *mandate != "" {
			payload["mandate"] = *mandate
		}
		if *requiredSkills != "" {
			payload["required_skills"] = *requiredSkills
		}
		data, err := sendWSRequest("create_agent_job_description", payload)
		if err != nil {
			fmt.Fprintf(os.Stderr, "greenlight: %v\n", err)
			os.Exit(1)
		}
		printJSON(data)
	case "update":
		fs := flag.NewFlagSet("job update", flag.ExitOnError)
		id := fs.Int("id", 0, "Job description ID")
		title := fs.String("title", "", "New title")
		mandate := fs.String("mandate", "", "New mandate")
		requiredSkills := fs.String("required-skills", "", "New required skills")
		priority := fs.Int("priority", 0, "New priority")
		fs.Parse(args[1:])
		if *id == 0 || *title == "" {
			fmt.Fprintf(os.Stderr, "greenlight: --id and --title required\n")
			os.Exit(1)
		}
		payload := map[string]interface{}{"id": *id, "title": *title}
		if *mandate != "" {
			payload["mandate"] = *mandate
		}
		if *requiredSkills != "" {
			payload["required_skills"] = *requiredSkills
		}
		if *priority != 0 {
			payload["priority"] = *priority
		}
		data, err := sendWSRequest("update_agent_job_description", payload)
		if err != nil {
			fmt.Fprintf(os.Stderr, "greenlight: %v\n", err)
			os.Exit(1)
		}
		printJSON(data)
	case "delete":
		fs := flag.NewFlagSet("job delete", flag.ExitOnError)
		id := fs.Int("id", 0, "Job description ID")
		fs.Parse(args[1:])
		if *id == 0 {
			fmt.Fprintf(os.Stderr, "greenlight: --id required\n")
			os.Exit(1)
		}
		data, err := sendWSRequest("delete_agent_job_description", map[string]interface{}{"id": *id})
		if err != nil {
			fmt.Fprintf(os.Stderr, "greenlight: %v\n", err)
			os.Exit(1)
		}
		printJSON(data)
	default:
		fmt.Fprintf(os.Stderr, "greenlight organization job_description: unknown command %q\n", args[0])
		os.Exit(1)
	}
}

// =============================================================================
// pos — organization positions
// =============================================================================

func runOrganizationPos(args []string) {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		fmt.Fprintf(os.Stderr, "Usage: greenlight organization position <list|get|create|delete>\n")
		os.Exit(0)
	}
	switch args[0] {
	case "list":
		fs := flag.NewFlagSet("pos list", flag.ExitOnError)
		orgID := fs.Int("org", 0, "Organization ID (defaults to working org from config)")
		fs.Parse(args[1:])
		if *orgID == 0 {
			*orgID = workingOrgID()
		}
		if *orgID == 0 {
			fmt.Fprintf(os.Stderr, "greenlight: --org required (or set with 'greenlight organization organization use --id <ID>')\n")
			os.Exit(1)
		}
		data, err := sendWSRequest("list_organization_positions", map[string]interface{}{"organization_id": *orgID})
		if err != nil {
			fmt.Fprintf(os.Stderr, "greenlight: %v\n", err)
			os.Exit(1)
		}
		printJSON(data)
	case "get":
		fs := flag.NewFlagSet("pos get", flag.ExitOnError)
		id := fs.Int("id", 0, "Position ID")
		fs.Parse(args[1:])
		if *id == 0 {
			fmt.Fprintf(os.Stderr, "greenlight: --id required\n")
			os.Exit(1)
		}
		data, err := sendWSRequest("get_organization_position", map[string]interface{}{"id": *id})
		if err != nil {
			fmt.Fprintf(os.Stderr, "greenlight: %v\n", err)
			os.Exit(1)
		}
		printJSON(data)
	case "create":
		fs := flag.NewFlagSet("pos create", flag.ExitOnError)
		jobID := fs.Int("job", 0, "Agent job description ID (optional)")
		wdID := fs.Int("wd", 0, "Working directory ID")
		parentID := fs.Int("parent", 0, "Parent position ID (optional)")
		name := fs.String("name", "", "Human-readable name (optional)")
		fs.Parse(args[1:])

		orgID := workingOrgID()
		if orgID == 0 {
			fmt.Fprintf(os.Stderr, "greenlight: no working organization set (run 'greenlight organization organization use --id <ID>')\n")
			os.Exit(1)
		}

		reader := bufio.NewReader(os.Stdin)
		if *name == "" {
			*name = promptLine(reader, "Name: ")
		}
		if *name == "" {
			fmt.Fprintf(os.Stderr, "greenlight: name required\n")
			os.Exit(1)
		}

		if *wdID == 0 {
			id, err := selectWorkingDirectory(orgID)
			if err != nil {
				fmt.Fprintf(os.Stderr, "greenlight: %v\n", err)
				os.Exit(1)
			}
			*wdID = id
		}

		if *jobID == 0 {
			id, err := selectOptionalAgentJobDescription(orgID)
			if err != nil {
				fmt.Fprintf(os.Stderr, "greenlight: %v\n", err)
				os.Exit(1)
			}
			*jobID = id
		}

		payload := map[string]interface{}{
			"organization_id":      orgID,
			"working_directory_id": *wdID,
		}
		if *name != "" {
			payload["name"] = *name
		}
		if *jobID != 0 {
			payload["agent_job_description_id"] = *jobID
		}
		if *parentID != 0 {
			payload["parent_position_id"] = *parentID
		}
		data, err := sendWSRequest("create_organization_position", payload)
		if err != nil {
			fmt.Fprintf(os.Stderr, "greenlight: %v\n", err)
			os.Exit(1)
		}
		printJSON(data)
	case "delete":
		fs := flag.NewFlagSet("pos delete", flag.ExitOnError)
		id := fs.Int("id", 0, "Position ID")
		fs.Parse(args[1:])
		if *id == 0 {
			fmt.Fprintf(os.Stderr, "greenlight: --id required\n")
			os.Exit(1)
		}
		data, err := sendWSRequest("delete_organization_position", map[string]interface{}{"id": *id})
		if err != nil {
			fmt.Fprintf(os.Stderr, "greenlight: %v\n", err)
			os.Exit(1)
		}
		printJSON(data)
	default:
		fmt.Fprintf(os.Stderr, "greenlight organization position: unknown command %q\n", args[0])
		os.Exit(1)
	}
}

// =============================================================================
// agent — AI agent instances
// =============================================================================

func runOrganizationAgent(args []string) {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		fmt.Fprintf(os.Stderr, "Usage: greenlight organization agent <list|get|create|retire|delete>\n")
		os.Exit(0)
	}
	switch args[0] {
	case "list":
		fs := flag.NewFlagSet("agent list", flag.ExitOnError)
		orgID := fs.Int("org", 0, "Organization ID (defaults to working org from config)")
		fs.Parse(args[1:])
		if *orgID == 0 {
			*orgID = workingOrgID()
		}
		if *orgID == 0 {
			fmt.Fprintf(os.Stderr, "greenlight: --org required (or set with 'greenlight organization organization use --id <ID>')\n")
			os.Exit(1)
		}
		data, err := sendWSRequest("list_ai_agent_instances", map[string]interface{}{"organization_id": *orgID})
		if err != nil {
			fmt.Fprintf(os.Stderr, "greenlight: %v\n", err)
			os.Exit(1)
		}
		printJSON(data)
	case "get":
		fs := flag.NewFlagSet("agent get", flag.ExitOnError)
		id := fs.Int("id", 0, "Agent instance ID")
		fs.Parse(args[1:])
		if *id == 0 {
			fmt.Fprintf(os.Stderr, "greenlight: --id required\n")
			os.Exit(1)
		}
		data, err := sendWSRequest("get_ai_agent_instance", map[string]interface{}{"id": *id})
		if err != nil {
			fmt.Fprintf(os.Stderr, "greenlight: %v\n", err)
			os.Exit(1)
		}
		printJSON(data)
	case "create":
		fs := flag.NewFlagSet("agent create", flag.ExitOnError)
		posID := fs.Int("pos", 0, "Organization position ID")
		modelID := fs.Int("model", 0, "AI brain model ID")
		harnessID := fs.Int("harness", 0, "Harness ID")
		name := fs.String("name", "", "Human-readable name")
		fs.Parse(args[1:])

		orgID := workingOrgID()
		if orgID == 0 {
			fmt.Fprintf(os.Stderr, "greenlight: no working organization set (run 'greenlight organization organization use --id <ID>')\n")
			os.Exit(1)
		}

		if *posID == 0 {
			id, err := selectOrganizationPosition(orgID)
			if err != nil {
				fmt.Fprintf(os.Stderr, "greenlight: %v\n", err)
				os.Exit(1)
			}
			*posID = id
		}

		if *modelID == 0 {
			id, err := selectAIBrainModel(orgID)
			if err != nil {
				fmt.Fprintf(os.Stderr, "greenlight: %v\n", err)
				os.Exit(1)
			}
			*modelID = id
		}

		if *harnessID == 0 {
			id, err := selectHarness()
			if err != nil {
				fmt.Fprintf(os.Stderr, "greenlight: %v\n", err)
				os.Exit(1)
			}
			*harnessID = id
		}

		reader := bufio.NewReader(os.Stdin)
		if *name == "" {
			*name = promptLine(reader, "Name: ")
		}
		if *name == "" {
			fmt.Fprintf(os.Stderr, "greenlight: name required\n")
			os.Exit(1)
		}

		payload := map[string]interface{}{
			"organization_id":          orgID,
			"organization_position_id": *posID,
			"ai_brain_model_id":        *modelID,
			"harness_id":               *harnessID,
			"name":                     *name,
		}
		data, err := sendWSRequest("create_ai_agent_instance", payload)
		if err != nil {
			fmt.Fprintf(os.Stderr, "greenlight: %v\n", err)
			os.Exit(1)
		}
		printJSON(data)
	case "retire":
		fs := flag.NewFlagSet("agent retire", flag.ExitOnError)
		id := fs.Int("id", 0, "Agent instance ID")
		fs.Parse(args[1:])
		if *id == 0 {
			fmt.Fprintf(os.Stderr, "greenlight: --id required\n")
			os.Exit(1)
		}
		now := time.Now().UTC().Format("2006-01-02T15:04:05Z")
		data, err := sendWSRequest("update_ai_agent_instance", map[string]interface{}{"id": *id, "retired_at": now})
		if err != nil {
			fmt.Fprintf(os.Stderr, "greenlight: %v\n", err)
			os.Exit(1)
		}
		printJSON(data)
	case "delete":
		fs := flag.NewFlagSet("agent delete", flag.ExitOnError)
		id := fs.Int("id", 0, "Agent instance ID")
		fs.Parse(args[1:])
		if *id == 0 {
			fmt.Fprintf(os.Stderr, "greenlight: --id required\n")
			os.Exit(1)
		}
		data, err := sendWSRequest("delete_ai_agent_instance", map[string]interface{}{"id": *id})
		if err != nil {
			fmt.Fprintf(os.Stderr, "greenlight: %v\n", err)
			os.Exit(1)
		}
		printJSON(data)
	default:
		fmt.Fprintf(os.Stderr, "greenlight organization agent: unknown command %q\n", args[0])
		os.Exit(1)
	}
}

// =============================================================================
// model — AI brain models (read-only)
// =============================================================================

func runOrganizationModel(args []string) {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		fmt.Fprintf(os.Stderr, "Usage: greenlight organization ai_model <list|get>\n")
		os.Exit(0)
	}
	switch args[0] {
	case "list":
		fs := flag.NewFlagSet("model list", flag.ExitOnError)
		orgID := fs.Int("org", 0, "Organization ID (defaults to working org from config)")
		fs.Parse(args[1:])
		if *orgID == 0 {
			*orgID = workingOrgID()
		}
		if *orgID == 0 {
			fmt.Fprintf(os.Stderr, "greenlight: --org required (or set with 'greenlight organization organization use --id <ID>')\n")
			os.Exit(1)
		}
		data, err := sendWSRequest("list_ai_brain_models", map[string]interface{}{"organization_id": *orgID})
		if err != nil {
			fmt.Fprintf(os.Stderr, "greenlight: %v\n", err)
			os.Exit(1)
		}
		printJSON(data)
	case "get":
		fs := flag.NewFlagSet("model get", flag.ExitOnError)
		id := fs.Int("id", 0, "Model ID")
		fs.Parse(args[1:])
		if *id == 0 {
			fmt.Fprintf(os.Stderr, "greenlight: --id required\n")
			os.Exit(1)
		}
		data, err := sendWSRequest("get_ai_brain_model", map[string]interface{}{"id": *id})
		if err != nil {
			fmt.Fprintf(os.Stderr, "greenlight: %v\n", err)
			os.Exit(1)
		}
		printJSON(data)
	default:
		fmt.Fprintf(os.Stderr, "greenlight organization ai_model: unknown command %q\n", args[0])
		os.Exit(1)
	}
}
