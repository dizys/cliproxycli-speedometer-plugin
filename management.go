package main

import (
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// Route paths are relative to the host's management base path; the host
// normalizes and prefixes them. A resource path of "/" is rejected by the host
// (it trims to empty), so the dashboard gets an explicit segment.
const (
	snapshotPath  = "/speedometer/snapshot"
	dashboardPath = "/dashboard"
)

// iconSVG is the gauge mark the control panel shows beside this plugin. It is
// embedded rather than linked, because the dashboard is already a single
// self-contained document and the icon should not depend on GitHub staying
// reachable or on a branch name that can move.
//
//go:embed assets/icon.svg
var iconSVG []byte

// iconDataURI renders iconSVG as a data: URI. The host only reads metadata at
// plugin.register, before management.register tells the plugin its own resource
// path, so a self-contained URI is the only way to show the icon without
// hardcoding a URL that the host has not yet assigned.
func iconDataURI() string {
	return "data:image/svg+xml;base64," + base64.StdEncoding.EncodeToString(iconSVG)
}

// handleManagementRegister declares one JSON route plus the dashboard resource.
func handleManagementRegister(req []byte) ([]byte, error) {
	var registration struct {
		BasePath         string
		ResourceBasePath string
	}
	if len(req) > 0 {
		if err := json.Unmarshal(req, &registration); err != nil {
			return nil, fmt.Errorf("decode management registration: %w", err)
		}
	}
	plugin.resourceBasePath = strings.TrimRight(registration.ResourceBasePath, "/")

	cfg := plugin.store.config()
	resp := managementRegistrationResponse{
		// Handler is left nil: the host replaces it with its own RPC adapter for
		// every route it accepts, and rejects nothing for a null here.
		Routes: []managementRoute{{
			Method:      "GET",
			Path:        snapshotPath,
			Description: "Live token rates and per-dimension breakdowns",
		}},
	}
	if cfg.ShowDashboard {
		resp.Resources = append(resp.Resources, resourceRoute{
			Path:        dashboardPath,
			Menu:        "Token Speedometer",
			Description: "Real-time tokens per second dashboard",
		})
	}
	return okEnvelope(resp)
}

// handleManagement serves the JSON snapshot route and the dashboard resource.
func handleManagement(req []byte) ([]byte, error) {
	var request managementRequest
	if len(req) > 0 {
		if err := json.Unmarshal(req, &request); err != nil {
			return nil, fmt.Errorf("decode management request: %w", err)
		}
	}
	// The host may pass the full path or the path relative to its base, so match
	// on the suffix we registered rather than on an exact string.
	path := strings.TrimRight(request.Path, "/")
	if path == "" {
		path = dashboardPath
	}

	resourceBase := plugin.resourceBasePath
	switch {
	case path == snapshotPath || strings.HasSuffix(path, snapshotPath):
		return plugin.snapshotResponse(request.Query), nil
	case path == dashboardPath || strings.HasSuffix(path, dashboardPath) ||
		(resourceBase != "" && (path == resourceBase || strings.HasPrefix(path, resourceBase+"/"))):
		return dashboardResponse(), nil
	default:
		return managementError(404, "not found"), nil
	}
}

// snapshotResponse builds the JSON body the dashboard polls.
func (p *pluginInstance) snapshotResponse(query url.Values) []byte {
	cfg, state := p.store.snapshot()

	window := cfg.window()
	if raw := query.Get("window"); raw != "" {
		if seconds, errParse := time.ParseDuration(raw); errParse == nil && seconds >= time.Second {
			limit := time.Duration(bucketCount-2) * time.Second
			if seconds > limit {
				seconds = limit
			}
			window = seconds
		} else if seconds, errInt := parseSeconds(raw); errInt == nil {
			window = seconds
		}
	}

	now := time.Now()
	view := state.snapshot(now, cfg, window)
	if !cfg.ClientLabels {
		for i := range view.Inflight {
			view.Inflight[i].ClientLabel = ""
		}
		for i := range view.Dimensions["client"] {
			view.Dimensions["client"][i].Label = ""
		}
	}

	payload := map[string]any{
		"plugin": map[string]any{
			"name":    pluginName,
			"version": pluginVersion,
			"uptime":  now.Sub(p.startedAt).Seconds(),
			"window":  window.Seconds(),
			"live":    cfg.liveWindow().Seconds(),
			"series":  cfg.seriesSeconds,
			"ratio":   cfg.charsPerToken,
		},
		"data": view,
	}
	body, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		return managementErrorJSON(500, "encode snapshot failed")
	}
	return jsonResponse(200, body, "no-store")
}

// jsonResponse wraps a body in the management response envelope the host
// expects. A management handler's result is always a ManagementResponse, never
// a bare body.
func jsonResponse(status int, body []byte, cacheControl string) []byte {
	headers := map[string][]string{
		"Content-Type": {"application/json; charset=utf-8"},
	}
	if cacheControl != "" {
		headers["Cache-Control"] = []string{cacheControl}
	}
	raw, _ := okEnvelope(managementResponse{
		StatusCode: status,
		Headers:    headers,
		Body:       body,
	})
	return raw
}

func parseSeconds(raw string) (time.Duration, error) {
	var n int
	if _, err := fmt.Sscanf(raw, "%d", &n); err != nil || n <= 0 {
		return 0, fmt.Errorf("invalid window %q", raw)
	}
	return time.Duration(n) * time.Second, nil
}

// managementError builds a management response envelope carrying a JSON error.
func managementError(status int, message string) []byte {
	return managementErrorJSON(status, message)
}

func managementErrorJSON(status int, message string) []byte {
	body, _ := json.Marshal(map[string]any{"error": message})
	return jsonResponse(status, body, "no-store")
}

// dashboardResponse serves the self-contained HTML dashboard.
func dashboardResponse() []byte {
	raw, _ := okEnvelope(managementResponse{
		StatusCode: 200,
		Headers: map[string][]string{
			"Content-Type":  {"text/html; charset=utf-8"},
			"Cache-Control": {"no-store"},
		},
		Body: []byte(dashboardHTML),
	})
	return raw
}
