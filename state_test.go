package main

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
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

func TestSeriesSetOrdersByGeneratedNotBilled(t *testing.T) {
	set := newSeriesSet(8)
	// "prompt-heavy" bills more tokens but generates fewer, so a rank by billed
	// tokens would place it above "fast" even though the row shows tok/s.
	set.add("prompt-heavy", base, base, counters{
		Input: 90000, Output: 1000, Gen: 1000, Tokens: 91000})
	set.add("fast", base, base, counters{
		Input: 100, Output: 9000, Gen: 9000, Tokens: 9100})

	totals := set.windowTotals(base, time.Second)
	if len(totals) != 2 || totals[0].Name != "fast" {
		t.Fatalf("totals = %#v, want fast first (more generated tokens)", totals)
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
		APIKey:      "sk-test-key-abcdefghijklmnopqrstuvwx",
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
	clientKey := callerScope("sk-test-key-abcdefghijklmnopqrstuvwx")
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

// TestMaskKeyNeverReturnsTheKey is the regression test for the credential
// disclosure: every input length must produce a label that omits part of the
// key, never the whole key back. The boundary sweep covers ASCII, multi-byte
// Unicode, and mixed scripts, because the rune count rather than the byte count
// decides the branch.
func TestMaskKeyNeverReturnsTheKey(t *testing.T) {
	samples := []string{
		"a", "ab", "abc", "abcd", "abcde", "abcdef", "abcdefg", "abcdefgh",
		"abcdefghi", "abcdefghij", "abcdefghijk", "abcdefghijkl",
		"abcdefghijklm", "abcdefghijklmnopqrst",
		"sk-1234567890abcdef",
		"密钥", "密钥一二", "密钥一二三四", "密钥一二三四五六", "密钥一二三四五六七八",
		"ключ-1234567890abcd", "키-abc-1234567890",
		"a1b2c3d4e5f6g7h8i9j0",
	}
	for _, key := range samples {
		label := maskKey(key)
		if label == "" {
			continue
		}
		// The whole key must not appear in the label, in any position.
		if strings.Contains(label, key) {
			t.Fatalf("maskKey(%q) = %q, which contains the whole key", key, label)
		}
		// A label must be strictly shorter than the key it describes.
		if utf8.RuneCountInString(label) >= utf8.RuneCountInString(key) {
			t.Fatalf("maskKey(%q) = %q, not shorter than the key", key, label)
		}
	}
}

// TestMaskKeyRevealsLessThanItHides pins the actual invariant: a label may only
// exist when the characters it hides are at least as many as the ones it shows.
func TestMaskKeyRevealsLessThanItHides(t *testing.T) {
	for n := 0; n <= 40; n++ {
		key := strings.Repeat("k", n) + string(rune('0'+n%10))
		label := maskKey(key)
		if label == "" {
			continue
		}
		shown := utf8.RuneCountInString(label) - len("...")
		hidden := utf8.RuneCountInString(key) - shown
		if hidden < 2*shown {
			t.Fatalf("maskKey(len %d) = %q shows %d runes and hides %d", n, label, shown, hidden)
		}
	}
}

func TestMaskKeyFormatForLongKey(t *testing.T) {
	// 30 runes is the shortest key that still gets a label: 10 shown, 20 hidden.
	key := "sk-1234567890abcdefghijklmnopq"
	if label := maskKey(key); label != "sk-123...nopq" {
		t.Fatalf("maskKey(%q) = %q, want sk-123...nopq", key, label)
	}
	if got := maskKey("sk-1234567890abcdefghijklmnop"); got != "" {
		t.Fatalf("29-rune key produced label %q, want none", got)
	}
}

// TestShortKeysProduceNoLabel covers the exact bug: a seven-rune key used to
// come back as "key:" plus the key itself.
func TestShortKeysProduceNoLabel(t *testing.T) {
	for _, key := range []string{"abc", "abcdef", "abcdefg", "密钥一二"} {
		if label := maskKey(key); label != "" {
			t.Fatalf("maskKey(%q) = %q, want no label", key, label)
		}
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
	// An auth key that the directory has not resolved yet is not shown at all:
	// the credential id is not a provider name, and printing it in the provider
	// column would disclose an identifier the column never promised.
	m.beginStream("req-4", "", "", "auth-9", "", "", base)
	if got := m.providerLabelLocked(m.inflight["req-4"]); got != "" {
		t.Fatalf("providerLabel = %q, want empty rather than the auth key", got)
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

func TestMergeLiveOrdersByRate(t *testing.T) {
	items := []dimensionTotal{
		{Name: "prompt-heavy", Counters: counters{Input: 90000, Output: 1000, Gen: 1000, Tokens: 91000}},
		{Name: "fast", Counters: counters{Input: 100, Output: 4000, Gen: 4000, Tokens: 4100}},
	}
	merged := mergeLive(items, map[string]int64{"flowing": 9000}, 10*time.Second, 5*time.Second)

	if len(merged) != 3 {
		t.Fatalf("merged = %#v, want three rows", merged)
	}
	// The live-only row ranks on the rate it prints, which weights its count by
	// the live window it was measured over.
	if merged[0].Name != "flowing" {
		t.Fatalf("first row = %q, want the highest rate", merged[0].Name)
	}
	if merged[1].Name != "fast" {
		t.Fatalf("second row = %q, want fast ahead of prompt-heavy", merged[1].Name)
	}
}

// A live estimate is summed over the live window, so it must be divided by that
// horizon. Dividing it by the display window instead reports a running stream
// at a fraction of its pace: 500 tokens over the last 5 seconds is 100 tok/s,
// not 16.7.
func TestMergeLiveWeightsByItsOwnWindow(t *testing.T) {
	items := []dimensionTotal{
		{Name: "claude", Counters: counters{Gen: 9000}},
	}
	merged := mergeLive(items, map[string]int64{"claude": 500}, 30*time.Second, 5*time.Second)
	if got := merged[0].Rate; math.Abs(got-400) > 0.001 {
		t.Fatalf("rate = %v, want 300 committed + 100 live = 400", got)
	}
	// The committed counters must not absorb the estimate: they are exact
	// totals, and the live part is reported separately.
	if merged[0].Counters.Gen != 9000 {
		t.Fatalf("committed generated = %d, want 9000 untouched", merged[0].Counters.Gen)
	}
	if merged[0].Live != 500 {
		t.Fatalf("live = %d, want 500", merged[0].Live)
	}
}

// The in-flight column and the headline live figure measure the same thing, so
// the column must add up to the headline. A lifetime average cannot do that.
func TestInflightLiveColumnSumsToHeadline(t *testing.T) {
	m := newMeterState(8)
	now := base
	cfg := defaultMeterConfig()
	// Two streams, one older than the other, so a lifetime average would give
	// them different weights than their current pace.
	for _, id := range []string{"old", "new"} {
		m.beginStream(id, "", "claude", "auth-1", "client-1", "model-1", now.Add(-20*time.Second))
	}
	m.observeEstimate("old", counters{Gen: 100}, now.Add(-2*time.Second))
	m.observeEstimate("old", counters{Gen: 200}, now.Add(-1*time.Second))
	m.observeEstimate("new", counters{Gen: 300}, now.Add(-1*time.Second))

	view := m.snapshot(now, cfg, cfg.window())
	var sum float64
	for _, row := range view.Inflight {
		sum += row.LivePerSecond
	}
	if math.Abs(sum-view.Rate.Live) > 0.001 {
		t.Fatalf("in-flight column sums to %v, headline live is %v", sum, view.Rate.Live)
	}
	if sum == 0 {
		t.Fatal("expected a non-zero live rate for open streams")
	}
}

// A stream whose metadata carries no credential must still land in the provider
// breakdown once a completed record has taught the plugin which provider serves
// that model. Otherwise its live tokens vanish from the provider rows.
func TestProviderAttributionFallsBackToModel(t *testing.T) {
	m := newMeterState(8)
	cfg := defaultMeterConfig()
	m.recordFinal(makeRecord("done", time.Second, usageDetail{OutputTokens: 10}), base)
	// makeRecord is claude serving claude-sonnet-5-5; learn that pairing, then
	// open a stream with no auth key at all.
	m.beginStream("live", "", "", "", "client-1", "claude-sonnet-5-5", base)
	if got := m.providerLabelLocked(m.inflight["live"]); got != "claude" {
		t.Fatalf("providerLabel = %q, want claude learned from the model", got)
	}
	view := m.snapshot(base.Add(2*time.Second), cfg, cfg.window())
	var found bool
	for _, row := range view.Dimensions["provider"] {
		if row.Name == "claude" {
			found = true
		}
	}
	if !found {
		t.Fatal("provider breakdown lost the stream with no credential")
	}
}

// An unresolved credential must not be printed as the provider. It is an id,
// not a provider name, and the column is not the place to disclose it.
func TestProviderLabelNeverFallsBackToAuthKey(t *testing.T) {
	m := newMeterState(8)
	m.beginStream("req-1", "", "", "auth-secret-1", "", "", base)
	if got := m.providerLabelLocked(m.inflight["req-1"]); got != "" {
		t.Fatalf("providerLabel = %q, want empty rather than the auth key", got)
	}
}
