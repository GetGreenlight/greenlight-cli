//go:build darwin || linux

package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// runTalk launches the interactive TUI for talking to active agent instances
// over the same /ws endpoint the phone uses.
func runTalk(args []string) {
	fs := flag.NewFlagSet("talk", flag.ExitOnError)
	focusID := fs.String("focus", "", "ai_agent_instance_id to start focused on (otherwise the first active instance)")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, `Usage: greenlight talk [--focus <ai_agent_instance_id>]

Opens a TUI that connects to your Greenlight account and shows live
transcripts from any agent instance you have running (spawned via
'greenlight org agent create'). Type a message and press Enter to send
it to the focused instance. Press Tab to switch focus between instances.
Press ctrl+c or esc to quit.
`)
	}
	fs.Parse(args)

	userID := readConfigValue("user_id")
	if userID == "" {
		fmt.Fprintf(os.Stderr, "greenlight: not registered (run 'greenlight register <email>' first)\n")
		os.Exit(1)
	}
	if wsURL == "" {
		fmt.Fprintf(os.Stderr, "greenlight: no relay server URL configured (build with -ldflags '-X main.wsURL=...')\n")
		os.Exit(1)
	}

	ws, err := newTalkWS(userID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "greenlight: %v\n", err)
		os.Exit(1)
	}

	m := newTalkModel(ws)
	m.initialFocus = *focusID

	p := tea.NewProgram(m, tea.WithAltScreen())
	ws.program = p
	go ws.run()

	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "greenlight: %v\n", err)
		os.Exit(1)
	}
	ws.close()
}

// =============================================================================
// model
// =============================================================================

type talkInstance struct {
	aiAgentInstanceID string
	project           string
	version           string
}

// talkPendingPermission holds the in-flight permission_request the user needs
// to act on. Only one can be displayed at a time; subsequent permission_request
// messages while a modal is up overwrite the previous one.
type talkPendingPermission struct {
	requestID         string
	toolName          string
	project           string
	aiAgentInstanceID string
	toolInput         json.RawMessage
}

type talkModel struct {
	ws *talkWS

	// Instances known to the model, in order received from the server.
	instances []talkInstance
	// Currently focused instance's ai_agent_instance_id ("" if none).
	focused string
	// initialFocus is set from the --focus CLI flag: if non-empty, the first
	// applyInstances call that sees this id will jump to it instead of
	// defaulting to instances[0]. Cleared after consumption.
	initialFocus string
	// Per-instance transcript buffers, in-memory only.
	transcripts map[string][]string

	// Pending permission request (modal state).
	pending *talkPendingPermission

	// Help overlay.
	helpOpen bool

	viewport viewport.Model
	input    textinput.Model

	width, height int
	ready         bool
	status        string
}

func newTalkModel(ws *talkWS) talkModel {
	ti := textinput.New()
	ti.Placeholder = "Type a message and press Enter…"
	ti.Prompt = "› "
	ti.CharLimit = 4096
	ti.Focus()

	return talkModel{
		ws:          ws,
		transcripts: make(map[string][]string),
		input:       ti,
		status:      "Connecting…",
	}
}

func (m talkModel) Init() tea.Cmd {
	return textinput.Blink
}

func (m talkModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.KeyMsg:
		if m.helpOpen {
			if msg.String() == "ctrl+c" {
				return m, tea.Quit
			}
			m.helpOpen = false
			return m, nil
		}

		if m.pending != nil {
			switch msg.String() {
			case "ctrl+c":
				return m, tea.Quit
			case "a":
				m.respondToPermission("allow")
				return m, nil
			case "d":
				m.respondToPermission("deny")
				return m, nil
			case "A":
				m.respondToPermission("always_allow")
				return m, nil
			case "esc":
				m.pending = nil
				m.input.Focus()
				m.refreshViewport()
				return m, nil
			}
			return m, nil
		}

		switch msg.String() {
		case "ctrl+c", "esc":
			return m, tea.Quit
		case "?":
			if m.input.Value() == "" {
				m.helpOpen = true
				return m, nil
			}
		case "tab":
			m.cycleFocus(1)
			m.refreshViewport()
			return m, nil
		case "shift+tab":
			m.cycleFocus(-1)
			m.refreshViewport()
			return m, nil
		case "pgup":
			if m.ready {
				m.viewport.HalfViewUp()
			}
			return m, nil
		case "pgdown":
			if m.ready {
				m.viewport.HalfViewDown()
			}
			return m, nil
		case "enter":
			text := strings.TrimSpace(m.input.Value())
			if text != "" && m.focused != "" {
				if err := m.sendInput(text); err != nil {
					m.status = "send failed: " + err.Error()
				} else {
					m.transcripts[m.focused] = append(
						m.transcripts[m.focused],
						youStyle.Render("you")+" "+text,
					)
					m.refreshViewport()
				}
				m.input.SetValue("")
			}
			return m, nil
		}
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		cmds = append(cmds, cmd)

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.input.Width = msg.Width - 4
		vpHeight := msg.Height - 3
		if vpHeight < 1 {
			vpHeight = 1
		}
		if !m.ready {
			m.viewport = viewport.New(msg.Width, vpHeight)
			m.viewport.KeyMap = viewport.KeyMap{}
			m.refreshViewport()
			m.ready = true
		} else {
			m.viewport.Width = msg.Width
			m.viewport.Height = vpHeight
		}

	case wsConnectedMsg:
		m.status = "Connected"

	case wsDisconnectedMsg:
		m.status = "Disconnected: " + msg.err

	case wsReconnectingMsg:
		m.status = fmt.Sprintf("Reconnecting in %s…", msg.after.Round(time.Second))

	case wsMessageMsg:
		m.handleServerMessage(msg.data)
	}

	return m, tea.Batch(cmds...)
}

func (m talkModel) View() string {
	if !m.ready {
		return m.status
	}
	pills := renderPills(m.instances, m.focused)

	if m.helpOpen {
		help := renderHelp(m.width)
		centered := lipgloss.Place(m.width, m.viewport.Height,
			lipgloss.Center, lipgloss.Center, help)
		return strings.Join([]string{
			pills,
			centered,
			modalHelpStyle.Render("(press any key to close)"),
			m.statusBar(),
		}, "\n")
	}

	if m.pending != nil {
		modal := renderPermissionModal(m.pending, m.width)
		centered := lipgloss.Place(m.width, m.viewport.Height,
			lipgloss.Center, lipgloss.Center, modal)
		help := modalHelpStyle.Render("[a] allow   [d] deny   [A] always allow   [esc] dismiss")
		return strings.Join([]string{
			pills,
			centered,
			help,
			m.statusBar(),
		}, "\n")
	}

	return strings.Join([]string{
		pills,
		m.viewport.View(),
		m.input.View(),
		m.statusBar(),
	}, "\n")
}

func (m talkModel) statusBar() string {
	hints := statusHints(m.pending != nil, m.helpOpen)
	if hints == "" {
		return statusStyle.Render(m.status)
	}
	return statusStyle.Render(m.status + " · " + hints)
}

// =============================================================================
// server message handling
// =============================================================================

// talkServerMsg is the union of every server-sent /ws field this TUI reads.
type talkServerMsg struct {
	Type              string                 `json:"type"`
	AIAgentInstanceID string                 `json:"ai_agent_instance_id,omitempty"`
	Project           string                 `json:"project,omitempty"`
	Agent             string                 `json:"agent,omitempty"`
	AgentInstances    []talkInstanceWire     `json:"ai_agent_instances,omitempty"`
	Data              json.RawMessage        `json:"data,omitempty"`
	Status            string                 `json:"status,omitempty"`
	Text              string                 `json:"text,omitempty"`
	Message           string                 `json:"message,omitempty"`
	Event             string                 `json:"event,omitempty"`
	ToolName          string                 `json:"tool_name,omitempty"`
	ToolInput         json.RawMessage        `json:"tool_input,omitempty"`
	RequestID         string                 `json:"request_id,omitempty"`
	Missed            []talkMissedWire       `json:"missed,omitempty"`
	Error             string                 `json:"error,omitempty"`
	Extra             map[string]interface{} `json:"-"`
}

// talkInstanceWire mirrors the server's AIAgentInstance payload.
// Only the fields the TUI cares about are decoded.
type talkInstanceWire struct {
	ID     string `json:"id"`
	Name   string `json:"name,omitempty"`
	Status string `json:"status,omitempty"`
	HostID string `json:"host_id,omitempty"`
}

type talkMissedWire struct {
	RequestID string          `json:"request_id"`
	ToolName  string          `json:"tool_name"`
	ToolInput json.RawMessage `json:"tool_input,omitempty"`
	Project   string          `json:"project,omitempty"`
	Agent     string          `json:"agent,omitempty"`
}

func (m *talkModel) handleServerMessage(data []byte) {
	debugLogFrame("recv", data)

	var msg talkServerMsg
	if err := json.Unmarshal(data, &msg); err != nil {
		debugLogFrame("recv-parse-error", []byte(err.Error()))
		return
	}

	switch msg.Type {
	case "ai_agent_instances_list":
		m.applyInstances(msg.AgentInstances)
		m.refreshViewport()

	case "transcript_entry":
		id := msg.AIAgentInstanceID
		if id == "" {
			debugLogFrame("transcript-drop-no-id", data)
			return
		}
		var rendered string
		if len(msg.Data) > 0 {
			rendered = renderTranscriptEntry(msg.Data)
		} else {
			rendered = renderDecomposedTranscript(msg.Event, msg.Text, msg.ToolName, msg.Message, msg.ToolInput)
		}
		if rendered == "" {
			debugLogFrame(fmt.Sprintf("transcript-drop-empty-render event=%q text_len=%d", msg.Event, len(msg.Text)), data)
			return
		}
		// Dedupe against recent entries. Enter-to-send renders the user's
		// line locally for snappy feedback, and the server later replays it
		// from the agent's transcript. Scan back a few entries instead of
		// just the last one — an activity_event or assistant block may land
		// between the local echo and the server replay.
		buf := m.transcripts[id]
		start := len(buf) - 8
		if start < 0 {
			start = 0
		}
		for i := len(buf) - 1; i >= start; i-- {
			if buf[i] == rendered {
				return
			}
		}
		debugLogf("transcript-append id=%s event=%q focused=%s match=%v rendered_len=%d", id, msg.Event, m.focused, id == m.focused, len(rendered))
		m.transcripts[id] = append(buf, rendered)
		if id == m.focused {
			m.refreshViewport()
		}

	case "activity_event":
		id := msg.AIAgentInstanceID
		if id == "" {
			return
		}
		m.transcripts[id] = append(m.transcripts[id],
			renderActivityEvent(msg.Event, msg.ToolName, msg.Project))
		if id == m.focused {
			m.refreshViewport()
		}

	case "session_status":
		switch {
		case msg.Message != "":
			m.status = msg.Message
		case msg.Status != "":
			m.status = msg.Status
		}

	case "permission_request":
		m.pending = &talkPendingPermission{
			requestID:         msg.RequestID,
			toolName:          msg.ToolName,
			project:           msg.Project,
			aiAgentInstanceID: msg.AIAgentInstanceID,
			toolInput:         msg.ToolInput,
		}
		m.input.Blur()

	case "cancel_request":
		if m.pending != nil && m.pending.requestID == msg.RequestID {
			m.pending = nil
			m.input.Focus()
			m.refreshViewport()
		}

	case "missed_requests":
		if m.focused == "" {
			return
		}
		for _, mr := range msg.Missed {
			m.transcripts[m.focused] = append(
				m.transcripts[m.focused],
				renderMissedRequest(mr.ToolName, mr.Project, mr.ToolInput),
			)
		}
		m.refreshViewport()
	}
}

func (m *talkModel) applyInstances(wire []talkInstanceWire) {
	m.instances = m.instances[:0]
	for _, s := range wire {
		if s.ID == "" {
			continue
		}
		// Hide retired instances; the UI treats them as gone.
		if s.Status == "retired" {
			continue
		}
		m.instances = append(m.instances, talkInstance{
			aiAgentInstanceID: s.ID,
			project:           s.Name,
		})
	}
	// If the caller passed --focus and the requested instance just appeared,
	// jump straight to it. One-shot: clear initialFocus after honoring it.
	if m.initialFocus != "" {
		for _, s := range m.instances {
			if s.aiAgentInstanceID == m.initialFocus {
				m.focused = m.initialFocus
				m.initialFocus = ""
				return
			}
		}
	}
	stillThere := false
	for _, s := range m.instances {
		if s.aiAgentInstanceID == m.focused {
			stillThere = true
			break
		}
	}
	if !stillThere {
		if len(m.instances) > 0 {
			m.focused = m.instances[0].aiAgentInstanceID
		} else {
			m.focused = ""
		}
	}
}

func (m *talkModel) cycleFocus(delta int) {
	if len(m.instances) == 0 {
		return
	}
	idx := -1
	for i, s := range m.instances {
		if s.aiAgentInstanceID == m.focused {
			idx = i
			break
		}
	}
	if idx == -1 {
		m.focused = m.instances[0].aiAgentInstanceID
		return
	}
	idx = (idx + delta + len(m.instances)) % len(m.instances)
	m.focused = m.instances[idx].aiAgentInstanceID
}

func (m *talkModel) refreshViewport() {
	if !m.ready {
		return
	}
	if m.focused == "" || len(m.transcripts[m.focused]) == 0 {
		m.viewport.SetContent(emptyStyle.Render("(no transcript yet — say something below)"))
		return
	}
	m.viewport.SetContent(strings.Join(m.transcripts[m.focused], "\n"))
	m.viewport.GotoBottom()
}

func (m *talkModel) sendInput(text string) error {
	payload, err := json.Marshal(map[string]interface{}{
		"type":                 "relay_input",
		"ai_agent_instance_id": m.focused,
		"text":                 text,
	})
	if err != nil {
		return err
	}
	return m.ws.send(payload)
}

// respondToPermission sends a permission_response with the given behavior
// ("allow", "deny", "always_allow") for the in-flight modal.
func (m *talkModel) respondToPermission(behavior string) {
	if m.pending == nil {
		return
	}
	payload, err := json.Marshal(map[string]interface{}{
		"type":       "permission_response",
		"request_id": m.pending.requestID,
		"behavior":   behavior,
	})
	if err != nil {
		m.status = "respond failed: " + err.Error()
		return
	}
	if err := m.ws.send(payload); err != nil {
		m.status = "respond failed: " + err.Error()
		return
	}
	id := m.pending.aiAgentInstanceID
	if id == "" {
		id = m.focused
	}
	if id != "" {
		m.transcripts[id] = append(
			m.transcripts[id],
			statusStyle.Render(fmt.Sprintf("→ %s %s", behavior, m.pending.toolName)),
		)
	}
	m.pending = nil
	m.input.Focus()
	m.refreshViewport()
}
