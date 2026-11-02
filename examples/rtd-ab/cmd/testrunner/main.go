// SPDX-License-Identifier: Apache-2.0

// Command testrunner is the local conformance harness for rtd-ab. It checks the
// onboarding contract from "Validating and deploying your container" and the
// rtd-ab experiment:
//
//   - GetMutations echoes RTBResponse.id and returns zero mutations in both arms
//     over at least 10,000 requests
//   - the REQUIRED rpc_server_duration metric keeps its seconds buckets and tags
//     (and carries no arm label)
//   - inherited rtd_simple_* metric names match the golden file
//   - test = the auctions whose last hex digit is below the aligned group count,
//     exactly (-expect-pct); unroutable ids and other lifecycles are counted in
//     rtd_ab_unrouted_total and not analyzed
//   - the rtd_ab_* metrics move with traffic, for both strategies
//   - series count stays at or below 300; idle payload at or below 6 KB and
//     loaded payload at or below 25 KB
//   - a planted sentinel token never appears in /metrics
//
// Exits non-zero on any failure, so it doubles as a CI gate.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	openrtb "github.com/indexexchange/rtd-ab/pkg/pb/openrtb"
	pb "github.com/indexexchange/rtd-ab/pkg/pb/artf"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

const (
	lifecyclePublisher  pb.Lifecycle = 1
	lifecycleDSPBidResp pb.Lifecycle = 2 // a lifecycle this container does not analyze
)

func s(v string) *string     { return &v }
func i32(v int32) *int32     { return &v }
func f64(v float64) *float64 { return &v }

// required rpc_server_duration boundaries, in seconds (Validating doc).
var requiredBuckets = []string{"0.001", "0.002", "0.003", "0.004", "0.005", "0.007", "0.010"}
var requiredTags = []string{"rpc_system", "rpc_grpc_status_code", "rpc_method", "rpc_service", "error_type"}

// buckets at or under the 5ms SLA (0.005s).
var slaBuckets = map[string]bool{"0.001": true, "0.002": true, "0.003": true, "0.004": true, "0.005": true}

// Families the rtd-ab experiment must declare (HELP and TYPE) even when idle.
var declaredFamilies = []string{
	"rtd_ab_test_pct", "rtd_ab_build_info", "rtd_ab_models_info", "rtd_ab_requests_total", "rtd_ab_unrouted_total", "rtd_ab_analyze_seconds", "rtd_ab_errors_total",
	"rtd_ab_content_enrichable_total",
	"rtd_ab_content_signals", "rtd_ab_content_signals_sumsq", "rtd_ab_content_signal_total",
}

const (
	maxIdlePayload   = 6 * 1024
	maxLoadedPayload = 25 * 1024
	maxSeries        = 300
)

// expect holds the content sources each arm's model enables, which decide the
// per-arm results the deterministic requests must produce (defaults match the
// built-in models).
var expect struct{ ctlSources, tstSources string }

type report struct{ pass, fail int }

func (r *report) check(name string, ok bool, detail string) {
	if ok {
		r.pass++
		fmt.Printf("  PASS %s\n", name)
	} else {
		r.fail++
		fmt.Printf("  FAIL %s  -- %s\n", name, detail)
	}
}

func main() {
	addr := flag.String("addr", "localhost:50051", "gRPC address")
	metricsURL := flag.String("metrics", "http://localhost:8080/metrics", "HTTP metrics URL")
	loadN := flag.Int("load", 2000, "requests for the latency test")
	expectPct := flag.String("expect-pct", "", "requested test share in percent (e.g. 33.33, aligned to 1/16 by the container); enables the split and metric-movement scenarios")
	splitN := flag.Int("split-n", 16000, "requests per lifecycle for the split scenario (multiple of 16)")
	fresh := flag.Bool("fresh", false, "the container has served no traffic yet: enables the idle payload checks")
	golden := flag.String("golden", "", "path to the inherited metric names golden file (exact-set check)")
	noModel := flag.Bool("no-model", false, "the container has no delivered model manifest: skip the model-present check")
	expectSource := flag.String("expect-source", "", "expected rtd_ab_models_info source (file, builtin_absent, builtin_invalid)")
	expectModels := flag.String("expect-models", "", "expected control,test model names in rtd_ab_models_info (e.g. model_a,model_b)")
	flag.StringVar(&expect.ctlSources, "ctl-sources", "category", "content sources the control model enables")
	flag.StringVar(&expect.tstSources, "tst-sources", "category,genre,keyword,network,eid", "content sources the test model enables")
	sentinel := flag.String("sentinel", "RTDABSENTINEL7f3a91c2", "token planted in request strings; must never appear in /metrics")
	flag.Parse()

	conn, err := grpc.NewClient(*addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		fmt.Fprintln(os.Stderr, "dial:", err)
		os.Exit(2)
	}
	defer conn.Close()
	client := pb.NewRTBExtensionPointClient(conn)
	rep := &report{}
	fmt.Printf("rtd-ab conformance harness -> %s (expect-pct=%q fresh=%t)\n\n", *addr, *expectPct, *fresh)

	if *fresh {
		scenarioIdleMetrics(*metricsURL, *expectPct, *expectSource, *expectModels, rep)
	}
	scenarioPublisherNoOp(client, rep)
	scenarioOtherLifecycleNoOp(client, rep)
	scenarioRichRequestStillNoOp(client, rep)
	scenarioGRPCHealth(conn, rep)
	scenarioMetric(*metricsURL, rep)
	if !*noModel {
		scenarioModelPresent(*metricsURL, rep)
	}
	scenarioResourceMetrics(*metricsURL, rep)
	if *golden != "" {
		scenarioInheritedNames(*metricsURL, *golden, rep)
	}
	if *expectPct != "" {
		scenarioSplitAndMetrics(client, *metricsURL, *expectPct, *splitN, *sentinel, rep)
	}
	scenarioLatency(client, rep, *loadN, *metricsURL)
	scenarioBoundsAfterLoad(*metricsURL, *sentinel, rep)

	fmt.Printf("\nResult: %d passed, %d failed\n", rep.pass, rep.fail)
	if rep.fail > 0 {
		os.Exit(1)
	}
}

func pubReq(id, domain string, floor float64, eids bool) *pb.RTBRequest {
	lc := lifecyclePublisher
	u := &openrtb.BidRequest_User{}
	if eids {
		u.Eids = []*openrtb.BidRequest_EID{{Source: s("ex.com")}}
	}
	return &pb.RTBRequest{
		Lifecycle: &lc, Id: s(id), Tmax: i32(5),
		BidRequest: &openrtb.BidRequest{
			Id:                       s(auctionID(len(id), "ssp")),
			DistributionchannelOneof: &openrtb.BidRequest_Site_{Site: &openrtb.BidRequest_Site{Domain: s(domain)}},
			User:                     u,
			Imp:                      []*openrtb.BidRequest_Imp{{Id: s("imp-1"), Bidfloor: f64(floor)}},
		},
	}
}

func call(c pb.RTBExtensionPointClient, r *pb.RTBRequest) (*pb.RTBResponse, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return c.GetMutations(ctx, r)
}

func scenarioPublisherNoOp(c pb.RTBExtensionPointClient, rep *report) {
	fmt.Println("Scenario: Publisher Request returns no mutations")
	resp, err := call(c, pubReq("pub", "example.com", 1.00, false))
	if err != nil {
		rep.check("call succeeds", false, err.Error())
		return
	}
	rep.check("id echoed", resp.GetId() == "pub", resp.GetId())
	rep.check("zero mutations", len(resp.GetMutations()) == 0, fmt.Sprintf("got %d", len(resp.GetMutations())))
	rep.check("api_version set", resp.GetMetadata().GetApiVersion() != "", "empty api_version")
}

func scenarioOtherLifecycleNoOp(c pb.RTBExtensionPointClient, rep *report) {
	fmt.Println("Scenario: a lifecycle this container does not analyze still returns no mutations")
	lc := lifecycleDSPBidResp
	r := pubReq("other", "example.com", 1.00, false)
	r.Lifecycle = &lc
	resp, err := call(c, r)
	if err != nil {
		rep.check("call succeeds", false, err.Error())
		return
	}
	rep.check("id echoed", resp.GetId() == "other", resp.GetId())
	rep.check("zero mutations", len(resp.GetMutations()) == 0, fmt.Sprintf("got %d", len(resp.GetMutations())))
}

func scenarioRichRequestStillNoOp(c pb.RTBExtensionPointClient, rep *report) {
	fmt.Println("Scenario: content-rich request is still a no-op (EIDs, floor, premium domain)")
	resp, err := call(c, pubReq("rich", "premium-news.example", 2.50, true))
	if err != nil {
		rep.check("call succeeds", false, err.Error())
		return
	}
	rep.check("zero mutations despite rich content", len(resp.GetMutations()) == 0, fmt.Sprintf("got %d", len(resp.GetMutations())))
}

func scenarioGRPCHealth(conn *grpc.ClientConn, rep *report) {
	fmt.Println("Scenario: gRPC healthcheck protocol")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	resp, err := healthpb.NewHealthClient(conn).Check(ctx, &healthpb.HealthCheckRequest{})
	if err != nil {
		rep.check("Health/Check succeeds", false, err.Error())
		return
	}
	rep.check("Health/Check = SERVING", resp.GetStatus() == healthpb.HealthCheckResponse_SERVING, resp.GetStatus().String())
}

func scenarioMetric(url string, rep *report) {
	fmt.Println("Scenario: required rpc_server_duration metric")
	body := fetch(url)
	if body == "" {
		rep.check("/metrics reachable", false, "empty/unreachable")
		return
	}
	rep.check("rpc_server_duration present", strings.Contains(body, "rpc_server_duration_bucket"), "metric missing")
	missingB := []string{}
	for _, b := range requiredBuckets {
		if !strings.Contains(body, `le="`+b+`"`) {
			missingB = append(missingB, b)
		}
	}
	rep.check("all required buckets present", len(missingB) == 0, "missing buckets: "+strings.Join(missingB, ","))
	missingT := []string{}
	for _, tag := range requiredTags {
		if !strings.Contains(body, tag+"=") {
			missingT = append(missingT, tag)
		}
	}
	rep.check("all required tags present", len(missingT) == 0, "missing tags: "+strings.Join(missingT, ","))
}

func scenarioResourceMetrics(url string, rep *report) {
	fmt.Println("Scenario: CPU / memory / response-time metrics exposed")
	body := fetch(url)
	if body == "" {
		rep.check("/metrics reachable", false, "empty/unreachable")
		return
	}
	rep.check("rtd_simple_response_seconds_avg present", strings.Contains(body, "rtd_simple_response_seconds_avg "), "latency metric missing")
	rep.check("rtd_simple_cpu_seconds_total present", strings.Contains(body, "rtd_simple_cpu_seconds_total "), "cpu metric missing (no cgroup source?)")
	rep.check("rtd_simple_cpu_limit_cores present", strings.Contains(body, "rtd_simple_cpu_limit_cores "), "cpu limit missing")
	rep.check("rtd_simple_memory_bytes present", strings.Contains(body, "rtd_simple_memory_bytes "), "memory metric missing (no cgroup source?)")
	rep.check("rtd_simple_memory_limit_bytes present", strings.Contains(body, "rtd_simple_memory_limit_bytes "), "memory limit missing")
	rep.check("rtd_simple_memory_percent present", strings.Contains(body, "rtd_simple_memory_percent "), "memory percent missing")
}

func scenarioModelPresent(url string, rep *report) {
	fmt.Println("Scenario: reference model detected at MODEL_PATH")
	body := fetch(url)
	if body == "" {
		rep.check("/metrics reachable", false, "empty/unreachable")
		return
	}
	rep.check("rtd_simple_model_present = 1", strings.Contains(body, "rtd_simple_model_present 1"),
		"model not detected (is ./models mounted with a versioned folder?)")
}

func scenarioLatency(c pb.RTBExtensionPointClient, rep *report, n int, metricsURL string) {
	fmt.Printf("Scenario: handler latency under load (%d requests, p95 < 5ms)\n", n)
	before := scrapeHist(metricsURL)
	lat := make([]float64, 0, n)
	req := pubReq("load", "example.com", 1.00, false)
	for i := 0; i < n; i++ {
		start := time.Now()
		if _, err := call(c, req); err != nil {
			rep.check("load call succeeds", false, err.Error())
			return
		}
		lat = append(lat, float64(time.Since(start).Microseconds())/1000.0)
	}
	sort.Float64s(lat)
	fmt.Printf("  client round-trip ms: p50=%.3f p95=%.3f (transport-inclusive, informational)\n", pct(lat, 50), pct(lat, 95))
	after := scrapeHist(metricsURL)
	if before == nil || after == nil {
		fmt.Println("  (server histogram unavailable; handler-latency check skipped)")
		return
	}
	bucket, ok := p95Bucket(before, after)
	if !ok {
		fmt.Println("  (no observations recorded; skipped)")
		return
	}
	fmt.Printf("  server handler p95 <= %ss bucket\n", bucket)
	rep.check("handler p95 within 5ms SLA", slaBuckets[bucket], "p95 bucket="+bucket)
}

// --- metrics helpers ---

func fetch(url string) string {
	if url == "" {
		return ""
	}
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	var sb strings.Builder
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	for sc.Scan() {
		sb.WriteString(sc.Text())
		sb.WriteByte('\n')
	}
	return sb.String()
}

func scrapeHist(url string) map[string]float64 {
	body := fetch(url)
	if body == "" {
		return nil
	}
	out := map[string]float64{}
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "rpc_server_duration_bucket{") {
			continue
		}
		i := strings.Index(line, `le="`)
		if i < 0 {
			continue
		}
		le := line[i+4:]
		le = le[:strings.Index(le, `"`)]
		fields := strings.Fields(line)
		var v float64
		fmt.Sscanf(fields[len(fields)-1], "%f", &v)
		out[le] += v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func p95Bucket(before, after map[string]float64) (string, bool) {
	total := after["+Inf"] - before["+Inf"]
	if total <= 0 {
		return "", false
	}
	order := []string{"0.001", "0.002", "0.003", "0.004", "0.005", "0.007", "0.010"}
	target := 0.95 * total
	for _, le := range order {
		if after[le]-before[le] >= target {
			return le, true
		}
	}
	return "+Inf", true
}

func pct(sorted []float64, p int) float64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := p * len(sorted) / 100
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}
