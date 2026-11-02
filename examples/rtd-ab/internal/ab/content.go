// SPDX-License-Identifier: Apache-2.0

package ab

import (
	"fmt"
	"io"
	"strings"
	"sync/atomic"

	"github.com/indexexchange/rtd-ab/internal/router"
	openrtb "github.com/indexexchange/rtd-ab/pkg/pb/openrtb"
	pb "github.com/indexexchange/rtd-ab/pkg/pb/artf"
)

// Content-signal coverage what-if on PUBLISHER_BID_REQUEST, the input
// to an ADD_CIDS style enrichment. Each arm counts the signals it could derive
// from the request; nothing is added to the auction.
//
// Which sources an arm reads, and the genre, keyword and network allowlists it
// matches against, come from its model (content.json, see internal/abmodel). The
// built-in control reads category fields only (site|app cat, sectioncat, pagecat
// and content.cat); the built-in test adds content.genre, content.keywords tokens,
// content.network or content.channel hits, and the presence of user.eids.

// Signal sources (bounded enum for rtd_ab_content_signal_total{source}).
const (
	srcCategory = iota
	srcGenre
	srcKeyword
	srcNetwork
	srcEID
	srcNone // requests with zero signals
	numSrc
)

var srcNames = [numSrc]string{"category", "genre", "keyword", "network", "eid", "none"}

// SourceMask is a set of enabled signal sources (bit i is source i).
type SourceMask uint8

// SourceBit returns the mask bit for a source name ("category", "genre",
// "keyword", "network", "eid"), and false for anything else.
func SourceBit(name string) (SourceMask, bool) {
	for i := 0; i < srcNone; i++ {
		if srcNames[i] == name {
			return 1 << i, true
		}
	}
	return 0, false
}

func (m SourceMask) has(src int) bool { return m&(1<<src) != 0 }

// Names lists the enabled sources in a fixed order, e.g. "category+genre".
func (m SourceMask) Names() string {
	var out []string
	for i := 0; i < srcNone; i++ {
		if m.has(i) {
			out = append(out, srcNames[i])
		}
	}
	return strings.Join(out, "+")
}

// ContentModel is one arm's content-signal settings. Nil allowlists mean the
// built-in lists.
type ContentModel struct {
	Sources  SourceMask
	Genres   []string
	Keywords []string
	Networks []string
}

// DefaultContentModels are the built-in control (categories only) and test (all
// five sources) settings.
func DefaultContentModels() [router.NumArms]ContentModel {
	cat, _ := SourceBit("category")
	all := cat
	for _, n := range []string{"genre", "keyword", "network", "eid"} {
		b, _ := SourceBit(n)
		all |= b
	}
	return [router.NumArms]ContentModel{router.Control: {Sources: cat}, router.Test: {Sources: all}}
}

// Per-source caps keep the signal count within 0..10 (the maximum is 3+1+3+1+1 = 9).
const (
	capCategory = 3
	capKeyword  = 3
	maxTokens   = 32   // keyword tokens scanned per request
	maxScan     = 1024 // bytes of content.keywords scanned
	maxTokLen   = 40
)

var signalsSpec = newHistSpec(1, 0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10)

// ContentObs is the content region of Observation.
type ContentObs struct {
	Signals uint8
	Src     [numSrc]uint8 // signals per source; Src[srcNone] is 1 when there are none
}

// Built-in allowlists, used when a model supplies none. these are defined here
// and are illustrative: enrichable rates depend on
// them, and a model may replace them.
var (
	defaultGenres   = []string{"action", "adventure", "animation", "comedy", "crime", "documentary", "drama", "family", "fantasy", "horror", "kids", "music", "mystery", "news", "reality", "romance", "sci-fi", "sports", "talk", "thriller"}
	defaultKeywords = []string{"news", "sports", "football", "soccer", "basketball", "baseball", "hockey", "cooking", "recipe", "travel", "finance", "business", "technology", "tech", "health", "fitness", "fashion", "beauty", "automotive", "cars", "gaming", "movies", "music", "politics", "weather", "education", "science", "home", "garden", "pets"}
	defaultNetworks = []string{"abc", "nbc", "cbs", "fox", "espn", "cnn", "bbc", "hbo", "pbs", "mtv", "amc", "tnt", "usa", "discovery", "history", "nickelodeon", "paramount", "peacock", "hulu", "roku channel", "pluto tv", "tubi"}
)

// setOf builds a lookup set of lower-cased entries; entries longer than maxTokLen
// can never match and are dropped.
func setOf(vals []string) map[string]struct{} {
	m := make(map[string]struct{}, len(vals))
	for _, v := range vals {
		if v = strings.ToLower(strings.TrimSpace(v)); v != "" && len(v) <= maxTokLen {
			m[v] = struct{}{}
		}
	}
	return m
}

// lookupFold reports whether s, ASCII-lowercased, is in m. It lowercases into a
// stack buffer and indexes the map with string(buf), which does not allocate.
func lookupFold(m map[string]struct{}, s string) bool {
	if len(s) == 0 || len(s) > maxTokLen {
		return false
	}
	var buf [maxTokLen]byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		if 'A' <= c && c <= 'Z' {
			c += 'a' - 'A'
		}
		buf[i] = c
	}
	_, ok := m[string(buf[:len(s)])]
	return ok
}

// knownCategory reports whether s is an IAB content category id: "IAB<1-26>" or
// "IAB<1-26>-<1-99>" (taxonomy 1.0), or a decimal id 1 to 1500 (taxonomy 2.x/3.x).
func knownCategory(s string) bool {
	if len(s) < 1 || len(s) > 8 {
		return false
	}
	i := 0
	if len(s) >= 4 && (s[0] == 'I' || s[0] == 'i') && (s[1] == 'A' || s[1] == 'a') && (s[2] == 'B' || s[2] == 'b') {
		i = 3
		n, j := digits(s, i)
		if j == i || n < 1 || n > 26 {
			return false
		}
		if j == len(s) {
			return true
		}
		if s[j] != '-' {
			return false
		}
		m, k := digits(s, j+1)
		return k == len(s) && k > j+1 && m >= 1 && m <= 99
	}
	n, j := digits(s, 0)
	return j == len(s) && j > 0 && n >= 1 && n <= 1500
}

func digits(s string, from int) (n, end int) {
	i := from
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		n = n*10 + int(s[i]-'0')
		i++
	}
	return n, i
}

type contentStrategy struct {
	name     string
	sources  SourceMask
	genres   map[string]struct{}
	keywords map[string]struct{}
	networks map[string]struct{}
}

func (s *contentStrategy) Name() string { return s.name }

func (s *contentStrategy) Analyze(req *pb.RTBRequest, o *Observation) {
	br := req.GetBidRequest()
	if br == nil {
		return
	}
	c := &o.Content

	var content *openrtb.BidRequest_Content
	var seen [capCategory]string
	nCat := 0
	addCats := func(list []string) {
		for _, v := range list {
			if nCat == capCategory {
				return
			}
			if !knownCategory(v) {
				continue
			}
			dup := false
			for k := 0; k < nCat; k++ {
				if seen[k] == v {
					dup = true
					break
				}
			}
			if !dup {
				seen[nCat] = v
				nCat++
			}
		}
	}
	switch {
	case br.GetSite() != nil:
		site := br.GetSite()
		content = site.GetContent()
		addCats(site.GetCat())
		addCats(site.GetSectioncat())
		addCats(site.GetPagecat())
	case br.GetApp() != nil:
		app := br.GetApp()
		content = app.GetContent()
		addCats(app.GetCat())
		addCats(app.GetSectioncat())
		addCats(app.GetPagecat())
	case br.GetDooh() != nil:
		content = br.GetDooh().GetContent()
	}
	addCats(content.GetCat())
	if s.sources.has(srcCategory) {
		c.Src[srcCategory] = uint8(nCat)
	}
	if s.sources.has(srcGenre) && (lookupFold(s.genres, content.GetGenre()) || len(content.GetGenres()) > 0) {
		c.Src[srcGenre] = 1
	}
	if s.sources.has(srcKeyword) {
		c.Src[srcKeyword] = countKeywordHits(s.keywords, content)
	}
	if s.sources.has(srcNetwork) && networkHit(s.networks, content) {
		c.Src[srcNetwork] = 1
	}
	if s.sources.has(srcEID) && len(br.GetUser().GetEids()) > 0 {
		c.Src[srcEID] = 1
	}

	var total uint8
	for i := 0; i < srcNone; i++ {
		total += c.Src[i]
	}
	if total == 0 {
		c.Src[srcNone] = 1
	}
	c.Signals = total
}

// countKeywordHits counts allowlisted tokens in content.keywords (split on comma,
// semicolon, space) and content.kwarray, at most capKeyword.
func countKeywordHits(allow map[string]struct{}, content *openrtb.BidRequest_Content) uint8 {
	var hits uint8
	tokens := 0
	for _, kw := range content.GetKwarray() {
		if tokens++; tokens > maxTokens || hits >= capKeyword {
			return hits
		}
		if lookupFold(allow, kw) {
			hits++
		}
	}
	s := content.GetKeywords()
	if len(s) > maxScan {
		s = s[:maxScan]
	}
	start := 0
	for i := 0; i <= len(s); i++ {
		if i < len(s) && s[i] != ',' && s[i] != ';' && s[i] != ' ' {
			continue
		}
		if i > start {
			if tokens++; tokens > maxTokens {
				return hits
			}
			if lookupFold(allow, s[start:i]) {
				if hits++; hits >= capKeyword {
					return hits
				}
			}
		}
		start = i + 1
	}
	return hits
}

func networkHit(allow map[string]struct{}, content *openrtb.BidRequest_Content) bool {
	n, ch := content.GetNetwork(), content.GetChannel()
	return lookupFold(allow, n.GetName()) || lookupFold(allow, n.GetDomain()) ||
		lookupFold(allow, ch.GetName()) || lookupFold(allow, ch.GetDomain())
}

type contentRecorder struct {
	enrichable atomic.Uint64
	src        [numSrc]atomic.Uint64
	signals    atomicHist
}

// ContentAnalysis is the content-signal coverage what-if.
type ContentAnalysis struct {
	strat [router.NumArms]*contentStrategy
	rec   [router.NumArms]contentRecorder
}

// NewContentAnalysis builds the analysis with one model per arm.
func NewContentAnalysis(m [router.NumArms]ContentModel) *ContentAnalysis {
	a := &ContentAnalysis{}
	for arm := range a.strat {
		cm := m[arm]
		a.strat[arm] = &contentStrategy{
			name: "content_" + cm.Sources.Names(), sources: cm.Sources,
			genres: setOf(orDefault(cm.Genres, defaultGenres)), keywords: setOf(orDefault(cm.Keywords, defaultKeywords)), networks: setOf(orDefault(cm.Networks, defaultNetworks)),
		}
		a.rec[arm].signals.spec = signalsSpec
	}
	return a
}

func orDefault(v, def []string) []string {
	if v == nil {
		return def
	}
	return v
}

func (a *ContentAnalysis) Name() string                     { return "content" }
func (a *ContentAnalysis) Strategy(arm router.Arm) Strategy { return a.strat[arm] }

func (a *ContentAnalysis) Record(arm router.Arm, o *Observation) {
	c := &o.Content
	r := &a.rec[arm]
	if c.Signals > 0 {
		r.enrichable.Add(1)
	}
	for i, n := range c.Src {
		if n != 0 {
			r.src[i].Add(uint64(n))
		}
	}
	r.signals.observe(int64(c.Signals))
}

func (a *ContentAnalysis) WriteMetrics(w io.Writer) {
	header(w, "rtd_ab_content_enrichable_total", "counter", "Analyzed requests with at least one derivable signal.")
	header(w, "rtd_ab_content_signals", "histogram", "Derivable signals per request (0 to 10).")
	header(w, "rtd_ab_content_signals_sumsq", "counter", "Sum of squared signal counts (for Welch t-test).")
	header(w, "rtd_ab_content_signal_total", "counter", "Signals by source; none counts requests with no signal.")
	for arm := router.Arm(0); arm < router.NumArms; arm++ {
		r := &a.rec[arm]
		lbl := fmt.Sprintf("arm=%q", arm.String())
		if v := r.enrichable.Load(); v != 0 {
			fmt.Fprintf(w, "rtd_ab_content_enrichable_total{%s} %d\n", lbl, v)
		}
	}
	for arm := router.Arm(0); arm < router.NumArms; arm++ {
		a.rec[arm].signals.writeSeries(w, "rtd_ab_content_signals", fmt.Sprintf("arm=%q", arm.String()))
	}
	writeSumsq(w, "rtd_ab_content_signals_sumsq", &a.rec[router.Control].signals, &a.rec[router.Test].signals)
	for arm := router.Arm(0); arm < router.NumArms; arm++ {
		for i := range a.rec[arm].src {
			if v := a.rec[arm].src[i].Load(); v != 0 {
				fmt.Fprintf(w, "rtd_ab_content_signal_total{arm=%q,source=%q} %d\n", arm.String(), srcNames[i], v)
			}
		}
	}
}

func (a *ContentAnalysis) SummaryLine(arm router.Arm) string {
	r := &a.rec[arm]
	n := r.signals.total()
	if n == 0 {
		return "enrichable_rate=0 mean_signals=0"
	}
	return fmt.Sprintf("enrichable_rate=%.4f mean_signals=%.4f", float64(r.enrichable.Load())/float64(n), r.signals.mean())
}
