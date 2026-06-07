//go:build darwin || linux

package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"time"
)

// runHook is the entry point for `greenlight hook`. It is invoked by Claude Code
// as a PermissionRequest hook for AskUserQuestion and ExitPlanMode. It reads the
// hook payload from stdin, forwards it as a permission request through the interpose
// socket, and writes a PermissionRequest hook decision to stdout.
//
// Claude Code parses the stdout JSON as:
//
//	{"hookSpecificOutput":{"hookEventName":"PermissionRequest",
//	  "decision":{"behavior":"allow"|"deny","message":"...",
//	    "updatedInput":{...}}}}
//
// behavior=allow approves the action without showing Claude's own terminal
// prompt; behavior=deny blocks it. For AskUserQuestion, decision.updatedInput
// carries the phone-collected {questions, answers} — the AskUserQuestion tool's
// call() simply echoes back the answers present in its input, so Claude skips
// its interactive menu entirely. The envelope (hookSpecificOutput.decision)
// and the hookEventName MUST match the registered event, or Claude silently
// ignores the output and falls back to its own prompt.
func runHook(args []string) {
	if os.Getenv("GREENLIGHT_DEVICE_ID") == "" {
		os.Exit(0)
	}

	sockPath := os.Getenv("GREENLIGHT_INTERPOSE_SOCK")
	if sockPath == "" {
		os.Exit(0)
	}

	var hookPayload struct {
		ToolName  string                 `json:"tool_name"`
		ToolInput map[string]interface{} `json:"tool_input"`
	}
	if err := json.NewDecoder(os.Stdin).Decode(&hookPayload); err != nil || hookPayload.ToolName == "" {
		os.Exit(0)
	}

	conn, err := net.DialTimeout("unix", sockPath, 3*time.Second)
	if err != nil {
		os.Exit(0)
	}
	defer conn.Close()

	req := interposeRequest{
		Type:      "hook",
		ToolName:  hookPayload.ToolName,
		ToolInput: hookPayload.ToolInput,
	}
	data, _ := json.Marshal(req)
	data = append(data, '\n')

	conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	if _, err := conn.Write(data); err != nil {
		os.Exit(0)
	}

	conn.SetReadDeadline(time.Now().Add(10 * time.Minute))
	var resp interposeResponse
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		os.Exit(0)
	}

	decision := map[string]interface{}{}
	if resp.Allow {
		decision["behavior"] = "allow"
		// For AskUserQuestion, supply the phone-collected answers as
		// updatedInput so Claude uses them instead of prompting in-terminal.
		// updatedInput is {"questions":[...], "answers":{"question text":"label"}};
		// multiSelect answers are comma-separated label strings.
		if hookPayload.ToolName == "AskUserQuestion" && len(resp.UpdatedInput) > 0 {
			if ui := buildAskUpdatedInput(hookPayload.ToolInput, resp.UpdatedInput); ui != nil {
				decision["updatedInput"] = ui
			} else {
				hookLog("buildAskUpdatedInput returned nil, updatedInput=%s", string(resp.UpdatedInput))
			}
		}
	} else {
		decision["behavior"] = "deny"
		msg := resp.Message
		if msg == "" {
			msg = "Permission denied"
		}
		decision["message"] = msg
	}

	out := map[string]interface{}{
		"hookSpecificOutput": map[string]interface{}{
			"hookEventName": hookEventType,
			"decision":      decision,
		},
	}
	encoded, err := json.Marshal(out)
	hookLog("stdout output err=%v json=%s", err, string(encoded))
	os.Stdout.Write(encoded)
	os.Stdout.Write([]byte("\n"))

	os.Exit(0)
}

func hookLog(format string, args ...interface{}) {
	f, err := os.OpenFile("/tmp/greenlight-hook.log", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, format+"\n", args...)
}

// buildAskUpdatedInput builds the updatedInput object for an AskUserQuestion
// PermissionRequest decision: {"questions":[...], "answers":{"question text":"label"}}.
// questions is the original array verbatim; answers maps each question's "question"
// text to the selected label (the AskUserQuestion tool keys answers by question
// text). Server answers arrive keyed by string index ("0","1",...) and are
// remapped here. Returns nil if no answers are present.
func buildAskUpdatedInput(toolInput map[string]interface{}, updatedInput json.RawMessage) map[string]interface{} {
	var updated struct {
		Questions []struct {
			Question string `json:"question"`
		} `json:"questions"`
		Answers map[string]string `json:"answers"`
	}
	if err := json.Unmarshal(updatedInput, &updated); err != nil || len(updated.Answers) == 0 {
		return nil
	}

	answers := make(map[string]string, len(updated.Questions))
	for i, q := range updated.Questions {
		key := fmt.Sprintf("%d", i)
		if a, ok := updated.Answers[key]; ok {
			answers[q.Question] = a
		}
	}
	if len(answers) == 0 {
		return nil
	}

	// Use the original questions array from the tool input so all fields
	// (header, description, etc.) are preserved exactly as Claude Code sent them.
	return map[string]interface{}{
		"questions": toolInput["questions"],
		"answers":   answers,
	}
}
