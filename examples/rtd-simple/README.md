# rtd-simple

A no-op Index Cloud RTD container for exercising the onboarding process. It
implements the full IAB ARTF `RTBExtensionPoint` contract but returns **zero
mutations** on every request, so it changes nothing about the auction. Every
request is logged, so you can watch exactly what Index sends into a container as
you walk through onboarding.

Use it to validate an onboarding run end to end (image build, deploy, health,
metrics, live traffic) without any decisioning risk.

## Prerequisites

- **Docker** — required to build and run the container (`make build`, `make up`).
- **Go** and **Python 3** — required for the tests and checks (`make test`,
  `make verify`). The Go toolchain version is pinned in `go.mod`.
- **grpcurl** — optional. The manifest validator uses it to introspect the gRPC
  endpoints; when it is not installed the validator falls back to a dockerized
  grpcurl, so you only need to install it where that fallback cannot reach the
  container (notably Linux CI runners — which is why the shipped workflow installs
  it).

The container image itself is `FROM scratch` and needs none of this at runtime.

## What it does

- Serves the gRPC `RTBExtensionPoint` `GetMutations` RPC. Every response echoes
  the request id, sets `metadata.api_version`, and carries an empty mutation
  list (a valid response).
- Serves the gRPC health service (`grpc.health.v1.Health`) and the HTTP
  `/heartbeat`, `/healthcheck`, and `/metrics` endpoints Index Cloud requires.
- Exposes the required `rpc_server_duration` histogram with the exact buckets and
  tags Index validates, plus self-reported CPU, memory, and average response time
  (see Metrics below).
- Logs a startup summary plus errors only by default; verbose per-request logging
  is opt-in via `RTD_LOG_SAMPLE_N` (see below), so it never floods logs under load.
- Checks for a delivered model at `MODEL_PATH` and reports it (present/absent,
  version, files) in the log and via the `rtd_simple_model_present` metric. It does
  not read model data (it is a no-op); models are optional.
- Makes no outbound calls.

## What the logs show

Startup:

Startup (a handful of lines):

```
rtd-simple v0.1.0-local starting: grpc=:50051 http=:8080 model_path=/models log_every=0
model present: "rtd-simple reference model 2026.09.21.0" files=1 [reference-model.csv] (at /models)
rtd-simple ready (no-op: all responses return zero mutations)
```

If no model is mounted, that line instead reads
`model check at /models: no model found (optional; container runs as a no-op)`.

**By default there is no per-request log**, only errors (an RPC that returns a
non-OK status). This keeps log volume low under load, which the Index validation
requires ("prefer metrics over verbose logging"; request volume and latency live
in `rpc_server_duration`, not the log).

Set `RTD_LOG_SAMPLE_N=N` to opt into verbose per-request logging (1 of every N),
as an onboarding visibility aid. Those lines look like:

```
rtd-simple no-op: lifecycle=PUBLISHER_BID_REQUEST req_id=pub bid_request=brq-pub supply=site:example.com imps=1 eids=0 tmax_ms=5 has_bid_response=false mutations=0 dur_ms=0.003
rtd-simple no-op: lifecycle=DSP_BID_RESPONSE   req_id=dsp bid_request=brq-dsp supply=none            imps=0 eids=0 tmax_ms=5 has_bid_response=true  mutations=0 dur_ms=0.002
```

Each reports the lifecycle, request and bid-request ids, supply source, impression
count, EID count, `tmax`, whether a DSP bid response was attached, the (always
zero) mutation count, and the handler duration. Emission is asynchronous and
non-blocking off the request path, and goes to STDERR.

## Metrics

Exposed on `/metrics` (Prometheus format):

| Metric | Type | Meaning |
|--------|------|---------|
| `rpc_server_duration` | histogram | Required Index metric: handler duration in seconds (buckets `0.001`…`0.010`). Use it for latency percentiles. |
| `rtd_simple_response_seconds_avg` | gauge | Convenience average handler duration, for a single-number response-time panel. |
| `rtd_simple_cpu_seconds_total` | counter | Cumulative container CPU seconds (from cgroup). `rate()` gives cores used. |
| `rtd_simple_cpu_limit_cores` | gauge | CPU allocation in cores (denominator for CPU %). |
| `rtd_simple_memory_bytes` | gauge | Current container memory in bytes (from cgroup). |
| `rtd_simple_memory_limit_bytes` | gauge | Memory allocation in bytes (denominator for memory %). |
| `rtd_simple_memory_percent` | gauge | Memory usage as a percent of the limit (0-100). |
| `rtd_simple_model_present` | gauge | 1 if a valid model was found at `MODEL_PATH`, else 0. |
| `rtd_simple_ready` | gauge | 1 when ready to serve. |
| `rtd_simple_build_info` | gauge | `app_version` / `model_version` labels; value 1. |

CPU and memory are read fresh at scrape time from the kernel cgroup files (no
background goroutine; scratch and read-only-fs safe), so they need no dependency.
The limits (`_cpu_limit_cores`, `_memory_limit_bytes`) are the container's own
allocation, so the percentages are "percent of allocation." Note that Index's
platform also collects pod CPU/memory via cAdvisor; these self-reported series are
for visibility in your own `/metrics` alongside the app signals.

### Baseline queries (PromQL)

```promql
# CPU % of allocation (0-100)
100 * rate(rtd_simple_cpu_seconds_total[5m]) / rtd_simple_cpu_limit_cores

# Memory % of allocation (0-100)
rtd_simple_memory_percent

# QPS (queries per second)
sum(rate(rpc_server_duration_count{rpc_method="GetMutations"}[1m]))

# Response time per query: recent average (seconds)
rate(rpc_server_duration_sum[5m]) / rate(rpc_server_duration_count[5m])
# Response time per query: p95 (seconds)
histogram_quantile(0.95, sum by (le) (rate(rpc_server_duration_bucket[5m])))
```

Aggregate across instances with `sum(...) by (...)` as needed; for a fleet CPU %
use `100 * sum(rate(rtd_simple_cpu_seconds_total[5m])) / sum(rtd_simple_cpu_limit_cores)`.

## Generated code (protobuf bindings)

`proto/` holds the IAB Tech Lab ARTF and OpenRTB `.proto` files, unmodified. The Go
bindings in `pkg/pb/` are **not committed**; they are generated from those files with a
pinned, checksum-verified `protoc` (33.4), `protoc-gen-go` (v1.36.11) and
`protoc-gen-go-grpc` (v1.6.2), by `scripts/gen-proto.sh`:

- `make build` (and `make up`) generate them inside the Docker build.
- `make unit`, `make harness` and `make verify` generate them first if they are
  missing or the `.proto` files changed.
- To run `go build` / `go test` on your own machine, run `make proto` once. It writes
  `pkg/pb/` (git-ignored). Docker is the only requirement.

## Run it

```bash
make up        # build and start (ports 50051 gRPC, 8080 HTTP)
make logs      # tail logs (startup + errors; set RTD_LOG_SAMPLE_N for per-request lines)
make health    # curl /heartbeat and /healthcheck
make test      # unit tests + conformance harness
make down      # stop
```

The harness (`make harness`, or `make test`) checks the onboarding-relevant
contract against the running container: id echo, zero mutations on both
lifecycles and on content-rich requests, gRPC health SERVING, the required
`rpc_server_duration` metric with correct buckets and tags, and handler p95
within the 5 ms SLA. It exits non-zero on any failure.

## Config

| Variable | Meaning | Default |
|----------|---------|---------|
| `GRPC_PORT` | gRPC RTBExtensionPoint + health port | `50051` |
| `SERVER_PORT` | HTTP metrics/health port | `8080` |
| `MODEL_PATH` | directory checked for a delivered model at startup | `/models` |
| `APP_VERSION` | version reported in response metadata | `0.1.0` |
| `RTD_LOG_SAMPLE_N` | `0` = errors only; `N` = log 1 of every N requests | `0` |

## Model

The container ships a **reference model** under `models/` so the presence check
has something to find. It follows the Index Cloud structure from *Updating models
in containers without rebuilding*: a `manifest.json` (a `{"name","models":[...]}`
list) plus a `model_data/` directory holding the listed files.

`models/` is what the container sees at `MODEL_PATH`, so the manifest is at the
**top level** (not in a version subfolder):

```
models/                     # = MODEL_PATH at runtime
  manifest.json
  model_data/
    reference-model.csv
```

**Versioning is the S3 folder name, not a path inside the container.** When you
publish a model update, you upload a CalVer-named folder to S3
(`2026.09.21.0/manifest.json` + `model_data/…`); Index mounts that folder's
**contents** at `MODEL_PATH`, so the version does not appear as a directory in the
container. Bump the folder version for each update (see the artifact naming
convention in the KB).

At startup the container parses the manifest, verifies every listed file exists,
and reports the result (it does not read the data, it is a no-op). The check
exists so onboarding can confirm the model-delivery step. Models are optional: with
an empty mount the container still runs. For robustness the check also accepts a
parent directory of versioned folders (highest version wins), but the documented
runtime layout is the top-level manifest above.

## Performance tuning

At startup the container matches the Go runtime to its cgroup limits
(`internal/runtimetune`, logged as `runtime tuning: ...`):

- `GOMAXPROCS` to the CPU limit (rounded up, minimum 1), so the runtime does not
  oversubscribe a fractional CPU allocation and trigger CFS throttling.
- `GOMEMLIMIT` to 90% of the memory limit, as a soft ceiling.
- `GOGC` raised to 600 (from the default 100) to cut GC frequency.

Explicit `GOMAXPROCS`, `GOGC`, or `GOMEMLIMIT` environment variables override these.

Rationale: profiling a no-op showed per-request CPU dominated by allocation-driven
GC. Each request allocates ~200 objects (protobuf unmarshal plus gRPC per-RPC
stream setup; the handler itself allocates almost nothing), and at `GOGC=100` that
drove GC to ~18% of CPU. GC is a memory-for-CPU trade: these containers run a tiny
live heap against a large memory allocation, so raising `GOGC` spends the spare
memory to reclaim CPU. `GOGC=600` cuts GC CPU to ~3% (from ~18%) with the heap
still only tens of MB, bounded by `GOMEMLIMIT`; the ~15 CPU points freed become
roughly 15 to 18% more QPS per core at the same allocation. The allocations
themselves are framework-inherent and are not reduced here. These are runtime
settings only, with no change to request handling.

## Performance dashboard (Grafana)

Grafana dashboards for this container live in the repository's
[`monitoring/rtd-simple/`](../../monitoring/rtd-simple/) folder, including a
peak-oriented performance dashboard (worst pod, worst-case p99, a "safe fleet QPS"
capacity estimate) so you can size QPS to the lowest common denominator rather than
the average. See [`monitoring/rtd-simple/README.md`](../../monitoring/rtd-simple/README.md)
for what each panel shows and step-by-step Grafana import instructions.

## Constraints met

x86/amd64, non-root 1000:1000, read-only root filesystem, **no egress** (the
container makes no outbound calls at runtime), `FROM scratch`, static binary,
image about 5 MB.

## Vulnerability scan

Index blocks deployment if any component scores **9 or higher on CVSS**
(*Checklist: what must pass before a container can auto-deploy*). Scan the image
yourself first:

```bash
trivy image rtd-simple:local
```

Current result: **0 vulnerabilities** (no OS layer on `scratch`; the static Go
binary is built with Go 1.26.10 and patched grpc/protobuf, so `govulncheck` also
reports none). Choosing `scratch` keeps the vulnerability surface minimal, per the
guidance to pick a minimal base image.

## Continuous integration

`make verify` runs the full pre-publish check suite against a locally built image and
**never publishes**: it builds the image, validates that the image matches
`manifest/manifest.json` (labels, declared endpoints, and the required
`rpc_server_duration` metric contract), runs a `trivy` CVSS-9 vulnerability scan, and
exercises both auction lifecycles against the no-op contract. It needs Docker, Go,
and Python 3; `grpcurl` is optional (the validator falls back to a dockerized grpcurl
when it is not installed — see Prerequisites).

A ready-to-run GitHub Actions workflow ships at
[.github/workflows/ci.yml](.github/workflows/ci.yml) and calls `make verify` on every
pull request, so a copy of this sample keeps itself honest against the contract as you
replace the no-op with real logic. Because `make verify` is a plain Make target, the
same checks run on any CI system or on your laptop — just run `make verify`.

Publishing is deliberately kept out of CI: pushing a compliant CalVer tag auto-deploys
to Index Cloud production (see below), so the push lives only in the manual, gated
`make release` path. Do not add a push/deploy step or registry credentials to CI.

## Publishing to Docker Hub for Index Cloud

Index pulls your container from Docker Hub and auto-deploys it. A **public** repo
is the default here: this container is non-confidential, so Index pulls it
anonymously (no read grant needed) and `docker push` auto-creates the repo. Keep
containers that carry proprietary models or logic in a private repo instead, and
grant read access to `nexus@indexexchange.onmicrosoft.com`.

`make release` runs the four auto-deploy gates (naming, manifest-vs-image
validation, a clean CVSS-9 vulnerability scan, and the required endpoints) and
then pushes:

```bash
make release REPO=your-namespace/rtd-simple
```

`TAG` defaults to today's global CalVer artifact tag (`YYYY.0M.0D.MICRO`); pass
`TAG=YYYY.0M.0D.N` to override, and append `_AMS`, `_EMEA`, or `_APAC` to deploy
to a single region instead of everywhere.

**Pushing a compliant tag triggers an automatic production deployment**, so
`make release` asks you to type `deploy` before it pushes. Only run it when you
intend to deploy.

## License

This is **sample code**, published as a reference for building Index Cloud RTD
containers. It is provided **as is**, with no warranty and no liability, and it is
not a supported product.

Licensed under **Apache-2.0**: you may freely use, copy, modify,
and redistribute it, including in your own containers, subject to the Apache License, Version 2.0. See
[LICENSE](LICENSE). Every source file carries an `SPDX-License-Identifier: Apache-2.0`
header, and the image is labelled `org.opencontainers.image.licenses=Apache-2.0`.

Use of the sample with Index services is also subject to the
[Index Exchange Sample Code Terms](../../SAMPLE-CODE-TERMS.md). The IAB Tech Lab protocol
definitions in `proto/` are third-party material under their own licences, not this
sample's Apache License; see [NOTICE](NOTICE) and the [root NOTICE](../../NOTICE).

### Build upon it: the source ships in the image

The buildable Go source is embedded in the image at **`/usr/src/rtd-simple`**, so
you can take this sample and extend it without cloning anything. It is read-only
image content and is not used at runtime (the entrypoint is the compiled binary).
Extract it with:

```bash
cid=$(docker create rtd-simple:<tag>)
docker cp "$cid":/usr/src/rtd-simple ./rtd-simple-src
docker rm "$cid"
cd rtd-simple-src && go build ./cmd/server   # or: make build
```

Internal-only material (deploy notes, ops dashboards) is deliberately excluded from
the image via `.dockerignore`; only the buildable project is shipped.
