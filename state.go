package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sort"
	"sync"
	"time"
)

var errHostCallFailed = errors.New("host call failed")

const (
	// bucketResolution is the width of one ring slot. Rates come from summing
	// whole seconds, so a coarser slot quantizes the displayed rate.
	bucketResolution = time.Second
	// bucketCount is the ring depth. It must cover the longest display window
	// plus the spread of one request's tokens.
	bucketCount = 90
	// spreadLimitSeconds caps how far a single request's tokens are spread
	// backwards so a very long request cannot reach past the ring.
	spreadLimitSeconds = bucketCount - 2
	// maxInflight bounds tracked streams so an abandoned client cannot grow
	// the map without limit.
	maxInflight = 4096
	// maxDimensions bounds each dimension's series map. Past it, further values
	// fold into one "__other__" series rather than being dropped.
	maxDimensions = 512
)

// counters is one accounting bucket. Tokens is the billed total (prompt plus
// completion); Gen is the part the model actually generated and is what every
// rate is computed from. The breakdown fields are informational and never
// summed into either, because providers disagree about whether reasoning and
// cached tokens are already included in the prompt/completion counts.
//
// Prompt tokens arrive in a single burst when the request starts, so dividing
// them by the request's duration invents sustained throughput that never
// happened: a 149k-token prompt over 40s reads as 3.7k tok/s of "work" while
// the model is emitting a few dozen tokens. Rates therefore use Gen only.
type counters struct {
	Input    int64 `json:"input"`
	Output   int64 `json:"output"`
	Reason   int64 `json:"reasoning"`
	Cached   int64 `json:"cached"`
	Gen      int64 `json:"generated"`
	Tokens   int64 `json:"tokens"`
	Requests int64 `json:"requests"`
}

func (c *counters) add(o counters) {
	c.Input += o.Input
	c.Output += o.Output
	c.Reason += o.Reason
	c.Cached += o.Cached
	c.Gen += o.Gen
	c.Tokens += o.Tokens
	c.Requests += o.Requests
}

func (c *counters) sub(o counters) {
	c.Input -= o.Input
	c.Output -= o.Output
	c.Reason -= o.Reason
	c.Cached -= o.Cached
	c.Gen -= o.Gen
	c.Tokens -= o.Tokens
	c.Requests -= o.Requests
}

func (c counters) isZero() bool {
	return c.Input == 0 && c.Output == 0 && c.Reason == 0 && c.Cached == 0 &&
		c.Gen == 0 && c.Tokens == 0 && c.Requests == 0
}

func (c counters) clone() counters { return c }

// ring is a fixed-size, second-aligned circular buffer of counters.
type ring struct {
	slots [bucketCount]counters
	// stamp records which absolute second each slot holds so a stale slot from
	// an older lap is never counted.
	stamp [bucketCount]int64
}

func bucketID(t time.Time) int64 {
	return t.UnixNano() / int64(bucketResolution)
}

func bucketIndex(id int64) int {
	return int(((id % bucketCount) + bucketCount) % bucketCount)
}

// add accumulates into the slot for t, clearing a stale slot first.
func (r *ring) add(t time.Time, c counters) {
	if c.isZero() {
		return
	}
	id := bucketID(t)
	idx := bucketIndex(id)
	if r.stamp[idx] != id {
		r.slots[idx] = counters{}
		r.stamp[idx] = id
	}
	r.slots[idx].add(c)
}

// sum returns the total over the window ending at now, inclusive.
func (r *ring) sum(now time.Time, window time.Duration) counters {
	if window <= 0 {
		return counters{}
	}
	end := bucketID(now)
	start := bucketID(now.Add(-window)) + 1
	var out counters
	for id := start; id <= end; id++ {
		idx := bucketIndex(id)
		if r.stamp[idx] == id {
			out.add(r.slots[idx])
		}
	}
	return out
}

// buckets returns the generated-token total of each of the last n seconds,
// oldest first. Generated, not billed: the chart is a rate, so a prompt burst
// must not appear as throughput.
func (r *ring) buckets(now time.Time, n int) []int64 {
	if n <= 0 {
		return nil
	}
	if n > bucketCount {
		n = bucketCount
	}
	end := bucketID(now)
	out := make([]int64, n)
	for i := 0; i < n; i++ {
		id := end - int64(n-1-i)
		idx := bucketIndex(id)
		if r.stamp[idx] == id {
			out[i] = r.slots[idx].Gen
		}
	}
	return out
}

// sumGen is sum restricted to the generated-token counter.
func (r *ring) sumGen(now time.Time, window time.Duration) int64 {
	return r.sum(now, window).Gen
}

// spread distributes c evenly across the seconds a request occupied, ending at
// to. Usage records only arrive at completion, so writing one into a single
// bucket would make every rate spiky; spreading recovers a smooth curve at the
// cost of assuming a constant pace, which is the standard meter approximation.
//
// The rate-bearing quantity is Gen, not Tokens: prompt tokens are a one-time
// cost paid at request start, and spreading them over the request's runtime is
// exactly what turned a 149k prompt into thousands of phantom tokens/second.
// Tokens is still spread so windowed totals stay correct.
//
// A duration of d seconds writes exactly d buckets (at least one), so the
// window that covers [to-d, to] contains the whole request.
func (r *ring) spread(from, to time.Time, c counters) {
	if c.isZero() {
		return
	}
	if to.Before(from) {
		from = to
	}

	// Round up: a 1.2s request touched two seconds.
	seconds := int64((to.Sub(from) + bucketResolution - 1) / bucketResolution)
	if seconds < 1 {
		seconds = 1
	}
	if seconds > spreadLimitSeconds {
		seconds = spreadLimitSeconds
	}

	// Walk backwards from the newest second so the remainder lands on the
	// leading edge, where the tokens were actually produced last.
	for i := int64(0); i < seconds; i++ {
		t := to.Add(-time.Duration(i) * bucketResolution)
		var part counters
		// Tokens is distributed exactly so windowed and lifetime totals agree;
		// Gen is distributed independently because it is the rate numerator and
		// may be a different quantity (a prompt-only request has Tokens but no
		// Gen, and the two must not be forced into the same proportions).
		part.Tokens = perBucket(c.Tokens, seconds, i)
		part.Gen = perBucket(c.Gen, seconds, i)
		part.Requests = perBucket(c.Requests, seconds, i)
		if c.Tokens > 0 {
			part.Input = scaleTo(part.Tokens, c.Input, c.Tokens)
			part.Output = scaleTo(part.Tokens, c.Output, c.Tokens)
			part.Reason = scaleTo(part.Tokens, c.Reason, c.Tokens)
			part.Cached = scaleTo(part.Tokens, c.Cached, c.Tokens)
		}
		r.add(t, part)
	}
}

// perBucket splits total evenly across seconds, putting the remainder on the
// newest buckets (low i).
func perBucket(total, seconds, i int64) int64 {
	part := total / seconds
	if rem := total % seconds; rem > 0 && i < rem {
		part++
	}
	return part
}

func scaleTo(part, whole, total int64) int64 {
	if total <= 0 || whole == 0 {
		return 0
	}
	return part * whole / total
}

// keyedRing is one named dimension value (a provider, client, model...).
type keyedRing struct {
	ring     ring
	lastSeen int64 // unix seconds, guarded by seriesSet.mu
}

// seriesSet holds dimension value -> ring.
type seriesSet struct {
	mu   sync.RWMutex
	data map[string]*keyedRing
	max  int
}

func newSeriesSet(max int) *seriesSet {
	if max <= 0 {
		max = 256
	}
	return &seriesSet{data: make(map[string]*keyedRing), max: max}
}

func (s *seriesSet) getOrCreate(name string, now time.Time) *keyedRing {
	s.mu.RLock()
	kr := s.data[name]
	s.mu.RUnlock()
	if kr != nil {
		return kr
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if kr = s.data[name]; kr != nil {
		return kr
	}
	if len(s.data) >= s.max {
		// At capacity, fold further values into one overflow series instead of
		// silently dropping their traffic.
		if kr = s.data["__other__"]; kr != nil {
			return kr
		}
		name = "__other__"
	}
	kr = &keyedRing{lastSeen: now.Unix()}
	s.data[name] = kr
	return kr
}

func (s *seriesSet) add(name string, from, to time.Time, c counters) {
	if name == "" {
		name = "__unknown__"
	}
	kr := s.getOrCreate(name, to)
	// Hold the write lock across the spread: windowTotals reads these rings
	// under RLock, so mutating one outside the lock would race.
	s.mu.Lock()
	kr.lastSeen = to.Unix()
	kr.ring.spread(from, to, c)
	s.mu.Unlock()
}

func (s *seriesSet) windowTotals(now time.Time, window time.Duration) []dimensionTotal {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]dimensionTotal, 0, len(s.data))
	for name, kr := range s.data {
		c := kr.ring.sum(now, window)
		if c.isZero() {
			continue
		}
		out = append(out, dimensionTotal{Name: name, Counters: c})
	}
	sort.Slice(out, func(i, j int) bool {
		// Every rate is a generation rate, so the ordering has to follow the
		// same counter the dashboard prints and the bar widths derive from. A
		// rank by billed tokens would put a large-prompt, low-generation row
		// above a fast one and make the column read out of order.
		if out[i].Counters.Gen != out[j].Counters.Gen {
			return out[i].Counters.Gen > out[j].Counters.Gen
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// prune drops series idle longer than keep, bounding memory over time.
func (s *seriesSet) prune(now time.Time, keep time.Duration) {
	cutoff := now.Add(-keep).Unix()
	s.mu.Lock()
	defer s.mu.Unlock()
	for name, kr := range s.data {
		if name == "__other__" {
			continue
		}
		if kr.lastSeen < cutoff {
			delete(s.data, name)
		}
	}
}

type dimensionTotal struct {
	Name     string   `json:"name"`
	Label    string   `json:"label,omitempty"`
	Counters counters `json:"counters"`
	Rate     float64  `json:"rate"`
}

// estStamp is one second of estimated tokens for a stream.
type estStamp struct {
	bucket int64
	c      counters
}

// inflight is one streaming request whose final usage record has not arrived.
type inflight struct {
	requestID string
	trace     string
	provider  string
	client    string
	model     string
	// authKey is the selected credential (auth id or index), resolved to a
	// provider through authProvider for display.
	authKey string

	startedAt time.Time
	lastChunk time.Time

	// total is the last estimate observed for this stream; deltas against it
	// become the per-second stamps the live window sums.
	total  counters
	stamps []estStamp

	done bool
}

// liveTokens sums this stream's estimated generated tokens over the live
// window ending at now.
func (fi *inflight) liveTokens(now time.Time, window time.Duration) int64 {
	if window <= 0 {
		return 0
	}
	start := bucketID(now.Add(-window)) + 1
	var tokens int64
	for _, st := range fi.stamps {
		if st.bucket >= start {
			tokens += st.c.Gen
		}
	}
	return tokens
}

// inflightView is the per-stream row shown on the dashboard.
type inflightView struct {
	RequestID       string   `json:"request_id"`
	Provider        string   `json:"provider,omitempty"`
	Client          string   `json:"client,omitempty"`
	ClientLabel     string   `json:"client_label,omitempty"`
	Model           string   `json:"model,omitempty"`
	StartedAt       string   `json:"started_at"`
	ElapsedSeconds  float64  `json:"elapsed_seconds"`
	Counters        counters `json:"counters"`
	TokensPerSecond float64  `json:"tokens_per_second"`
}

// meterState is the single shared store the hooks write and snapshots read.
type meterState struct {
	mu sync.Mutex

	total ring

	provider *seriesSet
	client   *seriesSet
	model    *seriesSet
	auth     *seriesSet

	inflight map[string]*inflight
	// traceIndex maps a trace id to the stream's request id. The stream
	// interceptor and the usage hook use different request id spaces, so a
	// usage record can only reach its live estimate through the trace.
	traceIndex map[string]string
	lifetime   counters

	// authKey -> provider, learned from host.auth.list.
	authProvider map[string]string
	// caller-scope hash -> masked client label, learned from usage records.
	// Only populated while labels are enabled; see setClientLabels.
	clientLabel map[string]string
	// labels enables recording into clientLabel. When false, no label is
	// derived or retained, so a disabled feature leaves nothing to disclose.
	labels bool

	startedAt time.Time
}

func newMeterState(maxDimensions int) *meterState {
	return &meterState{
		provider:     newSeriesSet(maxDimensions),
		client:       newSeriesSet(maxDimensions),
		model:        newSeriesSet(maxDimensions),
		auth:         newSeriesSet(maxDimensions),
		inflight:     make(map[string]*inflight),
		traceIndex:   make(map[string]string),
		authProvider: make(map[string]string),
		clientLabel:  make(map[string]string),
		labels:       true,
		startedAt:    time.Now(),
	}
}

// setClientLabels enables or disables label collection. Turning it off drops
// every label already derived, so the map holds no key material while the
// feature is disabled.
func (m *meterState) setClientLabels(enabled bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.labels = enabled
	if !enabled && len(m.clientLabel) > 0 {
		m.clientLabel = make(map[string]string)
	}
}

// countersFromDetail converts a wire usage record into counters.
func countersFromDetail(d usageDetail) counters {
	c := counters{
		Input:    d.InputTokens,
		Output:   d.OutputTokens,
		Reason:   d.ReasoningTokens,
		Cached:   d.CachedTokens,
		Requests: 1,
	}
	switch {
	case d.TotalTokens > 0:
		c.Tokens = d.TotalTokens
	default:
		c.Tokens = d.InputTokens + d.OutputTokens
	}
	if sum := d.InputTokens + d.OutputTokens; sum > c.Tokens {
		c.Tokens = sum
	}
	c.Gen = generatedFromDetail(d)
	return c
}

// generatedFromDetail returns the tokens the model emitted, which is what a
// token-per-second rate can honestly measure.
//
// Providers disagree about whether reasoning tokens are already inside the
// output count: OpenAI's completion_tokens and Anthropic's output_tokens
// include them, while Gemini reports thoughtsTokenCount alongside a
// candidatesTokenCount that excludes them. When the reported total is at least
// prompt + output + reasoning, the reasoning was counted separately and must be
// added; otherwise output already contains it.
func generatedFromDetail(d usageDetail) int64 {
	if d.OutputTokens <= 0 && d.ReasoningTokens <= 0 {
		return 0
	}
	if d.ReasoningTokens > 0 && d.TotalTokens >= d.InputTokens+d.OutputTokens+d.ReasoningTokens {
		return d.OutputTokens + d.ReasoningTokens
	}
	return d.OutputTokens
}

// recordFinal folds a completed request's exact token counts into the windowed
// aggregates, spread over the time the request was actually running.
func (m *meterState) recordFinal(rec usageRecord, now time.Time) {
	c := countersFromDetail(rec.Detail)
	if c.Requests == 0 {
		c.Requests = 1
	}
	from := rec.RequestedAt
	if from.IsZero() {
		from = now
	}
	to := from.Add(rec.Latency)
	if rec.Latency <= 0 || to.After(now) {
		to = now
	}
	if to.Before(from) {
		to = from
	}
	// Generation cannot start before the first token arrives. Folding the
	// time-to-first-token into the spread would divide the same output by a
	// longer interval and report a slower pace than the model actually managed,
	// which is most visible on short answers with a long prompt.
	if rec.TTFT > 0 {
		if start := from.Add(rec.TTFT); start.Before(to) {
			from = start
		}
	}

	clientKey := m.clientKeyFor(rec.APIKey)

	m.mu.Lock()
	m.total.spread(from, to, c)
	m.lifetime.add(c)
	if key := rec.AuthID; key != "" && rec.Provider != "" {
		m.authProvider[key] = rec.Provider
	}
	if rec.AuthIndex != "" && rec.Provider != "" {
		m.authProvider[rec.AuthIndex] = rec.Provider
	}
	m.mu.Unlock()

	m.provider.add(rec.Provider, from, to, c)
	m.client.add(clientKey, from, to, c)
	m.model.add(rec.Model, from, to, c)
	m.auth.add(rec.AuthID, from, to, c)
}

// clientKeyFor returns the dimension key used for a client. Live stream data
// only carries the irreversible caller-scope hash, so completed records use
// the same hash to keep one client from splitting into two series.
func (m *meterState) clientKeyFor(apiKey string) string {
	if apiKey == "" {
		return "__unknown__"
	}
	scope := callerScope(apiKey)
	m.mu.Lock()
	// The raw key never leaves this function. A label is derived and stored only
	// when the feature is on and the mask actually hides part of the key;
	// recording an empty string would mark the scope as seen with nothing to
	// show, so the map holds only real labels.
	if m.labels {
		if _, ok := m.clientLabel[scope]; !ok {
			if label := maskKey(apiKey); label != "" {
				m.clientLabel[scope] = label
			}
		}
	}
	m.mu.Unlock()
	return scope
}

// callerScope mirrors the host's caller-scope derivation so live and completed
// identities agree.
func callerScope(value string) string {
	if value == "" {
		return ""
	}
	sum := sha256.Sum256([]byte("cli-proxy-api:caller-scope:v1\x00" + value))
	return hex.EncodeToString(sum[:])
}

// Label geometry. A label shows this many leading and trailing characters of a
// key, and only when doing so hides at least twice as much as it reveals.
const (
	maskPrefixRunes = 6
	maskSuffixRunes = 4
)

// maskKey builds a display label for a client key. It reveals a fixed prefix and
// suffix only when the characters left hidden are at least twice the number
// shown, so a label identifies a key without describing a usable fraction of
// it. Shorter keys get no label at all: at those lengths any prefix/suffix pair
// hands over too much of the key. An empty label is safe everywhere, because the
// dashboard and the snapshot both fall back to the caller-scope hash, which
// cannot be reversed into the key.
func maskKey(key string) string {
	runes := []rune(key)
	shown := maskPrefixRunes + maskSuffixRunes
	if len(runes) < 3*shown {
		return ""
	}
	return string(runes[:maskPrefixRunes]) + "..." + string(runes[len(runes)-maskSuffixRunes:])
}

// beginStream registers an open stream on first sight of a request ID and
// refreshes any attribution learned since.
func (m *meterState) beginStream(reqID, trace, provider, authKey, client, model string, now time.Time) *inflight {
	if reqID == "" {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	fi, ok := m.inflight[reqID]
	if ok {
		m.applyAttributionLocked(fi, provider, authKey, client, model)
		m.setTraceLocked(fi, trace)
		return fi
	}
	if len(m.inflight) >= maxInflight {
		return nil
	}
	fi = &inflight{
		requestID: reqID,
		startedAt: now,
		lastChunk: now,
	}
	m.applyAttributionLocked(fi, provider, authKey, client, model)
	m.setTraceLocked(fi, trace)
	m.inflight[reqID] = fi
	return fi
}

// setTraceLocked records a stream's trace id and keeps the reverse index in
// step. The newest non-empty trace wins, since a request carries exactly one.
func (m *meterState) setTraceLocked(fi *inflight, trace string) {
	if trace == "" || trace == fi.trace {
		return
	}
	if fi.trace != "" && m.traceIndex[fi.trace] == fi.requestID {
		delete(m.traceIndex, fi.trace)
	}
	fi.trace = trace
	m.traceIndex[trace] = fi.requestID
}

// removeStreamLocked deletes a stream and its trace index entry.
func (m *meterState) removeStreamLocked(fi *inflight) {
	delete(m.inflight, fi.requestID)
	if fi.trace != "" && m.traceIndex[fi.trace] == fi.requestID {
		delete(m.traceIndex, fi.trace)
	}
}

// applyAttributionLocked fills attribution fields without overwriting what is
// already known, since metadata arrives progressively across hook calls.
func (m *meterState) applyAttributionLocked(fi *inflight, provider, authKey, client, model string) {
	if fi == nil {
		return
	}
	if provider != "" {
		fi.provider = provider
	}
	if authKey != "" {
		fi.authKey = authKey
	}
	if client != "" {
		fi.client = client
	}
	if model != "" && fi.model == "" {
		fi.model = model
	}
}

// setStreamProvider attaches provider/model info learned after auth selection.
func (m *meterState) setStreamProvider(reqID, provider, authKey, client, model string, now time.Time) {
	if reqID == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	fi, ok := m.inflight[reqID]
	if !ok {
		return
	}
	m.applyAttributionLocked(fi, provider, authKey, client, model)
	fi.lastChunk = now
}

// observeEstimate records the latest cumulative estimate for a stream and turns
// its growth since the previous observation into a per-second stamp. Both
// counters are monotonic within a stream, so a decrease means the provider
// re-reported a smaller snapshot and that counter's delta is clamped to zero
// rather than subtracted.
func (m *meterState) observeEstimate(reqID string, c counters, now time.Time) {
	if reqID == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	fi, ok := m.inflight[reqID]
	if !ok {
		return
	}
	fi.lastChunk = now
	delta := c
	delta.sub(fi.total)
	// Clamp the two monotonic counters independently. A prompt can be revised
	// downwards (reported then corrected) without losing the generated tokens
	// already counted, and vice versa.
	if delta.Tokens < 0 {
		delta.Tokens = 0
	}
	if delta.Gen < 0 {
		delta.Gen = 0
	}
	if delta.isZero() {
		return
	}
	fi.total = c
	id := bucketID(now)
	if n := len(fi.stamps); n > 0 && fi.stamps[n-1].bucket == id {
		fi.stamps[n-1].c.add(delta)
		return
	}
	fi.stamps = append(fi.stamps, estStamp{bucket: id, c: delta})
	if len(fi.stamps) > bucketCount {
		fi.stamps = append(fi.stamps[:0], fi.stamps[len(fi.stamps)-bucketCount:]...)
	}
}

// completeStream closes the stream registered under reqID and records its trace
// id. The trace is the only shared key between the stream interceptor, the
// request lifecycle hook, and the usage record: each allocates its own request
// id, but all three carry the inbound request's trace.
func (m *meterState) completeStream(reqID, trace string) {
	if reqID == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if fi, ok := m.inflight[reqID]; ok {
		m.setTraceLocked(fi, trace)
		fi.done = true
	}
}

// dropStream removes a stream outright. Used when its exact usage record has
// landed, so its estimates can no longer affect any window.
func (m *meterState) dropStream(reqID string) {
	if reqID == "" {
		return
	}
	m.mu.Lock()
	if fi, ok := m.inflight[reqID]; ok {
		m.removeStreamLocked(fi)
	}
	m.mu.Unlock()
}

// dropByTrace removes the stream a usage record belongs to. Stream chunks and
// usage records are keyed by different request id spaces, so the trace id is
// the only way to retire a live estimate from the exact record.
func (m *meterState) dropByTrace(trace string) {
	if trace == "" {
		return
	}
	m.mu.Lock()
	if reqID, ok := m.traceIndex[trace]; ok {
		if fi, ok := m.inflight[reqID]; ok {
			m.removeStreamLocked(fi)
		}
		delete(m.traceIndex, trace)
	}
	m.mu.Unlock()
}

// reap drops finished and abandoned streams and prunes idle dimension series.
func (m *meterState) reap(now time.Time) {
	m.mu.Lock()
	for _, fi := range m.inflight {
		switch {
		case fi.done && now.Sub(fi.lastChunk) > 10*time.Second:
			m.removeStreamLocked(fi)
		case !fi.done && now.Sub(fi.lastChunk) > 5*time.Minute:
			m.removeStreamLocked(fi)
		}
	}
	m.mu.Unlock()

	m.provider.prune(now, 10*time.Minute)
	m.client.prune(now, 10*time.Minute)
	m.model.prune(now, 10*time.Minute)
	m.auth.prune(now, 10*time.Minute)
}

// rateView is the headline rate split into committed and live parts.
type rateView struct {
	Committed float64 `json:"committed"`
	Live      float64 `json:"live"`
	Total     float64 `json:"total"`
}

type seriesPoint struct {
	TS        int64 `json:"ts"`
	Committed int64 `json:"committed"`
	Live      int64 `json:"live"`
}

// snapshotView is a consistent read of the whole meter.
type snapshotView struct {
	Now               time.Time                   `json:"now"`
	StartedAt         time.Time                   `json:"started_at"`
	UptimeSeconds     float64                     `json:"uptime_seconds"`
	WindowSeconds     float64                     `json:"window_seconds"`
	LiveWindowSeconds float64                     `json:"live_window_seconds"`
	Rate              rateView                    `json:"rate"`
	Window            counters                    `json:"window"`
	Lifetime          counters                    `json:"lifetime"`
	Dimensions        map[string][]dimensionTotal `json:"dimensions"`
	Inflight          []inflightView              `json:"inflight"`
	Streams           int                         `json:"streams"`
	Series            []seriesPoint               `json:"series"`
}

func (m *meterState) snapshot(now time.Time, cfg meterConfig, window time.Duration) snapshotView {
	if window <= 0 {
		window = cfg.window()
	}
	liveWindow := cfg.liveWindow()

	m.mu.Lock()
	committed := m.total.sum(now, window)
	lifetime := m.lifetime
	seriesBase := m.total.buckets(now, cfg.seriesSeconds)

	liveByDim := map[string]map[string]int64{}
	var liveTotal int64
	rows := make([]inflightView, 0, len(m.inflight))

	for _, fi := range m.inflight {
		if fi.done {
			continue
		}
		live := fi.liveTokens(now, liveWindow)
		if now.Sub(fi.lastChunk) > liveWindow {
			live = 0
		}
		liveTotal += live

		if cfg.TrackProvider {
			addLive(liveByDim, "provider", m.providerLabelLocked(fi), live)
		}
		if cfg.TrackClient {
			addLive(liveByDim, "client", fi.client, live)
		}
		if cfg.TrackModel {
			addLive(liveByDim, "model", fi.model, live)
		}
		if cfg.TrackAuth {
			addLive(liveByDim, "auth", fi.authKey, live)
		}

		elapsed := now.Sub(fi.startedAt).Seconds()
		if elapsed <= 0 {
			elapsed = 1
		}
		// A stream a few milliseconds old has an enormous instantaneous rate;
		// dividing by at least one second keeps the row readable and matches the
		// one-second resolution of everything else here. The numerator is the
		// generated count, so a stream that has only reported its prompt so far
		// shows 0 tok/s rather than the prompt size per second.
		rateBasis := elapsed
		if rateBasis < 1 {
			rateBasis = 1
		}
		rows = append(rows, inflightView{
			RequestID:       fi.requestID,
			Provider:        m.providerLabelLocked(fi),
			Client:          fi.client,
			ClientLabel:     m.clientLabel[fi.client],
			Model:           fi.model,
			StartedAt:       fi.startedAt.UTC().Format(time.RFC3339Nano),
			ElapsedSeconds:  elapsed,
			Counters:        fi.total,
			TokensPerSecond: float64(fi.total.Gen) / rateBasis,
		})
	}
	streams := len(rows)
	clientLabels := make(map[string]string, len(m.clientLabel))
	for k, v := range m.clientLabel {
		clientLabels[k] = v
	}
	m.mu.Unlock()

	sort.Slice(rows, func(i, j int) bool {
		if rows[i].TokensPerSecond != rows[j].TokensPerSecond {
			return rows[i].TokensPerSecond > rows[j].TokensPerSecond
		}
		return rows[i].StartedAt < rows[j].StartedAt
	})

	dims := map[string][]dimensionTotal{
		"provider": withRates(m.provider.windowTotals(now, window), window),
		"client":   withRates(m.client.windowTotals(now, window), window),
		"model":    withRates(m.model.windowTotals(now, window), window),
		"auth":     withRates(m.auth.windowTotals(now, window), window),
	}
	// Fold live estimates into the same dimension rows so a running stream is
	// visible per provider/client before its record lands.
	for dim, values := range liveByDim {
		dims[dim] = mergeLive(dims[dim], values, window)
	}
	for i := range dims["client"] {
		if label, ok := clientLabels[dims["client"][i].Name]; ok {
			dims["client"][i].Label = label
		}
	}

	windowSeconds := window.Seconds()
	liveSeconds := liveWindow.Seconds()
	// Rates are generation rates: the prompt is a one-off cost, not throughput.
	// Windowed totals still report billed tokens in full.
	rate := rateView{
		Committed: float64(committed.Gen) / windowSeconds,
		Live:      float64(liveTotal) / liveSeconds,
	}
	rate.Total = rate.Committed + rate.Live

	return snapshotView{
		Now:               now.UTC(),
		StartedAt:         m.startedAt.UTC(),
		UptimeSeconds:     now.Sub(m.startedAt).Seconds(),
		WindowSeconds:     windowSeconds,
		LiveWindowSeconds: liveSeconds,
		Rate:              rate,
		Window:            committed,
		Lifetime:          lifetime,
		Dimensions:        dims,
		Inflight:          rows,
		Streams:           streams,
		Series:            buildSeries(seriesBase, now, m.liveSeries(now)),
	}
}

func addLive(dst map[string]map[string]int64, dim, name string, tokens int64) {
	if tokens <= 0 || name == "" {
		return
	}
	if dst[dim] == nil {
		dst[dim] = make(map[string]int64)
	}
	dst[dim][name] += tokens
}

func withRates(items []dimensionTotal, window time.Duration) []dimensionTotal {
	secs := window.Seconds()
	out := make([]dimensionTotal, 0, len(items))
	for _, item := range items {
		item.Rate = float64(item.Counters.Gen) / secs
		out = append(out, item)
	}
	return out
}

func mergeLive(items []dimensionTotal, live map[string]int64, window time.Duration) []dimensionTotal {
	secs := window.Seconds()
	byName := make(map[string]int, len(items))
	for i := range items {
		byName[items[i].Name] = i
	}
	for name, tokens := range live {
		if tokens <= 0 {
			continue
		}
		if idx, ok := byName[name]; ok {
			items[idx].Counters.Gen += tokens
			// Live estimates are generation only, so the billed total moves by
			// the same amount until the exact record replaces it.
			items[idx].Counters.Tokens += tokens
			items[idx].Rate = float64(items[idx].Counters.Gen) / secs
			continue
		}
		items = append(items, dimensionTotal{
			Name:     name,
			Rate:     float64(tokens) / secs,
			Counters: counters{Gen: tokens, Tokens: tokens},
		})
	}
	sort.Slice(items, func(i, j int) bool {
		// Same order as the dashboard prints: generation, which is the rate
		// every row shows, with a stable name tiebreak.
		if items[i].Counters.Gen != items[j].Counters.Gen {
			return items[i].Counters.Gen > items[j].Counters.Gen
		}
		return items[i].Name < items[j].Name
	})
	return items
}

// liveSeries maps live estimates onto absolute second buckets for the chart.
// Finished streams are excluded: once a stream is done its committed record is
// authoritative, and keeping its estimates would double-count the same tokens
// on the chart while it waits for the reap grace period.
func (m *meterState) liveSeries(now time.Time) map[int64]int64 {
	out := make(map[int64]int64)
	current := bucketID(now)
	m.mu.Lock()
	for _, fi := range m.inflight {
		if fi.done {
			continue
		}
		for _, st := range fi.stamps {
			if st.bucket > current {
				continue
			}
			out[st.bucket] += st.c.Gen
		}
	}
	m.mu.Unlock()
	return out
}

func buildSeries(committed []int64, now time.Time, live map[int64]int64) []seriesPoint {
	n := len(committed)
	if n == 0 {
		return nil
	}
	end := bucketID(now)
	out := make([]seriesPoint, 0, n)
	for i := 0; i < n; i++ {
		id := end - int64(n-1-i)
		point := seriesPoint{
			TS:        id * int64(bucketResolution) / int64(time.Second),
			Committed: committed[i],
		}
		if v, ok := live[id]; ok {
			point.Live = v
		}
		out = append(out, point)
	}
	return out
}

// setAuthProvider records an auth id/index to provider mapping.
func (m *meterState) setAuthProvider(key, provider string) {
	if key == "" || provider == "" {
		return
	}
	m.mu.Lock()
	m.authProvider[key] = provider
	m.mu.Unlock()
}

// providerLabelLocked resolves the display provider for a stream, preferring
// the learned auth directory over the provider guessed at request time. An
// unresolved auth key falls through as itself: it is still a stable grouping
// label, and hiding it would drop the stream from the provider breakdown until
// its usage record lands.
func (m *meterState) providerLabelLocked(fi *inflight) string {
	if p, ok := m.authProvider[fi.authKey]; ok {
		return p
	}
	if fi.provider != "" {
		return fi.provider
	}
	return fi.authKey
}
