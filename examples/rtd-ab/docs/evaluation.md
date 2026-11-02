# Evaluating the rtd-ab test arm against control

rtd-ab routes each auction to a control arm or a test arm by the last hex digit of the auction id, and records how many content signals each arm could derive from the request. No arm returns a mutation, so no arm changes an auction. This document describes how to read the results.

**These are proxy measures.** They show decision coverage (how many content signals an enrichment could derive) and cost (CPU time per request). They do not show auction win rate or revenue, because nothing was applied to an auction.

## 1. What each arm does

| Arm | Built-in model | Signals counted per request |
|-----|----------------|-----------------------------|
| Control | `category` | IAB category ids in `cat`, `sectioncat`, `pagecat` (site or app) and `content.cat` (at most 3) |
| Test | `category`, `genre`, `keyword`, `network`, `eid` | The control signals plus `content.genre` (1), `content.keywords` and `content.kwarray` tokens (at most 3), `content.network` or `content.channel` hits (1), and `user.eids` presence (1); at most 9 |

In deployment each arm runs the model that `ab-config.json` names in the model folder. `content.json` sets the selected sources and the genre, keyword and network allowlists. `rtd_ab_models_info{arm,model,source}` reports which models are running. Before reading a result, confirm the fleet reports `source="file"` and the intended model names; an arm's meaning is whatever its model says.

For each request the container records whether at least one signal was derivable (the request is "enrichable"), the number of signals (0 to 10), and the count per source (`category`, `genre`, `keyword`, `network`, `eid`; `none` counts requests with no signal). Only `PUBLISHER_BID_REQUEST` is analyzed; any other lifecycle is counted in `rtd_ab_unrouted_total{reason="other_lifecycle"}`.

## 2. How requests are split

Index sends `bid_request.id` as `<auction_id>_<suffix>`, where `auction_id` is a UUID. Client Audit Logs (CAL) carry the same `auction_id`. The container takes the text before the first underscore (the whole id if there is none) and reads its last character as a hex digit `0` to `f`: 16 groups of 1/16 (6.25%) each. The first *g* digits are the test group and the rest are control. With *g* = 4 (25%), auctions ending in `0`, `1`, `2` or `3` are test.

The test percent comes from `ab-config.json` in the model folder (the control percent must make the pair sum to 100), or from the percentage baked into the image when the folder has no valid config. It is rounded to the nearest 1/16, halves up (10% becomes 2 groups, 12.5%; 33.33% becomes 5 groups, 31.25%). `rtd_ab_test_pct` and the `rtd.ab.test_pct` image label carry the aligned value.

Consequences:

- **The assignment is a function of the auction id.** The same auction reaches the same arm on every pod and after any restart. No state is shared.
- **CAL join.** The group of any logged auction is the last hex digit of its `auction_id`, so test and control auctions can be separated in CALs with no join to this container's data.
- **The share is statistical.** A random UUID's last digit is uniform, so the observed share over *N* auctions varies like a binomial with standard error `sqrt(p(1-p)/N)`. At 10,000 QPS one minute is 600,000 requests and puts the standard error at about 0.04 percentage points around 12.5%.
- **Not user-sticky.** One user's different auctions land in both arms. This is acceptable because both arms are observation-only: each measure is computed from one request's own fields and nothing the test arm does can change what a later request sees, so there is no carryover. If an arm ever applies mutations, the experiment needs a stickiness unit chosen for that effect (user or device), not the auction.
- **Unroutable requests are excluded.** A request whose `bid_request` is missing or has an empty id (`no_id`), or whose auction id does not end in a hex digit (`not_hex`), is counted in `rtd_ab_unrouted_total{reason}` and not analyzed in either arm. The expected rate is 0. A sustained non-zero rate means Index changed the id format; stop and re-check the assumption before reading any result.
- **An auction is one request here.** Each auction sends one `PUBLISHER_BID_REQUEST`, so requests are independent units and the standard formulas below apply. (Across auctions on the same page or publisher there is correlation; see section 6.)
- **Changing the mix needs a new model version, not a new image.** The file is read once at startup, so a new mix applies after the pods restart in a rollout. Counters from before and after describe different mixes; start a new comparison window at the rollout. The baked `AB_TEST_PCT` (default 12.5) is only the fallback.

### Expected volume

Volume depends on your traffic. For a reference rate of 10,000 QPS:

| Quantity | Value |
|----------|-------|
| QPS (example) | 10,000 |
| Test share | 12.5% (2 of 16 groups, the default) |
| Test-arm requests per second | about 1,250 |
| At 25% (4 groups) | about 2,500 per second |

The actual volume is `rate(rtd_ab_requests_total{arm="test"}[5m])`. At 1,250 requests per second the test arm collects 4.5 million requests per hour.

## 3. Procedure

Check first that `rtd_ab_models_info` shows one `source` and one model pair across the fleet (a mixed fleet means a partial rollout or an undelivered model folder). Pick the time range and the minimum effect worth acting on before looking at results. The dashboard refreshes every minute; reading it repeatedly and stopping at the first significant value inflates the false-positive rate well above 5%. Use a fixed window of at least 1 hour (24 hours covers the daily traffic cycle).

Run the checks in this order. If step 1 flags, stop: the arms are not comparable.

### Step 1: sample-ratio check

Compare the observed test share with the aligned share (`rtd_ab_test_pct`) from cumulative counters, and confirm `rtd_ab_unrouted_total` is 0.

- `p0 = rtd_ab_test_pct / 100` (a multiple of 6.25%)
- `N = test + control` requests
- `observed = test / N`
- `SE = sqrt(p0 * (1 - p0) / N)`
- **Flag when `|observed - p0| > 3 * SE`.**

```promql
# observed share (cumulative)
sum(rtd_ab_requests_total{arm="test"}) / sum(rtd_ab_requests_total)

# deviation in standard errors (flag above 3)
abs(
  (sum(rtd_ab_requests_total{arm="test"}) / sum(rtd_ab_requests_total)) - (scalar(max(rtd_ab_test_pct)) / 100)
) / sqrt(
  (scalar(max(rtd_ab_test_pct)) / 100) * (1 - scalar(max(rtd_ab_test_pct)) / 100) / sum(rtd_ab_requests_total)
)
```

Under correct operation the deviation behaves like the absolute value of a standard normal: above 3 in about 0.3% of checks. A flag points at skewed auction ids (a non-random last digit), a spike in unrouted requests, a scrape gap, a mixed fleet running two different splits (`rtd_ab_test_pct` differs by pod), or a routing bug.

### Step 2: two-proportion z-test (enrichable rate)

With `x1` enrichable requests in `n1` control requests and `x2` in `n2` test requests:

- `p1 = x1/n1`, `p2 = x2/n2`, `diff = p2 - p1`
- pooled `p = (x1 + x2) / (n1 + n2)`
- `z = diff / sqrt(p * (1 - p) * (1/n1 + 1/n2))`
- 95% confidence interval for `diff`: `diff ± 1.96 * sqrt(p1(1-p1)/n1 + p2(1-p2)/n2)`

Here `x` is `rtd_ab_content_enrichable_total` and `n` is `rtd_ab_requests_total`; a higher rate is better.

```promql
# enrichable rate per arm over 6 hours
sum by (arm) (increase(rtd_ab_content_enrichable_total[6h])) / sum by (arm) (increase(rtd_ab_requests_total[6h]))
```

The dashboard tile "z: enrichable rate" computes `z` directly. For a result with a confidence interval, run `abreport` (section 5).

### Step 3: Welch t-test (signals per request)

`rtd_ab_content_signals` exports `_sum`, `_count` and `_sumsq` per arm. From those, per arm:

- `mean = sum / count`
- `var = (sumsq - sum^2 / count) / (count - 1)`
- `t = (mean_test - mean_control) / sqrt(var_test/count_test + var_control/count_control)`
- degrees of freedom (Welch-Satterthwaite): `(a + b)^2 / (a^2/(count_control-1) + b^2/(count_test-1))`, with `a = var_control/count_control` and `b = var_test/count_test`
- 95% confidence interval for the difference of means: `diff ± 1.96 * sqrt(a + b)` (normal critical value; the degrees of freedom exceed 1,000 at these volumes)

```promql
# mean signals per request, per arm
sum by (arm) (increase(rtd_ab_content_signals_sum[6h])) / sum by (arm) (increase(rtd_ab_content_signals_count[6h]))

# variance: (sumsq - sum^2/count) / (count - 1), with S, Q, N the three increases
(Q - (S * S / N)) / (N - 1)
```

The dashboard tile "Welch t" expands this into one expression; the same expansion is `welch_t` in `scripts/gen-dashboard.py`.

### Step 4: decision rule

The test arm derives more content signals when the step 2 `z` and the step 3 `t` are both above 1.96, **and** the cost check (section 4) shows no more than the budgeted CPU increase. With two tests, a Bonferroni-adjusted threshold at an overall 5% level is `|z| or |t| > 2.24` (0.05 / 2 = 0.025 per test).

Statistical significance is not practical significance. At 3,750 test requests per second the test arm collects millions of requests per hour, so a negligible difference becomes statistically distinguishable. Decide the smallest difference worth acting on first. Sample size for a two-proportion test at 5% significance and 80% power: `n per arm = 7.85 * (p1(1-p1) + p2(1-p2)) / d^2`. For an enrichable rate of 50% and a difference of 1 percentage point, `n` is about 39,000 requests per arm (about 10 seconds at 3,750 per second). For 0.1 percentage points, `n` is about 3.9 million per arm (about 17 minutes).

## 4. Cost evaluation

`rtd_ab_analyze_seconds{arm}` times routing plus analysis. It is sampled at 1 request in 16 per arm (the HELP text says so). Budget: 1 microsecond average target, 3 microseconds fail.

```promql
# mean analysis time per arm
sum by (arm) (rate(rtd_ab_analyze_seconds_sum[5m])) / sum by (arm) (rate(rtd_ab_analyze_seconds_count[5m]))

# p99
histogram_quantile(0.99, sum by (le, arm) (rate(rtd_ab_analyze_seconds_bucket[5m])))
```

Judge the rollout on **CPU per request**, compared with the same time of day on the previous day. Compare QPS per CPU core used (`sum(rate(rpc_server_duration_count[5m])) / sum(rate(rtd_simple_cpu_seconds_total[5m]))`) now against 24 hours earlier. Raw QPS against the minutes before the rollout is not a valid comparison: traffic follows a daily cycle.

## 5. Reporting tool

`cmd/abreport` reads `/metrics` from URLs or files, sums several inputs (pass every pod for a fleet view), and prints per-arm rates, differences, 95% confidence intervals, the z or t statistic with a two-sided p-value (normal approximation) and the sample-ratio check. It exits 3 on a sample-ratio flag. It is not built into the image.

```bash
go run ./cmd/abreport http://pod-a:8080/metrics http://pod-b:8080/metrics

# exact window: scrape the same pods at the start and end, then subtract
curl -s http://pod-a:8080/metrics > start-a.txt   # at the start of the window
curl -s http://pod-a:8080/metrics > end-a.txt     # at the end
go run ./cmd/abreport -base start-a.txt end-a.txt
```

## 6. Limitations

- **Proxy only.** Signals derivable is not signals applied; whether an enrichment would change an auction outcome is not observed.
- **Not user-sticky** (section 2). One user can appear in both arms.
- **Different auctions in each arm.** The arms see different auctions, not the same auction under two treatments. Because the last UUID digit is independent of publisher and content, the traffic mix is the same in expectation, and at these volumes the difference is small. The comparison is between populations, not paired.
- **Correlation across auctions.** Auctions on the same page or from the same publisher share content fields, so requests are not fully independent and the standard errors above are somewhat optimistic. Treat `|z|` or `|t|` near 2 as unproven.
- **Allowlists are illustrative.** The built-in genre, keyword and network lists are 20 to 30 entries each, and categories are validated by range (IAB 1.0 `IAB1` to `IAB26-99`, numeric ids 1 to 1500). The enrichable rate depends on them. A model can replace the lists; both arms should use the same lists unless the lists are the thing under test.
- **Fixed-point precision.** Signal counts are integers, so `_sum` and `_sumsq` are exact. Sums of squares overflow `int64` only after more than 1e17 samples, which is never in practice.
- **Counters reset on restart.** Use `increase()` or `rate()`, never raw values, for anything except the sample-ratio check. The split itself does not reset: it depends only on the auction id.
- **`increase()` extrapolates** at the window edges, which adds an error of up to about one scrape interval per window edge (0.5% of a 1 hour window at a 15 second scrape interval). For a formal result, scrape the cumulative counters at the start and end of the window and run `abreport -base start.txt end.txt`, which subtracts exactly.
- **Replayed sample files are not random.** `rtd-test-tool-py` and the official tool cycle through a file of about 50 requests, so a functional run carries about 50 distinct auction ids repeated thousands of times. The observed share then reflects those few last digits, and the sample-ratio check can flag. Judge randomization only on live traffic or on a file with many distinct random UUIDs.
- **Synthetic validation only.** The functional runs use synthetic samples and one run of the official tool. The numbers in this repository do not describe live traffic.

## 7. Inherited metrics

Inherited metrics keep their `rtd_simple_*` names, types and units. `internal/metrics/golden_test.go` and the harness compare them with the set in `internal/metrics/testdata/inherited_names.golden`. `rpc_server_duration` is unchanged: seconds, buckets 0.001 to 0.010, five labels, no `arm` label.

Design notes on the `rtd_ab_*` metrics:

- `_sumsq` is a separate counter family, `rtd_ab_content_signals_sumsq`, with its own HELP and TYPE: the Prometheus text format gives a histogram family only `_bucket`, `_sum` and `_count`.
- `rtd_ab_requests_total` carries the `arm` label only and counts routed auctions (always present, 2 series), because one lifecycle is analyzed.
- `rtd_ab_unrouted_total{reason}` (`no_id`, `not_hex`, `other_lifecycle`) counts requests with no assignable arm, so they are visible and excluded from both arms.
- `rtd_ab_models_info{arm,model,source}` is always 1; `source` is `file`, `builtin_absent` or `builtin_invalid`, so a run shows which models it used.
- `rtd_ab_test_pct` is the effective percentage after alignment to the nearest 1/16.
- `rtd_ab_analyze_seconds` is sampled 1 in 16. Sampling costs about 3 ns per request on average. Set `analyzeSampleN` to 1 in `internal/ab/engine.go` to time every request.
- `rtd_ab_errors_total` is lazy: absent until the first recovered panic. Alert on `increase(rtd_ab_errors_total[5m]) > 0`, not on absence.
- `rtd_ab_content_signal_total{source="none"}` counts requests with zero signals (1 per such request); the other sources count signals.
