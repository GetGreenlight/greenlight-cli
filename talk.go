//go:build darwin || linux

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// runTalk launches the interactive TUI for talking to active agent sessions
// over the same /ws endpoint the phone uses.
//
// Phase 2: structured transcript rendering, session pills, text input. No
// permission modal yet (phase 3).
func runTalk(args []string) {
	if len(args) > 0 && (args[0] == "--help" || args[0] == "-h") {
		fmt.Fprintf(os.Stderr, `Usage: greenlight talk

Opens a TUI that connects to your Greenlight account and shows live
transcripts from any agent session you have running (started via
'greenlight connect'). Type a message and press Enter to send it to the
focused session. Press Tab to switch focus between sessions. Press
ctrl+c or esc to quit.
`)
		os.Exit(0)
	}

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

	// TODO(phone-coexistence): the server's /ws endpoint stores at most one
	// connection per human_user_id, so opening this TUI will knock the phone
	// (and any other live /ws client for the same user) offline. Acceptable
	// for v0; fixing it requires turning s.connections into a fan-out map
	// on the server.
	//
	// TODO(transcript-persistence): the model holds per-session transcript
	// buffers in memory only. Quitting the TUI loses everything. Phone
	// parity needs per-session JSON files under ~/.greenlight/talk/<id>.jsonl
	// that get tailed on next launch.

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

type talkSession struct {
	relayID string
	project string
	version string
}

// talkPendingPermission holds the in-flight permission_request the user needs
// to act on. Only one can be displayed at a time; subsequent permission_request
// messages while a modal is up overwrite the previous one (rare in practice,
// since the same /ws connection only fields one outstanding request per
// session at a time).
type talkPendingPermission struct {
	requestID string
	toolName  string
	project   string
	relayID   string
	toolInput json.RawMessage
}

type talkModel struct {
	ws *talkWS

	// Sessions known to the model, in order received from the server.
	sessions []talkSession
	// Currently focused session's relay_id ("" if none).
	focused string
	// Per-session transcript buffers, in-memory only. Each entry is one
	// already-rendered (lipgloss-styled) string.
	transcripts map[string][]string

	// Pending permission request (modal state). When non-nil the View
	// renders the modal in place of the viewport and key handling routes
	// through the modal branch.
	pending *talkPendingPermission

	// Help overlay. When true, View renders the help box and any key
	// dismisses it (except ctrl+c which always quits).
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
		// Help overlay: any key dismisses it (ctrl+c always quits).
		if m.helpOpen {
			if msg.String() == "ctrl+c" {
				return m, tea.Quit
			}
			m.helpOpen = false
			return m, nil
		}

		// Modal mode: route a/d/A/esc directly, swallow everything else.
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
				// Local dismiss only — does not respond to the server.
				m.pending = nil
				m.input.Focus()
				m.refreshViewport()
				return m, nil
			}
			// Swallow everything else so typing doesn't bleed into the
			// input field while the modal is up.
			return m, nil
		}

		switch msg.String() {
		case "ctrl+c", "esc":
			return m, tea.Quit
		case "?":
			// Only treat ? as a help shortcut when the input field is
			// empty — otherwise it's just a character in the message
			// the user is typing.
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
		// Forward all other keys to the input field for typing.
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		cmds = append(cmds, cmd)

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.input.Width = msg.Width - 4
		// Layout: pills(1) + viewport(rest) + input(1) + status(1) = height
		vpHeight := msg.Height - 3
		if vpHeight < 1 {
			vpHeight = 1
		}
		if !m.ready {
			m.viewport = viewport.New(msg.Width, vpHeight)
			// Disable the viewport's default keymap; we route scroll
			// keys explicitly so they don't fight the input field.
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
	pills := renderPills(m.sessions, m.focused)

	// Help overlay takes precedence over everything except quit.
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

	// Modal mode: replace the viewport pane with the centered modal and
	// the input row with a help line listing the modal key bindings.
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

// statusBar combines the current status string with contextual key hints.
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
// Matches the WSMessage struct on the server side.
type talkServerMsg struct {
	Type      string                 `json:"type"`
	SessionID string                 `json:"session_id,omitempty"`
	RelayID   string                 `json:"relay_id,omitempty"`
	Project   string                 `json:"project,omitempty"`
	Agent     string                 `json:"agent,omitempty"`
	Sessions  []talkSessionWire      `json:"sessions,omitempty"`
	Data      json.RawMessage        `json:"data,omitempty"`
	Status    string                 `json:"status,omitempty"`
	Text      string                 `json:"text,omitempty"`
	Message   string                 `json:"message,omitempty"`
	Event     string                 `json:"event,omitempty"`
	ToolName  string                 `json:"tool_name,omitempty"`
	ToolInput json.RawMessage        `json:"tool_input,omitempty"`
	RequestID string                 `json:"request_id,omitempty"`
	Missed    []talkMissedWire       `json:"missed,omitempty"`
	Error     string                 `json:"error,omitempty"`
	Extra     map[string]interface{} `json:"-"`
}

type talkSessionWire struct {
	SessionID string `json:"session_id"`
	RelayID   string `json:"relay_id"`
	Project   string `json:"project,omitempty"`
	Version   string `json:"version,omitempty"`
}

type talkMissedWire struct {
	RequestID string          `json:"request_id"`
	ToolName  string          `json:"tool_name"`
	ToolInput json.RawMessage `json:"tool_input,omitempty"`
	Project   string          `json:"project,omitempty"`
	Agent     string          `json:"agent,omitempty"`
}

func (m *talkModel) handleServerMessage(data []byte) {
	var msg talkServerMsg
	if err := json.Unmarshal(data, &msg); err != nil {
		return
	}

	switch msg.Type {
	case "relay_sessions":
		m.applySessions(msg.Sessions)
		m.refreshViewport()

	case "transcript_entry":
		rel := msg.RelayID
		if rel == "" {
			rel = msg.SessionID
		}
		if rel == "" {
			return
		}
		var rendered string
		if len(msg.Data) > 0 {
			rendered = renderTranscriptEntry(msg.Data)
		} else {
			rendered = renderDecomposedTranscript(msg.Event, msg.Text, msg.ToolName, msg.Message, msg.ToolInput)
		}
		if rendered == "" {
			return
		}
		m.transcripts[rel] = append(m.transcripts[rel], rendered)
		if rel == m.focused {
			m.refreshViewport()
		}

	case "activity_event":
		rel := msg.RelayID
		if rel == "" {
			rel = msg.SessionID
		}
		if rel == "" {
			return
		}
		m.transcripts[rel] = append(m.transcripts[rel],
			renderActivityEvent(msg.Event, msg.ToolName, msg.Project))
		if rel == m.focused {
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
			requestID: msg.RequestID,
			toolName:  msg.ToolName,
			project:   msg.Project,
			relayID:   msg.RelayID,
			toolInput: msg.ToolInput,
		}
		m.input.Blur()

	case "cancel_request":
		if m.pending != nil && m.pending.requestID == msg.RequestID {
			m.pending = nil
			m.input.Focus()
			m.refreshViewport()
		}

	case "missed_requests":
		// MissedRequest doesn't carry a relay_id, so we can't route per
		// session. Append each item to whichever session is currently
		// focused. If nothing's focused yet, drop them — the user can
		// reconnect once a session is up.
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

func (m *talkModel) applySessions(wire []talkSessionWire) {
	m.sessions = m.sessions[:0]
	for _, s := range wire {
		rel := s.RelayID
		if rel == "" {
			rel = s.SessionID
		}
		if rel == "" {
			continue
		}
		m.sessions = append(m.sessions, talkSession{
			relayID: rel,
			project: s.Project,
			version: s.Version,
		})
	}
	// If the previously focused session disappeared, fall back to the first
	// available (or unset).
	stillThere := false
	for _, s := range m.sessions {
		if s.relayID == m.focused {
			stillThere = true
			break
		}
	}
	if !stillThere {
		if len(m.sessions) > 0 {
			m.focused = m.sessions[0].relayID
		} else {
			m.focused = ""
		}
	}
}

func (m *talkModel) cycleFocus(delta int) {
	if len(m.sessions) == 0 {
		return
	}
	idx := -1
	for i, s := range m.sessions {
		if s.relayID == m.focused {
			idx = i
			break
		}
	}
	if idx == -1 {
		m.focused = m.sessions[0].relayID
		return
	}
	idx = (idx + delta + len(m.sessions)) % len(m.sessions)
	m.focused = m.sessions[idx].relayID
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
		"type":     "relay_input",
		"relay_id": m.focused,
		"text":     text,
	})
	if err != nil {
		return err
	}
	return m.ws.send(payload)
}

// respondToPermission sends a permission_response with the given behavior
// ("allow", "deny", "always_allow") for the in-flight modal. On any send
// error it leaves the modal up so the user can retry.
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
	// Drop a faint trail line into the relevant transcript so the user can
	// see what they decided.
	rel := m.pending.relayID
	if rel == "" {
		rel = m.focused
	}
	if rel != "" {
		m.transcripts[rel] = append(
			m.transcripts[rel],
			statusStyle.Render(fmt.Sprintf("→ %s %s", behavior, m.pending.toolName)),
		)
	}
	m.pending = nil
	m.input.Focus()
	m.refreshViewport()
}
