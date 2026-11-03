package steadfast

import (
	"time"

	"github.com/nokibsarkar/go-courier/httpx"
)

func httpxRetry(n int) httpx.RetryPolicy {
	return httpx.RetryPolicy{MaxAttempts: n, BaseDelay: time.Millisecond, MaxDelay: 2 * time.Millisecond}
}
