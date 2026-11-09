package main

import (
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// Defaults. The window is the "recent token rate" horizon: long enough to be
// stable, short enough to react to a traffic change.
const (
	defaultWindowSeconds     = 30
	defaultLiveWindowSeconds = 5
	defaultSeriesSeconds     = 60
	defaultCharsPerToken     = 4.0
	minWindowSeconds         = 2
	maxWindowSeconds         = bucketCount - 2
	maxSeriesSeconds         = bucketCount
)

// meterConfig is the plugin's view of plugins.configs.<pluginID>.
type meterConfig struct {
	windowSeconds     int
	liveWindowSeconds int
	seriesSeconds     int
	charsPerToken     float64

	TrackProvider bool
	TrackClient   bool
	TrackModel    bool
	TrackAuth     bool

	// ShowDashboard serves the HTML dashboard resource route.
	ShowDashboard bool
	// ClientLabels controls whether masked client key labels appear. Only the
	// masked form is ever held, never the raw key.
	ClientLabels bool
}

func defaultMeterConfig() meterConfig {
	return meterConfig{
		windowSeconds:     defaultWindowSeconds,
		liveWindowSeconds: defaultLiveWindowSeconds,
		seriesSeconds:     defaultSeriesSeconds,
		charsPerToken:     defaultCharsPerToken,
		TrackProvider:     true,
		TrackClient:       true,
		TrackModel:        true,
		TrackAuth:         false,
		ShowDashboard:     true,
		ClientLabels:      true,
	}
}

func (c meterConfig) window() time.Duration {
	return time.Duration(c.windowSeconds) * time.Second
}

func (c meterConfig) liveWindow() time.Duration {
	return time.Duration(c.liveWindowSeconds) * time.Second
}

// seriesSeconds is a field, so state.go reads it directly; no accessor needed.

// estimateTokensFromChars converts observed streamed characters into a token
// estimate. It is only ever used for live display; the authoritative totals come
// from usage records.
func estimateTokensFromChars(chars int64, cfg meterConfig) int64 {
	if chars <= 0 {
		return 0
	}
	ratio := cfg.charsPerToken
	if ratio <= 0 {
		ratio = defaultCharsPerToken
	}
	return int64(float64(chars)/ratio + 0.5)
}

// configStore holds the live config and the state built from it. The host may
// call reconfigure while hooks are running, so both swap together under a lock.
type configStore struct {
	mu    sync.RWMutex
	cfg   meterConfig
	state *meterState
}

func newConfigStore() *configStore {
	cfg := defaultMeterConfig()
	state := newMeterState(maxDimensions)
	state.setClientLabels(cfg.ClientLabels)
	return &configStore{cfg: cfg, state: state}
}

func (s *configStore) snapshot() (meterConfig, *meterState) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg, s.state
}

func (s *configStore) config() meterConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg
}

func (s *configStore) meter() *meterState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state
}

// reload parses the host-supplied YAML and installs the new config. The state
// is carried over unless dimensions were toggled in a way that invalidates it,
// so a hot reload does not blank the dashboard.
func (s *configStore) reload(raw []byte) error {
	cfg := defaultMeterConfig()
	if len(raw) > 0 {
		parsed, errParse := parseMeterConfig(raw)
		if errParse != nil {
			return errParse
		}
		cfg = parsed
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	resized := cfg.windowSeconds != s.cfg.windowSeconds ||
		cfg.seriesSeconds != s.cfg.seriesSeconds
	s.cfg = cfg
	if s.state == nil || resized {
		s.state = newMeterState(maxDimensions)
	}
	// Apply the label switch to the state itself, so turning labels off both
	// stops collecting them and drops any already held.
	s.state.setClientLabels(cfg.ClientLabels)
	return nil
}

// parseMeterConfig reads the plugin's own keys from the host payload. Unknown
// keys are ignored, which keeps the plugin working when the host template grows.
func parseMeterConfig(raw []byte) (meterConfig, error) {
	cfg := defaultMeterConfig()

	var doc struct {
		Enabled  *bool `yaml:"enabled"`
		Priority *int  `yaml:"priority"`

		WindowSeconds     *int     `yaml:"window_seconds"`
		LiveWindowSeconds *int     `yaml:"live_window_seconds"`
		SeriesSeconds     *int     `yaml:"series_seconds"`
		CharsPerToken     *float64 `yaml:"chars_per_token"`

		TrackProvider *bool `yaml:"track_provider"`
		TrackClient   *bool `yaml:"track_client"`
		TrackModel    *bool `yaml:"track_model"`
		TrackAuth     *bool `yaml:"track_auth"`
		ShowDashboard *bool `yaml:"show_dashboard"`
		ClientLabels  *bool `yaml:"client_labels"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return cfg, err
	}

	if doc.WindowSeconds != nil {
		cfg.windowSeconds = clampInt(*doc.WindowSeconds, minWindowSeconds, maxWindowSeconds)
	}
	if doc.LiveWindowSeconds != nil {
		cfg.liveWindowSeconds = clampInt(*doc.LiveWindowSeconds, 1, maxWindowSeconds)
	}
	if doc.SeriesSeconds != nil {
		cfg.seriesSeconds = clampInt(*doc.SeriesSeconds, 1, maxSeriesSeconds)
	}
	if doc.CharsPerToken != nil && *doc.CharsPerToken > 0 {
		cfg.charsPerToken = *doc.CharsPerToken
	}
	if doc.TrackProvider != nil {
		cfg.TrackProvider = *doc.TrackProvider
	}
	if doc.TrackClient != nil {
		cfg.TrackClient = *doc.TrackClient
	}
	if doc.TrackModel != nil {
		cfg.TrackModel = *doc.TrackModel
	}
	if doc.TrackAuth != nil {
		cfg.TrackAuth = *doc.TrackAuth
	}
	if doc.ShowDashboard != nil {
		cfg.ShowDashboard = *doc.ShowDashboard
	}
	if doc.ClientLabels != nil {
		cfg.ClientLabels = *doc.ClientLabels
	}

	// The series chart cannot be longer than the ring.
	if cfg.seriesSeconds > bucketCount {
		cfg.seriesSeconds = bucketCount
	}
	return cfg, nil
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// configFields advertises our settings so a management client can render them.
func configFields() []configField {
	return []configField{
		{
			Name: "window_seconds", Type: "integer", Default: defaultWindowSeconds,
			Description: "Horizon in seconds for the committed token rate and per-dimension totals.",
		},
		{
			Name: "live_window_seconds", Type: "integer", Default: defaultLiveWindowSeconds,
			Description: "Horizon in seconds for estimated tokens from streams still in flight.",
		},
		{
			Name: "series_seconds", Type: "integer", Default: defaultSeriesSeconds,
			Description: "Number of one-second points returned for the rate chart.",
		},
		{
			Name: "chars_per_token", Type: "number", Default: defaultCharsPerToken,
			Description: "Characters divided by this value estimate tokens when upstream reports no usage mid-stream.",
		},
		{
			Name: "track_provider", Type: "boolean", Default: true,
			Description: "Break down traffic by upstream provider.",
		},
		{
			Name: "track_client", Type: "boolean", Default: true,
			Description: "Break down traffic by client API key (kept as an irreversible hash, shown masked).",
		},
		{
			Name: "track_model", Type: "boolean", Default: true,
			Description: "Break down traffic by model.",
		},
		{
			Name: "track_auth", Type: "boolean", Default: false,
			Description: "Break down traffic by the selected credential.",
		},
		{
			Name: "show_dashboard", Type: "boolean", Default: true,
			Description: "Serve the read-only HTML dashboard under the plugin resource path.",
		},
		{
			Name: "client_labels", Type: "boolean", Default: true,
			Description: "Show a masked client key label beside the client hash in the dashboard.",
		},
	}
}

// normalizeProviderKey lowercases a provider key for stable grouping.
func normalizeProviderKey(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}
