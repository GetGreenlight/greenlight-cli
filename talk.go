//go:build darwin || linux

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
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

type talkModel struct {
	ws *talkWS

	// Sessions known to the model, in order received from the server.
	sessions []talkSession
	// Currently focused session's relay_id ("" if none).
	focused string
	// Per-session transcript buffers, in-memory only. Each entry is one
	// already-rendered (lipgloss-styled) string.
	transcripts map[string][]string

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
		switch msg.String() {
		case "ctrl+c", "esc":
			return m, tea.Quit
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
	return strings.Join([]string{
		pills,
		m.viewport.View(),
		m.input.View(),
		statusStyle.Render(m.status),
	}, "\n")
}

// =============================================================================
// server message handling
// =============================================================================

// talkServerMsg is the union of every server-sent /ws field this phase reads.
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
	Message   string                 `json:"message,omitempty"`
	Event     string                 `json:"event,omitempty"`
	ToolName  string                 `json:"tool_name,omitempty"`
	Error     string                 `json:"error,omitempty"`
	Extra     map[string]interface{} `json:"-"`
}

type talkSessionWire struct {
	SessionID string `json:"session_id"`
	RelayID   string `json:"relay_id"`
	Project   string `json:"project,omitempty"`
	Version   string `json:"version,omitempty"`
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
		rendered := renderTranscriptEntry(msg.Data)
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

	case "permission_request", "cancel_request", "missed_requests":
		// TODO(phase-3): wire the permission modal. For now, drop a faint
		// note into the focused transcript so the user knows something
		// happened.
		if m.focused != "" {
			m.transcripts[m.focused] = append(m.transcripts[m.focused],
				statusStyle.Render("(received "+msg.Type+" — phase-3 modal not implemented yet)"))
			m.refreshViewport()
		}
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
