//go:build darwin || linux

package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
)

// runTalk launches the interactive TUI for talking to active agent sessions
// over the same /ws endpoint the phone uses.
//
// Phase 1 (this file): connects, dumps raw incoming JSON into a viewport,
// quits on q/ctrl+c. No transcript parsing, no session pills, no input,
// no permission modal. Just prove the connection and the message pump.
func runTalk(args []string) {
	if len(args) > 0 && (args[0] == "--help" || args[0] == "-h") {
		fmt.Fprintf(os.Stderr, `Usage: greenlight talk

Opens a TUI that connects to your Greenlight account and shows live
activity from any agent session you have running (started via
'greenlight connect'). Press q or ctrl+c to quit.
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
	// TODO(transcript-persistence): the model holds the message log in
	// memory only. Quitting the TUI loses everything. Phone parity needs a
	// per-session JSON file under ~/.greenlight/talk/<relay_id>.jsonl that
	// gets tailed on next launch.

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

type talkModel struct {
	viewport viewport.Model
	ws       *talkWS

	// Phase 1: raw incoming JSON gets accumulated here and dumped into the
	// viewport unparsed. Replaced with structured per-session transcript
	// buffers in phase 2.
	log []string

	status        string
	width, height int
	ready         bool
}

func newTalkModel(ws *talkWS) talkModel {
	return talkModel{
		ws:     ws,
		status: "Connecting…",
	}
}

func (m talkModel) Init() tea.Cmd {
	return nil
}

func (m talkModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c", "esc":
			return m, tea.Quit
		}

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		// Reserve one row for the status line at the bottom.
		vpHeight := msg.Height - 1
		if vpHeight < 1 {
			vpHeight = 1
		}
		if !m.ready {
			m.viewport = viewport.New(msg.Width, vpHeight)
			m.viewport.SetContent(m.renderLog())
			m.ready = true
		} else {
			m.viewport.Width = msg.Width
			m.viewport.Height = vpHeight
		}

	case wsConnectedMsg:
		m.status = "Connected — listening for activity (q to quit)"

	case wsDisconnectedMsg:
		m.status = "Disconnected: " + msg.err

	case wsMessageMsg:
		m.log = append(m.log, string(msg.data))
		if m.ready {
			m.viewport.SetContent(m.renderLog())
			m.viewport.GotoBottom()
		}
	}

	if m.ready {
		var cmd tea.Cmd
		m.viewport, cmd = m.viewport.Update(msg)
		cmds = append(cmds, cmd)
	}
	return m, tea.Batch(cmds...)
}

func (m talkModel) View() string {
	if !m.ready {
		return m.status
	}
	return m.viewport.View() + "\n" + m.status
}

func (m talkModel) renderLog() string {
	if len(m.log) == 0 {
		return "(no messages yet)"
	}
	return strings.Join(m.log, "\n\n")
}
