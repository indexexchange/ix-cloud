// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"math"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	openrtb "github.com/indexexchange/rtd-ab/pkg/pb/openrtb"
	pb "github.com/indexexchange/rtd-ab/pkg/pb/artf"
)

// parseMetrics maps each series line ("name{labels}") to its value.
func parseMetrics(body string) map[string]float64 {
	out := map[string]float64{}
	for _, ln := range strings.Split(body, "\n") {
		if ln == "" || strings.HasPrefix(ln, "#") {
			continue
		}
		i := strings.LastIndex(ln, " ")
		if i < 0 {
			continue
		}
		if v, err := strconv.ParseFloat(strings.TrimSpace(ln[i+1:]), 64); err == nil {
			out[ln[:i]] = v
		}
	}
	return out
}

func countSeries(body string) int { return len(parseMetrics(body)) }

func familyNames(body string) map[string]string {
	out := map[string]string{}
	for _, m := range regexp.MustCompile(`(?m)^# TYPE (\S+) (\S+)$`).FindAllStringSubmatch(body, -1) {
		out[m[1]] = m[2]
	}
	return out
}

// groupsFor mirrors router.GroupsFor: the percentage aligned to the nearest 1/16,
// rounding half up, from the value in basis points (two decimals at most).
func groupsFor(pct string) (int, error) {
	f, err := strconv.ParseFloat(pct, 64)
	if err != nil {
		return 0, err
	}
	bp := int(math.Round(f * 100))
	return (bp*16 + 5000) / 10000, nil
}

// auctionID builds a bid_request.id as Index sends it: a UUID, an underscore and
// a suffix. The UUID ends in the hex digit n%16, which selects the group.
func auctionID(n int, suffix string) string {
	return fmt.Sprintf("00000000-0000-4000-8000-%012x_%s", n, suffix)
}

// scenarioIdleMetrics runs against a container that has served no traffic.
func scenarioIdleMetrics(url, expectPct, expectSource, expectModels string, rep *report) {
	fmt.Println("Scenario: idle /metrics (HELP/TYPE always present, lazy series, payload size)")
	body := fetch(url)
	if body == "" {
		rep.check("/metrics reachable", false, "empty/unreachable")
		return
	}
	var missing []string
	for _, n := range declaredFamilies {
		if !strings.Contains(body, "# HELP "+n+" ") || !strings.Contains(body, "# TYPE "+n+" ") {
			missing = append(missing, n)
		}
	}
	rep.check("HELP and TYPE for every rtd_ab_* family", len(missing) == 0, "missing: "+strings.Join(missing, ","))
	m := parseMetrics(body)
	_, hasPct := m["rtd_ab_test_pct"]
	rep.check("rtd_ab_test_pct present", hasPct, "missing")
	if expectPct != "" {
		g, _ := groupsFor(expectPct)
		want := float64(g) * 6.25
		rep.check("rtd_ab_test_pct equals the requested value aligned to 1/16", math.Abs(m["rtd_ab_test_pct"]-want) < 1e-9, fmt.Sprintf("got %v want %v", m["rtd_ab_test_pct"], want))
	}
	if expectSource != "" {
		names := strings.Split(expectModels, ",")
		if len(names) != 2 {
			rep.check("-expect-models is control,test", false, expectModels)
		} else {
			for i, arm := range []string{"control", "test"} {
				k := fmt.Sprintf(`rtd_ab_models_info{arm=%q,model=%q,source=%q}`, arm, names[i], expectSource)
				rep.check("models info: "+k, m[k] == 1, "series missing or not 1")
			}
		}
	}
	routing := 0
	for k := range m {
		if strings.HasPrefix(k, "rtd_ab_requests_total{") {
			routing++
		}
	}
	rep.check("routing counters present before traffic (2 series)", routing == 2, fmt.Sprintf("got %d", routing))
	lazy := 0
	for k := range m {
		if strings.HasPrefix(k, "rtd_ab_content_") || strings.HasPrefix(k, "rtd_ab_analyze_") || strings.HasPrefix(k, "rtd_ab_unrouted_") {
			lazy++
		}
	}
	rep.check("analysis series are lazy (none before traffic)", lazy == 0, fmt.Sprintf("%d series already present", lazy))
	rep.check(fmt.Sprintf("idle payload <= %d bytes", maxIdlePayload), len(body) <= maxIdlePayload, fmt.Sprintf("%d bytes", len(body)))
	fmt.Printf("  idle payload: %d bytes, %d series\n", len(body), len(m))
}

// scenarioInheritedNames requires the exposed non-rtd_ab families to equal the
// golden set exactly (name and type).
func scenarioInheritedNames(url, goldenPath string, rep *report) {
	fmt.Println("Scenario: inherited metric names intact (exact set vs golden)")
	raw, err := os.ReadFile(goldenPath)
	if err != nil {
		rep.check("golden file readable", false, err.Error())
		return
	}
	golden := map[string]string{}
	for _, l := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if f := strings.Fields(l); len(f) == 2 {
			golden[f[0]] = f[1]
		}
	}
	// rtd_simple_response_seconds_avg needs a request; the earlier scenarios sent some.
	got := map[string]string{}
	for n, t := range familyNames(fetch(url)) {
		if !strings.HasPrefix(n, "rtd_ab_") {
			got[n] = t
		}
	}
	var diff []string
	for n, t := range golden {
		if got[n] != t {
			diff = append(diff, fmt.Sprintf("%s: have %q want %q", n, got[n], t))
		}
	}
	for n := range got {
		if _, ok := golden[n]; !ok {
			diff = append(diff, "unexpected "+n)
		}
	}
	sort.Strings(diff)
	rep.check(fmt.Sprintf("%d inherited families match golden exactly", len(golden)), len(diff) == 0, strings.Join(diff, "; "))
}

// splitPubReq is a deterministic publisher request with a known outcome:
// cat IAB1, genre Drama, keywords "news,sports", network ESPN, one EID. The
// auction id ends in the hex digit i%16. Per request an arm yields 1 category,
// 1 genre, 2 keyword, 1 network and 1 eid signal for each source it enables.
func splitPubReq(token string, i int) *pb.RTBRequest {
	lc := lifecyclePublisher
	return &pb.RTBRequest{
		Lifecycle: &lc, Id: s(fmt.Sprintf("%s-pub-%d", token, i)), Tmax: i32(5),
		BidRequest: &openrtb.BidRequest{
			Id: s(auctionID(i, token)),
			DistributionchannelOneof: &openrtb.BidRequest_Site_{Site: &openrtb.BidRequest_Site{
				Domain: s(token + ".example"), Page: s("https://" + token + ".example/a?q=" + token), Cat: []string{"IAB1"},
				Content: &openrtb.BidRequest_Content{
					Genre: s("Drama"), Keywords: s("news,sports," + token), Title: s(token),
					Network: &openrtb.BidRequest_Network{Name: s("ESPN")},
				}}},
			User: &openrtb.BidRequest_User{Id: s(token), Eids: []*openrtb.BidRequest_EID{{Source: s("liveramp.com")}}},
			Imp:  []*openrtb.BidRequest_Imp{{Id: s("imp-" + token)}},
		},
	}
}

// sendAll sends n requests built by mk from 8 workers and returns the number of
// failed calls and the number of mutations received.
func sendAll(c pb.RTBExtensionPointClient, n int, mk func(i int) *pb.RTBRequest) (errs, mutations int64) {
	var next atomic.Int64
	var wg sync.WaitGroup
	var e, m atomic.Int64
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := int(next.Add(1)) - 1
				if i >= n {
					return
				}
				r := mk(i)
				resp, err := call(c, r)
				if err != nil || resp.GetId() != r.GetId() {
					e.Add(1)
					continue
				}
				m.Add(int64(len(resp.GetMutations())))
			}
		}()
	}
	wg.Wait()
	return e.Load(), m.Load()
}

func delta(after, before map[string]float64, key string) float64 { return after[key] - before[key] }

func scenarioSplitAndMetrics(c pb.RTBExtensionPointClient, url, pctStr string, n int, token string, rep *report) {
	groups, err := groupsFor(pctStr)
	if err != nil {
		rep.check("-expect-pct parses", false, err.Error())
		return
	}
	fmt.Printf("Scenario: requested %s%% -> %d of 16 hex groups (%.2f%%); %d requests cycle the 16 digits; metrics move; zero mutations\n",
		pctStr, groups, float64(groups)*6.25, n)
	if n%16 != 0 {
		rep.check("-split-n is a multiple of 16", false, strconv.Itoa(n))
		return
	}

	before := parseMetrics(fetch(url))
	errs, muts := sendAll(c, n, func(i int) *pb.RTBRequest { return splitPubReq(token, i) })
	after := parseMetrics(fetch(url))
	rep.check("all calls succeed and echo the id", errs == 0, fmt.Sprintf("%d failed", errs))
	rep.check("zero mutations over "+strconv.Itoa(n)+" requests", muts == 0, fmt.Sprintf("%d mutations", muts))

	tst, ctl := delta(after, before, `rtd_ab_requests_total{arm="test"}`), delta(after, before, `rtd_ab_requests_total{arm="control"}`)
	want := float64(n) * float64(groups) / 16
	rep.check("routed total equals sent", tst+ctl == float64(n), fmt.Sprintf("test=%v control=%v sent=%d", tst, ctl, n))
	rep.check(fmt.Sprintf("test = auctions whose last hex digit is below %d: %v of %d, want %v", groups, tst, n, want), tst == want, fmt.Sprintf("test=%v want %v", tst, want))

	// Strategy metrics must move for each arm that received traffic, by the exact
	// amounts the deterministic request implies for the sources its model enables.
	perReq := map[string]float64{"category": 1, "genre": 1, "keyword": 2, "network": 1, "eid": 1}
	for _, arm := range []string{"control", "test"} {
		k := tst
		list := expect.tstSources
		if arm == "control" {
			k, list = ctl, expect.ctlSources
		}
		if k == 0 {
			continue
		}
		enabled := map[string]bool{}
		for _, nme := range strings.Split(list, ",") {
			enabled[nme] = true
		}
		wantSig := 0.0
		for src, cnt := range perReq {
			if enabled[src] {
				wantSig += cnt
			}
		}
		lab := fmt.Sprintf(`{arm=%q}`, arm)
		enr := delta(after, before, "rtd_ab_content_enrichable_total"+lab)
		cnt := delta(after, before, "rtd_ab_content_signals_count"+lab)
		sig := delta(after, before, "rtd_ab_content_signals_sum"+lab)
		rep.check(fmt.Sprintf("%s: analyzed requests = %v", arm, k), cnt == k, fmt.Sprintf("got %v", cnt))
		rep.check(fmt.Sprintf("%s: enrichable = %v", arm, k), enr == k, fmt.Sprintf("got %v", enr))
		rep.check(fmt.Sprintf("%s: signals per request = %v (sources %s)", arm, wantSig, list), math.Abs(sig-wantSig*k) < 1e-6, fmt.Sprintf("got %v for %v requests", sig, k))
		rep.check(fmt.Sprintf("%s: sumsq moves", arm), delta(after, before, "rtd_ab_content_signals_sumsq"+lab) > 0, "no change")
		for _, src := range []string{"category", "genre", "keyword", "network", "eid"} {
			d := delta(after, before, fmt.Sprintf(`rtd_ab_content_signal_total{arm=%q,source=%q}`, arm, src))
			wantSrc := 0.0
			if enabled[src] {
				wantSrc = perReq[src]
			}
			rep.check(fmt.Sprintf("%s: source %s = %v per request", arm, src, wantSrc), d == wantSrc*k, fmt.Sprintf("got %v for %v requests", d, k))
		}
		cntT := delta(after, before, fmt.Sprintf(`rtd_ab_analyze_seconds_count{arm=%q}`, arm))
		rep.check(fmt.Sprintf("%s: analyze_seconds sampled 1 in 16 (%v of %v)", arm, cntT, k), cntT >= math.Floor(k/16)-1 && cntT <= math.Ceil(k/16)+1, "unexpected sample count")
	}

	// Requests with no assignable auction id, and other lifecycles, are counted,
	// not analyzed, and still get the empty OK response.
	ub := parseMetrics(fetch(url))
	errs, muts = sendAll(c, 300, func(i int) *pb.RTBRequest {
		r := splitPubReq(token, i)
		switch i % 3 {
		case 0:
			r.BidRequest.Id = s("xyz-not-hex-g_" + token)
		case 1:
			r.BidRequest.Id = nil
		default:
			lc := lifecycleDSPBidResp
			r.Lifecycle = &lc
		}
		return r
	})
	ua := parseMetrics(fetch(url))
	rep.check("unroutable and other-lifecycle requests: calls succeed, zero mutations", errs == 0 && muts == 0, fmt.Sprintf("errs=%d muts=%d", errs, muts))
	var routed float64
	for _, arm := range []string{"control", "test"} {
		routed += delta(ua, ub, fmt.Sprintf(`rtd_ab_requests_total{arm=%q}`, arm))
	}
	rep.check("none of the 300 was routed or analyzed", routed == 0, fmt.Sprintf("routed=%v", routed))
	for reason, wantN := range map[string]float64{"not_hex": 100, "no_id": 100, "other_lifecycle": 100} {
		d := delta(ua, ub, fmt.Sprintf(`rtd_ab_unrouted_total{reason=%q}`, reason))
		rep.check(fmt.Sprintf("rtd_ab_unrouted_total{reason=%s} = %.0f", reason, wantN), d == wantN, fmt.Sprintf("got %v", d))
	}
	rep.check("no recovered strategy panics", ua[`rtd_ab_errors_total{arm="control"}`]+ua[`rtd_ab_errors_total{arm="test"}`] == 0, "rtd_ab_errors_total is non-zero")
}

// scenarioBoundsAfterLoad checks the loaded-state limits and the sentinel.
func scenarioBoundsAfterLoad(url, token string, rep *report) {
	fmt.Println("Scenario: series count, loaded payload size, sentinel, rpc_server_duration labels")
	body := fetch(url)
	if body == "" {
		rep.check("/metrics reachable", false, "empty/unreachable")
		return
	}
	n := countSeries(body)
	rep.check(fmt.Sprintf("series count <= %d", maxSeries), n <= maxSeries, fmt.Sprintf("%d series", n))
	rep.check(fmt.Sprintf("loaded payload <= %d bytes", maxLoadedPayload), len(body) <= maxLoadedPayload, fmt.Sprintf("%d bytes", len(body)))
	fmt.Printf("  loaded payload: %d bytes, %d series\n", len(body), n)
	rep.check("sentinel token absent from /metrics", !strings.Contains(body, token) && !strings.Contains(strings.ToLower(body), strings.ToLower(token)), "request string leaked")
	armOnRPC := false
	for _, ln := range strings.Split(body, "\n") {
		if strings.HasPrefix(ln, "rpc_server_duration") && strings.Contains(ln, "arm=") {
			armOnRPC = true
		}
	}
	rep.check("rpc_server_duration has no arm label", !armOnRPC, "arm label found")
}
