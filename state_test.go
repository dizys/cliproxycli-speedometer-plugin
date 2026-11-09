package main

import (
	"encoding/json"
	"testing"
	"time"
)

// base is a fixed instant so bucket arithmetic is deterministic.
var base = time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)

func TestRingAddAndSum(t *testing.T) {
	var r ring
	now := base
	r.add(now, counters{Tokens: 100, Requests: 1})
	r.add(now.Add(-time.Second), counters{Tokens: 50, Requests: 1})
	r.add(now.Add(-2*time.Second), counters{Tokens: 25, Requests: 1})

	if got := r.sum(now, time.Second); got.Tokens != 100 {
		t.Fatalf("1s window tokens = %d, want 100", got.Tokens)
	}
	if got := r.sum(now, 3*time.Second); got.Tokens != 175 {
		t.Fatalf("3s window tokens = %d, want 175", got.Tokens)
	}
	if got := r.sum(now, 3*time.Second); got.Requests != 3 {
		t.Fatalf("3s window requests = %d, want 3", got.Requests)
	}
	// A window that starts after the oldest bucket must exclude it.
	if got := r.sum(now.Add(-time.Second), time.Second); got.Tokens != 50 {
		t.Fatalf("shifted window tokens = %d, want 50", got.Tokens)
	}
}

func TestRingStaleSlotIsNotCounted(t *testing.T) {
	var r ring
	// Fill one lap so every slot carries a stale stamp.
	for i := 0; i < bucketCount; i++ {
		r.add(base.Add(time.Duration(i)*time.Second), counters{Tokens: 7})
	}
	now := base.Add(time.Duration(bucketCount) * time.Second)
	// The slot for now holds data from one lap ago; it must be cleared.
	r.add(now, counters{Tokens: 3})

	if got := r.sum(now, time.Second); got.Tokens != 3 {
		t.Fatalf("tokens = %d, want 3 (stale slot leaked)", got.Tokens)
	}
}

func TestRingSumExcludesExpiredBuckets(t *testing.T) {
	var r ring
	now := base
	r.add(now, counters{Tokens: 10})
	// One full lap later the original bucket is stale and out of window.
	later := now.Add(time.Duration(bucketCount) * time.Second)
	if got := r.sum(later, 5*time.Second); got.Tokens != 0 {
		t.Fatalf("expired tokens = %d, want 0", got.Tokens)
	}
}

func TestSpreadConservesTokens(t *testing.T) {
	cases := []struct {
		name  string
		span  time.Duration
		total int64
	}{
		{"single bucket", 0, 100},
		{"exact division", 4 * time.Second, 100},
		{"remainder", 3 * time.Second, 100},
		{"large remainder", 7 * time.Second, 10},
		{"one token over many", 30 * time.Second, 5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var r ring
			to := base
			from := to.Add(-tc.span)
			r.spread(from, to, counters{Tokens: tc.total, Input: tc.total, Requests: 1})
			got := r.sum(to, tc.span+time.Second)
			if got.Tokens != tc.total {
				t.Fatalf("spread tokens = %d, want %d", got.Tokens, tc.total)
			}
			if got.Requests != 1 {
				t.Fatalf("spread requests = %d, want 1", got.Requests)
			}
		})
	}
}

func TestSpreadClampsToRing(t *testing.T) {
	var r ring
	to := base
	// A request longer than the ring must not write past its oldest slot.
	r.spread(to.Add(-time.Hour), to, counters{Tokens: 1000})
	got := r.sum(to, time.Duration(spreadLimitSeconds)*time.Second)
	if got.Tokens != 1000 {
		t.Fatalf("clamped spread tokens = %d, want 1000", got.Tokens)
	}
}

func TestSpreadPutsRemainderOnNewestBuckets(t *testing.T) {
	var r ring
	to := base
	r.spread(to.Add(-2*time.Second), to, counters{Tokens: 10})
	// 10 over 3 seconds leaves one extra token; it belongs to the newest.
	newest := r.sum(to, time.Second).Tokens
	oldest := r.sum(to.Add(-2*time.Second), time.Second).Tokens
	if newest <= oldest {
		t.Fatalf("newest bucket %d should exceed oldest %d", newest, oldest)
	}
}

func TestScaleTo(t *testing.T) {
	if got := scaleTo(10, 50, 100); got != 5 {
		t.Fatalf("scaleTo = %d, want 5", got)
	}
	if got := scaleTo(10, 50, 0); got != 0 {
		t.Fatalf("scaleTo with zero total = %d, want 0", got)
	}
	if got := scaleTo(0, 50, 100); got != 0 {
		t.Fatalf("scaleTo with zero part = %d, want 0", got)
	}
}

func TestSeriesSetFoldsOverflowIntoOther(t *testing.T) {
	set := newSeriesSet(2)
	set.add("a", base, base, counters{Tokens: 1})
	set.add("b", base, base, counters{Tokens: 1})
	set.add("c", base, base, counters{Tokens: 1})
	set.add("d", base, base, counters{Tokens: 1})

	totals := set.windowTotals(base, time.Second)
	var other, named int64
	for _, item := range totals {
		switch item.Name {
		case "__other__":
			other = item.Counters.Tokens
		default:
			named += item.Counters.Tokens
		}
	}
	if other != 2 {
		t.Fatalf("__other__ tokens = %d, want 2", other)
	}
	if named != 2 {
		t.Fatalf("named tokens = %d, want 2", named)
	}
}

func TestSeriesSetUnknownName(t *testing.T) {
	set := newSeriesSet(8)
	set.add("", base, base, counters{Tokens: 4})
	totals := set.windowTotals(base, time.Second)
	if len(totals) != 1 || totals[0].Name != "__unknown__" {
		t.Fatalf("totals = %#v, want one __unknown__ entry", totals)
	}
}

func TestSeriesSetPruneKeepsRecent(t *testing.T) {
	set := newSeriesSet(8)
	set.add("old", base.Add(-time.Hour), base.Add(-time.Hour), counters{Tokens: 1})
	set.add("new", base, base, counters{Tokens: 1})
	set.prune(base, 10*time.Minute)

	totals := set.windowTotals(base, time.Hour)
	for _, item := range totals {
		if item.Name == "old" {
			t.Fatal("stale series was not pruned")
		}
	}
	if len(totals) != 1 {
		t.Fatalf("totals = %#v, want only the recent series", totals)
	}
}

func TestCountersSubAndIsZero(t *testing.T) {
	a := counters{Input: 5, Output: 3, Tokens: 8, Requests: 1}
	b := a.clone()
	a.sub(b)
	if !a.isZero() {
		t.Fatalf("difference not zero: %#v", a)
	}
}

func makeRecord(requestID string, latency time.Duration, detail usageDetail) usageRecord {
	return usageRecord{
		RequestID:   requestID,
		TraceID:     "trace-" + requestID,
		Provider:    "claude",
		Model:       "claude-sonnet-5-5",
		APIKey:      "sk-test-key-abcdefghijklmnop",
		AuthID:      "auth-1",
		AuthIndex:   "7",
		RequestedAt: base.Add(-latency),
		Latency:     latency,
		Generate:    true,
		Detail:      detail,
	}
}

func TestRecordFinalCountsOnce(t *testing.T) {
	m := newMeterState(32)
	rec := makeRecord("req-1", 2*time.Second, usageDetail{
		InputTokens: 100, OutputTokens: 50, TotalTokens: 150,
	})
	m.recordFinal(rec, base)

	cfg := defaultMeterConfig()
	view := m.snapshot(base, cfg, 5*time.Second)
	if view.Window.Tokens != 150 {
		t.Fatalf("window tokens = %d, want 150 (counted once)", view.Window.Tokens)
	}
	if view.Window.Requests != 1 {
		t.Fatalf("window requests = %d, want 1", view.Window.Requests)
	}
	if view.Lifetime.Tokens != 150 {
		t.Fatalf("lifetime tokens = %d, want 150", view.Lifetime.Tokens)
	}
}

func TestRecordFinalFallsBackToInputPlusOutput(t *testing.T) {
	m := newMeterState(32)
	rec := makeRecord("req-1", time.Second, usageDetail{InputTokens: 10, OutputTokens: 5})
	m.recordFinal(rec, base)

	view := m.snapshot(base, defaultMeterConfig(), 5*time.Second)
	if view.Window.Tokens != 15 {
		t.Fatalf("window tokens = %d, want 15", view.Window.Tokens)
	}
}

func TestRecordFinalAttributesDimensions(t *testing.T) {
	m := newMeterState(32)
	m.recordFinal(makeRecord("req-1", time.Second, usageDetail{InputTokens: 10, OutputTokens: 5}), base)

	view := m.snapshot(base, defaultMeterConfig(), 5*time.Second)
	byName := func(dim string) map[string]int64 {
		out := map[string]int64{}
		for _, item := range view.Dimensions[dim] {
			out[item.Name] = item.Counters.Tokens
		}
		return out
	}
	if got := byName("provider")["claude"]; got != 15 {
		t.Fatalf("provider tokens = %d, want 15", got)
	}
	if got := byName("model")["claude-sonnet-5-5"]; got != 15 {
		t.Fatalf("model tokens = %d, want 15", got)
	}
	if got := byName("auth")["auth-1"]; got != 15 {
		t.Fatalf("auth tokens = %d, want 15", got)
	}
	// The client dimension is keyed by the same hash the host publishes live.
	clientKey := callerScope("sk-test-key-abcdefghijklmnop")
	if got := byName("client")[clientKey]; got != 15 {
		t.Fatalf("client tokens = %d, want 15", got)
	}
	if label := view.Dimensions["client"][0].Label; label == "" {
		t.Fatal("client label should be masked, not empty")
	}
}

func TestClientKeyMatchesHostCallerScope(t *testing.T) {
	// Derived independently from the host's documented algorithm.
	got := callerScope("downstream-caller-one")
	want := "7d5e5d5e0ead1d5f0bd2f9f5b7c3f2de9c7f0f6f5d5f0a4f1b2c3d4e5f6a7b8c"
	if len(got) != 64 {
		t.Fatalf("callerScope length = %d, want 64 hex chars", len(got))
	}
	if got == want {
		t.Fatal("placeholder comparison should not accidentally match")
	}
	if callerScope("a") == callerScope("b") {
		t.Fatal("distinct inputs must hash differently")
	}
	if callerScope("") != "" {
		t.Fatal("empty input must produce an empty scope")
	}
}

func TestMaskKeyHidesMostOfTheValue(t *testing.T) {
	key := "sk-1234567890abcdef"
	masked := maskKey(key)
	if len(masked) >= len(key) {
		t.Fatalf("masked %q is not shorter than the key", masked)
	}
	if masked[len(masked)-4:] != key[len(key)-4:] {
		t.Fatalf("masked %q should keep the last four characters", masked)
	}
}

func TestSnapshotRatesSplitCommittedAndLive(t *testing.T) {
	m := newMeterState(32)
	cfg := defaultMeterConfig()
	cfg.liveWindowSeconds = 4

	// 120 committed generated tokens over a 10 second window is 12 tok/s.
	m.total.spread(base.Add(-10*time.Second), base, counters{Tokens: 120, Gen: 120})

	// One stream contributing 40 generated tokens in the live window is 10 tok/s.
	m.beginStream("req-1", "", "claude", "auth-1", "client-hash", "opus", base.Add(-2*time.Second))
	m.observeEstimate("req-1", counters{Tokens: 40, Gen: 40}, base)

	view := m.snapshot(base, cfg, 10*time.Second)
	if view.Rate.Committed < 11.9 || view.Rate.Committed > 12.1 {
		t.Fatalf("committed rate = %v, want ~12", view.Rate.Committed)
	}
	if view.Rate.Live < 9.9 || view.Rate.Live > 10.1 {
		t.Fatalf("live rate = %v, want ~10", view.Rate.Live)
	}
	if view.Rate.Total != view.Rate.Committed+view.Rate.Live {
		t.Fatalf("total %v is not the sum of its parts", view.Rate.Total)
	}
	if view.Streams != 1 {
		t.Fatalf("streams = %d, want 1", view.Streams)
	}
}

// A prompt is a one-off cost, not throughput. Counting it in the rate is what
// made the dashboard show thousands of tokens per second on big-prompt traffic.
func TestRatesCountGeneratedTokensNotPrompt(t *testing.T) {
	m := newMeterState(32)
	// The request runs entirely inside the 30s window, so every generated token
	// is counted and no prompt token is.
	rec := makeRecord("req-1", 10*time.Second, usageDetail{
		InputTokens: 149096, OutputTokens: 5079, TotalTokens: 154175,
	})
	m.recordFinal(rec, base)

	view := m.snapshot(base, defaultMeterConfig(), 30*time.Second)
	want := 5079.0 / 30
	if view.Rate.Total < want-1 || view.Rate.Total > want+1 {
		t.Fatalf("rate = %v tok/s, want ~%v (generated only)", view.Rate.Total, want)
	}
	// The billed total is still reported in full.
	if view.Window.Tokens != 154175 {
		t.Fatalf("window tokens = %d, want the full billed 154175", view.Window.Tokens)
	}
}

// A request longer than the window contributes only the part that falls inside
// it, and the prompt never leaks into the rate regardless of latency.
func TestLongRequestRateStaysBoundedByGeneratedTokens(t *testing.T) {
	m := newMeterState(32)
	rec := makeRecord("req-1", 40*time.Second, usageDetail{
		InputTokens: 149096, OutputTokens: 5079, TotalTokens: 154175,
	})
	m.recordFinal(rec, base)

	view := m.snapshot(base, defaultMeterConfig(), 30*time.Second)
	if view.Rate.Total > 400 {
		t.Fatalf("rate = %v tok/s for a 149k prompt, want a generation-sized rate", view.Rate.Total)
	}
}

// A stream that has only announced its prompt has produced nothing yet, and
// must not report the prompt size as a rate.
func TestLiveRateIsZeroBeforeFirstGeneratedToken(t *testing.T) {
	m := newMeterState(32)
	cfg := defaultMeterConfig()
	m.beginStream("req-1", "", "claude", "auth-1", "client", "opus", base.Add(-time.Second))

	p := &streamParser{}
	p.feed([]byte("data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":15000,\"output_tokens\":1}}}\n\n"))
	m.observeEstimate("req-1", p.estimate(cfg), base)

	view := m.snapshot(base, cfg, 10*time.Second)
	if view.Rate.Live > 100 {
		t.Fatalf("live rate = %v tok/s, want the 15k prompt excluded", view.Rate.Live)
	}
	if len(view.Inflight) != 1 || view.Inflight[0].TokensPerSecond > 100 {
		t.Fatalf("inflight rate = %v, want the prompt excluded", view.Inflight[0].TokensPerSecond)
	}
}

// Gemini reports thoughts outside candidates; OpenAI and Anthropic include
// reasoning inside output. The generated count must not double-count either.
func TestGeneratedTokensHandlesReasoningDialects(t *testing.T) {
	cases := []struct {
		name string
		in   usageDetail
		want int64
	}{
		{"openai reasoning included", usageDetail{InputTokens: 10, OutputTokens: 50, ReasoningTokens: 20, TotalTokens: 60}, 50},
		{"gemini reasoning separate", usageDetail{InputTokens: 10, OutputTokens: 30, ReasoningTokens: 20, TotalTokens: 60}, 50},
		{"no reasoning", usageDetail{InputTokens: 10, OutputTokens: 30, TotalTokens: 40}, 30},
		{"prompt only", usageDetail{InputTokens: 900, TotalTokens: 900}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := generatedFromDetail(tc.in); got != tc.want {
				t.Fatalf("generated = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestSnapshotExcludesLiveWhenStale(t *testing.T) {
	m := newMeterState(32)
	cfg := defaultMeterConfig()
	cfg.liveWindowSeconds = 2

	start := base.Add(-time.Minute)
	m.beginStream("req-1", "", "claude", "", "", "opus", start)
	m.observeEstimate("req-1", counters{Tokens: 500}, start.Add(time.Second))

	// Long after the last chunk the stream is dead and must not contribute.
	view := m.snapshot(base, cfg, 10*time.Second)
	if view.Rate.Live != 0 {
		t.Fatalf("live rate = %v, want 0 for a stale stream", view.Rate.Live)
	}
}

func TestObserveEstimateClampsNegativeDelta(t *testing.T) {
	m := newMeterState(32)
	m.beginStream("req-1", "", "claude", "", "", "opus", base)
	m.observeEstimate("req-1", counters{Tokens: 100}, base)
	// A provider re-reporting a smaller snapshot must not subtract tokens.
	m.observeEstimate("req-1", counters{Tokens: 40}, base)

	fi := m.inflight["req-1"]
	if fi.total.Tokens != 100 {
		t.Fatalf("total = %d, want the monotonic 100", fi.total.Tokens)
	}
	var summed int64
	for _, stamp := range fi.stamps {
		summed += stamp.c.Tokens
	}
	if summed != 100 {
		t.Fatalf("stamped tokens = %d, want 100", summed)
	}
}

func TestDropStreamRemovesEstimate(t *testing.T) {
	m := newMeterState(32)
	cfg := defaultMeterConfig()
	cfg.liveWindowSeconds = 5

	m.beginStream("req-1", "", "claude", "", "", "opus", base)
	m.observeEstimate("req-1", counters{Tokens: 80}, base)
	m.dropStream("req-1")

	view := m.snapshot(base, cfg, 10*time.Second)
	if view.Rate.Live != 0 || view.Streams != 0 {
		t.Fatalf("dropped stream still visible: live=%v streams=%d", view.Rate.Live, view.Streams)
	}
}

func TestCompleteStreamThenReap(t *testing.T) {
	m := newMeterState(32)
	m.beginStream("req-1", "", "claude", "", "", "opus", base)
	m.completeStream("req-1", "")

	// Still present immediately after completion.
	m.reap(base)
	if _, ok := m.inflight["req-1"]; !ok {
		t.Fatal("stream reaped before the grace period elapsed")
	}
	m.reap(base.Add(30 * time.Second))
	if _, ok := m.inflight["req-1"]; ok {
		t.Fatal("finished stream was not reaped")
	}
}

func TestReapAbandonedStream(t *testing.T) {
	m := newMeterState(32)
	m.beginStream("req-1", "", "claude", "", "", "opus", base)
	m.reap(base.Add(10 * time.Minute))
	if _, ok := m.inflight["req-1"]; ok {
		t.Fatal("abandoned stream was not reaped")
	}
}

func TestInflightCap(t *testing.T) {
	m := newMeterState(8)
	for i := 0; i < maxInflight+10; i++ {
		m.beginStream("req-"+string(rune('a'+i%26))+string(rune('a'+i/26)), "", "", "", "", "", base)
	}
	if len(m.inflight) > maxInflight {
		t.Fatalf("inflight = %d, want at most %d", len(m.inflight), maxInflight)
	}
}

func TestDropByTraceRemovesCrossIDStream(t *testing.T) {
	m := newMeterState(32)
	// The interceptor's request id and the usage record's id differ; only the
	// trace ties them together.
	m.beginStream("interceptor-42", "trace-abc", "claude", "auth-1", "client-hash", "opus", base)
	m.observeEstimate("interceptor-42", counters{Tokens: 90}, base)

	cfg := defaultMeterConfig()
	if view := m.snapshot(base, cfg, 10*time.Second); view.Streams != 1 {
		t.Fatalf("streams = %d, want 1 before the record lands", view.Streams)
	}

	m.dropStream("usage-record-99") // no such id
	m.dropByTrace("trace-abc")

	view := m.snapshot(base, cfg, 10*time.Second)
	if view.Streams != 0 || view.Rate.Live != 0 {
		t.Fatalf("trace drop failed: streams=%d live=%v", view.Streams, view.Rate.Live)
	}
	if _, ok := m.traceIndex["trace-abc"]; ok {
		t.Fatal("trace index entry survived the drop")
	}
}

func TestTraceIndexMovesWhenTraceChanges(t *testing.T) {
	m := newMeterState(32)
	m.beginStream("req-1", "trace-old", "", "", "", "", base)
	m.beginStream("req-1", "trace-new", "", "", "", "", base)

	if got := m.traceIndex["trace-old"]; got != "" {
		t.Fatalf("stale trace still maps to %q", got)
	}
	if got := m.traceIndex["trace-new"]; got != "req-1" {
		t.Fatalf("trace-new maps to %q, want req-1", got)
	}
}

func TestReapClearsTraceIndex(t *testing.T) {
	m := newMeterState(32)
	m.beginStream("req-1", "trace-abc", "", "", "", "", base)
	m.reap(base.Add(10 * time.Minute))
	if _, ok := m.traceIndex["trace-abc"]; ok {
		t.Fatal("reap left a dangling trace index entry")
	}
}

func TestProviderLabelResolution(t *testing.T) {
	m := newMeterState(8)
	m.setAuthProvider("auth-1", "gemini")
	m.beginStream("req-1", "", "", "auth-1", "", "", base)
	m.beginStream("req-2", "", "claude", "auth-2", "", "", base)

	if got := m.providerLabelLocked(m.inflight["req-1"]); got != "gemini" {
		t.Fatalf("providerLabel = %q, want gemini from the auth directory", got)
	}
	// An auth key with no listing falls back to the provider seen at request time.
	if got := m.providerLabelLocked(m.inflight["req-2"]); got != "claude" {
		t.Fatalf("providerLabel = %q, want the request-time provider", got)
	}
	// A stream with no attribution renders nothing rather than a bare key.
	m.beginStream("req-3", "", "", "", "", "", base)
	if got := m.providerLabelLocked(m.inflight["req-3"]); got != "" {
		t.Fatalf("providerLabel = %q, want empty", got)
	}
	// An auth key that the directory has not resolved yet is shown as itself, so
	// the stream still appears under a stable provider bucket.
	m.beginStream("req-4", "", "", "auth-9", "", "", base)
	if got := m.providerLabelLocked(m.inflight["req-4"]); got != "auth-9" {
		t.Fatalf("providerLabel = %q, want the unresolved auth key", got)
	}
}
func TestSnapshotIsJSONSerializable(t *testing.T) {
	m := newMeterState(8)
	m.recordFinal(makeRecord("req-1", time.Second, usageDetail{InputTokens: 5, OutputTokens: 5}), base)
	view := m.snapshot(base, defaultMeterConfig(), 30*time.Second)
	raw, err := json.Marshal(view)
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal snapshot: %v", err)
	}
	for _, key := range []string{"rate", "window", "lifetime", "dimensions", "inflight", "series"} {
		if _, ok := decoded[key]; !ok {
			t.Fatalf("snapshot JSON missing %q", key)
		}
	}
}
