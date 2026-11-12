package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Method names, copied from sdk/pluginabi so the strings cannot drift.
const (
	methodPluginRegister    = "plugin.register"
	methodPluginReconfigure = "plugin.reconfigure"
	methodPluginQuiesce     = "plugin.quiesce"
	methodPluginShutdown    = "plugin.shutdown"

	methodUsageHandle = "usage.handle"

	methodStreamChunk = "response.intercept_stream_chunk"
	methodComplete    = "request.complete"

	methodManagementRegister = "management.register"
	methodManagementHandle   = "management.handle"

	methodHostAuthList = "host.auth.list"
)

const (
	pluginName   = "CLIProxyAPI Token Speedometer"
	pluginAuthor = "dizys"
	pluginRepo   = "https://github.com/dizys/cliproxycli-speedometer-plugin"
)

// pluginVersion is overridden at build time with -X main.pluginVersion=<version>
// so the version a release reports matches its filename and release tag. The
// fallback only applies to a bare `go build`.
var pluginVersion = "0.1.7"

// plugin is the process-wide singleton. The host loads one shared library per
// plugin instance, so package-level state is the correct scope.
var plugin = newPluginInstance()

type pluginInstance struct {
	store *configStore

	// parsers holds one streamParser per in-flight request id, created on the
	// header-init call and removed when the stream completes.
	parsersMu sync.Mutex
	parsers   map[string]*streamParser

	// authMu guards the last auth directory listing.
	authMu       sync.Mutex
	authListedAt time.Time

	// resourceBasePath is learned at management.register so the dashboard can
	// emit correct absolute URLs.
	resourceBasePath string

	// startedAt is used for uptime in the snapshot.
	startedAt time.Time
}

func newPluginInstance() *pluginInstance {
	return &pluginInstance{
		store:     newConfigStore(),
		parsers:   make(map[string]*streamParser),
		startedAt: time.Now(),
	}
}

// handleMethod is the single entry point the C ABI calls. A returned error
// becomes an ok:false envelope; returning nil with a marshaled result means the
// host transport succeeded.
func handleMethod(method string, req []byte) ([]byte, error) {
	switch method {
	case methodPluginRegister, methodPluginReconfigure:
		return handleRegister(req)
	case methodPluginQuiesce, methodPluginShutdown:
		return okEnvelope(emptyResponse{})
	case methodUsageHandle:
		return handleUsage(req)
	case methodStreamChunk:
		return handleStreamChunk(req)
	case methodComplete:
		return handleComplete(req)
	case methodManagementRegister:
		return handleManagementRegister(req)
	case methodManagementHandle:
		return handleManagement(req)
	default:
		return errorEnvelope("unknown_method", "unknown method "+method), nil
	}
}

// handleRegister parses the host's config payload and declares capabilities.
func handleRegister(req []byte) ([]byte, error) {
	var lifecycle rpcLifecycleRequest
	if len(req) > 0 {
		if err := json.Unmarshal(req, &lifecycle); err != nil {
			return nil, fmt.Errorf("decode lifecycle request: %w", err)
		}
	}
	if err := plugin.store.reload(lifecycle.ConfigYAML); err != nil {
		// A malformed config must not disable the meter; fall back to defaults and
		// keep serving rather than failing registration.
		plugin.log("warn", "invalid plugin config, using defaults: "+err.Error())
		_ = plugin.store.reload(nil)
	}

	return okEnvelope(pluginRegistration{
		SchemaVersion: schemaVersion,
		Metadata: pluginMetadata{
			Name:             pluginName,
			Version:          pluginVersion,
			Author:           pluginAuthor,
			GitHubRepository: pluginRepo,
			Logo:             iconDataURI(),
			ConfigFields:     configFields(),
		},
		Capabilities: rpcCapabilities{
			RequestLifecyclePlugin: true,
			StreamChunkInterceptor: true,
			UsagePlugin:            true,
			ManagementAPI:          true,
		},
	})
}

// handleUsage folds one exact usage record into the aggregates.
func handleUsage(req []byte) ([]byte, error) {
	if len(req) == 0 {
		return okEnvelope(emptyResponse{})
	}
	var record usageRecord
	if err := json.Unmarshal(req, &record); err != nil {
		return nil, fmt.Errorf("decode usage record: %w", err)
	}
	now := time.Now()
	state := plugin.store.meter()
	if !record.Generate {
		// The host normalizes omitted values to true, so false is explicit: the
		// request produced no tokens and must not skew rates or request counts.
		return okEnvelope(emptyResponse{})
	}
	state.recordFinal(record, now)

	// The exact record supersedes any live estimate for the same request. The
	// two hooks use different request id spaces, so retire by trace as well as
	// by id; whichever matches is harmless when both do.
	plugin.dropParser(record.RequestID)
	state.dropStream(record.RequestID)
	state.dropByTrace(record.TraceID)
	return okEnvelope(emptyResponse{})
}

// handleStreamChunk estimates token growth for a streaming response.
func handleStreamChunk(req []byte) ([]byte, error) {
	var chunk streamChunkRequest
	if len(req) > 0 {
		if err := json.Unmarshal(req, &chunk); err != nil {
			return nil, fmt.Errorf("decode stream chunk: %w", err)
		}
	}

	cfg, state := plugin.store.snapshot()
	now := time.Now()

	provider, authKey, client := attributionFromMetadata(chunk.Metadata)
	model := strings.TrimSpace(chunk.Model)
	if model == "" {
		model = strings.TrimSpace(chunk.RequestedModel)
	}

	if chunk.ChunkIndex < 0 {
		// Header-init: register the stream and start a fresh parser.
		state.beginStream(chunk.RequestID, traceFromMetadata(chunk.Metadata), provider, authKey, client, model, now)
		plugin.setParser(chunk.RequestID, &streamParser{})
		plugin.maybeRefreshAuthList()
		return okEnvelope(streamChunkResponse{})
	}

	if len(chunk.Body) == 0 {
		return okEnvelope(streamChunkResponse{})
	}

	parser := plugin.parser(chunk.RequestID)
	if parser == nil {
		// A stream that began before the plugin loaded still deserves an estimate.
		parser = &streamParser{}
		plugin.setParser(chunk.RequestID, parser)
		state.beginStream(chunk.RequestID, traceFromMetadata(chunk.Metadata), provider, authKey, client, model, now)
	} else if provider != "" || authKey != "" || client != "" || model != "" {
		state.setStreamProvider(chunk.RequestID, provider, authKey, client, model, now)
	}

	parser.feed(chunk.Body)
	estimated := parser.estimate(cfg)
	state.observeEstimate(chunk.RequestID, estimated, now)
	return okEnvelope(streamChunkResponse{})
}

// handleComplete closes out a request lifecycle event.
func handleComplete(req []byte) ([]byte, error) {
	var completion requestCompletion
	if len(req) > 0 {
		if err := json.Unmarshal(req, &completion); err != nil {
			return nil, fmt.Errorf("decode completion: %w", err)
		}
	}
	if completion.RequestID == "" {
		return okEnvelope(emptyResponse{})
	}

	state := plugin.store.meter()
	now := time.Now()

	// Flush any tail the stream held back, then stop tracking it.
	if parser := plugin.takeParser(completion.RequestID); parser != nil {
		parser.finish()
		cfg := plugin.store.config()
		state.observeEstimate(completion.RequestID, parser.estimate(cfg), now)
	}
	state.completeStream(completion.RequestID, completion.TraceID)
	state.reap(now)
	return okEnvelope(emptyResponse{})
}

// estimate converts the parser's running observations into counters.
//
// Gen is the generated part only and is what every rate divides by. The prompt
// is excluded deliberately: Claude announces its whole prompt in message_start
// and emits output tokens seconds later, so counting the prompt would make a
// 15k-token prompt read as 3k tok/s in the live window before the model had
// produced a single token.
func (p *streamParser) estimate(cfg meterConfig) counters {
	detail := p.reported
	// The provider's own count wins, but it can lag (OpenAI sends usage only on
	// the final chunk, Claude sends a placeholder output_tokens in
	// message_start). Observed text fills the gap and is monotonic, so the
	// greater of the two is always the better estimate.
	gen := generatedFromDetail(detail)
	if chars := estimateTokensFromChars(p.text, cfg); chars > gen {
		gen = chars
	}
	if detail.TotalTokens <= 0 {
		detail.TotalTokens = detail.InputTokens + gen
	}
	if sum := detail.InputTokens + gen; sum > detail.TotalTokens {
		detail.TotalTokens = sum
	}
	return counters{
		Input:  detail.InputTokens,
		Output: detail.OutputTokens,
		Reason: detail.ReasoningTokens,
		Cached: detail.CachedTokens,
		Gen:    gen,
		Tokens: detail.TotalTokens,
	}
}

// --- parser registry -------------------------------------------------------

func (p *pluginInstance) setParser(requestID string, parser *streamParser) {
	if requestID == "" {
		return
	}
	p.parsersMu.Lock()
	if len(p.parsers) >= maxInflight {
		// Evict nothing; refusing the new entry is safer than losing a live one.
		p.parsersMu.Unlock()
		return
	}
	p.parsers[requestID] = parser
	p.parsersMu.Unlock()
}

func (p *pluginInstance) parser(requestID string) *streamParser {
	p.parsersMu.Lock()
	defer p.parsersMu.Unlock()
	return p.parsers[requestID]
}

func (p *pluginInstance) takeParser(requestID string) *streamParser {
	p.parsersMu.Lock()
	defer p.parsersMu.Unlock()
	parser := p.parsers[requestID]
	delete(p.parsers, requestID)
	return parser
}

func (p *pluginInstance) dropParser(requestID string) {
	p.parsersMu.Lock()
	delete(p.parsers, requestID)
	p.parsersMu.Unlock()
}

// --- metadata helpers ------------------------------------------------------

// attributionFromMetadata reads the attribution the host publishes into the
// execution metadata. The provider is only a guess until an auth listing
// resolves the auth id, so all three values are passed through verbatim.
func attributionFromMetadata(metadata map[string]any) (provider, authKey, client string) {
	if len(metadata) == 0 {
		return "", "", ""
	}
	if value, ok := metadata["selected_auth_id"].(string); ok {
		authKey = strings.TrimSpace(value)
	}
	if authKey == "" {
		if value, ok := metadata["selected_auth_index"].(string); ok {
			authKey = strings.TrimSpace(value)
		}
	}
	if value, ok := metadata["caller_scope"].(string); ok {
		client = strings.TrimSpace(value)
	}
	return "", authKey, client
}

func traceFromMetadata(metadata map[string]any) string {
	if len(metadata) == 0 {
		return ""
	}
	for _, key := range []string{"trace_id", "TraceID", "x_cpa_trace_id"} {
		if value, ok := metadata[key].(string); ok && value != "" {
			return value
		}
	}
	return ""
}

// --- auth listing ----------------------------------------------------------

// maybeRefreshAuthList learns auth id -> provider mappings so live streams can
// show a provider before their usage record lands. The listing costs a host
// round trip on the streaming hot path, so it runs at most once a minute, and a
// failed attempt still counts so a broken host callback is not retried per
// request.
func (p *pluginInstance) maybeRefreshAuthList() {
	p.authMu.Lock()
	due := time.Since(p.authListedAt) > time.Minute
	if due {
		p.authListedAt = time.Now()
	}
	p.authMu.Unlock()
	if !due {
		return
	}

	raw, err := callHost(methodHostAuthList, []byte(`{}`))
	if err != nil {
		return
	}
	var envelope envelope
	if err := json.Unmarshal(raw, &envelope); err != nil || !envelope.OK {
		return
	}
	var listing hostAuthListResponse
	if err := json.Unmarshal(envelope.Result, &listing); err != nil {
		return
	}

	state := p.store.meter()
	for _, entry := range listing.Files {
		provider := normalizeProviderKey(entry.Provider)
		if provider == "" {
			provider = normalizeProviderKey(entry.Type)
		}
		if provider == "" {
			continue
		}
		state.setAuthProvider(entry.ID, provider)
		state.setAuthProvider(entry.AuthIndex, provider)
	}
}

// --- logging ---------------------------------------------------------------

// log forwards a message to the host log so plugin problems appear in the
// server log rather than vanishing.
func (p *pluginInstance) log(level, message string) {
	payload, errMarshal := json.Marshal(map[string]any{
		"level":   level,
		"message": "[token-speedometer] " + message,
	})
	if errMarshal != nil {
		return
	}
	_, _ = callHost("host.log", payload)
}
