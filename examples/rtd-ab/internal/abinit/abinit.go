// SPDX-License-Identifier: Apache-2.0

// Package abinit resolves the experiment's split at startup. The deployed image
// uses the percentage baked in at build time; RTD_AB_TEST_PCT may override it
// for local harness runs only. Nothing here can fail the process: a bad value
// logs one error line and falls back to the baked value.
package abinit

import (
	"github.com/indexexchange/rtd-ab/internal/router"
)

// DefaultPct is used only if the baked value is itself unparsable. 12.5 is
// exactly 2 of 16 hex groups.
const DefaultPct = "12.5"

// Result is the resolved split and what to log about it.
type Result struct {
	Groups    uint8  // test groups, 0 to 16 (digits 0 .. Groups-1 are test)
	Pct       string // effective percentage after alignment to 1/16, e.g. "12.5"
	Requested string // the value asked for (baked or override), as given
	Lines     []string
}

// align turns a validated request into groups and records the alignment.
func align(bp router.BasisPoints) uint8 { return router.GroupsFor(bp) }

// Resolve picks the effective split from the configured value (the model
// folder's test percent, else the baked percentage) and the optional override,
// aligned to the nearest 1/16. get reads an environment variable.
func Resolve(baked string, get func(string) string) Result {
	var res Result
	bp, err := router.ParsePct(baked)
	res.Requested = baked
	if err != nil {
		res.Lines = append(res.Lines, "ERROR baked test_pct invalid: "+err.Error()+"; using "+DefaultPct)
		bp, _ = router.ParsePct(DefaultPct)
		res.Requested = DefaultPct
	}
	if v := get("RTD_AB_TEST_PCT"); v != "" {
		if o, oerr := router.ParsePct(v); oerr != nil {
			res.Lines = append(res.Lines, "ERROR RTD_AB_TEST_PCT invalid: "+oerr.Error()+"; using configured value "+res.Requested)
		} else {
			res.Lines = append(res.Lines, "WARN RTD_AB_TEST_PCT="+v+" overrides the configured value "+res.Requested+" (local runs only)")
			bp, res.Requested = o, v
		}
	}
	res.Groups = align(bp)
	res.Pct = router.BPOf(res.Groups).Pct()
	return res
}
