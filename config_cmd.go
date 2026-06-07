//go:build darwin || linux

package main

import (
	"fmt"
	"os"
	"sort"
)

// runConfig implements `greenlight config get/set/unset/list [--project P]`.
// It is the generic plumbing the `agent` subcommand and the daemon's
// config_get/config_set control frames both delegate to. device_id is never
// readable or writable through it; agent and tickets_provider are
// enum-validated.
//
// All output goes to stderr — the outer terminal may be in raw mode, and stdout
// is reserved for relay traffic (see the cli/CLAUDE.md "never print to stdout"
// convention).
func runConfig(args []string) {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		printConfigUsage()
		if len(args) == 0 {
			os.Exit(1)
		}
		return
	}

	action := args[0]
	rest := args[1:]

	// Parse a leading/trailing `--project P` out of rest.
	project := ""
	var positional []string
	for i := 0; i < len(rest); i++ {
		if rest[i] == "--project" || rest[i] == "-p" {
			if i+1 >= len(rest) {
				fmt.Fprintln(os.Stderr, "greenlight: --project requires a value")
				os.Exit(1)
			}
			project = rest[i+1]
			i++
			continue
		}
		positional = append(positional, rest[i])
	}

	scope := scopeHost
	if project != "" {
		scope = scopeProject
	}

	switch action {
	case "list":
		// Show the effective view (host defaults overlaid with project
		// overrides), hiding the host/project storage split from the human-
		// facing CLI. The daemon config_get path keeps the scopes separate so
		// the apps can mark overrides.
		entries := effectiveConfig(project)
		for _, k := range sortedStringKeys(entries) {
			fmt.Fprintf(os.Stderr, "%s=%s\n", k, entries[k])
		}

	case "get":
		if len(positional) != 1 {
			fmt.Fprintln(os.Stderr, "Usage: greenlight config get [--project P] <key>")
			os.Exit(1)
		}
		key := positional[0]
		if key == configKeyDeviceID {
			fmt.Fprintln(os.Stderr, "greenlight: device_id is not available via config")
			os.Exit(1)
		}
		// Resolved value: project override falls back to the host default, so
		// `get` reports the value the session would actually use.
		fmt.Fprintf(os.Stderr, "%s\n", resolveConfig(project, key))

	case "set":
		if len(positional) != 2 {
			fmt.Fprintln(os.Stderr, "Usage: greenlight config set [--project P] <key> <value>")
			os.Exit(1)
		}
		key, value := positional[0], positional[1]
		set := map[string]string{key: value}
		if errCode := validateConfigBatch(set, nil); errCode != "" {
			fmt.Fprintf(os.Stderr, "greenlight: %s\n", configErrorMessage(errCode))
			os.Exit(1)
		}
		if err := applyConfigBatch(scope, project, set, nil); err != nil {
			fmt.Fprintf(os.Stderr, "greenlight: %v\n", err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "Set %s=%s (%s)\n", key, value, scopeLabel(scope, project))

	case "unset":
		if len(positional) != 1 {
			fmt.Fprintln(os.Stderr, "Usage: greenlight config unset [--project P] <key>")
			os.Exit(1)
		}
		key := positional[0]
		if errCode := validateConfigBatch(nil, []string{key}); errCode != "" {
			fmt.Fprintf(os.Stderr, "greenlight: %s\n", configErrorMessage(errCode))
			os.Exit(1)
		}
		if err := applyConfigBatch(scope, project, nil, []string{key}); err != nil {
			fmt.Fprintf(os.Stderr, "greenlight: %v\n", err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "Unset %s (%s)\n", key, scopeLabel(scope, project))

	default:
		fmt.Fprintf(os.Stderr, "greenlight: unknown config action %q\n", action)
		printConfigUsage()
		os.Exit(1)
	}
}

func printConfigUsage() {
	fmt.Fprintf(os.Stderr, "Usage: greenlight config <get|set|unset|list> [--project P] [args]\n\n")
	fmt.Fprintf(os.Stderr, "  config list  [--project P]              show the effective view (host defaults + project overrides)\n")
	fmt.Fprintf(os.Stderr, "  config get   [--project P] <key>        print the resolved value (project override, else host default)\n")
	fmt.Fprintf(os.Stderr, "  config set   [--project P] <key> <val>  set a value\n")
	fmt.Fprintf(os.Stderr, "  config unset [--project P] <key>        remove a value\n\n")
	fmt.Fprintf(os.Stderr, "Without --project, set/unset operate on host (global) defaults and get/list show host values.\n")
	fmt.Fprintf(os.Stderr, "Well-known keys: agent, tickets_provider, tickets_secret, shim_secret.\n")
	fmt.Fprintf(os.Stderr, "device_id is managed by `greenlight register` and is not exposed here.\n")
}

// configErrorMessage maps a wire error code to a human-readable CLI message.
func configErrorMessage(code string) string {
	switch code {
	case "device_id_forbidden":
		return "device_id cannot be set via config"
	case "invalid_agent":
		return fmt.Sprintf("invalid agent (supported: %s)", joinSortedSet(knownAgents))
	case "invalid_provider":
		return fmt.Sprintf("invalid tickets_provider (supported: %s)", joinSortedSet(knownTicketProviders))
	case "invalid_key":
		return "invalid key (no '=', whitespace, leading '#', or 'project.' prefix)"
	default:
		return code
	}
}

func scopeLabel(scope, project string) string {
	if scope == scopeProject {
		return "project " + project
	}
	return "host"
}

func sortedStringKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func joinSortedSet(m map[string]bool) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := ""
	for i, k := range keys {
		if i > 0 {
			out += ", "
		}
		out += k
	}
	return out
}
