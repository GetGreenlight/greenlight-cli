//go:build darwin || linux

package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"strings"
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

// workingOrgID returns the organization_id from ~/.greenlight/config, or "" if not set.
func workingOrgID() string {
	return readConfigValue("organization_id")
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
	case "org":
		runOrganizationOrg(args[1:])
	case "user":
		runOrganizationUser(args[1:])
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
		fmt.Fprintf(os.Stderr, "greenlight org: unknown entity %q\nRun 'greenlight org --help' for usage.\n", args[0])
		os.Exit(1)
	}
}

func printOrganizationUsage() {
	fmt.Fprintf(os.Stderr, `Usage: greenlight org <entity> <command> [flags]

Entities:
  org              Organizations
  user             Human users
  wd               Working directories
  job_description  Agent job descriptions
  position         Organization positions
  agent            AI agent instances
  ai_model         AI brain models (read-only)

Run 'greenlight org <entity> --help' for details.
`)
}

// =============================================================================
// org — organizations
// =============================================================================

func runOrganizationOrg(args []string) {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		fmt.Fprintf(os.Stderr, "Usage: greenlight org org <list|get|create|update|delete|use>\n")
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
		id := fs.String("id", "", "Organization ID")
		fs.Parse(args[1:])
		if *id == "" {
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
		fs.Parse(args[1:])
		if *name == "" {
			reader := bufio.NewReader(os.Stdin)
			*name = promptLine(reader, "Name: ")
		}
		if *name == "" {
			fmt.Fprintf(os.Stderr, "greenlight: name required\n")
			os.Exit(1)
		}
		data, err := sendWSRequest("create_organization", map[string]interface{}{"name": *name})
		if err != nil {
			fmt.Fprintf(os.Stderr, "greenlight: %v\n", err)
			os.Exit(1)
		}
		printJSON(data)
	case "update":
		fs := flag.NewFlagSet("org update", flag.ExitOnError)
		id := fs.String("id", "", "Organization ID")
		name := fs.String("name", "", "New name")
		fs.Parse(args[1:])
		if *id == "" || *name == "" {
			fmt.Fprintf(os.Stderr, "greenlight: --id and --name required\n")
			os.Exit(1)
		}
		data, err := sendWSRequest("update_organization", map[string]interface{}{"id": *id, "name": *name})
		if err != nil {
			fmt.Fprintf(os.Stderr, "greenlight: %v\n", err)
			os.Exit(1)
		}
		printJSON(data)
	case "delete":
		fs := flag.NewFlagSet("org delete", flag.ExitOnError)
		id := fs.String("id", "", "Organization ID")
		fs.Parse(args[1:])
		if *id == "" {
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
		id := fs.String("id", "", "Organization ID to set (omit for an interactive picker)")
		fs.Parse(args[1:])
		// No --id: render an interactive picker scoped to the current user's
		// organization memberships, with a "create new" escape hatch.
		if *id == "" {
			userID := readConfigValue("user_id")
			if userID == "" {
				fmt.Fprintf(os.Stderr, "greenlight: not registered (run 'greenlight register <email>' first)\n")
				os.Exit(1)
			}
			chosen, err := selectUserOrganization(userID)
			if err != nil {
				fmt.Fprintf(os.Stderr, "greenlight: %v\n", err)
				os.Exit(1)
			}
			*id = chosen
		}
		if err := writeConfigValue("organization_id", *id); err != nil {
			fmt.Fprintf(os.Stderr, "greenlight: failed to save organization_id: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Working organization set to %s.\n", *id)
	default:
		fmt.Fprintf(os.Stderr, "greenlight org org: unknown command %q\n", args[0])
		os.Exit(1)
	}
}

// =============================================================================
// user — human users (scoped to working organization)
// =============================================================================

func runOrganizationUser(args []string) {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		fmt.Fprintf(os.Stderr, "Usage: greenlight org user <list|get|create>\n")
		os.Exit(0)
	}
	switch args[0] {
	case "list":
		orgID := workingOrgID()
		if orgID == "" {
			fmt.Fprintf(os.Stderr, "greenlight: no working organization set (run 'greenlight org org use --id <ID>')\n")
			os.Exit(1)
		}
		data, err := sendWSRequest("list_human_users", map[string]interface{}{"organization_id": orgID})
		if err != nil {
			fmt.Fprintf(os.Stderr, "greenlight: %v\n", err)
			os.Exit(1)
		}
		printJSON(data)
	case "get":
		fs := flag.NewFlagSet("user get", flag.ExitOnError)
		id := fs.String("id", "", "User ID")
		fs.Parse(args[1:])
		if *id == "" {
			fmt.Fprintf(os.Stderr, "greenlight: --id required\n")
			os.Exit(1)
		}
		orgID := workingOrgID()
		if orgID == "" {
			fmt.Fprintf(os.Stderr, "greenlight: no working organization set (run 'greenlight org org use --id <ID>')\n")
			os.Exit(1)
		}
		data, err := sendWSRequest("get_human_user", map[string]interface{}{"id": *id, "organization_id": orgID})
		if err != nil {
			fmt.Fprintf(os.Stderr, "greenlight: %v\n", err)
			os.Exit(1)
		}
		printJSON(data)
	case "create":
		fs := flag.NewFlagSet("user create", flag.ExitOnError)
		name := fs.String("name", "", "User name")
		role := fs.String("role", "", "Membership role (owner|member, default member)")
		fs.Parse(args[1:])

		orgID := workingOrgID()
		if orgID == "" {
			fmt.Fprintf(os.Stderr, "greenlight: no working organization set (run 'greenlight org org use --id <ID>')\n")
			os.Exit(1)
		}

		if *name == "" {
			reader := bufio.NewReader(os.Stdin)
			*name = promptLine(reader, "Name: ")
		}
		if *name == "" {
			fmt.Fprintf(os.Stderr, "greenlight: name required\n")
			os.Exit(1)
		}

		payload := map[string]interface{}{
			"organization_id": orgID,
			"name":            *name,
		}
		if *role != "" {
			payload["role"] = *role
		}
		data, err := sendWSRequest("create_human_user", payload)
		if err != nil {
			fmt.Fprintf(os.Stderr, "greenlight: %v\n", err)
			os.Exit(1)
		}
		printJSON(data)
	default:
		fmt.Fprintf(os.Stderr, "greenlight org user: unknown command %q\n", args[0])
		os.Exit(1)
	}
}

// =============================================================================
// wd — working directories
// =============================================================================

func runOrganizationWD(args []string) {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		fmt.Fprintf(os.Stderr, "Usage: greenlight org wd <list|get|create|update|delete>\n")
		os.Exit(0)
	}
	switch args[0] {
	case "list":
		fs := flag.NewFlagSet("wd list", flag.ExitOnError)
		orgID := fs.String("org", "", "Organization ID (defaults to working org from config)")
		fs.Parse(args[1:])
		if *orgID == "" {
			*orgID = workingOrgID()
		}
		if *orgID == "" {
			fmt.Fprintf(os.Stderr, "greenlight: --org required (or set with 'greenlight org org use --id <ID>')\n")
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
		id := fs.String("id", "", "Working directory ID")
		fs.Parse(args[1:])
		if *id == "" {
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
		if orgID == "" {
			fmt.Fprintf(os.Stderr, "greenlight: no working organization set (run 'greenlight org org use --id <ID>')\n")
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
		id := fs.String("id", "", "Working directory ID")
		hostID := fs.String("host-id", "", "New host ID")
		dir := fs.String("dir", "", "New directory path")
		fs.Parse(args[1:])
		if *id == "" {
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
		id := fs.String("id", "", "Working directory ID")
		fs.Parse(args[1:])
		if *id == "" {
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
		fmt.Fprintf(os.Stderr, "greenlight org wd: unknown command %q\n", args[0])
		os.Exit(1)
	}
}

// =============================================================================
// job — agent job descriptions
// =============================================================================

func runOrganizationJob(args []string) {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		fmt.Fprintf(os.Stderr, "Usage: greenlight org job_description <list|get|create|update|delete>\n")
		os.Exit(0)
	}
	switch args[0] {
	case "list":
		fs := flag.NewFlagSet("job list", flag.ExitOnError)
		orgID := fs.String("org", "", "Organization ID (defaults to working org from config)")
		fs.Parse(args[1:])
		if *orgID == "" {
			*orgID = workingOrgID()
		}
		if *orgID == "" {
			fmt.Fprintf(os.Stderr, "greenlight: --org required (or set with 'greenlight org org use --id <ID>')\n")
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
		id := fs.String("id", "", "Job description ID")
		fs.Parse(args[1:])
		if *id == "" {
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
		if orgID == "" {
			fmt.Fprintf(os.Stderr, "greenlight: no working organization set (run 'greenlight org org use --id <ID>')\n")
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
		id := fs.String("id", "", "Job description ID")
		title := fs.String("title", "", "New title")
		mandate := fs.String("mandate", "", "New mandate")
		requiredSkills := fs.String("required-skills", "", "New required skills")
		priority := fs.Int("priority", 0, "New priority")
		fs.Parse(args[1:])
		if *id == "" || *title == "" {
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
		id := fs.String("id", "", "Job description ID")
		fs.Parse(args[1:])
		if *id == "" {
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
		fmt.Fprintf(os.Stderr, "greenlight org job_description: unknown command %q\n", args[0])
		os.Exit(1)
	}
}

// =============================================================================
// pos — organization positions
// =============================================================================

func runOrganizationPos(args []string) {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		fmt.Fprintf(os.Stderr, "Usage: greenlight org position <list|get|create|delete>\n")
		os.Exit(0)
	}
	switch args[0] {
	case "list":
		fs := flag.NewFlagSet("pos list", flag.ExitOnError)
		orgID := fs.String("org", "", "Organization ID (defaults to working org from config)")
		fs.Parse(args[1:])
		if *orgID == "" {
			*orgID = workingOrgID()
		}
		if *orgID == "" {
			fmt.Fprintf(os.Stderr, "greenlight: --org required (or set with 'greenlight org org use --id <ID>')\n")
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
		id := fs.String("id", "", "Position ID")
		fs.Parse(args[1:])
		if *id == "" {
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
		jobID := fs.String("job", "", "Agent job description ID (optional)")
		wdID := fs.String("wd", "", "Working directory ID")
		parentID := fs.String("parent", "", "Parent position ID (optional)")
		name := fs.String("name", "", "Human-readable name (optional)")
		fs.Parse(args[1:])

		orgID := workingOrgID()
		if orgID == "" {
			fmt.Fprintf(os.Stderr, "greenlight: no working organization set (run 'greenlight org org use --id <ID>')\n")
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

		if *wdID == "" {
			id, err := selectWorkingDirectory(orgID)
			if err != nil {
				fmt.Fprintf(os.Stderr, "greenlight: %v\n", err)
				os.Exit(1)
			}
			*wdID = id
		}

		if *jobID == "" {
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
		if *jobID != "" {
			payload["agent_job_description_id"] = *jobID
		}
		if *parentID != "" {
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
		id := fs.String("id", "", "Position ID")
		fs.Parse(args[1:])
		if *id == "" {
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
		fmt.Fprintf(os.Stderr, "greenlight org position: unknown command %q\n", args[0])
		os.Exit(1)
	}
}

// =============================================================================
// agent — AI agent instances
// =============================================================================

func runOrganizationAgent(args []string) {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		fmt.Fprintf(os.Stderr, "Usage: greenlight org agent <list|get|create|stop|delete>\n")
		os.Exit(0)
	}
	switch args[0] {
	case "list":
		fs := flag.NewFlagSet("agent list", flag.ExitOnError)
		orgID := fs.String("org", "", "Organization ID (defaults to working org from config)")
		fs.Parse(args[1:])
		if *orgID == "" {
			*orgID = workingOrgID()
		}
		if *orgID == "" {
			fmt.Fprintf(os.Stderr, "greenlight: --org required (or set with 'greenlight org org use --id <ID>')\n")
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
		id := fs.String("id", "", "Agent instance ID")
		fs.Parse(args[1:])
		if *id == "" {
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
		posID := fs.String("pos", "", "Organization position ID")
		modelID := fs.String("model", "", "AI brain model ID")
		harnessID := fs.Int("harness", 0, "Harness ID")
		name := fs.String("name", "", "Human-readable name")
		fs.Parse(args[1:])

		orgID := workingOrgID()
		if orgID == "" {
			fmt.Fprintf(os.Stderr, "greenlight: no working organization set (run 'greenlight org org use --id <ID>')\n")
			os.Exit(1)
		}

		reader := bufio.NewReader(os.Stdin)
		if *name == "" {
			*name = promptLine(reader, "Agent Name: ")
		}
		if *name == "" {
			fmt.Fprintf(os.Stderr, "greenlight: name required\n")
			os.Exit(1)
		}

		if *harnessID == 0 {
			id, err := selectHarness()
			if err != nil {
				fmt.Fprintf(os.Stderr, "greenlight: %v\n", err)
				os.Exit(1)
			}
			*harnessID = id
		}

		if *modelID == "" {
			id, err := selectAIBrainModel(orgID)
			if err != nil {
				fmt.Fprintf(os.Stderr, "greenlight: %v\n", err)
				os.Exit(1)
			}
			*modelID = id
		}

		if *posID == "" {
			id, err := selectOrganizationPosition(orgID)
			if err != nil {
				fmt.Fprintf(os.Stderr, "greenlight: %v\n", err)
				os.Exit(1)
			}
			*posID = id
		}

		// The daemon will spawn the harness with cwd set to the position's
		// working_directory. Make sure that directory exists on disk first —
		// prompt the user to create it if it doesn't, and bail otherwise.
		if err := ensureWorkingDirOnDisk(reader, *posID); err != nil {
			fmt.Fprintf(os.Stderr, "greenlight: %v\n", err)
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
	case "stop":
		fs := flag.NewFlagSet("agent stop", flag.ExitOnError)
		id := fs.String("id", "", "Agent instance ID")
		fs.Parse(args[1:])
		if *id == "" {
			fmt.Fprintf(os.Stderr, "greenlight: --id required\n")
			os.Exit(1)
		}
		// The daemon special-cases update_ai_agent_instance with retired_at:
		// it kills the tracked PID locally before forwarding the row update.
		now := time.Now().UTC().Format("2006-01-02T15:04:05Z")
		data, err := sendWSRequest("update_ai_agent_instance", map[string]interface{}{"id": *id, "retired_at": now})
		if err != nil {
			fmt.Fprintf(os.Stderr, "greenlight: %v\n", err)
			os.Exit(1)
		}
		printJSON(data)
	case "delete":
		fs := flag.NewFlagSet("agent delete", flag.ExitOnError)
		id := fs.String("id", "", "Agent instance ID")
		fs.Parse(args[1:])
		if *id == "" {
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
		fmt.Fprintf(os.Stderr, "greenlight org agent: unknown command %q\n", args[0])
		os.Exit(1)
	}
}

// =============================================================================
// model — AI brain models (read-only)
// =============================================================================

func runOrganizationModel(args []string) {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		fmt.Fprintf(os.Stderr, "Usage: greenlight org ai_model <list|get>\n")
		os.Exit(0)
	}
	switch args[0] {
	case "list":
		fs := flag.NewFlagSet("model list", flag.ExitOnError)
		orgID := fs.String("org", "", "Organization ID (defaults to working org from config)")
		fs.Parse(args[1:])
		if *orgID == "" {
			*orgID = workingOrgID()
		}
		if *orgID == "" {
			fmt.Fprintf(os.Stderr, "greenlight: --org required (or set with 'greenlight org org use --id <ID>')\n")
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
		id := fs.String("id", "", "Model ID")
		fs.Parse(args[1:])
		if *id == "" {
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
		fmt.Fprintf(os.Stderr, "greenlight org ai_model: unknown command %q\n", args[0])
		os.Exit(1)
	}
}

// ensureWorkingDirOnDisk resolves the position's working_directory path,
// checks if it exists locally, and prompts the user to create it if missing.
// Returns an error if the user declines or the lookup/mkdir fails.
func ensureWorkingDirOnDisk(reader *bufio.Reader, positionID string) error {
	posData, err := sendWSRequest("get_organization_position", map[string]interface{}{"id": positionID})
	if err != nil {
		return fmt.Errorf("failed to fetch organization_position: %w", err)
	}
	var posWrap struct {
		OrganizationPosition struct {
			WorkingDirectoryID string `json:"working_directory_id"`
		} `json:"organization_position"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(posData, &posWrap); err != nil {
		return fmt.Errorf("failed to parse organization_position response: %w", err)
	}
	if posWrap.Error != "" {
		return fmt.Errorf("organization_position lookup: %s", posWrap.Error)
	}
	if posWrap.OrganizationPosition.WorkingDirectoryID == "" {
		return fmt.Errorf("organization_position has no working_directory")
	}

	wdData, err := sendWSRequest("get_working_directory", map[string]interface{}{"id": posWrap.OrganizationPosition.WorkingDirectoryID})
	if err != nil {
		return fmt.Errorf("failed to fetch working_directory: %w", err)
	}
	var wdWrap struct {
		WorkingDirectory struct {
			DirectoryPath string `json:"directory_path"`
		} `json:"working_directory"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(wdData, &wdWrap); err != nil {
		return fmt.Errorf("failed to parse working_directory response: %w", err)
	}
	if wdWrap.Error != "" {
		return fmt.Errorf("working_directory lookup: %s", wdWrap.Error)
	}

	dir := wdWrap.WorkingDirectory.DirectoryPath
	if dir == "" {
		return fmt.Errorf("working_directory has no directory_path")
	}

	info, err := os.Stat(dir)
	if err == nil {
		if !info.IsDir() {
			return fmt.Errorf("%s exists but is not a directory", dir)
		}
		return nil
	}
	if !os.IsNotExist(err) {
		return fmt.Errorf("stat %s: %w", dir, err)
	}

	answer := promptLine(reader, fmt.Sprintf("Directory %q doesn't exist. Create it? (y/N): ", dir))
	if !strings.HasPrefix(strings.ToLower(answer), "y") {
		return fmt.Errorf("aborted: working directory does not exist")
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create %s: %w", dir, err)
	}
	fmt.Fprintf(os.Stderr, "Created %s\n", dir)
	return nil
}
