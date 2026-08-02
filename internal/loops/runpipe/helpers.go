package runpipe

import (
	"encoding/json"
	"time"
)

func MustMarshalJSON(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return string(encoded)
}

func StringPtr(value string) *string { return &value }

func OptionalString(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func IsQueueRetryEligible(kind QueueFailureKind) bool {
	return kind == FailureRetryableTransient || kind == FailureRetryableAfterResume || kind == FailureNonRetryable
}

func ShouldRetryQueueFailure(kind QueueFailureKind, nextAttempts, maxAttempts int64) bool {
	if !IsQueueRetryEligible(kind) {
		return false
	}
	if maxAttempts < 0 {
		return kind != FailureNonRetryable
	}
	return maxAttempts > 0 && nextAttempts < maxAttempts
}

func CappedRetryDelayAttempt(attempts, maxAttempts int64) int64 {
	if attempts <= 0 {
		return 1
	}
	if maxAttempts > 0 && attempts > maxAttempts {
		return maxAttempts
	}
	return attempts
}

func BackoffDelayExponential(base time.Duration, attempts int64) time.Duration {
	delay := base
	for i := int64(1); i < attempts; i++ {
		if delay >= MaxRetryDelay || delay > MaxRetryDelay/2 {
			return MaxRetryDelay
		}
		delay *= 2
	}
	if delay > MaxRetryDelay {
		return MaxRetryDelay
	}
	return delay
}

func BackoffDelayLinear(base time.Duration, attempts int64) time.Duration {
	if attempts <= 0 {
		attempts = 1
	}
	delay := time.Duration(attempts) * base
	if delay > MaxRetryDelay {
		return MaxRetryDelay
	}
	return delay
}
