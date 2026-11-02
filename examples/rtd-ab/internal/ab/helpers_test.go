// SPDX-License-Identifier: Apache-2.0

package ab

import (
	"fmt"
	"math/rand"
	"strings"

	"github.com/indexexchange/rtd-ab/internal/router"
	openrtb "github.com/indexexchange/rtd-ab/pkg/pb/openrtb"
	pb "github.com/indexexchange/rtd-ab/pkg/pb/artf"
)

func sp(v string) *string   { return &v }
func fp(v float64) *float64 { return &v }

func lcp(l pb.Lifecycle) *pb.Lifecycle { return &l }

// auctionID returns a bid_request.id whose auction id (before the first
// underscore) is a UUID ending in the hex digit n%16, as Index sends it.
func auctionID(n int) string {
	return fmt.Sprintf("00000000-0000-4000-8000-%012x_ssp", n)
}

type pubOpts struct {
	cat, sectioncat, pagecat, contentCat []string
	genre, keywords                      string
	kwarray                              []string
	network, channel                     string
	eids                                 int
	app                                  bool
	domain                               string
}

func pubReq(o pubOpts) *pb.RTBRequest {
	content := &openrtb.BidRequest_Content{Cat: o.contentCat, Kwarray: o.kwarray}
	if o.genre != "" {
		content.Genre = sp(o.genre)
	}
	if o.keywords != "" {
		content.Keywords = sp(o.keywords)
	}
	if o.network != "" {
		content.Network = &openrtb.BidRequest_Network{Name: sp(o.network)}
	}
	if o.channel != "" {
		content.Channel = &openrtb.BidRequest_Channel{Name: sp(o.channel)}
	}
	br := &openrtb.BidRequest{Id: sp(auctionID(0)), Imp: []*openrtb.BidRequest_Imp{{Id: sp("imp-1")}}}
	domain := o.domain
	if domain == "" {
		domain = "pub.example"
	}
	if o.app {
		br.DistributionchannelOneof = &openrtb.BidRequest_App_{App: &openrtb.BidRequest_App{
			Cat: o.cat, Sectioncat: o.sectioncat, Pagecat: o.pagecat, Content: content, Domain: sp(domain)}}
	} else {
		br.DistributionchannelOneof = &openrtb.BidRequest_Site_{Site: &openrtb.BidRequest_Site{
			Cat: o.cat, Sectioncat: o.sectioncat, Pagecat: o.pagecat, Content: content, Domain: sp(domain)}}
	}
	if o.eids > 0 {
		u := &openrtb.BidRequest_User{}
		for i := 0; i < o.eids; i++ {
			u.Eids = append(u.Eids, &openrtb.BidRequest_EID{Source: sp("liveramp.com")})
		}
		br.User = u
	}
	return &pb.RTBRequest{Lifecycle: lcp(LifecyclePublisherBidRequest), Id: sp("req"), BidRequest: br, Tmax: func() *int32 { v := int32(5); return &v }()}
}

// forceArm returns an engine whose router sends every request to one arm.
func forceArm(arm router.Arm, analyses ...Analysis) *Engine {
	g := uint8(0)
	if arm == router.Test {
		g = router.Groups
	}
	return NewEngine(router.New(g), router.BPOf(g).Pct(), analyses...)
}

// withAuction sets the auction id of r to auctionID(n) and returns r.
func withAuction(r *pb.RTBRequest, n int) *pb.RTBRequest {
	r.BidRequest.Id = sp(auctionID(n))
	return r
}

// mixedTraffic returns n deterministic publisher requests that exercise every
// content source, with the token planted in the domain, ids and keywords. Some
// requests use another lifecycle or carry an unroutable id.
func mixedTraffic(n int, token string) []*pb.RTBRequest {
	rng := rand.New(rand.NewSource(7))
	cats := []string{"IAB1", "IAB12-3", "632", "IAB99", "bogus"}
	genres := []string{"Drama", "news", "polka", ""}
	kws := []string{"news,sports,cooking", "zzz qqq", "Football; travel", ""}
	nets := []string{"ESPN", "local station", ""}
	out := make([]*pb.RTBRequest, 0, n)
	for i := 0; i < n; i++ {
		o := pubOpts{
			genre: genres[rng.Intn(len(genres))], keywords: kws[rng.Intn(len(kws))] + "," + token,
			network: nets[rng.Intn(len(nets))], eids: rng.Intn(3), app: rng.Intn(5) == 0,
			domain: "d" + token + ".example",
		}
		for k := rng.Intn(4); k > 0; k-- {
			o.cat = append(o.cat, cats[rng.Intn(len(cats))])
		}
		if rng.Intn(3) == 0 {
			o.contentCat = []string{cats[rng.Intn(len(cats))]}
		}
		r := pubReq(o)
		r.Id = sp("id-" + token)
		r.BidRequest.Id = sp(fmt.Sprintf("%s_%s", auctionID(rng.Intn(1 << 20))[:36], token))
		switch rng.Intn(20) {
		case 0:
			r.Lifecycle = lcp(pb.Lifecycle(2)) // another lifecycle
		case 1:
			r.BidRequest.Id = sp("not-hex-g_" + token) // unroutable
		}
		out = append(out, r)
	}
	return out
}

func series(body string) (n int) {
	for _, l := range strings.Split(body, "\n") {
		if l != "" && !strings.HasPrefix(l, "#") {
			n++
		}
	}
	return n
}

// sample returns the value of the first series line that starts with prefix.
func sample(body, prefix string) (string, bool) {
	for _, l := range strings.Split(body, "\n") {
		if strings.HasPrefix(l, prefix) {
			return strings.TrimSpace(l[strings.LastIndex(l, " "):]), true
		}
	}
	return "", false
}
