package management

import "testing"

func TestNormalizeRoutingStrategyAcceptsSoonestReset(t *testing.T) {
	for _, input := range []string{"soonest-reset", "SoonestReset", " sr "} {
		got, ok := normalizeRoutingStrategy(input)
		if !ok || got != "soonest-reset" {
			t.Fatalf("normalizeRoutingStrategy(%q) = %q, %v", input, got, ok)
		}
	}
	if _, ok := normalizeRoutingStrategy("bogus"); ok {
		t.Fatal("bogus strategy accepted")
	}
}
