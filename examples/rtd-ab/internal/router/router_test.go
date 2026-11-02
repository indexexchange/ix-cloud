// SPDX-License-Identifier: Apache-2.0

package router

import (
	"fmt"
	"math"
	"math/rand"
	"sync"
	"testing"
)

const uuidPrefix = "3f2b8c1e-9a4d-4c6e-8b1a-5d7e0f2a419"

func id(hex byte) string { return uuidPrefix + string(hex) + "_exch-1" }

func TestGroupFromAuctionID(t *testing.T) {
	for i, c := range "0123456789abcdef" {
		g, why := Group(id(byte(c)))
		if why != Routed || int(g) != i {
			t.Fatalf("digit %c: group=%d reason=%v", c, g, why)
		}
	}
	for i, c := range "ABCDEF" {
		if g, why := Group(id(byte(c))); why != Routed || int(g) != 10+i {
			t.Fatalf("upper-case %c: %d %v", c, g, why)
		}
	}
	// A bare UUID with no underscore is the auction id itself.
	if g, why := Group(uuidPrefix + "9"); why != Routed || g != 9 {
		t.Fatalf("bare uuid: %d %v", g, why)
	}
	// Only the text before the FIRST underscore counts.
	if g, why := Group("aaaa-0003_xx_7"); why != Routed || g != 3 {
		t.Fatalf("first underscore: %d %v", g, why)
	}
}

func TestUnassignableIDs(t *testing.T) {
	for in, want := range map[string]Reason{
		"": NoID, "_suffix": NotHex, "uuid-with-g_x": NotHex, "abc-_x": NotHex, "abc 3_x": Routed, "zzzz_1": NotHex,
		"plain-uuid-no-underscore-g": NotHex,
	} {
		if _, why := Group(in); why != want {
			t.Errorf("Group(%q) reason=%v want %v", in, why, want)
		}
	}
	r := New(8)
	if arm, why := r.Route("nounderscoreg"); arm != Control || why != NotHex {
		t.Fatalf("%v %v", arm, why)
	}
}

// 25% is 4 groups: auction ids ending in 0, 1, 2 or 3 are test; the rest control.
func TestFirstGroupsAreTest(t *testing.T) {
	r := New(4)
	for i, c := range "0123456789abcdef" {
		want := Control
		if i < 4 {
			want = Test
		}
		if arm, why := r.Route(id(byte(c))); arm != want || why != Routed {
			t.Fatalf("digit %c: %v %v want %v", c, arm, why, want)
		}
	}
}

func TestExtremes(t *testing.T) {
	r0, r16 := New(0), New(16)
	for _, c := range "0123456789abcdef" {
		if a, _ := r0.Route(id(byte(c))); a != Control {
			t.Fatal("0 groups must be all control")
		}
		if a, _ := r16.Route(id(byte(c))); a != Test {
			t.Fatal("16 groups must be all test")
		}
	}
	if New(200).TestGroups() != 16 {
		t.Fatal("groups clamp to 16")
	}
}

func TestSameAuctionSameArmOnEveryRouter(t *testing.T) {
	a, b := New(5), New(5)
	rng := rand.New(rand.NewSource(3))
	for i := 0; i < 5000; i++ {
		in := fmt.Sprintf("%08x-%04x-4%03x-8%03x-%012x_ssp", rng.Uint32(), rng.Intn(1<<16), rng.Intn(1<<12), rng.Intn(1<<12), rng.Int63n(1<<48))
		x, _ := a.Route(in)
		y, _ := b.Route(in)
		if x != y {
			t.Fatalf("auction %s routed to %v then %v", in, x, y)
		}
	}
}

func TestAlignmentToNearestSixteenth(t *testing.T) {
	for _, tc := range []struct {
		pct    string
		groups uint8
		eff    string
	}{
		{"0", 0, "0"}, {"3.12", 0, "0"}, {"3.13", 1, "6.25"}, {"6.25", 1, "6.25"}, {"10", 2, "12.5"}, {"12.5", 2, "12.5"},
		{"25", 4, "25"}, {"33.33", 5, "31.25"}, {"50", 8, "50"}, {"96.87", 15, "93.75"}, {"96.88", 16, "100"}, {"100", 16, "100"},
	} {
		bp, err := ParsePct(tc.pct)
		if err != nil {
			t.Fatal(err)
		}
		if g := GroupsFor(bp); g != tc.groups || BPOf(g).Pct() != tc.eff {
			t.Errorf("%s%%: groups=%d eff=%s, want %d / %s", tc.pct, g, BPOf(g).Pct(), tc.groups, tc.eff)
		}
	}
}

// A random UUID's last digit is uniform, so the observed share is binomial around
// groups/16. Fixed seed: this is deterministic.
func TestStatisticalShareOnRandomUUIDs(t *testing.T) {
	for _, g := range []uint8{1, 2, 4, 5, 8, 12} {
		r := New(g)
		rng := rand.New(rand.NewSource(int64(g)))
		const n = 200000
		var test int
		for i := 0; i < n; i++ {
			in := fmt.Sprintf("%08x-%04x-4%03x-8%03x-%012x_ssp", rng.Uint32(), rng.Intn(1<<16), rng.Intn(1<<12), rng.Intn(1<<12), rng.Int63n(1<<48))
			if a, _ := r.Route(in); a == Test {
				test++
			}
		}
		p := float64(g) / 16
		se := math.Sqrt(p * (1 - p) / n)
		if dev := math.Abs(float64(test)/n - p); dev > 3*se {
			t.Errorf("groups=%d: share %.5f, expected %.5f, %.1f SE", g, float64(test)/n, p, dev/se)
		}
	}
}

func TestParsePctRejects(t *testing.T) {
	for _, in := range []string{"", "-1", "+5", "abc", "100.01", "101", "1e1", "5.", ".5", "1.234", "NaN", "Inf", "1,5", " 5", "5 ", "0x10", "1000000"} {
		if bp, err := ParsePct(in); err == nil {
			t.Errorf("ParsePct(%q)=%d, want error", in, bp)
		}
	}
	for in, want := range map[string]BasisPoints{"0": 0, "100": 10000, "100.00": 10000, "10": 1000, "00010": 1000, "0.0": 0, "33.33": 3333, "0.5": 50} {
		if bp, err := ParsePct(in); err != nil || bp != want {
			t.Errorf("ParsePct(%q)=%d,%v want %d", in, bp, err, want)
		}
	}
	if FromFloat(33.33) != 3333 || FromFloat(-1) != 0 || FromFloat(250) != MaxBP {
		t.Fatal("FromFloat rounding or clamping wrong")
	}
}

func TestConcurrentUse(t *testing.T) {
	r := New(4)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var test [16]int
	for g := 0; g < 64; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var local [16]int
			for i := 0; i < 1600; i++ {
				c := "0123456789abcdef"[i%16]
				if a, _ := r.Route(id(c)); a == Test {
					local[i%16]++
				}
			}
			mu.Lock()
			for k, v := range local {
				test[k] += v
			}
			mu.Unlock()
		}()
	}
	wg.Wait()
	for k, v := range test {
		want := 0
		if k < 4 {
			want = 64 * 100
		}
		if v != want {
			t.Fatalf("digit %d: %d test, want %d", k, v, want)
		}
	}
}

func TestRouteAllocsZero(t *testing.T) {
	if raceEnabled {
		t.Skip("allocation counts differ under -race")
	}
	r := New(4)
	in := id('7')
	if n := testing.AllocsPerRun(1000, func() { r.Route(in) }); n != 0 {
		t.Fatalf("Route allocates %v per call", n)
	}
}

func BenchmarkRoute(b *testing.B) {
	r := New(4)
	in := id('7')
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		r.Route(in)
	}
}

func BenchmarkRouteParallel(b *testing.B) {
	r := New(4)
	in := id('7')
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			r.Route(in)
		}
	})
}
