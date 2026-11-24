package main

import (
	"errors"
	"math/rand/v2"
)

// ErrNoValidPairing means the exclusions make a draw impossible
// (for example, 3 people where everyone excludes everyone).
var ErrNoValidPairing = errors.New("no valid pairing exists with these exclusions")

// Draw assigns each participant a recipient.
//
// Rules:
//   - nobody gives to themselves
//   - nobody gives to someone in their exclusion set (exclusions are
//     treated as two-way: if A excludes B, B can't draw A either)
//
// It prefers a single big loop (A→B→C→…→A) because that avoids two people
// simply swapping gifts, which makes it harder to guess your Santa. If the
// exclusions make a single loop impossible it falls back to any valid
// assignment.
//
// The result maps giver ID → recipient ID.
func Draw(ids []string, excluded func(a, b string) bool, rng *rand.Rand) (map[string]string, error) {
	n := len(ids)
	if n < 2 {
		return nil, errors.New("need at least 2 participants")
	}

	allowed := func(giver, recipient string) bool {
		return giver != recipient && !excluded(giver, recipient) && !excluded(recipient, giver)
	}

	// Attempt 1: a single cycle. Randomized backtracking over orderings
	// (a Hamiltonian cycle search). Exponential in the worst case, so cap
	// the work; with few exclusions it finds one almost immediately.
	if res, ok := singleCycle(ids, allowed, rng, 200_000); ok {
		return res, nil
	}

	// Attempt 2: any derangement that respects exclusions. Backtracking
	// over givers, trying recipients in random order.
	return anyAssignment(ids, allowed, rng)
}

func singleCycle(ids []string, allowed func(a, b string) bool, rng *rand.Rand, budget int) (map[string]string, bool) {
	n := len(ids)
	order := make([]string, 0, n)
	used := make(map[string]bool, n)

	// Start from a random person; every cycle passes through everyone, so
	// the start choice doesn't limit which cycles we can find.
	start := ids[rng.IntN(n)]
	order = append(order, start)
	used[start] = true

	steps := 0
	var extend func() bool
	extend = func() bool {
		steps++
		if steps > budget {
			return false
		}
		last := order[len(order)-1]
		if len(order) == n {
			return allowed(last, order[0]) // close the loop
		}
		for _, i := range rng.Perm(n) {
			next := ids[i]
			if used[next] || !allowed(last, next) {
				continue
			}
			order = append(order, next)
			used[next] = true
			if extend() {
				return true
			}
			order = order[:len(order)-1]
			used[next] = false
		}
		return false
	}

	if !extend() {
		return nil, false
	}
	res := make(map[string]string, n)
	for i, giver := range order {
		res[giver] = order[(i+1)%n]
	}
	return res, true
}

func anyAssignment(ids []string, allowed func(a, b string) bool, rng *rand.Rand) (map[string]string, error) {
	n := len(ids)
	res := make(map[string]string, n)
	taken := make(map[string]bool, n)

	var assign func(k int) bool
	assign = func(k int) bool {
		if k == n {
			return true
		}
		giver := ids[k]
		for _, i := range rng.Perm(n) {
			r := ids[i]
			if taken[r] || !allowed(giver, r) {
				continue
			}
			res[giver] = r
			taken[r] = true
			if assign(k + 1) {
				return true
			}
			delete(res, giver)
			taken[r] = false
		}
		return false
	}

	if !assign(0) {
		return nil, ErrNoValidPairing
	}
	return res, nil
}
