package main

import "time"

// The host marshals pluginapi structs with encoding/json and no tags of their
// own, so every field name here is the exported Go name verbatim. Byte slices
// arrive base64-encoded and decode straight into []byte; time.Time is RFC3339;
// time.Duration is an integer nanosecond count. Metadata maps pass through as
// plain JSON, with non-JSON values already normalized by the host.

// usageRecord mirrors pluginapi.UsageRecord for usage.handle.
type usageRecord struct {
	RequestID       string
	TraceID         string
	SelectionID     string
	Provider        string
	BaseURL         string
	ExecutorType    string
	Model           string
	Alias           string
	APIKey          string
	IsNativeKey     bool
	AccessProvider  string
	SessionID       string
	ParentSessionID string
	AuthID          string
	AuthIndex       string
	AuthType        string
	Source          string

	ReasoningEffort      string
	ServiceTier          string
	EffectiveServiceTier string
	ResponseServiceTier  string
	ResponseModel        string

	Generate    bool
	Stream      bool
	RequestedAt time.Time
	Latency     time.Duration
	TTFT        time.Duration
	Failed      bool
	Failure     usageFailure
	Detail      usageDetail
}

type usageFailure struct {
	StatusCode int
	Body       string
}

// usageDetail carries the exact token counts. The per-modality pointer fields
// are deliberately absent: they are always nil for our purposes and decoding
// them would cost allocations on the hot path.
type usageDetail struct {
	InputTokens         int64
	OutputTokens        int64
	ReasoningTokens     int64
	CachedTokens        int64
	CacheReadTokens     int64
	CacheCreationTokens int64
	TotalTokens         int64
}

// streamChunkRequest mirrors pluginapi.StreamChunkInterceptRequest as sent for
// response.intercept_stream_chunk. ChunkIndex is -1 for the header-init call.
// At schema version 5 the host omits HistoryChunks and the request bodies on
// payload chunks, so those fields stay empty here.
type streamChunkRequest struct {
	RequestID      string
	SourceFormat   string
	Model          string
	RequestedModel string
	Body           []byte
	ChunkIndex     int
	Metadata       map[string]any

	// HostCallbackID is added by the host's RPC wrapper.
	HostCallbackID string
}

// streamChunkResponse mirrors pluginapi.StreamChunkInterceptResponse. All zero
// values mean "leave the chunk exactly as it is", which is what a pure observer
// must return.
type streamChunkResponse struct{}

// requestCompletion mirrors pluginapi.RequestCompletion for request.complete.
type requestCompletion struct {
	RequestID      string
	TraceID        string
	SourceFormat   string
	Model          string
	RequestedModel string
	Stream         bool
	Outcome        string
	StatusCode     int
	Error          string
	StartedAt      time.Time
	CompletedAt    time.Time
	Metadata       map[string]any

	HostCallbackID string `json:"host_callback_id,omitempty"`
}

// managementRequest mirrors pluginapi.ManagementRequest plus the host's
// callback id. It serves both management.handle and resource routes.
type managementRequest struct {
	Method  string
	Path    string
	Query   map[string][]string
	Body    []byte
	Headers map[string][]string

	HostCallbackID string `json:"host_callback_id,omitempty"`
}

// managementResponse mirrors pluginapi.ManagementResponse. Body is []byte so
// it round-trips as base64.
type managementResponse struct {
	StatusCode int
	Headers    map[string][]string
	Body       []byte
}

// managementRoute mirrors pluginapi.ManagementRoute. Handler is nil here
// because the host substitutes its own RPC adapter for every route it accepts;
// sending null keeps the field's presence irrelevant to validation.
type managementRoute struct {
	Method      string
	Path        string
	Menu        string
	Description string
	Handler     any
}

type resourceRoute struct {
	Path        string
	Menu        string
	Description string
	Handler     any
}

type managementRegistrationResponse struct {
	Routes    []managementRoute `json:"routes,omitempty"`
	Resources []resourceRoute   `json:"resources,omitempty"`
}

// hostAuthListResponse is the host.auth.list callback result.
type hostAuthListResponse struct {
	Files []hostAuthFileEntry `json:"files"`
}

type hostAuthFileEntry struct {
	ID        string `json:"id,omitempty"`
	AuthIndex string `json:"auth_index,omitempty"`
	Name      string `json:"name"`
	Type      string `json:"type,omitempty"`
	Provider  string `json:"provider,omitempty"`
	Label     string `json:"label,omitempty"`
}

// configField mirrors pluginapi.ConfigField, advertised so the management UI
// can render our settings.
type configField struct {
	Name        string   `json:"Name"`
	Type        string   `json:"Type"`
	Default     any      `json:"Default"`
	EnumValues  []string `json:"EnumValues"`
	Description string   `json:"Description"`
}

type pluginMetadata struct {
	Name             string        `json:"Name"`
	Version          string        `json:"Version"`
	Author           string        `json:"Author"`
	GitHubRepository string        `json:"GitHubRepository"`
	Logo             string        `json:"Logo"`
	ConfigFields     []configField `json:"ConfigFields"`
}

// rpcCapabilities mirrors the host's decoder. Every flag must be present by
// name for the host to wire the matching capability.
type rpcCapabilities struct {
	RequestLifecyclePlugin bool `json:"request_lifecycle_plugin"`
	StreamChunkInterceptor bool `json:"response_stream_interceptor"`
	UsagePlugin            bool `json:"usage_plugin"`
	ManagementAPI          bool `json:"management_api"`
}

type pluginRegistration struct {
	SchemaVersion uint32          `json:"schema_version"`
	Metadata      pluginMetadata  `json:"metadata"`
	Capabilities  rpcCapabilities `json:"capabilities"`
}

// rpcLifecycleRequest is the plugin.register / plugin.reconfigure payload.
type rpcLifecycleRequest struct {
	ConfigYAML         []byte `json:"config_yaml"`
	SchemaVersion      uint32 `json:"schema_version"`
	MultiAuthProviders bool   `json:"multi_auth_providers,omitempty"`
}

// emptyResponse is the result body for hook methods that return nothing.
type emptyResponse struct{}
