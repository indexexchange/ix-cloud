// SPDX-License-Identifier: Apache-2.0

package abinit

import (
	"strings"
	"testing"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestBakedValueAlignedToSixteenth(t *testing.T) {
	for baked, want := range map[string]struct {
		g   uint8
		pct string
	}{"12.5": {2, "12.5"}, "10": {2, "12.5"}, "25": {4, "25"}, "33.33": {5, "31.25"}, "0": {0, "0"}, "100": {16, "100"}} {
		r := Resolve(baked, env(nil))
		if r.Groups != want.g || r.Pct != want.pct || len(r.Lines) != 0 || r.Requested != baked {
			t.Fatalf("%s: %+v", baked, r)
		}
	}
}

func TestOverrideWarnsAndApplies(t *testing.T) {
	r := Resolve("25", env(map[string]string{"RTD_AB_TEST_PCT": "50"}))
	if r.Groups != 8 || r.Pct != "50" || len(r.Lines) != 1 || !strings.HasPrefix(r.Lines[0], "WARN ") {
		t.Fatalf("%+v", r)
	}
}

func TestInvalidOverrideFallsBackWithOneErrorLine(t *testing.T) {
	for _, bad := range []string{"abc", "101", "-5", "1.234", "NaN", "5%"} {
		r := Resolve("25", env(map[string]string{"RTD_AB_TEST_PCT": bad}))
		if r.Groups != 4 || len(r.Lines) != 1 || !strings.HasPrefix(r.Lines[0], "ERROR ") {
			t.Fatalf("override %q: %+v", bad, r)
		}
	}
}

func TestInvalidBakedFallsBackToDefault(t *testing.T) {
	r := Resolve("garbage", env(nil))
	if r.Groups != 2 || r.Pct != "12.5" || len(r.Lines) != 1 || !strings.HasPrefix(r.Lines[0], "ERROR ") {
		t.Fatalf("%+v", r)
	}
}
