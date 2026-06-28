package httpapi

import "testing"

func TestRateLimiterFixedWindow(t *testing.T) {
	rl := newRateLimiter(3)
	for i := 0; i < 3; i++ {
		if !rl.allow("1.2.3.4") {
			t.Fatalf("request %d should be allowed", i)
		}
	}
	if rl.allow("1.2.3.4") {
		t.Fatalf("4th request should be denied")
	}
	// A different key has its own budget.
	if !rl.allow("5.6.7.8") {
		t.Fatalf("different key should be allowed")
	}
}

func TestRateLimiterDisabled(t *testing.T) {
	rl := newRateLimiter(0)
	for i := 0; i < 1000; i++ {
		if !rl.allow("x") {
			t.Fatalf("disabled limiter must always allow")
		}
	}
}
