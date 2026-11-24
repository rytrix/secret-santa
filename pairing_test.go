package main

import (
	"fmt"
	"math/rand/v2"
	"testing"
)

func noExclusions(a, b string) bool { return false }

func checkValid(t *testing.T, ids []string, res map[string]string, excluded func(a, b string) bool) {
	t.Helper()
	if len(res) != len(ids) {
		t.Fatalf("expected %d assignments, got %d", len(ids), len(res))
	}
	seen := map[string]bool{}
	for _, g := range ids {
		r, ok := res[g]
		if !ok {
			t.Fatalf("%s has no recipient", g)
		}
		if r == g {
			t.Fatalf("%s drew themselves", g)
		}
		if excluded(g, r) || excluded(r, g) {
			t.Fatalf("%s drew excluded %s", g, r)
		}
		if seen[r] {
			t.Fatalf("%s is a recipient twice", r)
		}
		seen[r] = true
	}
}

func isSingleCycle(res map[string]string, start string) bool {
	cur, steps := res[start], 1
	for cur != start {
		cur = res[cur]
		steps++
		if steps > len(res) {
			return false
		}
	}
	return steps == len(res)
}

func TestDrawBasic(t *testing.T) {
	for n := 2; n <= 40; n++ {
		ids := make([]string, n)
		for i := range ids {
			ids[i] = fmt.Sprint(i)
		}
		for seed := uint64(0); seed < 20; seed++ {
			rng := rand.New(rand.NewPCG(seed, 1))
			res, err := Draw(ids, noExclusions, rng)
			if err != nil {
				t.Fatal(err)
			}
			checkValid(t, ids, res, noExclusions)
			if !isSingleCycle(res, ids[0]) {
				t.Fatalf("n=%d: expected one loop with no exclusions", n)
			}
		}
	}
}

func TestDrawExclusions(t *testing.T) {
	ids := []string{"alice", "bob", "carol", "dave", "erin", "frank"}
	ex := map[[2]string]bool{
		{"alice", "bob"}:   true, // couple
		{"carol", "dave"}:  true, // couple
		{"erin", "alice"}:  true,
		{"frank", "carol"}: true,
	}
	excluded := func(a, b string) bool { return ex[[2]string{a, b}] }
	for seed := uint64(0); seed < 200; seed++ {
		res, err := Draw(ids, excluded, rand.New(rand.NewPCG(seed, 2)))
		if err != nil {
			t.Fatal(err)
		}
		checkValid(t, ids, res, excluded)
	}
}

// Two couples (A,B) and (C,D) where everyone also excludes across one way
// so no single loop exists but swaps do: forces the fallback path.
func TestDrawFallbackNoCycle(t *testing.T) {
	ids := []string{"a", "b", "c", "d"}
	// Only allowed edges (undirected): a-c, b-d. Single 4-cycle impossible,
	// but a↔c and b↔d works.
	allowedPairs := map[[2]string]bool{{"a", "c"}: true, {"c", "a"}: true, {"b", "d"}: true, {"d", "b"}: true}
	excluded := func(x, y string) bool { return !allowedPairs[[2]string{x, y}] }
	res, err := Draw(ids, excluded, rand.New(rand.NewPCG(1, 1)))
	if err != nil {
		t.Fatal(err)
	}
	checkValid(t, ids, res, excluded)
}

func TestDrawImpossible(t *testing.T) {
	ids := []string{"a", "b", "c"}
	excluded := func(x, y string) bool { return x == "a" || y == "a" }
	if _, err := Draw(ids, excluded, rand.New(rand.NewPCG(1, 1))); err != ErrNoValidPairing {
		t.Fatalf("expected ErrNoValidPairing, got %v", err)
	}
}
