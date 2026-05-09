//go:build darwin || linux

package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// SkillFile is an optional auxiliary file bundled with a skill (script,
// reference, asset). Path is relative to the skill directory and must stay
// within it.
type SkillFile struct {
	Path    string `json:"path"`
	Content string `json:"content"`
	Mode    uint32 `json:"mode,omitempty"`
}

// Skill is a server-delivered skill bundle conforming to the agentskills.io
// open standard. SkillMD is the full SKILL.md contents (frontmatter + body).
type Skill struct {
	Name    string      `json:"name"`
	SkillMD string      `json:"skill_md"`
	Files   []SkillFile `json:"files,omitempty"`
}

// greenlightNamespace is the subdirectory under each agent's skills root that
// holds greenlight-installed skills. Isolating them under a single dir keeps
// cleanup trivial (rm -rf) and avoids colliding with user-authored skills.
const greenlightNamespace = "_greenlight"

// skillNameRe enforces the agentskills.io spec: lowercase letters, digits, and
// hyphens; no leading/trailing/consecutive hyphens; ≤64 chars.
var skillNameRe = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9]|-[a-z0-9])*$`)

// frontmatterNameRe extracts the `name:` field from a SKILL.md frontmatter
// block. We don't pull in a YAML lib for two simple string fields.
var frontmatterNameRe = regexp.MustCompile(`(?m)^name:\s*(.+?)\s*$`)

// installSkills writes each skill to <cwd>/<skillsRoot(agent)>/_greenlight/<name>/.
// Returns the absolute path to the namespace dir that was created (for cleanup),
// or "" if no skills were installed. Per-skill failures are logged and skipped
// rather than gating agent launch.
func installSkills(agent, cwd string, skills []Skill) string {
	if len(skills) == 0 {
		return ""
	}
	root := skillsRoot(agent)
	if root == "" {
		log.Printf("Skills: agent %q does not support skills, skipping %d skill(s)", agent, len(skills))
		return ""
	}
	nsDir := filepath.Join(cwd, root, greenlightNamespace)
	if err := os.MkdirAll(nsDir, 0755); err != nil {
		log.Printf("Skills: cannot create %s: %v", nsDir, err)
		return ""
	}
	installed := 0
	for _, sk := range skills {
		if err := writeSkill(nsDir, sk); err != nil {
			log.Printf("Skills: skipping %q: %v", sk.Name, err)
			continue
		}
		installed++
	}
	if installed == 0 {
		// Nothing landed; remove the empty namespace dir so we don't leave junk.
		os.Remove(nsDir)
		return ""
	}
	log.Printf("Skills: installed %d skill(s) for %s at %s", installed, agent, nsDir)
	return nsDir
}

// writeSkill validates and materializes a single skill bundle.
func writeSkill(nsDir string, sk Skill) error {
	if !skillNameRe.MatchString(sk.Name) || len(sk.Name) > 64 {
		return fmt.Errorf("invalid skill name %q", sk.Name)
	}
	if sk.SkillMD == "" {
		return fmt.Errorf("empty SKILL.md")
	}
	// The spec requires the frontmatter `name` to match the parent directory.
	// Validate so we fail loudly rather than silently shipping a skill the
	// agent will reject.
	if m := frontmatterNameRe.FindStringSubmatch(sk.SkillMD); m == nil {
		return fmt.Errorf("SKILL.md missing `name:` frontmatter field")
	} else if strings.TrimSpace(m[1]) != sk.Name {
		return fmt.Errorf("SKILL.md name %q does not match skill %q", m[1], sk.Name)
	}

	skillDir := filepath.Join(nsDir, sk.Name)
	if err := os.MkdirAll(skillDir, 0755); err != nil {
		return fmt.Errorf("mkdir %s: %w", skillDir, err)
	}
	skillMD := filepath.Join(skillDir, "SKILL.md")
	if err := os.WriteFile(skillMD, []byte(sk.SkillMD), 0644); err != nil {
		return fmt.Errorf("write SKILL.md: %w", err)
	}

	for _, f := range sk.Files {
		if err := writeSkillFile(skillDir, f); err != nil {
			log.Printf("Skills: %q file %q: %v", sk.Name, f.Path, err)
		}
	}
	return nil
}

// writeSkillFile materializes a bundled file under skillDir, refusing any path
// that escapes the skill directory.
func writeSkillFile(skillDir string, f SkillFile) error {
	if f.Path == "" {
		return fmt.Errorf("empty path")
	}
	clean := filepath.Clean(f.Path)
	if filepath.IsAbs(clean) || strings.HasPrefix(clean, "..") || strings.Contains(clean, "/../") {
		return fmt.Errorf("path %q escapes skill dir", f.Path)
	}
	dest := filepath.Join(skillDir, clean)
	// Re-check after Join in case of edge cases.
	rel, err := filepath.Rel(skillDir, dest)
	if err != nil || strings.HasPrefix(rel, "..") {
		return fmt.Errorf("path %q escapes skill dir", f.Path)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
		return err
	}
	mode := os.FileMode(0644)
	if f.Mode != 0 {
		// Cap to 0777 to discourage setuid bits.
		mode = os.FileMode(f.Mode) & 0777
	}
	return os.WriteFile(dest, []byte(f.Content), mode)
}

// listSkills scans the agent's _greenlight namespace dir and returns the names
// of skills found there. Names are read from the SKILL.md frontmatter rather
// than the directory name, so the result reflects what the agent will actually
// see. Skills with missing/unreadable SKILL.md or invalid frontmatter are
// skipped silently. Returns an empty slice if the dir doesn't exist.
func listSkills(agent, cwd string) []string {
	root := skillsRoot(agent)
	if root == "" {
		return nil
	}
	nsDir := filepath.Join(cwd, root, greenlightNamespace)
	entries, err := os.ReadDir(nsDir)
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(nsDir, e.Name(), "SKILL.md"))
		if err != nil {
			continue
		}
		m := frontmatterNameRe.FindStringSubmatch(string(data))
		if m == nil {
			continue
		}
		name := strings.TrimSpace(m[1])
		if !skillNameRe.MatchString(name) {
			continue
		}
		names = append(names, name)
	}
	return names
}

// removeSkills removes the _greenlight namespace dir from the agent's skills
// root. Walks up and prunes empty parent dirs we created (skills, .agents,
// etc.) so a clean checkout is left behind. Caller should guard with
// hasOtherSessions.
func removeSkills(agent, cwd string) {
	root := skillsRoot(agent)
	if root == "" {
		return
	}
	nsDir := filepath.Join(cwd, root, greenlightNamespace)
	if err := os.RemoveAll(nsDir); err != nil {
		log.Printf("Skills: failed to remove %s: %v", nsDir, err)
		return
	}
	// Prune empty ancestors up to (but not including) cwd.
	dir := filepath.Dir(nsDir)
	cwdAbs, _ := filepath.Abs(cwd)
	for {
		abs, err := filepath.Abs(dir)
		if err != nil || abs == cwdAbs || abs == "/" {
			return
		}
		if err := os.Remove(dir); err != nil {
			return // not empty, or other error — stop pruning
		}
		dir = filepath.Dir(dir)
	}
}
