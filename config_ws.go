//go:build darwin || linux

package main

import (
	"encoding/json"
	"log"
)

// handleConfigGet replies with the host-scope config and, when a project is
// supplied, that project's overrides. Both are bare key→value maps (the
// on-disk project prefix is stripped); device_id is excluded. The app overlays
// project over host for an effective view and marks overridden fields.
//
// Device-scoped (relay_id ""), correlated by request_id.
func (d *DaemonWS) handleConfigGet(data []byte) {
	var msg struct {
		RequestID string `json:"request_id"`
		Project   string `json:"project"`
	}
	if json.Unmarshal(data, &msg) != nil || msg.RequestID == "" {
		log.Printf("daemon-ws: config_get missing request_id")
		return
	}

	// Config maps are nested under a single "config" object so their "host" /
	// "project" keys can't collide with the apps' top-level WSMessage fields.
	config := map[string]interface{}{
		"host": listScoped(scopeHost, ""),
	}
	if msg.Project != "" {
		config["project"] = listScoped(scopeProject, msg.Project)
		config["project_name"] = msg.Project
	}
	resp := map[string]interface{}{
		"type":       "config_loaded",
		"request_id": msg.RequestID,
		"config":     config,
	}
	out, err := json.Marshal(resp)
	if err != nil {
		log.Printf("daemon-ws: config_get: marshal config_loaded: %v", err)
		return
	}
	d.ws.SendText(out)
}

// handleConfigSet applies a batched set/unset at the requested scope and replies
// config_set_result. It validates first (device_id forbidden, enum keys), so an
// invalid batch writes nothing. Only the named keys are touched — see
// applyConfigBatch's no-clobber guarantee. The app refetches with config_get
// after a success to refresh its view.
//
// Device-scoped (relay_id ""), correlated by request_id.
func (d *DaemonWS) handleConfigSet(data []byte) {
	var msg struct {
		RequestID string            `json:"request_id"`
		Scope     string            `json:"scope"`
		Project   string            `json:"project"`
		Set       map[string]string `json:"set"`
		Unset     []string          `json:"unset"`
	}
	if json.Unmarshal(data, &msg) != nil || msg.RequestID == "" {
		log.Printf("daemon-ws: config_set missing request_id")
		return
	}

	reply := func(success bool, errMsg string) {
		resp := map[string]interface{}{
			"type":       "config_set_result",
			"request_id": msg.RequestID,
			"success":    success,
		}
		if errMsg != "" {
			resp["error"] = errMsg
		}
		if out, err := json.Marshal(resp); err == nil {
			d.ws.SendText(out)
		}
	}

	scope := msg.Scope
	if scope == "" {
		scope = scopeHost
	}
	if scope != scopeHost && scope != scopeProject {
		reply(false, "invalid_scope")
		return
	}
	if scope == scopeProject && msg.Project == "" {
		reply(false, "missing_project")
		return
	}
	if len(msg.Set) == 0 && len(msg.Unset) == 0 {
		reply(true, "") // nothing to do
		return
	}
	if errCode := validateConfigBatch(msg.Set, msg.Unset); errCode != "" {
		reply(false, errCode)
		return
	}

	// If this batch touches the idle-notification threshold, snapshot each live
	// session's resolved value before the write so we can tell the server which
	// sessions actually changed afterwards. Recomputing per session and diffing
	// captures inheritance for free: a host-scope change reaches sessions that
	// inherit it but skips ones whose project overrides the key, and a
	// project-scope change reaches only that project's sessions — without
	// reasoning about which scope was edited.
	idleTouched := configBatchTouches(msg.Set, msg.Unset, configKeyIdleNotifyAfter)
	var liveProjects map[string]string
	var idleBefore map[string]int
	if idleTouched {
		liveProjects = d.liveSessionProjects()
		idleBefore = resolveIdleSecsByRelay(liveProjects)
	}

	if err := applyConfigBatch(scope, msg.Project, msg.Set, msg.Unset); err != nil {
		log.Printf("daemon-ws: config_set apply: %v", err)
		reply(false, "write_error")
		return
	}
	reply(true, "")
	log.Printf("daemon-ws: config_set applied scope=%s project=%q set=%d unset=%d", scope, msg.Project, len(msg.Set), len(msg.Unset))

	if idleTouched {
		for id, after := range resolveIdleSecsByRelay(liveProjects) {
			if after != idleBefore[id] {
				log.Printf("daemon-ws: idle threshold for session %s changed %ds→%ds, notifying server", id, idleBefore[id], after)
				d.sendSessionIdleConfig(id, after)
			}
		}
	}
}

// configBatchTouches reports whether a set/unset batch modifies the given key.
func configBatchTouches(set map[string]string, unset []string, key string) bool {
	if _, ok := set[key]; ok {
		return true
	}
	for _, k := range unset {
		if k == key {
			return true
		}
	}
	return false
}

// resolveIdleSecsByRelay returns relay_id → resolved idle_notify_after_secs for
// the given relay_id → project map, caching per project so sessions sharing a
// project don't each re-read the config file.
func resolveIdleSecsByRelay(projects map[string]string) map[string]int {
	cache := map[string]int{}
	out := make(map[string]int, len(projects))
	for id, proj := range projects {
		secs, ok := cache[proj]
		if !ok {
			secs = resolveIdleNotifyAfterSecs(proj)
			cache[proj] = secs
		}
		out[id] = secs
	}
	return out
}
