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

// =============================================================================
// runHousehold — entry point
// =============================================================================

func runHousehold(args []string) {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		printHouseholdUsage()
		os.Exit(0)
	}
	switch args[0] {
	case "org":
		runHouseholdOrg(args[1:])
	case "wd":
		runHouseholdWD(args[1:])
	case "job":
		runHouseholdJob(args[1:])
	case "pos":
		runHouseholdPos(args[1:])
	case "agent":
		runHouseholdAgent(args[1:])
	case "model":
		runHouseholdModel(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "greenlight household: unknown entity %q\nRun 'greenlight household --help' for usage.\n", args[0])
		os.Exit(1)
	}
}

func printHouseholdUsage() {
	fmt.Fprintf(os.Stderr, `Usage: greenlight household <entity> <command> [flags]

Entities:
  org      Organizations
  wd       Working directories
  job      Agent job descriptions
  pos      Organization positions
  agent    AI agent instances
  model    AI brain models (read-only)

Run 'greenlight household <entity> --help' for details.
`)
}

// =============================================================================
// org — organizations
// =============================================================================

func runHouseholdOrg(args []string) {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		fmt.Fprintf(os.Stderr, "Usage: greenlight household org <list|get|create|update|delete>\n")
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
	default:
		fmt.Fprintf(os.Stderr, "greenlight household org: unknown command %q\n", args[0])
		os.Exit(1)
	}
}

// =============================================================================
// wd — working directories
// =============================================================================

func runHouseholdWD(args []string) {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		fmt.Fprintf(os.Stderr, "Usage: greenlight household wd <list|get|create|update|delete>\n")
		os.Exit(0)
	}
	switch args[0] {
	case "list":
		fs := flag.NewFlagSet("wd list", flag.ExitOnError)
		orgID := fs.Int("org", 0, "Filter by organization ID")
		fs.Parse(args[1:])
		payload := map[string]interface{}{}
		if *orgID != 0 {
			payload["organization_id"] = *orgID
		}
		data, err := sendWSRequest("list_working_directories", payload)
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
		orgID := fs.Int("org", 0, "Organization ID")
		hostID := fs.String("host-id", "", "Host ID (defaults to daemon's host)")
		dir := fs.String("dir", "", "Directory path (defaults to cwd)")
		fs.Parse(args[1:])

		reader := bufio.NewReader(os.Stdin)

		if *orgID == 0 {
			s := promptLine(reader, "Organization ID: ")
			fmt.Sscanf(s, "%d", orgID)
		}
		if *orgID == 0 {
			fmt.Fprintf(os.Stderr, "greenlight: --org required\n")
			os.Exit(1)
		}
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
			"organization_id": *orgID,
			"host_id":         *hostID,
		}
		if *dir != "" {
			payload["directory_path"] = *dir
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
		fmt.Fprintf(os.Stderr, "greenlight household wd: unknown command %q\n", args[0])
		os.Exit(1)
	}
}

// =============================================================================
// job — agent job descriptions
// =============================================================================

func runHouseholdJob(args []string) {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		fmt.Fprintf(os.Stderr, "Usage: greenlight household job <list|get|create|update|delete>\n")
		os.Exit(0)
	}
	switch args[0] {
	case "list":
		fs := flag.NewFlagSet("job list", flag.ExitOnError)
		orgID := fs.Int("org", 0, "Filter by organization ID")
		fs.Parse(args[1:])
		payload := map[string]interface{}{}
		if *orgID != 0 {
			payload["organization_id"] = *orgID
		}
		data, err := sendWSRequest("list_agent_job_descriptions", payload)
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
		orgID := fs.Int("org", 0, "Organization ID")
		title := fs.String("title", "", "Job title (e.g. \"Gardener\")")
		mandate := fs.String("mandate", "", "Mandate / responsibilities (markdown)")
		priority := fs.Int("priority", 5, "Priority 1-10 (1=highest)")
		fs.Parse(args[1:])

		reader := bufio.NewReader(os.Stdin)
		if *orgID == 0 {
			s := promptLine(reader, "Organization ID: ")
			fmt.Sscanf(s, "%d", orgID)
		}
		if *orgID == 0 {
			fmt.Fprintf(os.Stderr, "greenlight: --org required\n")
			os.Exit(1)
		}
		if *title == "" {
			*title = promptLine(reader, "Title: ")
		}
		if *title == "" {
			fmt.Fprintf(os.Stderr, "greenlight: title required\n")
			os.Exit(1)
		}

		payload := map[string]interface{}{
			"organization_id": *orgID,
			"title":           *title,
			"priority":        *priority,
		}
		if *mandate != "" {
			payload["mandate"] = *mandate
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
		fmt.Fprintf(os.Stderr, "greenlight household job: unknown command %q\n", args[0])
		os.Exit(1)
	}
}

// =============================================================================
// pos — organization positions
// =============================================================================

func runHouseholdPos(args []string) {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		fmt.Fprintf(os.Stderr, "Usage: greenlight household pos <list|get|create|delete>\n")
		os.Exit(0)
	}
	switch args[0] {
	case "list":
		fs := flag.NewFlagSet("pos list", flag.ExitOnError)
		orgID := fs.Int("org", 0, "Filter by organization ID")
		fs.Parse(args[1:])
		payload := map[string]interface{}{}
		if *orgID != 0 {
			payload["organization_id"] = *orgID
		}
		data, err := sendWSRequest("list_organization_positions", payload)
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
		orgID := fs.Int("org", 0, "Organization ID")
		jobID := fs.Int("job", 0, "Agent job description ID")
		wdID := fs.Int("wd", 0, "Working directory ID")
		parentID := fs.Int("parent", 0, "Parent position ID (optional)")
		fs.Parse(args[1:])

		reader := bufio.NewReader(os.Stdin)
		if *orgID == 0 {
			s := promptLine(reader, "Organization ID: ")
			fmt.Sscanf(s, "%d", orgID)
		}
		if *jobID == 0 {
			s := promptLine(reader, "Agent job description ID: ")
			fmt.Sscanf(s, "%d", jobID)
		}
		if *wdID == 0 {
			s := promptLine(reader, "Working directory ID: ")
			fmt.Sscanf(s, "%d", wdID)
		}
		if *orgID == 0 || *jobID == 0 || *wdID == 0 {
			fmt.Fprintf(os.Stderr, "greenlight: --org, --job, and --wd required\n")
			os.Exit(1)
		}

		payload := map[string]interface{}{
			"organization_id":         *orgID,
			"agent_job_description_id": *jobID,
			"working_directory_id":    *wdID,
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
		fmt.Fprintf(os.Stderr, "greenlight household pos: unknown command %q\n", args[0])
		os.Exit(1)
	}
}

// =============================================================================
// agent — AI agent instances
// =============================================================================

func runHouseholdAgent(args []string) {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		fmt.Fprintf(os.Stderr, "Usage: greenlight household agent <list|get|create|retire|delete>\n")
		os.Exit(0)
	}
	switch args[0] {
	case "list":
		fs := flag.NewFlagSet("agent list", flag.ExitOnError)
		orgID := fs.Int("org", 0, "Filter by organization ID")
		fs.Parse(args[1:])
		payload := map[string]interface{}{}
		if *orgID != 0 {
			payload["organization_id"] = *orgID
		}
		data, err := sendWSRequest("list_ai_agent_instances", payload)
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
		orgID := fs.Int("org", 0, "Organization ID")
		jobID := fs.Int("job", 0, "Agent job description ID")
		posID := fs.Int("pos", 0, "Organization position ID")
		modelID := fs.Int("model", 0, "AI brain model ID (optional)")
		harnessID := fs.Int("harness", 0, "Harness ID (optional)")
		fs.Parse(args[1:])

		reader := bufio.NewReader(os.Stdin)
		if *orgID == 0 {
			s := promptLine(reader, "Organization ID: ")
			fmt.Sscanf(s, "%d", orgID)
		}
		if *jobID == 0 {
			s := promptLine(reader, "Agent job description ID: ")
			fmt.Sscanf(s, "%d", jobID)
		}
		if *posID == 0 {
			s := promptLine(reader, "Organization position ID: ")
			fmt.Sscanf(s, "%d", posID)
		}
		if *orgID == 0 || *jobID == 0 || *posID == 0 {
			fmt.Fprintf(os.Stderr, "greenlight: --org, --job, and --pos required\n")
			os.Exit(1)
		}

		payload := map[string]interface{}{
			"organization_id":          *orgID,
			"agent_job_description_id": *jobID,
			"organization_position_id": *posID,
		}
		if *modelID != 0 {
			payload["ai_brain_model_id"] = *modelID
		}
		if *harnessID != 0 {
			payload["harness_id"] = *harnessID
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
		fmt.Fprintf(os.Stderr, "greenlight household agent: unknown command %q\n", args[0])
		os.Exit(1)
	}
}

// =============================================================================
// model — AI brain models (read-only)
// =============================================================================

func runHouseholdModel(args []string) {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		fmt.Fprintf(os.Stderr, "Usage: greenlight household model <list|get>\n")
		os.Exit(0)
	}
	switch args[0] {
	case "list":
		fs := flag.NewFlagSet("model list", flag.ExitOnError)
		orgID := fs.Int("org", 0, "Filter by organization ID")
		fs.Parse(args[1:])
		payload := map[string]interface{}{}
		if *orgID != 0 {
			payload["organization_id"] = *orgID
		}
		data, err := sendWSRequest("list_ai_brain_models", payload)
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
		fmt.Fprintf(os.Stderr, "greenlight household model: unknown command %q\n", args[0])
		os.Exit(1)
	}
}
