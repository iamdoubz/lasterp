package money

import "testing"

func amounts(parts []Money) []int64 {
	out := make([]int64, len(parts))
	for i, p := range parts {
		out[i] = p.Amount()
	}
	return out
}

func TestAllocateEqualSplit(t *testing.T) {
	// $1.00 three ways: the odd cent goes to the first part (tie, lowest index).
	parts, err := mustNew(t, 100, "USD").AllocateEqual(3)
	if err != nil {
		t.Fatalf("AllocateEqual: %v", err)
	}
	if got := amounts(parts); !equalInts(got, []int64{34, 33, 33}) {
		t.Fatalf("got %v, want [34 33 33]", got)
	}
}

func TestAllocateByRatio(t *testing.T) {
	parts, err := mustNew(t, 100, "USD").Allocate([]int64{1, 1, 2})
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	if got := amounts(parts); !equalInts(got, []int64{25, 25, 50}) {
		t.Fatalf("got %v, want [25 25 50]", got)
	}
}

func TestAllocateNegative(t *testing.T) {
	parts, err := mustNew(t, -100, "USD").AllocateEqual(3)
	if err != nil {
		t.Fatalf("AllocateEqual: %v", err)
	}
	if got := amounts(parts); !equalInts(got, []int64{-34, -33, -33}) {
		t.Fatalf("got %v, want [-34 -33 -33]", got)
	}
}

func TestAllocateErrors(t *testing.T) {
	m := mustNew(t, 100, "USD")
	if _, err := m.Allocate(nil); err == nil {
		t.Fatal("Allocate(nil): want error")
	}
	if _, err := m.Allocate([]int64{1, -1}); err == nil {
		t.Fatal("Allocate with negative ratio: want error")
	}
	if _, err := m.Allocate([]int64{0, 0}); err == nil {
		t.Fatal("Allocate with zero-sum ratios: want error")
	}
	if _, err := m.AllocateEqual(0); err == nil {
		t.Fatal("AllocateEqual(0): want error")
	}
}

func equalInts(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
