package admin

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRateLimiter_Allow(t *testing.T) {
	t.Parallel()
	rl := NewRateLimiter(1, 3) // 1 req/s, burst 3

	require.True(t, rl.Allow(), "burst token 1")
	require.True(t, rl.Allow(), "burst token 2")
	require.True(t, rl.Allow(), "burst token 3")
	require.False(t, rl.Allow(), "no tokens left")
}

func TestRateLimiter_Refill(t *testing.T) {
	rl := NewRateLimiter(100, 1) // 100 req/s, burst 1

	require.True(t, rl.Allow(), "initial token")
	rl.UpdateRate(0, 1)
	require.False(t, rl.Allow(), "no tokens")

	// Re-enable refill after the empty-bucket assertion so scheduler delays
	// cannot refill the token between the two immediate checks above.
	rl.UpdateRate(100, 1)
	require.Eventually(t, rl.Allow, 500*time.Millisecond, 10*time.Millisecond, "refilled token")
}

func TestRateLimiter_UpdateRate(t *testing.T) {
	rl := NewRateLimiter(1, 10)

	// Drain most tokens
	for i := 0; i < 8; i++ {
		rl.Allow()
	}

	// Update to lower burst — tokens should be capped
	rl.UpdateRate(1, 2)
	require.Equal(t, float64(2), rl.tokens, "tokens capped to new max")

	// Should have exactly 2 tokens minus any refill since UpdateRate
	require.True(t, rl.Allow())
	require.True(t, rl.Allow())
}

func TestRateLimiter_Concurrent(t *testing.T) {
	rl := NewRateLimiter(0, 100)

	allowed := make(chan bool, 200)
	for i := 0; i < 200; i++ {
		go func() {
			allowed <- rl.Allow()
		}()
	}

	var allowedCount int
	for i := 0; i < 200; i++ {
		if <-allowed {
			allowedCount++
		}
	}

	// Refill behavior is covered separately; this test isolates concurrent burst accounting.
	require.Equal(t, 100, allowedCount, "concurrent allows should consume each burst token once")
}
