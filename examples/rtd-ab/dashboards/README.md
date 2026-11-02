# rtd-ab dashboards

- `rtd-ab-dashboard.json`: control versus test for the content-signal A/B example, plus the standard capacity and latency panels. Rows: split and sample ratio (test share, which models each arm runs, the sample-ratio check, observed versus configured share), content-signal coverage (enrichable rate, signals per request, z and Welch t tiles, per-source breakdown), cost and health (analysis time, requests per second, recovered panics, unrouted requests), and the performance panels. The z and t tiles use the increase over the dashboard time range; select at least 1 hour. Decision rules are in `../docs/evaluation.md`.

rtd-ab keeps every inherited metric under its `rtd_simple_*` name, so panels built for those names keep working.

## Importing into Grafana

1. In Grafana, go to **Dashboards -> New -> Import**.
2. Click **Upload dashboard JSON file** and choose `rtd-ab-dashboard.json`.
3. When prompted for the **Metrics (Prometheus)** data source, select the Prometheus data source that scrapes this container's `/metrics` endpoint. A query error such as `parse error ... expecting NUMBER or { or (` means a Loki (Logs) data source was chosen; switch to Prometheus.
4. Click **Import**, and save it into a folder you own. The file sets no fixed `uid`, so it cannot collide with a provisioned dashboard.
5. Set the time range to at least 1 hour. Compare performance panels with the same time of day on the previous day, not with the minutes before the rollout.
