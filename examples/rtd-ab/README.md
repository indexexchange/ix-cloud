# rtd-ab

An A/B testing example for Index Cloud: an ARTF `RTBExtensionPoint` container that splits auctions between a **control** and a **test** arm, and records how each arm's content-signal enrichment would have performed. It is the platform, not the experiment: the split, the model folder, the metrics and the evaluation method are what to reuse.

**No arm ever returns a mutation.** Every response is what the [rtd-simple](../rtd-simple/) example returns (echoed `id`, `api_version`, `model_version="none"`, empty mutation list, OK), so the arms are indistinguishable to the auction. Results are proxy measures (how many content signals each arm could derive, and the CPU it costs). They do not show auction win rate or revenue. How to read them: [docs/evaluation.md](docs/evaluation.md).

This is **sample code**, not a supported product; use of it with Index services is subject to the [Index Exchange Sample Code Terms](../../SAMPLE-CODE-TERMS.md). 

## Best practices it demonstrates

| Practice | How |
|----------|-----|
| Assign by a stable id, not by arrival order | The last hex digit of the auction id picks the arm: same auction, same arm, on every pod, with no shared state |
| Make the assignment recomputable offline | The auction id is in the Client Audit Logs (CAL), so any logged auction's arm is its last hex digit |
| Keep the mix out of the image | `ab-config.json` in the model folder sets the split; a new model version changes it with no rebuild |
| Fail safe | A missing or invalid config falls back to built-in models at the baked percentage with at most one error line; it never crashes or fails readiness |
| Check the split before the result | A sample-ratio check, an unrouted-request counter and a models-info metric say whether the arms are comparable |
| Do not contaminate an arm | A request with no usable auction id is counted and not analyzed |
| Report with statistics, not eyeballs | `_sum`, `_count` and `_sumsq` per arm support a z-test, a Welch t-test and confidence intervals |
| Respect the CPU budget | 0 allocations on the request path, atomic counters only, timing sampled 1 in 16, no per-request logging |
| Bound the telemetry | Labels are bounded enums only; idle payload about 3 KB, loaded about 10 KB, about 100 series |

## How it works

For each `PUBLISHER_BID_REQUEST`:

1. **Route.** Take `bid_request.id`, keep the text before the first underscore (the whole id if there is none), and read its last character as a hex digit `0` to `f`. A digit below the number of test groups goes to **test**, anything else to **control**. 4 groups (digits `0` to `3`) is 25%. An id with no usable auction id (`no_id`, `not_hex`), or any other lifecycle (`other_lifecycle`), is counted in `rtd_ab_unrouted_total{reason}` and not analyzed.
2. **Analyze.** The arm's strategy reads the request through generated getters (no reflection) and counts the content signals it can derive, from the sources its model enables.
3. **Record.** Atomic counters and fixed-point histograms (`_sum`, `_count`, `_sumsq`) per arm. One request in 16 per arm is timed into `rtd_ab_analyze_seconds`. A panic is recovered and counted in `rtd_ab_errors_total{arm}`.
4. **Respond.** The empty OK response, identical in both arms.

### The split: auction id, hex groups

Both lifecycles carry `bid_request.id` as `<auction_id>_<suffix>`; `auction_id` is a UUID. Each hex digit is one group of 1/16 (6.25%), so the share is aligned to the nearest 1/16, halves up:

| Test groups | Test digits | Test share |
|-------------|-------------|------------|
| 1 | `0` | 6.25% |
| 2 | `0`, `1` | 12.5% |
| 4 | `0` to `3` | 25% |
| 8 | `0` to `7` | 50% |

10% becomes 2 groups (12.5%); 33.33% becomes 5 groups (31.25%). The first digits are always test and the rest control. The share is statistical, not exact: a random UUID's last digit is uniform, so the observed share varies like a binomial over auctions (the sample-ratio check in the evaluation guide uses that). Assignment is auction-sticky and not user-sticky: one user's different auctions land in both arms, which is acceptable because neither arm acts on the auction.

### What the arms compute

The signals an arm can derive from one request, from the sources its model enables:

| Source | Counts | Cap |
|--------|--------|-----|
| `category` | IAB category ids in `cat`, `sectioncat`, `pagecat` (site or app) and `content.cat` (ids `IAB1` to `IAB26`, optional `-1` to `-99`, or a number 1 to 1500); duplicates once | 3 |
| `genre` | `content.genre` in the genre list, or any `content.genres` entry | 1 |
| `keyword` | tokens of `content.keywords` and `content.kwarray` in the keyword list (at most 32 scanned) | 3 |
| `network` | `content.network` or `content.channel` name or domain in the network list | 1 |
| `eid` | any `user.eids` | 1 |

The built-in control uses `category` only; the built-in test uses all five (at most 9 signals). Example: a site request with `cat=["IAB1"]`, genre `Drama`, keywords `news,sports` and one EID gives control 1 signal and test 5. A better test arm shows a higher enrichable rate (at least one signal) and more signals per request at equal or lower CPU cost. The genre, keyword and network lists are small and illustrative; a model can replace them.

## The model folder: mix and sources

The mix and each arm's settings are read at startup from the model folder (`MODEL_PATH`, `/models`). Index mounts a versioned S3 folder's contents there, so **a new model version changes the mix or the sources with no image rebuild and no new image tag**. The layout is the standard one (a single `manifest.json` at the top, all content under `model_data/`, as in rtd-simple's `models/2026.09.22.1`):

```
models/<CalVer>/
  manifest.json                    {"name": ..., "models": [files under model_data/]}
  model_data/
    ab-config.json                 which model each arm runs, and the percent mix
    model_a/content.json           content-signal settings
    model_b/content.json
```

`ab-config.json` names one model directory and a percent per arm. The percentages must sum to 100; the test percent is aligned to 1/16 and control takes the rest:

```json
{ "version": 1,
  "arms": { "control": { "model": "model_a", "pct": 75 },
            "test":    { "model": "model_b", "pct": 25 } } }
```

`content.json` (optional per model; a missing file means category-only):

| Key | Meaning |
|-----|---------|
| `sources` | Required, at least one of `category`, `genre`, `keyword`, `network`, `eid` |
| `genres`, `keywords`, `networks` | Optional allowlists, at most 5,000 entries each, matched case-insensitively (entries over 40 characters never match). Omitted means the built-in list; an empty list matches nothing |

`./models/2026.10.30.0` reproduces the built-ins: `model_a` is `["category"]`, `model_b` is all five sources.

Rules:

- **Fallback, never failure.** With no `ab-config.json` the container runs the built-in models at the baked percentage (`AB_TEST_PCT`, default 12.5; source `builtin_absent`). If the file or a model is invalid (bad JSON, an unknown key, percentages not summing to 100, a missing model directory, an unknown source) it logs **one error line** and runs the same fallback (`builtin_invalid`). Unknown keys are rejected so a typo cannot pass silently.
- **Read once, at startup.** A new mix applies when pods restart in a rollout, so start a new comparison window at each change.
- **Visible.** `rtd_ab_models_info{arm,model,source}` and the startup log line show which models run and where they came from. A fleet should report `source="file"`; a `builtin_*` row means the folder was not delivered or was rejected.
- `AB_TEST_PCT` (Makefile, default 12.5) is only the fallback: validated by the Makefile, injected with `-ldflags "-X main.bakedTestPct=..."`, and the aligned value is the image label `rtd.ab.test_pct`. `RTD_AB_TEST_PCT` overrides the percentage for **local harness runs only** and logs a WARN.
- The model folder must be delivered to the container by Index Cloud; until one is, the container runs `builtin_absent`.

## Metrics

Inherited from the rtd-simple example with unchanged names, types and units: `rpc_server_duration` (histogram in seconds, buckets 0.001 to 0.010, no `arm` label) and `rtd_simple_response_seconds_avg`, `rtd_simple_cpu_seconds_total`, `rtd_simple_cpu_limit_cores`, `rtd_simple_memory_bytes`, `rtd_simple_memory_limit_bytes`, `rtd_simple_memory_percent`, `rtd_simple_build_info`, `rtd_simple_model_present`, `rtd_simple_ready`.

New, prefix `rtd_ab_`:

| Metric | Type | Labels |
|--------|------|--------|
| `test_pct` | gauge | none |
| `build_info` | gauge | `test_pct`, `strategy_control`, `strategy_test` |
| `models_info` | gauge | `arm`, `model`, `source` |
| `requests_total` | counter | `arm` (always present) |
| `unrouted_total` | counter | `reason` |
| `analyze_seconds` | histogram, 1 µs to 1 ms, sampled 1 in 16 | `arm` |
| `errors_total` | counter | `arm` |
| `content_enrichable_total` | counter | `arm` |
| `content_signals` (+ `_sumsq` counter) | histogram | `arm` |
| `content_signal_total` | counter | `arm`, `source` |

Series are emitted after their first observation; HELP and TYPE lines are always present. Design notes are in [docs/evaluation.md](docs/evaluation.md) section 7.

## Logging

Startup lines (4 to 6; an invalid config adds one error line), errors, and at most one summary line per arm per minute, skipped when the arm saw no requests. No per-request logging and no request dumps; summary lines carry counts and rates only. A unit test and the harness check that planted request strings never reach logs or `/metrics`.

## Layout

| Path | Purpose |
|------|---------|
| `internal/router` | Auction-id router: last hex digit, test groups, alignment to 1/16 |
| `internal/ab` | `Strategy` and `Analysis` interfaces, `Observation`, the content analysis, atomic recorders, `/metrics` exporter, summary log |
| `internal/abmodel` | Reads `ab-config.json` and `content.json` from the model folder, with built-in fallback |
| `internal/abinit` | Resolves the configured percentage and the local override |
| `internal/agent` | `GetMutations`: calls the engine, returns the empty OK response |
| `internal/metrics`, `internal/model` | Inherited `rpc_server_duration` and `rtd_simple_*` metrics; model-presence check |
| `cmd/server`, `cmd/testrunner`, `cmd/abreport` | The container binary; the conformance harness; the reporting CLI (not in the image) |
| `dashboards/`, `docs/evaluation.md` | Grafana dashboard and import steps; how to judge test against control |
| `scripts/` | `harness.sh`, `validate-manifest.py`, `gen-proto.sh` (generates `pkg/pb` from the IAB `.proto` files) |
| `manifest/manifest.json` | Agent manifest |

## Adding a strategy

A new what-if is an `ab.Analysis` holding a control and a test `Strategy`.

1. **Implement the strategies.** `Name() string` and `Analyze(req *pb.RTBRequest, o *Observation)`. Read the request with generated getters only, write into your region of `Observation`, and do not allocate, lock or log.
2. **Add a region to `Observation`** in `internal/ab/types.go` (a fixed-size struct of counters; see `ContentObs`).
3. **Implement `ab.Analysis`**: `Name`, `Strategy(arm)`, `Record(arm, *Observation)` (atomic adds, `atomicHist.observe`), `WriteMetrics` (always `header(...)`, series only after the first observation) and `SummaryLine(arm)`. Use fixed-point `int64` values and a `histSpec`; keep the largest square below 1e8 per sample so `_sumsq` cannot overflow.
4. **Register it** in `cmd/server/main.go` in the `ab.NewEngine(...)` call.
5. **Cover it**: a strategy test, an entry in `internal/ab/bench_test.go` (the allocation test and benchmarks), its families in `declared` (`internal/ab/ab_test.go`) and `declaredFamilies` (`cmd/testrunner/main.go`), and a harness scenario with deterministic requests.
6. **Check the budget**: `make bench` (0 allocs/op, average added cost at most 1 microsecond, fail above 3) and the series and payload limits.

A panic in `Analyze` or `Record` is recovered and counted in `rtd_ab_errors_total{arm}`; the RPC still returns the normal empty response. A different lifecycle (for example `DSP_BID_RESPONSE` for shading) is a different example: it needs its own manifest lifecycle and analysis.

## Build, verify and release

```bash
docker info || open -a Docker    # Docker must be running
make proto                       # generate pkg/pb from proto/ (needed once before host go build/test; make unit/verify do it for you)
make build                       # amd64 image rtd-ab:local; runs unit tests in the build (models are not in the image)
make unit                        # go test ./... in a golang container
make race                        # go test -race ./... (host Go with cgo)
make bench                       # router, strategy and recorder benchmarks with allocations
make harness                     # fresh containers: shipped models, overrides 0/10/33.33/100, no config, invalid config, custom models
make verify                      # build + manifest vs image + trivy gate + harness; never pushes
make trivy IMAGE=<image>         # the vulnerability gate alone
```

**Vulnerability gate.** `make verify` (and so `make release`) fails on a trivy finding of any severity (CRITICAL to UNKNOWN) that has a fixed version; findings with no fix are ignored because a rebuild cannot clear them. Index Cloud's deploy scan can reject an image that a CRITICAL-only gate passed, so this gate is stricter. `make release` refuses to run with `TRIVY_SEVERITY` weakened. The scan pulls the current trivy database each run, so a new advisory can fail a build that was clean yesterday: bump the Go toolchain (the `golang:` tag in the `Dockerfile` and `Makefile`) or the module (`go get <module>@<fixed>`, never `go mod tidy`), then re-run.

`make release` is gated and pushes. Only run it when you mean to deploy:

```bash
echo deploy | make release REPO=your-namespace/rtd-ab TAG=<CalVer, unused> AB_TEST_PCT=12.5
```

It refuses a tag that is not a valid Index artifact tag (`YYYY.0M.0D.MICRO`), the placeholder repo, and a tag that already exists (check first: `docker buildx imagetools inspect your-namespace/rtd-ab:<tag>` must fail). A push auto-deploys to Index Cloud and replaces whatever is running; detection can lag, so do not re-push on lag.

`make verify` runs `scripts/harness.sh`, which starts eight fresh containers. Each run checks: zero mutations over 16,000 requests; the test count exactly equal to the auctions whose last hex digit is below the group count (requests cycle all 16 digits); unroutable ids and other lifecycles counted and not analyzed; per-arm signal counts matching the arm's model; `rpc_server_duration` unchanged; inherited metric names equal to the golden set; at most 300 series; idle payload at most 6 KB and loaded payload at most 25 KB; at most 10 container log lines; and the sentinel token absent from `/metrics` and logs. The runs cover the shipped models (75/25), four overrides, no `ab-config.json`, an invalid one (exactly one ERROR line), and custom 50/50 models whose sources differ from the built-ins.

## Performance budget

The manifest declares `targetQps` 1000 at 1000m CPU and 2Gi. At 4,000 QPS per pod, 1 microsecond per request is about 0.4% of one core. Budget for routing (an id scan of about 3 ns) plus one arm's analysis: 1 microsecond average target, 3 microseconds fail. `make bench` on a development laptop (arm64, not production amd64): control 33 ns and test 82 ns per request through the whole engine, 0 allocations.

## License

Apache-2.0 (see [`LICENSE`](LICENSE)); use with Index services is also subject to the
[Index Exchange Sample Code Terms](../../SAMPLE-CODE-TERMS.md). The IAB Tech Lab protocol
definitions in `proto/` are third-party material under their own licences, not this
sample's Apache License; see [`NOTICE`](NOTICE) and the [root NOTICE](../../NOTICE). The Go
bindings for them are generated at build time (`make proto`), not committed. The Go source is
also embedded in the image at `/usr/src/rtd-ab`.
