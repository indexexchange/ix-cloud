# Examples

Complete, buildable Index Cloud container samples. Each one lives in its own folder
and is self-contained — copy it out, rename it, and build on it.

| Example | What it is |
|---------|------------|
| [`rtd-simple/`](rtd-simple/) | A **no-op** ARTF `RTBExtensionPoint` container. It satisfies the full container contract (required `rpc_server_duration` metric in seconds buckets, gRPC health, HTTP `/metrics` + `/heartbeat` + `/healthcheck`, the agent-manifest image label, CalVer release tagging, a model-presence check, self-reported CPU/memory/latency metrics, Go runtime tuning) but returns **zero mutations** on every request. It is the recommended first deploy and the scaffold to start from. |
| [`rtd-ab/`](rtd-ab/) | An **A/B testing** example. It splits auctions between a control and a test arm by the last hex digit of the auction id, reads the mix and each arm's settings from the delivered model folder, and records how each arm's content-signal enrichment would have performed (`rtd_ab_*` metrics, a Grafana dashboard, and an evaluation guide). Like rtd-simple, both arms return **zero mutations**, so it is safe to run alongside live traffic. |

## Using an example

Each example's `README.md` covers building, running, the metrics/endpoints, and
publishing. In short:

```bash
cd rtd-simple
make build IMAGE=my-rtd:dev        # builds the linux/amd64 image, runs unit tests
docker run --rm -p 50051:50051 -p 8080:8080 my-rtd:dev
curl -s localhost:8080/metrics | head
```

To build your own container, copy the example folder, rename the Go module and the
metric prefix, and replace the no-op logic with your decisioning. Then validate it
with [`../tools/validate-manifest/`](../tools/validate-manifest/) before you publish.

## Contract reminders (the easy things to get wrong)

- The required metric is `rpc_server_duration`, a **histogram in seconds** with
  buckets `0.001 … 0.010` and a fixed tag set. Wrong units or extra tags fail Index
  validation.
- Images must be **linux/amd64**, `FROM scratch`, non-root `1000:1000`, read-only,
  make **no outbound calls** at runtime.
- The release tag **is** the version and must be CalVer `YYYY.0M.0D.MICRO`;
  **pushing a compliant tag auto-deploys to production.**

Each example encodes these so you inherit them for free.
