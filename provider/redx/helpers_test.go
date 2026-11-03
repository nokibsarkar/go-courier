package redx

import (
	"time"

	"github.com/nokibsarkar/go-courier/httpx"
)

func retryFast() httpx.RetryPolicy {
	return httpx.RetryPolicy{MaxAttempts: 3, BaseDelay: time.Millisecond, MaxDelay: 2 * time.Millisecond}
}
