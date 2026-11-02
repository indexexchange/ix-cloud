// SPDX-License-Identifier: Apache-2.0

// Package router assigns each request to the control or test arm from the
// auction id, so the assignment is a pure function of the request.
//
// Index sends every container the bid request, and bid_request.id is
// "<auction_id>_<suffix>", where auction_id is a UUID. The router takes the text
// before the first underscore (the whole id if there is none, as in a bare UUID),
// reads its last character as a hexadecimal digit
// (0 to f, 16 groups), and routes to test when that digit is below the number of
// test groups g, otherwise to control. With g test groups the test share is g/16
// of auctions: 4 groups (digits 0, 1, 2, 3) is 25%. The first digits are always
// the test group and the rest are control.
//
// Properties:
//   - The same auction always gets the same arm on every pod, with no shared
//     state. Partner Client Audit Logs carry the same
//     auction_id, so the group of any logged auction can be recomputed offline.
//   - The share is statistical, not exact: a random UUID's last digit is uniform,
//     so the observed share varies like a binomial around g/16.
//   - A request whose bid_request.id is missing, or whose auction id does not end
//     in a hex digit, cannot be assigned. It is reported
//     as unrouted and is not analyzed, so neither arm is contaminated.
//   - Not user-sticky: one user's different auctions land in both arms.
//
// The path is a byte scan and integer compare: no lock, no allocation.
package router

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Arm is the experiment arm a request is routed to.
type Arm uint8

const (
	Control Arm = iota
	Test
	NumArms
)

func (a Arm) String() string {
	if a == Test {
		return "test"
	}
	return "control"
}

// BasisPoints is a split in units of 0.01%, 0 to 10000.
type BasisPoints uint64

const MaxBP BasisPoints = 10000

// ParsePct parses a percentage with at most two decimals, 0 to 100, into basis
// points. It accepts "10", "0.5", "33.33", "100", and rejects everything else
// (empty, signs, exponents, more than two decimals, values above 100).
func ParsePct(s string) (BasisPoints, error) {
	if s == "" || len(s) > 6 {
		return 0, fmt.Errorf("invalid percentage %q: want a number from 0 to 100 with at most 2 decimals", s)
	}
	whole, frac := s, ""
	for i := 0; i < len(s); i++ {
		if s[i] == '.' {
			whole, frac = s[:i], s[i+1:]
			if frac == "" {
				return 0, fmt.Errorf("invalid percentage %q: empty decimals", s)
			}
			break
		}
	}
	if whole == "" || len(frac) > 2 {
		return 0, fmt.Errorf("invalid percentage %q: want a number from 0 to 100 with at most 2 decimals", s)
	}
	for _, part := range []string{whole, frac} {
		for i := 0; i < len(part); i++ {
			if part[i] < '0' || part[i] > '9' {
				return 0, fmt.Errorf("invalid percentage %q: want digits only", s)
			}
		}
	}
	w, err := strconv.ParseUint(whole, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid percentage %q: %v", s, err)
	}
	var f uint64
	if frac != "" {
		f, _ = strconv.ParseUint(frac, 10, 64)
		if len(frac) == 1 {
			f *= 10
		}
	}
	bp := BasisPoints(w*100 + f)
	if bp > MaxBP {
		return 0, fmt.Errorf("invalid percentage %q: above 100", s)
	}
	return bp, nil
}

// FromFloat converts a percentage to basis points: round(pct*100), clamped to 0..10000.
func FromFloat(pct float64) BasisPoints {
	if math.IsNaN(pct) || pct <= 0 {
		return 0
	}
	bp := math.Round(pct * 100)
	if bp > float64(MaxBP) {
		return MaxBP
	}
	return BasisPoints(bp)
}

// Pct renders bp as a percentage string with the fewest decimals ("10", "33.33").
func (bp BasisPoints) Pct() string {
	return strconv.FormatFloat(float64(bp)/100, 'f', -1, 64)
}

// Groups is the number of hex digits (of 16) that make up the test group.
const Groups = 16

// GroupsFor aligns a split to the nearest 1/16 (6.25%), rounding half up, and
// returns the number of test groups, 0 to 16. 10% is 2 groups (12.5%).
func GroupsFor(bp BasisPoints) uint8 {
	return uint8((uint64(bp)*Groups + uint64(MaxBP)/2) / uint64(MaxBP))
}

// BPOf is the share in basis points of g test groups (625 per group).
func BPOf(g uint8) BasisPoints {
	if g > Groups {
		g = Groups
	}
	return BasisPoints(uint64(g) * uint64(MaxBP) / Groups)
}

// Reason says why a request could not be assigned.
type Reason uint8

const (
	Routed         Reason = iota
	NoID                  // bid_request missing or its id is empty
	NotHex                // the auction id is empty or does not end in a hex digit
	OtherLifecycle        // not a lifecycle this container analyzes (set by the engine)
	NumReasons
)

func (r Reason) String() string {
	switch r {
	case NoID:
		return "no_id"
	case NotHex:
		return "not_hex"
	case OtherLifecycle:
		return "other_lifecycle"
	default:
		return "routed"
	}
}

// Router is immutable after New and safe for concurrent use.
type Router struct{ groups uint8 }

// New returns a Router with g test groups (0 to 16).
func New(g uint8) *Router {
	if g > Groups {
		g = Groups
	}
	return &Router{groups: g}
}

// TestGroups returns the number of test groups.
func (r *Router) TestGroups() uint8 { return r.groups }

// BP returns the effective test share in basis points.
func (r *Router) BP() BasisPoints { return BPOf(r.groups) }

// Group extracts the hex group (0 to 15) of an auction from bid_request.id.
func Group(bidRequestID string) (uint8, Reason) {
	if bidRequestID == "" {
		return 0, NoID
	}
	auction := bidRequestID
	if i := strings.IndexByte(bidRequestID, '_'); i >= 0 {
		auction = bidRequestID[:i]
	}
	if auction == "" {
		return 0, NotHex
	}
	c := auction[len(auction)-1]
	switch {
	case c >= '0' && c <= '9':
		return c - '0', Routed
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, Routed
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, Routed
	}
	return 0, NotHex
}

// Route assigns an auction. bidRequestID is bid_request.id. An unassignable id
// returns the Reason and arm Control; the caller must not analyze it.
func (r *Router) Route(bidRequestID string) (Arm, Reason) {
	g, why := Group(bidRequestID)
	if why != Routed {
		return Control, why
	}
	if g < r.groups {
		return Test, Routed
	}
	return Control, Routed
}
