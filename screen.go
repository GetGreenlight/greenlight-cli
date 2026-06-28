package main

// PTY screen tap (issue #38).
//
// Instead of scraping styled runs out of the raw escape stream, this feeds the
// raw PTY bytes the daemon already reads into a virtual terminal emulator
// (a screen grid). The grid is maintained continuously for the life of the
// session; suggestion() reads the composer ghost-suggestion off it on demand,
// sampling a few rapid frames so a single mid-repaint frame doesn't yield a
// partial or missing result.
//
// The tap is observe-only. The screen model can't be reconstructed
// retroactively, so it must observe from session start — but it's only created
// for agents in agentSupportsSuggestions (claude today), so non-claude sessions
// carry no emulation cost.

import (
	"strings"
	"sync"
	"time"

	"github.com/hinshun/vt10x"
)

// agentSupportsSuggestions reports whether composer-suggestion extraction (#38)
// is enabled for an agent runtime. Claude-only for now: other agents render the
// composer differently and would yield stray styled text, so we leave the tap
// observe-only for them until each is validated. Add agents here as they're
// confirmed.
func agentSupportsSuggestions(agent string) bool {
	return agent == "claude"
}

// screenTap maintains a virtual terminal screen model fed by the raw PTY byte
// stream. Safe for concurrent Write/render/resize.
type screenTap struct {
	mu    sync.Mutex
	term  vt10x.Terminal
	carry []byte // incomplete trailing escape sequence held for the faint rewrite
}

// greyFaintReplacement is the SGR foreground vt10x DOES track, substituted for
// the faint (SGR 2) parameter it ignores. 240 is in the 256-colour greyscale
// ramp, so colorIsDim recognises it as ghost styling.
const greyFaintReplacement = "38;5;240"

// maxEscCarry caps the held incomplete-escape buffer so a never-terminating
// sequence can't grow it without bound. Any real SGR/CSI is far shorter.
const maxEscCarry = 64

// rewriteFaintToGrey scans data for SGR sequences containing the faint
// parameter (SGR 2) and rewrites that parameter to an explicit grey foreground.
// vt10x has no handler for SGR 2, so faint text would otherwise land in the
// grid default-coloured — indistinguishable from normal text. Claude Code
// renders the composer ghost-suggestion with faint, so this is what makes the
// suggestion detectable. An incomplete trailing escape sequence is returned as
// carry and must be prepended on the next call, so sequences split across PTY
// reads are still rewritten.
func rewriteFaintToGrey(carry, p []byte) (out, newCarry []byte) {
	data := p
	if len(carry) > 0 {
		data = append(append(make([]byte, 0, len(carry)+len(p)), carry...), p...)
	}
	out = make([]byte, 0, len(data)+16)
	i := 0
	for i < len(data) {
		if data[i] != 0x1b { // not ESC
			out = append(out, data[i])
			i++
			continue
		}
		tail := data[i:]
		// Need at least ESC '[' to be a CSI; if the next byte hasn't arrived,
		// carry the lone ESC.
		if len(tail) < 2 {
			if len(tail) <= maxEscCarry {
				return out, append(newCarry, tail...)
			}
			out = append(out, tail...)
			return out, newCarry
		}
		if data[i+1] != '[' {
			out = append(out, data[i]) // some other escape; copy ESC and move on
			i++
			continue
		}
		// CSI: scan for the final byte (0x40–0x7e).
		j := i + 2
		for j < len(data) && !(data[j] >= 0x40 && data[j] <= 0x7e) {
			j++
		}
		if j >= len(data) { // incomplete CSI
			if len(tail) <= maxEscCarry {
				return out, append(newCarry, tail...)
			}
			out = append(out, tail...)
			return out, newCarry
		}
		if data[j] == 'm' { // SGR — rewrite faint
			out = append(out, 0x1b, '[')
			out = append(out, rewriteSGRFaint(data[i+2:j])...)
			out = append(out, 'm')
		} else {
			out = append(out, data[i:j+1]...)
		}
		i = j + 1
	}
	return out, newCarry
}

// rewriteSGRFaint replaces a standalone faint parameter ("2") among
// semicolon-separated SGR params with greyFaintReplacement, leaving everything
// else untouched. "2" is unambiguous — faint is the only SGR with that code
// (22 = normal intensity is a different token).
func rewriteSGRFaint(params []byte) []byte {
	if len(params) == 0 {
		return params
	}
	parts := strings.Split(string(params), ";")
	changed := false
	for k, p := range parts {
		if p == "2" {
			parts[k] = greyFaintReplacement
			changed = true
		}
	}
	if !changed {
		return params
	}
	return []byte(strings.Join(parts, ";"))
}

func newScreenTap(cols, rows int) *screenTap {
	if cols <= 0 {
		cols = 80
	}
	if rows <= 0 {
		rows = 24
	}
	return &screenTap{term: vt10x.New(vt10x.WithSize(cols, rows))}
}

// Write feeds raw PTY bytes into the screen model. Observe-only: it never
// returns an error to the caller and must never block the real data path.
func (s *screenTap) Write(p []byte) {
	if s == nil {
		return
	}
	s.mu.Lock()
	out, carry := rewriteFaintToGrey(s.carry, p)
	s.carry = carry
	s.term.Write(out)
	s.mu.Unlock()
}

func (s *screenTap) resize(cols, rows int) {
	if s == nil || cols <= 0 || rows <= 0 {
		return
	}
	s.mu.Lock()
	s.term.Resize(cols, rows)
	s.mu.Unlock()
}

// vtAttrItalic mirrors vt10x's unexported attrItalic bit (1<<4). The bit
// positions are fixed VT attributes, but if vt10x is ever swapped out this is
// the one place that assumption lives.
const vtAttrItalic int16 = 1 << 4

// composerMarkers are the glyphs an agent uses to mark its input line. Claude
// Code uses ❯ (U+276F). Kept deliberately narrow: a bare '>' would also match
// non-composer rows (e.g. a faint markdown blockquote in the transcript), which
// rewriteFaintToGrey makes ghost-styled. Add other agents' markers alongside
// their entry in agentSupportsSuggestions, once validated.
var composerMarkers = []rune{'❯'}

// ghostStyled reports whether a cell carries the "ghost suggestion" styling —
// italic, or a dim/grey foreground — as opposed to the default styling of
// user-typed text. This is the grep predicate: the suggestion is the styled
// run on the composer line.
//
// Claude renders the suggestion with SGR 2 (faint), which vt10x has no handler
// for and would drop. rewriteFaintToGrey (run in Write, before bytes hit the
// grid) rewrites faint → grey 240, which colorIsDim then recognises — so by the
// time a cell reaches here, faint already reads as a dim foreground.
func ghostStyled(g vt10x.Glyph) bool {
	if g.Char == ' ' || g.Char == 0 {
		return false
	}
	if g.Mode&vtAttrItalic != 0 {
		return true
	}
	return colorIsDim(g.FG)
}

// colorIsDim reports whether a vt10x foreground colour reads as a dim grey —
// the usual ghost-text colour. Covers ANSI bright-black (8), the 256-colour
// greyscale ramp (232–255), and low-luminance packed RGB.
func colorIsDim(c vt10x.Color) bool {
	if c == vt10x.DefaultFG || c == vt10x.DefaultBG {
		return false
	}
	if c == 8 { // ANSI bright black / grey
		return true
	}
	if c >= 232 && c <= 255 { // xterm-256 greyscale ramp
		return true
	}
	if c > 255 && c < vt10x.DefaultFG { // packed RGB
		r := int(c>>16) & 0xff
		g := int(c>>8) & 0xff
		b := int(c) & 0xff
		lum := (r*299 + g*587 + b*114) / 1000
		return lum >= 0x30 && lum <= 0xb0 // mid-grey band
	}
	return false
}

// composerSuggestion scans the screen from the bottom up for the agent's
// composer input line and returns the styled "ghost suggestion" text on it, or
// "" if none is found. The suggestion is the trailing run of ghost-styled cells
// after the prompt marker — i.e. text the agent is proposing, not what the user
// has typed (default styling).
func (s *screenTap) composerSuggestion() string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	s.term.Lock()
	defer s.term.Unlock()
	cols, rows := s.term.Size()

	for y := rows - 1; y >= 0; y-- {
		runes := make([]rune, cols)
		styled := make([]bool, cols)
		marker := -1
		for x := 0; x < cols; x++ {
			g := s.term.Cell(x, y)
			ch := g.Char
			if ch == 0 {
				ch = ' '
			}
			runes[x] = ch
			styled[x] = ghostStyled(g)
			if marker < 0 && isComposerMarker(ch) {
				marker = x
			}
		}
		if marker < 0 {
			continue
		}
		if sug := suggestionFromRow(runes, styled, marker); sug != "" {
			return sug
		}
	}
	return ""
}

func isComposerMarker(ch rune) bool {
	for _, m := range composerMarkers {
		if ch == m {
			return true
		}
	}
	return false
}

// suggestionFromRow extracts the ghost suggestion from a single composer row:
// the text spanning the first to the last ghost-styled cell after the marker.
// Interior spaces are not themselves "styled" but fall within the span, so
// multi-word suggestions stay intact. Returns "" if the span has no letters
// (avoids matching stray styling).
func suggestionFromRow(runes []rune, styled []bool, marker int) string {
	first, last := -1, -1
	for x := marker + 1; x < len(runes); x++ {
		if styled[x] {
			if first < 0 {
				first = x
			}
			last = x
		}
	}
	if first < 0 {
		return ""
	}
	text := strings.TrimSpace(string(runes[first : last+1]))
	if !hasLetter(text) {
		return ""
	}
	return text
}

func hasLetter(s string) bool {
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
			return true
		}
	}
	return false
}

// suggestion returns the agent's current composer ghost-suggestion, voted
// across a few rapid samples so a mid-repaint frame doesn't yield a partial or
// missing result. Returns the most frequently seen non-empty suggestion (ties
// broken toward the most recent). "" when there is no suggestion or the tap is
// off. Blocks ~120ms — run off the hot path.
func (s *screenTap) suggestion() string {
	if s == nil {
		return ""
	}
	counts := map[string]int{}
	recent := map[string]int{}
	for i := 0; i < 4; i++ {
		if i > 0 {
			time.Sleep(40 * time.Millisecond)
		}
		if v := s.composerSuggestion(); v != "" {
			counts[v]++
			recent[v] = i
		}
	}
	best := ""
	bestCount, bestRecent := 0, -1
	for v, c := range counts {
		if c > bestCount || (c == bestCount && recent[v] > bestRecent) {
			best, bestCount, bestRecent = v, c, recent[v]
		}
	}
	return best
}

