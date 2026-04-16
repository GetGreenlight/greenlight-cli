//go:build darwin || linux

package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// =============================================================================
// styles
// =============================================================================

var (
	pillFocusedStyle = lipgloss.NewStyle().
				Bold(true).
				Foreground(lipgloss.Color("15")).
				Background(lipgloss.Color("33")).
				Padding(0, 1)
	pillStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("241")).
			Padding(0, 1)

	statusStyle = lipgloss.NewStyle().Faint(true)
	emptyStyle  = lipgloss.NewStyle().Faint(true).Italic(true)

	youStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("33"))

	toolUseStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("36"))
	toolResultStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))

	activityStyle = lipgloss.NewStyle().Faint(true).Italic(true)
)

// =============================================================================
// pills
// =============================================================================

func renderPills(sessions []talkSession, focusedID string) string {
	if len(sessions) == 0 {
		return emptyStyle.Render("(no active sessions — start one with 'greenlight connect')")
	}
	parts := make([]string, 0, len(sessions))
	for _, s := range sessions {
		label := s.project
		if label == "" {
			label = "session"
		}
		if s.relayID == focusedID {
			parts = append(parts, pillFocusedStyle.Render("● "+label))
		} else {
			parts = append(parts, pillStyle.Render("○ "+label))
		}
	}
	return strings.Join(parts, " ")
}

// =============================================================================
// transcript content blocks
// =============================================================================

// renderTranscriptEntry parses a transcript_entry's `data` payload and returns
// rendered lines. Targets the claude transcript JSONL format
// ({type, message:{role, content:[...]}}); falls back to a faint dump of the
// raw JSON when the shape doesn't match.
func renderTranscriptEntry(data json.RawMessage) string {
	if len(data) == 0 {
		return ""
	}
	var claude struct {
		Type    string `json:"type"`
		Message struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(data, &claude); err == nil && claude.Type != "" {
		if rendered := renderClaudeMessage(claude.Message.Role, claude.Message.Content); rendered != "" {
			return rendered
		}
	}
	return statusStyle.Render(strings.TrimSpace(string(data)))
}

// renderDecomposedTranscript renders a transcript entry from the server's
// pre-decomposed fields (Event, Text, ToolName, Message, ToolInput) rather
// than from raw transcript JSON.
func renderDecomposedTranscript(event, text, toolName, message string, toolInput json.RawMessage) string {
	switch event {
	case "user":
		if text != "" {
			return youStyle.Render("you") + " " + text
		}
	case "text":
		if text != "" {
			return text
		}
	case "tool_use":
		label := "⚙ " + toolName
		if message != "" {
			label += " " + message
		}
		return toolUseStyle.Render(label)
	case "tool_result":
		if message != "" {
			return toolResultStyle.Render(message)
		}
	}
	return ""
}

func renderClaudeMessage(role string, content json.RawMessage) string {
	// content is either an array of blocks or a plain string.
	var blocks []contentBlock
	if err := json.Unmarshal(content, &blocks); err == nil {
		return renderClaudeBlocks(role, blocks)
	}
	var contentStr string
	if err := json.Unmarshal(content, &contentStr); err == nil {
		return renderClaudeText(role, contentStr)
	}
	return ""
}

type contentBlock struct {
	Type    string          `json:"type"`
	Text    string          `json:"text,omitempty"`
	Name    string          `json:"name,omitempty"`
	Input   json.RawMessage `json:"input,omitempty"`
	Content json.RawMessage `json:"content,omitempty"`
}

func renderClaudeBlocks(role string, blocks []contentBlock) string {
	var lines []string
	for _, b := range blocks {
		switch b.Type {
		case "text":
			text := strings.TrimSpace(b.Text)
			if text == "" {
				continue
			}
			if role == "user" {
				lines = append(lines, youStyle.Render("you")+" "+text)
			} else {
				lines = append(lines, text)
			}
		case "tool_use":
			preview := summarizeToolInput(b.Name, b.Input)
			label := "⚙ " + b.Name
			if preview != "" {
				label += " " + preview
			}
			lines = append(lines, toolUseStyle.Render(label))
		case "tool_result":
			body := contentToString(b.Content)
			if body != "" {
				lines = append(lines, toolResultStyle.Render("✓ "+truncate(body, 200)))
			} else {
				lines = append(lines, toolResultStyle.Render("✓"))
			}
		case "thinking":
			// skip
		}
	}
	return strings.Join(lines, "\n")
}

func renderClaudeText(role, text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	if role == "user" {
		return youStyle.Render("you") + " " + text
	}
	return text
}

// contentToString tries to coax a content block payload into a single line of
// readable text. tool_result contents can be a string or an array of nested
// blocks; we handle both, falling back to the raw JSON.
func contentToString(content json.RawMessage) string {
	if len(content) == 0 {
		return ""
	}
	var asString string
	if err := json.Unmarshal(content, &asString); err == nil {
		return strings.TrimSpace(asString)
	}
	var nested []contentBlock
	if err := json.Unmarshal(content, &nested); err == nil {
		var parts []string
		for _, b := range nested {
			if b.Type == "text" && b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
		return strings.TrimSpace(strings.Join(parts, " "))
	}
	return strings.TrimSpace(string(content))
}

// summarizeToolInput pulls the most useful field out of a tool_use input map
// so the rendered line shows the actual command/path/pattern instead of the
// full JSON blob. Falls back to a truncated raw dump for unknown tools.
func summarizeToolInput(name string, input json.RawMessage) string {
	if len(input) == 0 {
		return ""
	}
	var asMap map[string]interface{}
	if err := json.Unmarshal(input, &asMap); err != nil {
		return truncate(string(input), 80)
	}
	switch name {
	case "Bash":
		if cmd, ok := asMap["command"].(string); ok {
			return "$ " + truncate(cmd, 80)
		}
	case "Read", "Write", "Edit", "NotebookEdit":
		if path, ok := asMap["file_path"].(string); ok {
			return path
		}
	case "Glob", "Grep":
		if pat, ok := asMap["pattern"].(string); ok {
			return pat
		}
	case "WebFetch":
		if u, ok := asMap["url"].(string); ok {
			return u
		}
	case "WebSearch":
		if q, ok := asMap["query"].(string); ok {
			return q
		}
	case "TodoWrite":
		return "(todos updated)"
	}
	return truncate(string(input), 80)
}

func truncate(s string, max int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// renderActivityEvent formats an activity_event message as a single faint line
// for the focused transcript pane.
func renderActivityEvent(event, toolName, project string) string {
	parts := []string{"·"}
	if event != "" {
		parts = append(parts, event)
	}
	if toolName != "" {
		parts = append(parts, toolName)
	}
	if project != "" {
		parts = append(parts, fmt.Sprintf("[%s]", project))
	}
	return activityStyle.Render(strings.Join(parts, " "))
}

// =============================================================================
// permission modal
// =============================================================================

var (
	modalBoxStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("33")).
			Padding(1, 2)
	modalHeaderStyle = lipgloss.NewStyle().
				Bold(true).
				Foreground(lipgloss.Color("33"))
	modalLabelStyle = lipgloss.NewStyle().
			Faint(true)
	modalDetailStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("36"))
	modalHelpStyle = lipgloss.NewStyle().
			Faint(true).
			Italic(true)

	missedStyle = lipgloss.NewStyle().
			Faint(true).
			Foreground(lipgloss.Color("214"))
)

// renderPermissionModal draws the centered permission request box. The caller
// is responsible for placing it in the viewport area (e.g. via lipgloss.Place).
func renderPermissionModal(p *talkPendingPermission, terminalWidth int) string {
	boxWidth := 64
	if terminalWidth > 0 && terminalWidth-4 < boxWidth {
		boxWidth = terminalWidth - 4
	}
	if boxWidth < 30 {
		boxWidth = 30
	}

	var body strings.Builder
	body.WriteString(modalHeaderStyle.Render("Permission request"))
	body.WriteString("\n\n")
	body.WriteString(modalLabelStyle.Render("Tool:    ") + p.toolName + "\n")
	if p.project != "" {
		body.WriteString(modalLabelStyle.Render("Project: ") + p.project + "\n")
	}
	body.WriteString("\n")
	if preview := summarizeToolInput(p.toolName, p.toolInput); preview != "" {
		body.WriteString(modalDetailStyle.Render(preview))
	} else {
		body.WriteString(modalLabelStyle.Render("(no preview available)"))
	}

	return modalBoxStyle.Width(boxWidth).Render(body.String())
}

// renderMissedRequest formats a single backfilled MissedRequest as a faint
// one-line entry tagged "missed".
func renderMissedRequest(toolName, project string, toolInput json.RawMessage) string {
	preview := summarizeToolInput(toolName, toolInput)
	parts := []string{"⏱ missed", toolName}
	if preview != "" {
		parts = append(parts, "—", preview)
	}
	if project != "" {
		parts = append(parts, fmt.Sprintf("[%s]", project))
	}
	return missedStyle.Render(strings.Join(parts, " "))
}

// =============================================================================
// help overlay
// =============================================================================

var helpKeyStyle = lipgloss.NewStyle().
	Bold(true).
	Foreground(lipgloss.Color("33"))

// renderHelp draws the centered help overlay shown when the user presses '?'.
func renderHelp(terminalWidth int) string {
	boxWidth := 56
	if terminalWidth > 0 && terminalWidth-4 < boxWidth {
		boxWidth = terminalWidth - 4
	}
	if boxWidth < 30 {
		boxWidth = 30
	}

	row := func(key, label string) string {
		return helpKeyStyle.Render(fmt.Sprintf("  %-12s", key)) + label
	}

	var b strings.Builder
	b.WriteString(modalHeaderStyle.Render("greenlight talk") + "\n\n")
	b.WriteString(modalLabelStyle.Render("Normal mode") + "\n")
	b.WriteString(row("tab", "switch focused session") + "\n")
	b.WriteString(row("shift+tab", "switch backwards") + "\n")
	b.WriteString(row("pgup / pgdn", "scroll the transcript") + "\n")
	b.WriteString(row("enter", "send the input as a message") + "\n")
	b.WriteString(row("?", "show this help") + "\n")
	b.WriteString(row("ctrl+c, esc", "quit") + "\n")
	b.WriteString("\n")
	b.WriteString(modalLabelStyle.Render("Permission modal") + "\n")
	b.WriteString(row("a", "allow") + "\n")
	b.WriteString(row("d", "deny") + "\n")
	b.WriteString(row("A", "always allow") + "\n")
	b.WriteString(row("esc", "dismiss") + "\n")
	b.WriteString("\n")
	b.WriteString(modalHelpStyle.Render("(press any key to close)"))

	return modalBoxStyle.Width(boxWidth).Render(b.String())
}

// statusHints returns a faint key-hint suffix for the bottom status bar.
// Different content depending on whether the help/modal layers are active.
func statusHints(modalActive, helpActive bool) string {
	switch {
	case modalActive:
		return "" // modal already shows its own help line
	case helpActive:
		return "(any key) close"
	default:
		return "tab switch · pgup/pgdn scroll · ? help · ctrl+c quit"
	}
}
