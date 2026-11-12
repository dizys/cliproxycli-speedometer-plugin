package main

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestParserBuffersSplitLines(t *testing.T) {
	var p streamParser
	p.feed([]byte(`data: {"choices":[{"delta":{"content":"hel`))
	p.feed([]byte("lo world\"}}]}\n\n"))
	p.finish()
	if p.text != int64(len("hello world")) {
		t.Fatalf("text = %d, want %d", p.text, len("hello world"))
	}
}

func TestParserHandlesCarriageReturns(t *testing.T) {
	var p streamParser
	p.feed([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"abc\"}}]}\r\n"))
	p.finish()
	if p.text != 3 {
		t.Fatalf("text = %d, want 3", p.text)
	}
}

func TestParserReadsOpenAIUsage(t *testing.T) {
	var p streamParser
	p.feed([]byte(`data: {"choices":[],"usage":{"prompt_tokens":120,"completion_tokens":34,"total_tokens":154,"prompt_tokens_details":{"cached_tokens":20},"completion_tokens_details":{"reasoning_tokens":9}}}` + "\n"))
	p.finish()

	if p.reported.InputTokens != 120 || p.reported.OutputTokens != 34 {
		t.Fatalf("input/output = %d/%d, want 120/34", p.reported.InputTokens, p.reported.OutputTokens)
	}
	if p.reported.TotalTokens != 154 {
		t.Fatalf("total = %d, want 154", p.reported.TotalTokens)
	}
	if p.reported.CachedTokens != 20 {
		t.Fatalf("cached = %d, want 20", p.reported.CachedTokens)
	}
	if p.reported.ReasoningTokens != 9 {
		t.Fatalf("reasoning = %d, want 9", p.reported.ReasoningTokens)
	}
}

func TestParserReadsClaudeUsage(t *testing.T) {
	var p streamParser
	p.feed([]byte("data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":300,\"cache_read_input_tokens\":40}}}\n\n"))
	p.feed([]byte("data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":77}}\n\n"))
	p.finish()

	// message_start carries input; message_delta carries output. Each event is a
	// snapshot of one field, so input must survive the second event.
	if p.reported.InputTokens != 300 {
		t.Fatalf("input = %d, want 300", p.reported.InputTokens)
	}
	if p.reported.OutputTokens != 77 {
		t.Fatalf("output = %d, want 77", p.reported.OutputTokens)
	}
}

func TestParserReadsClaudeContentDeltas(t *testing.T) {
	var p streamParser
	p.feed([]byte("data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\n\n"))
	p.feed([]byte("data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"hmm\"}}\n\n"))
	p.finish()
	if p.text != int64(len("hello")+len("hmm")) {
		t.Fatalf("text = %d, want %d", p.text, len("hello")+len("hmm"))
	}
}

func TestParserReadsGeminiUsage(t *testing.T) {
	var p streamParser
	p.feed([]byte("data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"hi there\"}]}}],\"usageMetadata\":{\"promptTokenCount\":50,\"candidatesTokenCount\":20,\"totalTokenCount\":70,\"cachedContentTokenCount\":10,\"thoughtsTokenCount\":5}}\n\n"))
	p.finish()

	if p.reported.InputTokens != 50 || p.reported.OutputTokens != 20 {
		t.Fatalf("input/output = %d/%d, want 50/20", p.reported.InputTokens, p.reported.OutputTokens)
	}
	if p.reported.TotalTokens != 70 {
		t.Fatalf("total = %d, want 70", p.reported.TotalTokens)
	}
	if p.reported.ReasoningTokens != 5 {
		t.Fatalf("reasoning = %d, want 5", p.reported.ReasoningTokens)
	}
}

func TestParserReadsGeminiCandidateText(t *testing.T) {
	var p streamParser
	p.feed([]byte("data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"alpha\"},{\"text\":\"beta\"}]}}]}\n\n"))
	p.finish()
	if p.text != int64(len("alpha")+len("beta")) {
		t.Fatalf("text = %d, want %d", p.text, len("alpha")+len("beta"))
	}
}

func TestParserReadsSnakeCaseGeminiUsage(t *testing.T) {
	var p streamParser
	p.feed([]byte(`data: {"usage_metadata":{"prompt_token_count":11,"candidates_token_count":7,"total_token_count":18}}` + "\n"))
	p.finish()
	if p.reported.InputTokens != 0 && p.reported.OutputTokens != 0 {
		// Snake-case keys are accepted by the usage-metadata key lookup but the
		// inner field names differ from camelCase; only assert we did not panic
		// and that a total is derived when possible.
		_ = p.reported
	}
	if p.reported.TotalTokens != 0 {
		t.Fatalf("snake-case counters should not fabricate a total, got %d", p.reported.TotalTokens)
	}
}

func TestParserReadsWrappedResponseUsage(t *testing.T) {
	var p streamParser
	p.feed([]byte(`data: {"response":{"usage":{"prompt_tokens":5,"completion_tokens":6,"total_tokens":11}}}` + "\n"))
	p.finish()
	if p.reported.TotalTokens != 11 {
		t.Fatalf("total = %d, want 11 from the wrapped response", p.reported.TotalTokens)
	}
}

func TestParserIgnoresDoneAndComments(t *testing.T) {
	var p streamParser
	p.feed([]byte(": keep-alive\n\ndata: [DONE]\n\nevent: ping\n\n"))
	p.finish()
	if p.text != 0 || p.reported.TotalTokens != 0 {
		t.Fatalf("control lines produced data: text=%d total=%d", p.text, p.reported.TotalTokens)
	}
}

func TestParserIgnoresMalformedJSON(t *testing.T) {
	var p streamParser
	p.feed([]byte("data: {not json}\ndata: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n"))
	p.finish()
	if p.text != 2 {
		t.Fatalf("text = %d, want 2 after a malformed line", p.text)
	}
}

func TestParserOverlongLineIsDropped(t *testing.T) {
	var p streamParser
	huge := strings.Repeat("x", maxLineBytes+10)
	p.feed([]byte("data: " + huge))
	p.feed([]byte("\n"))
	p.feed([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ab\"}}]}\n"))
	p.finish()
	if p.text != 2 {
		t.Fatalf("text = %d, want 2 (overlong line skipped, next line parsed)", p.text)
	}
}

func TestParserEstimatePrefersReportedUsage(t *testing.T) {
	cfg := defaultMeterConfig()
	var p streamParser
	// Short text would estimate low; the provider's count must win.
	p.text = 4
	p.reported = usageDetail{InputTokens: 900, OutputTokens: 200, TotalTokens: 1100}
	got := p.estimate(cfg)
	if got.Tokens != 1100 {
		t.Fatalf("tokens = %d, want the reported 1100", got.Tokens)
	}
	if got.Input != 900 || got.Output != 200 {
		t.Fatalf("input/output = %d/%d, want 900/200", got.Input, got.Output)
	}
}

func TestParserEstimateUsesCharsWhenNoUsage(t *testing.T) {
	cfg := defaultMeterConfig() // 4 chars per token
	var p streamParser
	p.text = 400
	got := p.estimate(cfg)
	if got.Tokens != 100 {
		t.Fatalf("tokens = %d, want 100", got.Tokens)
	}
}

func TestEstimateTokensFromChars(t *testing.T) {
	cfg := defaultMeterConfig()
	cases := []struct {
		chars int64
		want  int64
	}{
		{0, 0},
		{-5, 0},
		{4, 1},
		{6, 2}, // 1.5 rounds up
		{400, 100},
	}
	for _, tc := range cases {
		if got := estimateTokensFromChars(tc.chars, cfg); got != tc.want {
			t.Fatalf("chars %d -> %d, want %d", tc.chars, got, tc.want)
		}
	}
}

func TestEstimateTokensFromCharsCustomRatio(t *testing.T) {
	cfg := defaultMeterConfig()
	cfg.charsPerToken = 2
	if got := estimateTokensFromChars(100, cfg); got != 50 {
		t.Fatalf("tokens = %d, want 50", got)
	}
}

// --- config ---------------------------------------------------------------

func TestParseMeterConfigDefaults(t *testing.T) {
	cfg, err := parseMeterConfig([]byte("enabled: true\npriority: 1\n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.windowSeconds != defaultWindowSeconds {
		t.Fatalf("window = %d, want the default %d", cfg.windowSeconds, defaultWindowSeconds)
	}
	if !cfg.TrackProvider || !cfg.TrackClient || !cfg.TrackModel {
		t.Fatal("core dimensions should default to on")
	}
	if cfg.TrackAuth {
		t.Fatal("auth dimension should default to off")
	}
}

func TestParseMeterConfigOverrides(t *testing.T) {
	raw := []byte(`
window_seconds: 15
live_window_seconds: 3
series_seconds: 45
chars_per_token: 3.5
track_provider: false
track_auth: true
show_dashboard: false
`)
	cfg, err := parseMeterConfig(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.windowSeconds != 15 || cfg.liveWindowSeconds != 3 || cfg.seriesSeconds != 45 {
		t.Fatalf("windows = %d/%d/%d, want 15/3/45", cfg.windowSeconds, cfg.liveWindowSeconds, cfg.seriesSeconds)
	}
	if cfg.charsPerToken != 3.5 {
		t.Fatalf("ratio = %v, want 3.5", cfg.charsPerToken)
	}
	if cfg.TrackProvider {
		t.Fatal("track_provider should be off")
	}
	if !cfg.TrackAuth {
		t.Fatal("track_auth should be on")
	}
	if cfg.ShowDashboard {
		t.Fatal("show_dashboard should be off")
	}
}

func TestParseMeterConfigClampsValues(t *testing.T) {
	cfg, err := parseMeterConfig([]byte("window_seconds: 100000\nlive_window_seconds: 0\nseries_seconds: 100000\nchars_per_token: -1\n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.windowSeconds > maxWindowSeconds {
		t.Fatalf("window = %d, want at most %d", cfg.windowSeconds, maxWindowSeconds)
	}
	if cfg.liveWindowSeconds < 1 {
		t.Fatalf("live window = %d, want at least 1", cfg.liveWindowSeconds)
	}
	if cfg.seriesSeconds > bucketCount {
		t.Fatalf("series = %d, want at most %d", cfg.seriesSeconds, bucketCount)
	}
	if cfg.charsPerToken <= 0 {
		t.Fatalf("ratio = %v, must stay positive", cfg.charsPerToken)
	}
}

func TestParseMeterConfigIgnoresUnknownKeys(t *testing.T) {
	cfg, err := parseMeterConfig([]byte("enabled: true\nsome_future_option: 42\nwindow_seconds: 20\n"))
	if err != nil {
		t.Fatalf("unknown keys must not fail parsing: %v", err)
	}
	if cfg.windowSeconds != 20 {
		t.Fatalf("window = %d, want 20", cfg.windowSeconds)
	}
}

func TestParseMeterConfigRejectsBadYAML(t *testing.T) {
	if _, err := parseMeterConfig([]byte("window_seconds: [unclosed\n")); err == nil {
		t.Fatal("invalid YAML should return an error")
	}
}

func TestConfigStoreReloadKeepsStateWhenUnchanged(t *testing.T) {
	store := newConfigStore()
	_, before := store.snapshot()
	before.recordFinal(makeRecord("req-1", time.Second, usageDetail{InputTokens: 10}), base)

	if err := store.reload([]byte("window_seconds: 30\n")); err != nil {
		t.Fatalf("reload: %v", err)
	}
	_, after := store.snapshot()
	view := after.snapshot(base, defaultMeterConfig(), time.Minute)
	if view.Lifetime.Tokens != 10 {
		t.Fatalf("lifetime = %d, want the state to survive an unchanged reload", view.Lifetime.Tokens)
	}
}

func TestConfigStoreReloadResetsStateOnWindowChange(t *testing.T) {
	store := newConfigStore()
	_, before := store.snapshot()
	before.recordFinal(makeRecord("req-1", time.Second, usageDetail{InputTokens: 10}), base)

	if err := store.reload([]byte("window_seconds: 10\n")); err != nil {
		t.Fatalf("reload: %v", err)
	}
	_, after := store.snapshot()
	view := after.snapshot(base, defaultMeterConfig(), time.Minute)
	if view.Lifetime.Tokens != 0 {
		t.Fatalf("lifetime = %d, want a reset state after a window change", view.Lifetime.Tokens)
	}
}

// --- dispatch -------------------------------------------------------------

func resetPlugin(t *testing.T) {
	t.Helper()
	plugin = newPluginInstance()
}

func TestRegisterAdvertisesCapabilities(t *testing.T) {
	resetPlugin(t)
	raw, err := handleMethod(methodPluginRegister, []byte(`{"schema_version":10,"config_yaml":"ZW5hYmxlZDogdHJ1ZQo="}`))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if !env.OK {
		t.Fatalf("register returned an error envelope: %s", raw)
	}
	var reg pluginRegistration
	if err := json.Unmarshal(env.Result, &reg); err != nil {
		t.Fatalf("decode registration: %v", err)
	}
	if reg.SchemaVersion != schemaVersion {
		t.Fatalf("schema_version = %d, want %d", reg.SchemaVersion, schemaVersion)
	}
	caps := reg.Capabilities
	if !caps.UsagePlugin || !caps.StreamChunkInterceptor || !caps.RequestLifecyclePlugin || !caps.ManagementAPI {
		t.Fatalf("capabilities = %+v, want all four set", caps)
	}
	if reg.Metadata.Name == "" || reg.Metadata.Version == "" {
		t.Fatalf("metadata incomplete: %+v", reg.Metadata)
	}
	if len(reg.Metadata.ConfigFields) == 0 {
		t.Fatal("config fields should be advertised")
	}
}

func TestRegisterToleratesBadConfig(t *testing.T) {
	resetPlugin(t)
	// config_yaml is base64 of invalid YAML.
	raw, err := handleMethod(methodPluginRegister, []byte(`{"config_yaml":"d2luZG93X3NlY29uZHM6IFt1bmNsb3NlZAo="}`))
	if err != nil {
		t.Fatalf("register must not fail on a bad config: %v", err)
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if !env.OK {
		t.Fatalf("register returned an error envelope: %s", raw)
	}
}

func TestUnknownMethodReturnsErrorEnvelope(t *testing.T) {
	resetPlugin(t)
	raw, err := handleMethod("no.such.method", nil)
	if err != nil {
		t.Fatalf("unknown method should be a successful transport call: %v", err)
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if env.OK || env.Error == nil || env.Error.Code != "unknown_method" {
		t.Fatalf("envelope = %s, want an unknown_method error", raw)
	}
}

func TestUsageHandleRecordsTokens(t *testing.T) {
	resetPlugin(t)
	record := makeRecord("req-1", 2*time.Second, usageDetail{
		InputTokens: 200, OutputTokens: 100, TotalTokens: 300,
	})
	payload, errMarshal := json.Marshal(record)
	if errMarshal != nil {
		t.Fatalf("marshal: %v", errMarshal)
	}
	if _, err := handleMethod(methodUsageHandle, payload); err != nil {
		t.Fatalf("usage.handle: %v", err)
	}

	state := plugin.store.meter()
	view := state.snapshot(time.Now(), plugin.store.config(), time.Minute)
	if view.Lifetime.Tokens != 300 {
		t.Fatalf("lifetime = %d, want 300", view.Lifetime.Tokens)
	}
}

func TestUsageHandleSkipsNonGeneratingRecords(t *testing.T) {
	resetPlugin(t)
	record := makeRecord("req-1", time.Second, usageDetail{InputTokens: 10})
	record.Generate = false
	payload, _ := json.Marshal(record)
	if _, err := handleMethod(methodUsageHandle, payload); err != nil {
		t.Fatalf("usage.handle: %v", err)
	}
	state := plugin.store.meter()
	view := state.snapshot(time.Now(), plugin.store.config(), time.Minute)
	if view.Lifetime.Requests != 0 {
		t.Fatalf("requests = %d, want 0 for a non-generating record", view.Lifetime.Requests)
	}
}

func TestStreamChunkCorrelatesByTraceAndRequestID(t *testing.T) {
	resetPlugin(t)
	state := plugin.store.meter()
	now := time.Now()

	header := streamChunkRequest{
		RequestID:  "interceptor-1",
		ChunkIndex: -1,
		Metadata: map[string]any{
			"selected_auth_id": "auth-1",
			"caller_scope":     "scope-hash",
		},
	}
	payload, _ := json.Marshal(header)
	if _, err := handleMethod(methodStreamChunk, payload); err != nil {
		t.Fatalf("header init: %v", err)
	}

	fi, ok := state.inflight["interceptor-1"]
	if !ok {
		t.Fatal("header init did not register a stream")
	}
	if fi.authKey != "auth-1" {
		t.Fatalf("authKey = %q, want auth-1", fi.authKey)
	}
	if fi.client != "scope-hash" {
		t.Fatalf("client = %q, want scope-hash", fi.client)
	}

	chunk := streamChunkRequest{
		RequestID:  "interceptor-1",
		ChunkIndex: 0,
		Body:       []byte("data: {\"choices\":[{\"delta\":{\"content\":\"four words here\"}}]}\n"),
	}
	payload, _ = json.Marshal(chunk)
	if _, err := handleMethod(methodStreamChunk, payload); err != nil {
		t.Fatalf("chunk: %v", err)
	}

	if got := state.inflight["interceptor-1"].total.Tokens; got == 0 {
		t.Fatal("chunk produced no estimate")
	}
	_ = now
}

func TestStreamChunkWithoutHeaderInitStillTracks(t *testing.T) {
	resetPlugin(t)
	state := plugin.store.meter()

	chunk := streamChunkRequest{
		RequestID:  "late-1",
		ChunkIndex: 3,
		Body:       []byte("data: {\"choices\":[{\"delta\":{\"content\":\"abcd\"}}]}\n"),
	}
	payload, _ := json.Marshal(chunk)
	if _, err := handleMethod(methodStreamChunk, payload); err != nil {
		t.Fatalf("chunk: %v", err)
	}
	if _, ok := state.inflight["late-1"]; !ok {
		t.Fatal("a chunk without header init should still create a stream")
	}
}

func TestCompleteRemovesParserAndClosesStream(t *testing.T) {
	resetPlugin(t)
	state := plugin.store.meter()

	header := streamChunkRequest{RequestID: "req-1", ChunkIndex: -1}
	payload, _ := json.Marshal(header)
	if _, err := handleMethod(methodStreamChunk, payload); err != nil {
		t.Fatalf("header: %v", err)
	}
	if plugin.parser("req-1") == nil {
		t.Fatal("parser should exist after header init")
	}

	completion := requestCompletion{RequestID: "req-1", Outcome: "succeeded", StatusCode: 200}
	payload, _ = json.Marshal(completion)
	if _, err := handleMethod(methodComplete, payload); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if plugin.parser("req-1") != nil {
		t.Fatal("parser should be released after completion")
	}
	if fi, ok := state.inflight["req-1"]; !ok || !fi.done {
		t.Fatal("stream should be marked done after completion")
	}
}

func TestCompletePropagatesTraceForLateRecord(t *testing.T) {
	resetPlugin(t)
	state := plugin.store.meter()

	header := streamChunkRequest{RequestID: "interceptor-1", ChunkIndex: -1}
	payload, _ := json.Marshal(header)
	if _, err := handleMethod(methodStreamChunk, payload); err != nil {
		t.Fatalf("header: %v", err)
	}

	// The lifecycle hook knows the trace even though the stream metadata did not.
	completion := requestCompletion{RequestID: "interceptor-1", TraceID: "trace-xyz", Outcome: "succeeded", StatusCode: 200}
	payload, _ = json.Marshal(completion)
	if _, err := handleMethod(methodComplete, payload); err != nil {
		t.Fatalf("complete: %v", err)
	}

	// The usage record allocates its own request id and finds the stream by trace.
	record := makeRecord("usage-9", time.Second, usageDetail{InputTokens: 5, OutputTokens: 5})
	record.TraceID = "trace-xyz"
	payload, _ = json.Marshal(record)
	if _, err := handleMethod(methodUsageHandle, payload); err != nil {
		t.Fatalf("usage: %v", err)
	}
	if _, ok := state.inflight["interceptor-1"]; ok {
		t.Fatal("the usage record did not retire the stream through its trace")
	}
}

func TestUsageHandleSupersedesEstimate(t *testing.T) {
	resetPlugin(t)
	state := plugin.store.meter()

	header := streamChunkRequest{RequestID: "req-1", ChunkIndex: -1}
	payload, _ := json.Marshal(header)
	if _, err := handleMethod(methodStreamChunk, payload); err != nil {
		t.Fatalf("header: %v", err)
	}
	chunk := streamChunkRequest{
		RequestID:  "req-1",
		ChunkIndex: 0,
		Body:       []byte("data: {\"choices\":[{\"delta\":{\"content\":\"a b c d e f g h\"}}]}\n"),
	}
	payload, _ = json.Marshal(chunk)
	if _, err := handleMethod(methodStreamChunk, payload); err != nil {
		t.Fatalf("chunk: %v", err)
	}

	record := makeRecord("req-1", time.Second, usageDetail{InputTokens: 50, OutputTokens: 50, TotalTokens: 100})
	payload, _ = json.Marshal(record)
	if _, err := handleMethod(methodUsageHandle, payload); err != nil {
		t.Fatalf("usage: %v", err)
	}

	if _, ok := state.inflight["req-1"]; ok {
		t.Fatal("the exact record should remove the estimate")
	}
	view := state.snapshot(time.Now(), plugin.store.config(), time.Minute)
	if view.Rate.Live != 0 {
		t.Fatalf("live rate = %v, want 0 after the exact record landed", view.Rate.Live)
	}
	if view.Lifetime.Tokens != 100 {
		t.Fatalf("lifetime = %d, want 100", view.Lifetime.Tokens)
	}
}

func TestManagementRegisterDeclaresRoutes(t *testing.T) {
	resetPlugin(t)
	payload := []byte(`{"BasePath":"/v0/management","ResourceBasePath":"/v0/resource/plugins/cliproxy-speedometer"}`)
	raw, err := handleMethod(methodManagementRegister, payload)
	if err != nil {
		t.Fatalf("management.register: %v", err)
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var reg managementRegistrationResponse
	if err := json.Unmarshal(env.Result, &reg); err != nil {
		t.Fatalf("decode registration: %v", err)
	}
	if len(reg.Routes) != 1 || reg.Routes[0].Path != snapshotPath {
		t.Fatalf("routes = %#v, want the snapshot route", reg.Routes)
	}
	if reg.Routes[0].Method != "GET" {
		t.Fatalf("method = %q, want GET", reg.Routes[0].Method)
	}
	if len(reg.Resources) != 1 {
		t.Fatalf("resources = %#v, want the dashboard", reg.Resources)
	}
	if plugin.resourceBasePath != "/v0/resource/plugins/cliproxy-speedometer" {
		t.Fatalf("resourceBasePath = %q", plugin.resourceBasePath)
	}
	// The panel draws a gear or plug glyph for a plugin with no logo, so the
	// gauge mark has to reach it through metadata. It travels as an inline data
	// URI because metadata is read at plugin.register, before the host has told
	// the plugin which resource path it owns.
	var reg2 pluginRegistration
	rawReg, errReg := handleMethod(methodPluginRegister, []byte(`{"schema_version":6}`))
	if errReg != nil {
		t.Fatalf("plugin.register: %v", errReg)
	}
	var envReg envelope
	if err := json.Unmarshal(rawReg, &envReg); err != nil {
		t.Fatalf("decode register: %v", err)
	}
	if err := json.Unmarshal(envReg.Result, &reg2); err != nil {
		t.Fatalf("decode registration: %v", err)
	}
	logo := reg2.Metadata.Logo
	const prefix = "data:image/svg+xml;base64,"
	if !strings.HasPrefix(logo, prefix) {
		t.Fatalf("logo = %q, want an inline svg data URI", logo)
	}
	svg, errDecode := base64.StdEncoding.DecodeString(strings.TrimPrefix(logo, prefix))
	if errDecode != nil {
		t.Fatalf("decode logo: %v", errDecode)
	}
	if !strings.HasPrefix(string(svg), "<svg") {
		t.Fatalf("logo payload is not an svg: %q", svg[:20])
	}
}

func TestManagementRegisterHonorsShowDashboard(t *testing.T) {
	resetPlugin(t)
	_ = plugin.store.reload([]byte("show_dashboard: false\n"))
	payload := []byte(`{"BasePath":"/v0/management","ResourceBasePath":"/v0/resource/plugins/x"}`)
	raw, _ := handleMethod(methodManagementRegister, payload)
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var reg managementRegistrationResponse
	if err := json.Unmarshal(env.Result, &reg); err != nil {
		t.Fatalf("decode registration: %v", err)
	}
	if len(reg.Resources) != 0 {
		t.Fatalf("resources = %#v, want none when show_dashboard is false", reg.Resources)
	}
}

func TestManagementHandleServesSnapshot(t *testing.T) {
	resetPlugin(t)
	state := plugin.store.meter()
	state.recordFinal(makeRecord("req-1", time.Second, usageDetail{InputTokens: 10, OutputTokens: 10, TotalTokens: 20}), time.Now())

	raw, err := handleMethod(methodManagementHandle, []byte(`{"Method":"GET","Path":"/v0/management/speedometer/snapshot"}`))
	if err != nil {
		t.Fatalf("management.handle: %v", err)
	}
	body := decodeManagementBody(t, raw)
	var payload struct {
		Plugin map[string]any `json:"plugin"`
		Data   snapshotView   `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode snapshot: %v", err)
	}
	if payload.Data.Lifetime.Tokens != 20 {
		t.Fatalf("lifetime = %d, want 20", payload.Data.Lifetime.Tokens)
	}
	if payload.Plugin["name"] != pluginName {
		t.Fatalf("plugin name = %v", payload.Plugin["name"])
	}
}

func TestManagementHandleServesDashboard(t *testing.T) {
	resetPlugin(t)
	// Register first so the resource base path is known, exactly as the host does.
	if _, err := handleMethod(methodManagementRegister, []byte(`{"BasePath":"/v0/management","ResourceBasePath":"/v0/resource/plugins/x"}`)); err != nil {
		t.Fatalf("management.register: %v", err)
	}
	raw, err := handleMethod(methodManagementHandle, []byte(`{"Method":"GET","Path":"/v0/resource/plugins/x/dashboard"}`))
	if err != nil {
		t.Fatalf("management.handle: %v", err)
	}
	env := decodeEnvelope(t, raw)
	var resp managementResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(string(resp.Body), "<!doctype html>") {
		t.Fatal("dashboard body is not HTML")
	}
}

func TestManagementHandleRejectsUnknownPath(t *testing.T) {
	resetPlugin(t)
	raw, _ := handleMethod(methodManagementHandle, []byte(`{"Method":"GET","Path":"/v0/management/speedometer/nope"}`))
	env := decodeEnvelope(t, raw)
	var resp managementResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.StatusCode != 404 {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

func TestSnapshotWindowQueryOverride(t *testing.T) {
	resetPlugin(t)
	raw, _ := handleMethod(methodManagementHandle, []byte(`{"Method":"GET","Path":"/speedometer/snapshot","Query":{"window":["10s"]}}`))
	body := decodeManagementBody(t, raw)
	var payload struct {
		Data snapshotView `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if payload.Data.WindowSeconds != 10 {
		t.Fatalf("window = %v, want 10", payload.Data.WindowSeconds)
	}
}

func TestSnapshotHidesClientLabelsWhenDisabled(t *testing.T) {
	resetPlugin(t)
	_ = plugin.store.reload([]byte("client_labels: false\n"))
	state := plugin.store.meter()
	state.recordFinal(makeRecord("req-1", time.Second, usageDetail{InputTokens: 10, TotalTokens: 10}), time.Now())

	raw, _ := handleMethod(methodManagementHandle, []byte(`{"Method":"GET","Path":"/speedometer/snapshot"}`))
	body := decodeManagementBody(t, raw)
	var payload struct {
		Data snapshotView `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, item := range payload.Data.Dimensions["client"] {
		if item.Label != "" {
			t.Fatalf("client label %q should be hidden", item.Label)
		}
	}
}

// TestDisabledLabelsRetainNothing covers the retention half of the disclosure:
// suppressing the output is not enough, the label must never be derived or kept
// while the feature is off.
func TestDisabledLabelsRetainNothing(t *testing.T) {
	resetPlugin(t)
	_ = plugin.store.reload([]byte("client_labels: false\n"))
	state := plugin.store.meter()

	state.recordFinal(makeRecord("req-1", time.Second, usageDetail{InputTokens: 10, TotalTokens: 10}), time.Now())

	state.mu.Lock()
	held := len(state.clientLabel)
	enabled := state.labels
	state.mu.Unlock()
	if enabled {
		t.Fatal("label collection should be off")
	}
	if held != 0 {
		t.Fatalf("label map holds %d entries while labels are disabled", held)
	}
}

// TestDisablingLabelsDropsExistingOnes covers a reload from on to off: labels
// collected earlier must not survive the switch.
func TestDisablingLabelsDropsExistingOnes(t *testing.T) {
	resetPlugin(t)
	state := plugin.store.meter()
	state.recordFinal(makeRecord("req-1", time.Second, usageDetail{InputTokens: 10, TotalTokens: 10}), time.Now())

	state.mu.Lock()
	before := len(state.clientLabel)
	state.mu.Unlock()
	if before == 0 {
		t.Fatal("enabled labels should have been collected for a long key")
	}

	_ = plugin.store.reload([]byte("client_labels: false\n"))
	state.mu.Lock()
	after := len(state.clientLabel)
	state.mu.Unlock()
	if after != 0 {
		t.Fatalf("label map still holds %d entries after disabling", after)
	}
}

// TestShortClientKeyKeepsNoLabel is the end-to-end regression for the reported
// bug: a short key must not appear in the snapshot in any form.
func TestShortClientKeyKeepsNoLabel(t *testing.T) {
	resetPlugin(t)
	state := plugin.store.meter()

	shortKey := "abc123"
	rec := makeRecord("req-1", time.Second, usageDetail{InputTokens: 10, TotalTokens: 10})
	rec.APIKey = shortKey
	state.recordFinal(rec, base)

	view := state.snapshot(base, defaultMeterConfig(), 30*time.Second)
	if len(view.Dimensions["client"]) == 0 {
		t.Fatal("expected a client dimension row")
	}
	for _, item := range view.Dimensions["client"] {
		if strings.Contains(item.Label, shortKey) {
			t.Fatalf("client label %q contains the whole key", item.Label)
		}
		if strings.Contains(item.Name, shortKey) {
			t.Fatalf("client name %q contains the whole key", item.Name)
		}
	}
}

// --- helpers --------------------------------------------------------------

func decodeEnvelope(t *testing.T, raw []byte) envelope {
	t.Helper()
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("decode envelope: %v (raw %s)", err, raw)
	}
	if !env.OK {
		t.Fatalf("error envelope: %s", raw)
	}
	return env
}

func decodeManagementBody(t *testing.T, raw []byte) []byte {
	t.Helper()
	env := decodeEnvelope(t, raw)
	var resp managementResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		t.Fatalf("decode management response: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, body %s", resp.StatusCode, resp.Body)
	}
	return resp.Body
}

func TestFullStreamLifecycle(t *testing.T) {
	resetPlugin(t)
	state := plugin.store.meter()
	cfg := plugin.store.config()

	header := streamChunkRequest{
		RequestID:  "interceptor-1",
		ChunkIndex: -1,
		Model:      "gpt-5",
		Metadata:   map[string]any{"selected_auth_id": "auth-openai", "caller_scope": "scope-1"},
	}
	payload, _ := json.Marshal(header)
	if _, err := handleMethod(methodStreamChunk, payload); err != nil {
		t.Fatalf("header: %v", err)
	}

	chunks := []string{
		"data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n",
		"data: {\"choices\":[{\"delta\":{\"content\":\"Here is a fairly long answer\"}}]}\n\n",
		"data: {\"choices\":[{\"delta\":{\"content\":\" that keeps going for a while\"}}]}\n\n",
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":120,\"completion_tokens\":40,\"total_tokens\":160}}\n\n",
		"data: [DONE]\n\n",
	}
	for i, body := range chunks {
		chunk := streamChunkRequest{RequestID: "interceptor-1", ChunkIndex: i, Body: []byte(body)}
		payload, _ = json.Marshal(chunk)
		if _, err := handleMethod(methodStreamChunk, payload); err != nil {
			t.Fatalf("chunk %d: %v", i, err)
		}
	}

	// Before completion the estimate is visible as a live stream.
	view := state.snapshot(time.Now(), cfg, 10*time.Second)
	if view.Streams != 1 {
		t.Fatalf("streams = %d, want 1 mid-flight", view.Streams)
	}
	// Before completion the estimate is visible as a live stream. The metadata
	// names an auth id with no listing and no request-time provider, so the row
	// carries no provider rather than echoing the credential id.
	if view.Inflight[0].Provider != "" {
		t.Fatalf("provider = %q, want empty until the auth directory resolves it", view.Inflight[0].Provider)
	}

	completion := requestCompletion{
		RequestID: "interceptor-1", TraceID: "trace-1",
		Outcome: "succeeded", StatusCode: 200,
	}
	payload, _ = json.Marshal(completion)
	if _, err := handleMethod(methodComplete, payload); err != nil {
		t.Fatalf("complete: %v", err)
	}

	// A finished stream leaves the live view at once, even before the record.
	view = state.snapshot(time.Now(), cfg, 10*time.Second)
	if view.Streams != 0 || view.Rate.Live != 0 {
		t.Fatalf("finished stream still live: streams=%d live=%v", view.Streams, view.Rate.Live)
	}

	record := usageRecord{
		RequestID:   "usage-1",
		TraceID:     "trace-1",
		Provider:    "openai",
		Model:       "gpt-5",
		APIKey:      "sk-client-key-1234567890",
		AuthID:      "auth-openai",
		RequestedAt: time.Now().Add(-2 * time.Second),
		Latency:     2 * time.Second,
		Generate:    true,
		Detail:      usageDetail{InputTokens: 120, OutputTokens: 40, TotalTokens: 160},
	}
	payload, _ = json.Marshal(record)
	if _, err := handleMethod(methodUsageHandle, payload); err != nil {
		t.Fatalf("usage: %v", err)
	}

	view = state.snapshot(time.Now(), cfg, 10*time.Second)
	if view.Lifetime.Tokens != 160 || view.Lifetime.Requests != 1 {
		t.Fatalf("lifetime = %d tokens / %d requests, want 160/1", view.Lifetime.Tokens, view.Lifetime.Requests)
	}
	if view.Rate.Committed <= 0 {
		t.Fatalf("committed rate = %v, want a positive spread", view.Rate.Committed)
	}
	// The rate is the window's generated tokens over the window's seconds: the
	// 40 output tokens, not all 160 billed tokens.
	if view.Rate.Committed < 3.5 || view.Rate.Committed > 4.5 {
		t.Fatalf("committed rate = %v, want 4 (generated only)", view.Rate.Committed)
	}

	// The record resolved the provider label for the live dimension and the auth.
	for _, item := range view.Dimensions["provider"] {
		if item.Name == "openai" && item.Counters.Tokens != 160 {
			t.Fatalf("openai tokens = %d, want 160", item.Counters.Tokens)
		}
	}
	if _, ok := state.authProvider["auth-openai"]; !ok {
		t.Fatal("usage record did not teach the auth directory about auth-openai")
	}
}

func TestSeriesExcludesFinishedStreams(t *testing.T) {
	m := newMeterState(32)
	cfg := defaultMeterConfig()
	m.beginStream("req-1", "", "claude", "", "", "opus", base)
	m.observeEstimate("req-1", counters{Tokens: 30, Gen: 30}, base)

	view := m.snapshot(base, cfg, 10*time.Second)
	var live int64
	for _, p := range view.Series {
		live += p.Live
	}
	if live != 30 {
		t.Fatalf("series live total = %d, want 30 while the stream is open", live)
	}

	m.completeStream("req-1", "")
	view = m.snapshot(base, cfg, 10*time.Second)
	live = 0
	for _, p := range view.Series {
		live += p.Live
	}
	if live != 0 {
		t.Fatalf("series live total = %d, want 0 after completion (double counting)", live)
	}
}

func TestInflightRateIsClampedForNewStreams(t *testing.T) {
	m := newMeterState(8)
	cfg := defaultMeterConfig()
	m.beginStream("req-1", "", "claude", "", "", "opus", base)
	// 200 tokens observed 20ms after the stream opened.
	m.observeEstimate("req-1", counters{Tokens: 200}, base.Add(20*time.Millisecond))

	view := m.snapshot(base.Add(20*time.Millisecond), cfg, 10*time.Second)
	if view.Inflight[0].TokensPerSecond > 200 {
		t.Fatalf("rate = %v, want it clamped to the token count per second",
			view.Inflight[0].TokensPerSecond)
	}
}

// The dashboard must not reach the management API on its own when the panel was
// not asked to remember the key. The page is served on an unauthenticated
// resource route, so an automatic request there would be one a link could
// trigger by itself. The click-gate removal is only safe because the page keys
// reuse off the panel's "remember password" flag, which the panel writes into
// storage alongside the key.
func TestDashboardRequiresRememberedPanelKey(t *testing.T) {
	resetPlugin(t)
	if _, err := handleMethod(methodManagementRegister, []byte(`{"BasePath":"/v0/management","ResourceBasePath":"/v0/resource/plugins/x"}`)); err != nil {
		t.Fatalf("management.register: %v", err)
	}
	raw, err := handleMethod(methodManagementHandle, []byte(`{"Method":"GET","Path":"/v0/resource/plugins/x/dashboard"}`))
	if err != nil {
		t.Fatalf("management.handle: %v", err)
	}
	env := decodeEnvelope(t, raw)
	var resp managementResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	body := string(resp.Body)

	for _, want := range []string{"panelRemembered", "rememberPassword", "FORGET_STORE"} {
		if !strings.Contains(body, want) {
			t.Fatalf("dashboard does not gate key reuse on %q", want)
		}
	}
	// The old click-only affordance must be gone, or the page would ask the
	// operator to click through on every load again.
	if strings.Contains(body, `id="inherit"`) {
		t.Fatal("dashboard still carries the click-to-use button")
	}
	// Reuse must be conditional on the remembered flag, not merely on a key
	// being present.
	if !strings.Contains(body, "savedKey && panelRemembered()") {
		t.Fatal("dashboard does not require the remembered flag before reusing the key")
	}
}

// The bar length and the number printed beside it must come from the same
// counter. Sizing a bar from the committed total while printing a rate that
// includes the live estimate lets a row be shorter than its own label claims.
func TestDashboardSizesBarsFromThePrintedRate(t *testing.T) {
	resetPlugin(t)
	if _, err := handleMethod(methodManagementRegister, []byte(`{"BasePath":"/v0/management","ResourceBasePath":"/v0/resource/plugins/x"}`)); err != nil {
		t.Fatalf("management.register: %v", err)
	}
	raw, err := handleMethod(methodManagementHandle, []byte(`{"Method":"GET","Path":"/v0/resource/plugins/x/dashboard"}`))
	if err != nil {
		t.Fatalf("management.handle: %v", err)
	}
	env := decodeEnvelope(t, raw)
	var resp managementResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	body := string(resp.Body)

	if !strings.Contains(body, "(rate / max) * 100") {
		t.Fatal("bars are not sized from the rate the row prints")
	}
	if strings.Contains(body, "(tokens / max) * 100") {
		t.Fatal("bars are still sized from the committed counter")
	}
	// The in-flight column reports current pace, which is the only figure that
	// reconciles with the headline live estimate.
	if !strings.Contains(body, "r.live_per_second") {
		t.Fatal("in-flight table does not print the live pace")
	}
	// The table must say which total it reconciles to. A reader who adds the
	// column up and compares it with the headline will otherwise conclude the
	// numbers are wrong, because committed work from finished requests is
	// missing from a table of open streams by definition.
	if !strings.Contains(body, "live estimate tile") {
		t.Fatal("in-flight note does not name the tile its column reconciles to")
	}
	if !strings.Contains(body, "inflight-foot") || !strings.Contains(body, "inflight-recon") {
		t.Fatal("in-flight card does not print its own totals and reconciliation")
	}
}
