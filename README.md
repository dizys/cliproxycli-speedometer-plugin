<img src="assets/icon.svg" width="64" height="64" alt="">

# CLIProxyAPI Token Speedometer

A native CLIProxyAPI plugin that shows how many tokens per second the proxy is
serving, in real time, broken down by provider, client, model, and credential.

CLIProxyAPI reports exact token usage only after a request completes, so a
speedometer built on `usage.handle` alone would lurch once per request. This
plugin also intercepts the response stream, parses the SSE deltas as they pass,
and estimates token growth live. When the exact usage record lands, it replaces the estimate.
The dashboard therefore shows two stacked numbers: **committed** (exact, spread
across each request's latency) and **live** (estimated, from streams still open).

![The Token Speedometer dashboard: a headline rate, a stacked committed-and-live chart, breakdowns by provider, model, client, and credential, open streams, and totals](docs/dashboard.png)

The dashboard follows the system light/dark preference; [docs/dashboard-dark.png](docs/dashboard-dark.png)
shows the dark rendering. The **Table view** toggle opens the same numbers as a
table, including the per-dimension rows ([docs/dashboard-table.png](docs/dashboard-table.png)).
Numbers in the screenshots come from a synthetic traffic fixture, not a live
install.

## How it works

| Host hook | What the plugin does |
|---|---|
| `usage.handle` | Folds the exact record into second-aligned buckets, spread over the request's actual runtime so rates read as a smooth curve instead of spikes. |
| `response.intercept_stream_chunk` | Parses `data:` lines for upstream usage objects and new text; keeps a monotonic per-request estimate. Never modifies the chunk. |
| `request.complete` | Flushes the parser tail and closes the stream. |
| `management.register` / `management.handle` | Serves the JSON snapshot and the HTML dashboard. |

Correlation detail: every request path allocates its own request id (the
interceptor, the lifecycle hook, and the usage reporter each mint one), so the
three views are joined by the inbound request's `TraceID`. `request.complete`
carries that trace, and the usage record retires the matching live estimate by
it. Client identity joins on `caller_scope`, the host's irreversible key hash.

## Configuration

Settings live under `plugins.configs.<plugin-id>` in the host's `config.yaml`.
Every key is optional and unknown keys are ignored.

```yaml
plugins:
  configs:
    cliproxy-speedometer:
      window_seconds: 30        # committed rate horizon
      live_window_seconds: 5    # live estimate horizon
      series_seconds: 60        # chart points (one per second)
      chars_per_token: 4.0      # fallback estimate when a stream reports no usage
      track_provider: true
      track_client: true
      track_model: true
      track_auth: false
      show_dashboard: true
      client_labels: true
```

| Key | Type | Default | Meaning |
|---|---|---|---|
| `window_seconds` | integer | `30` | Horizon for the committed rate and windowed totals. Clamped to `[2, 88]`. |
| `live_window_seconds` | integer | `5` | Horizon for estimates from streams still in flight. |
| `series_seconds` | integer | `60` | Number of one-second points returned for the chart. Max `90`. |
| `chars_per_token` | number | `4.0` | Characters per token used when an upstream reports no usage mid-stream. |
| `track_provider` | boolean | `true` | Break down by upstream provider. |
| `track_client` | boolean | `true` | Break down by client. Only the irreversible caller-scope hash is stored. |
| `track_model` | boolean | `true` | Break down by model. |
| `track_auth` | boolean | `false` | Break down by the selected credential (auth id or index). |
| `show_dashboard` | boolean | `true` | Serve the HTML dashboard as a plugin resource. |
| `client_labels` | boolean | `true` | Show a masked client key label beside the hash in the dashboard. |

Changing `window_seconds` or `series_seconds` rebuilds the ring buffers, so the
window resets; every other key takes effect without losing data.

## Endpoints

- **Dashboard** - `/v0/resource/plugins/<plugin-id>/dashboard`, listed in the
  plugin menu as "Token Speedometer". Self-contained, no external assets.
- **Snapshot** - `GET /v0/management/speedometer/snapshot`, optionally with
  `?window=15s` or `?window=15`. Returns:

```json
{
  "plugin": { "name": "...", "version": "...", "uptime": 12.3,
              "window": 30, "live": 5, "series": 60, "ratio": 4 },
  "data": {
    "now": "2026-11-09T12:00:00Z",
    "rate": { "committed": 12.0, "live": 10.0, "total": 22.0 },
    "window":   { "input": 0, "output": 0, "reasoning": 0, "cached": 0,
                  "generated": 0, "tokens": 0, "requests": 0 },
    "lifetime": { "...": "same shape" },
    "dimensions": {
      "provider": [{ "name": "claude", "label": "", "counters": {}, "rate": 0 }],
      "client":   [],
      "model":    [],
      "auth":     []
    },
    "inflight": [],
    "streams": 0,
    "series": [{ "ts": 1794201540, "committed": 120, "live": 40 }]
  }
}
```

### What the rate means

`rate` and every per-dimension `rate` count **generated** tokens only
(`counters.generated`): completion tokens, plus reasoning when the provider
reports it outside the completion count. Prompt tokens are excluded.

That distinction is the difference between a speedometer and a random number
generator. A prompt is paid in one burst when the request starts, so dividing
it by the request's latency invents sustained throughput: a 149k-token prompt
answering in 5k tokens over 40 seconds reads as 3.8k tok/s of "work" while the
model was emitting a few dozen tokens per second.

`counters.tokens` remains the full total (prompt plus completion, counted the
way the provider bills them), so window and lifetime totals are complete. Only
the rates narrow to generation.

Both sets of totals live in memory and reset when the host restarts, or when
`window_seconds` or `series_seconds` change. This is an instrument, not an
accounting ledger: nothing is written to disk, priced, or reconciled.

### Why the dashboard needs a key, and when it does not

The HTML lives on the plugin resource route, which the host serves without
authentication. The snapshot lives on the management route, which the host gates
behind the management key. Being embedded in the control panel does not supply
that key: the host authenticates with a bearer key, not a session cookie, so the
iframe inherits no credential.

The dashboard offers the key the control panel already saved in this origin's
`localStorage` (`cli-proxy-auth`, the panel's own storage format), but only
after you click **Use the control panel's saved key**. It never spends a stored
credential on page load: the resource route is unauthenticated, so a link to
this page must not be able to trigger an authenticated request by itself. A
saved key is offered only when its recorded API base is this origin, so a key
for another proxy is never spent against this host's failure ban.

The key is held in page memory for the lifetime of the tab and is not written
back to storage. The key-entry form is shown when the panel was told not to
remember the key, or when the panel is served from a different origin than the
API.

The page sends `Authorization: Bearer <management key>` and stops polling on a
401/403/429 instead of retrying, so a rejected key can never contribute to the
host's per-IP management ban. "Forget key" stops the page offering the panel's
key again and clears the form.

## Privacy

- `usage.handle` exposes the real client `APIKey`. It is hashed with the host's
  own caller-scope algorithm and only the hash is kept as the dimension key. The
  raw key is never stored, logged, or emitted.
- A masked label (`sk-abc...wxyz`) is derived only when `client_labels` is on and
  only when the key is long enough that the characters shown are fewer than the
  characters hidden. Short keys get no label at all and are identified by hash
  alone. Turning `client_labels` off stops labels being derived and drops any
  already held.
- Live attribution uses `selected_auth_id` / `selected_auth_index` from the
  execution metadata, resolved to a provider through `host.auth.list`. The
  listing is refreshed at most once a minute.

## Install

The plugin is published as a GitHub release. Once its registry entry is merged,
open **Plugin Store** in the control panel and install **Token Speedometer**; the host
downloads the archive for its platform and verifies the checksum.

Until then, install straight from the release: download
`cliproxy-speedometer_<version>_<goos>_<goarch>.zip` for your platform, unzip it,
and put the library at the host's plugin root (see below).

Manual install: copy `cliproxy-speedometer-v<version>.so` into the host's plugins
directory (`plugins/linux/amd64/` on Linux), add a `plugins.configs.cliproxy-speedometer`
entry with `enabled: true`, and restart the host. The plugin id and version come
from the filename, and the host disables any plugin that has no config entry.

## Build

Requires Go 1.24+ and a C compiler (cgo).

```sh
./build.sh                     # -> cliproxy-speedometer-v0.1.3.so (linux/amd64)
ARCH=arm64 ./build.sh          # native arm64 build
```

`./build-release.sh` produces the plugin-store artifacts in `dist/`: one zip per
platform with the library at the zip root, plus `checksums.txt`. The store
requires all five supported platforms, so the script treats each one as
mandatory and fails rather than skipping a platform whose compiler is missing:

| Platform | Compiler |
|---|---|
| darwin/amd64, darwin/arm64 | `clang` on macOS (no portable cross toolchain exists) |
| linux/amd64 | host `cc` |
| linux/arm64 | `aarch64-linux-gnu-gcc` |
| windows/amd64 | `x86_64-w64-mingw32-gcc` |

`TARGET=<goos>/<goarch> ./build-release.sh` builds one platform, and
`./build-release.sh --package` zips whatever has been built, failing unless the
whole matrix is present. Tagging `v<version>` triggers
`.github/workflows/release.yml`, which runs the tests, builds each platform on a
matching runner, verifies the asset matrix, and publishes the release.

## Tests

```sh
go test ./...
```

Covers the ring/spread accounting, live-vs-committed rate split, dimension
overflow, SSE parsing for OpenAI/Anthropic/Gemini dialects, config clamping, and
the envelope shape of every RPC method.

## License

MIT. See [LICENSE](LICENSE).
