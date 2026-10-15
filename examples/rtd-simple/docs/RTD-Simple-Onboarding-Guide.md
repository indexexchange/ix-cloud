# Using rtd-simple to Validate Container Onboarding

rtd-simple is a deliberately inert Index Cloud RTD container: it satisfies the full
ARTF `RTBExtensionPoint` contract but returns zero mutations. Because it decides
nothing, any problem you hit while onboarding it is an **onboarding problem**
(packaging, deploy, health, metrics, traffic wiring), not a decisioning problem.
That makes it a clean canary for the onboarding process itself.

This guide is standalone. It assumes only Docker on the host; the Go and build
toolchain run inside containers.

## What "no-op" buys you

- No model to deliver, so the model-delivery step is optional and cannot fail the
  container.
- No mutations, so it is safe to point at live traffic during a staged rollout.
- One log line per request, so you can confirm traffic is actually reaching the
  container and see the exact fields Index sends in.

## Map to the onboarding steps

Each step below cites the standard Index Cloud documentation for the real
procedure; rtd-simple is what you run through it.

**1. Build the image.** `make build` (or `docker build --platform=linux/amd64 -t
rtd-simple:local .`). The build runs the unit tests and produces an amd64,
non-root, read-only, `FROM scratch` image of about 5 MB. This exercises the
packaging requirements from [Building a Docker container](https://kb.indexexchange.com/index_cloud_partners/real_time_data_integration/building_your_container.htm) without any model or egress
in the way.

**2. Run it locally against the runtime contract.** `make up` starts the
container with the same constraints Index applies: non-root 1000:1000, read-only
root filesystem, ports and paths from the environment. Watch the startup log:

```
rtd-simple v0.1.0-local starting: grpc=:50051 http=:8080 model_path=/models log_every=1
model_path /models: 1 entries: .gitkeep
rtd-simple ready (no-op: all responses return zero mutations)
```

The `model_path` line lists whatever is mounted at `MODEL_PATH`. Drop a file into
`./models` and restart to confirm the mount wiring; rtd-simple reads nothing from it,
it only reports what it sees.

**3. Confirm health and metrics.** `make health` shows `/heartbeat` and
`/healthcheck` returning `SERVING`. `make metrics` shows the required
`rpc_server_duration` histogram. `make grpc-health` (needs `grpcurl`) checks the
gRPC health protocol. These are the signals Index Cloud polls before it routes
traffic, per [Building a Docker container](https://kb.indexexchange.com/index_cloud_partners/real_time_data_integration/building_your_container.htm).

**4. Send conformance traffic.** `make harness` runs a gRPC client that asserts
the contract: id echo, zero mutations on both the Publisher Request and DSP Bid
Response lifecycles, zero mutations even on content-rich requests, gRPC health
SERVING, the required metric with correct buckets and tags, and handler p95
within the 5 ms SLA. It exits non-zero on any failure, so it doubles as a CI
gate. Expected result:

```
Result: 11 passed, 0 failed
```

**5. Watch the traffic land.** `make logs` streams one line per request:

```
rtd-simple no-op: lifecycle=PUBLISHER_BID_REQUEST req_id=... supply=site:example.com imps=1 eids=0 tmax_ms=5 has_bid_response=false mutations=0 dur_ms=0.003
```

During a staged production rollout this line is your proof that Index is actually
calling the container, and it shows the exact request shape you will decision on
later. Lines go to STDERR (Index collects STDOUT/STDERR), emission is asynchronous
and off the request path, and it is sampled: if you point rtd-simple at high-volume
traffic, raise `RTD_LOG_SAMPLE_N` so it logs 1 of every N requests. Request volume
and latency come from the metrics endpoint, not the log, per the guidance to
prefer metrics over verbose logging.

**6. Clear the auto-deploy checklist.** *Checklist: what must pass before a
container can auto-deploy* defines three gates. rtd-simple is built to pass all three:

- **Artifact naming.** Tag the image with a CalVer version plus an optional
  region suffix (`_AMS`, `_EMEA`, `_APAC`). A non-compliant tag produces no
  deployment event at all (the upload is silently ignored), so get this right
  first.
- **Vulnerability scan.** Index blocks deployment if any component scores 9 or
  higher on CVSS. Catch it yourself first:
  ```bash
  trivy image yourrepo/rtd-simple:2026.07.10.2
  ```
  Confirm nothing comes back at CVSS 9 or higher. rtd-simple on `scratch` with a
  patched static binary scans clean (0 findings). A failed Index scan writes a
  Grafana log entry naming the component to update.
- **Required endpoints.** The gRPC health protocol on `GRPC_PORT` and the
  Prometheus `/metrics` endpoint on `SERVER_PORT` must both be exposed. The
  testing tool sends no traffic until both pass. `make harness` checks these
  locally first.

**7. Deploy through the Index Cloud onboarding flow.** Push the image and request
onboarding per *[Deploying your services in Index Cloud](https://kb.indexexchange.com/index_cloud_partners/real_time_data_integration/deploying_your_services_to_index_cloud.htm). Because rtd-simple is inert,
a green run confirms the pipeline (registry, deploy, health gating, traffic
routing) end to end. A red run points at the pipeline, not the code.

## Interpreting a failure

- Container will not start or is unhealthy: check the packaging constraints
  (architecture, non-root, read-only fs) and the port environment variables.
- No traffic in the logs: the routing or health gating is not letting traffic
  through; recheck the health and metrics endpoints.
- Metric validation fails: confirm `/metrics` exposes `rpc_server_duration` with
  the exact buckets and the five required tags, and that nothing added an extra
  tag to that metric.

## What rtd-simple is not

It does not model any real policy, so it tells you nothing about enrichment
behavior, latency under a real model, or mutation correctness. Use one of the
decisioning containers for that. rtd-simple answers one question only: does the
onboarding process carry a well-formed container all the way to serving traffic.
