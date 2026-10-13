package oauth

import "time"

const (
	defaultTokenLifetime = time.Hour
	// DefaultHTTPTimeout bounds token exchange and authenticated profile calls.
	DefaultHTTPTimeout      = 30 * time.Second
	defaultUserInfoTimeout  = 15 * time.Second
	callbackShutdownTimeout = 5 * time.Second
)

// Expiry converts a provider lifetime without overflowing time.Duration. Missing
// or unusable lifetimes use the consumer's conventional one-hour lifetime.
// Known absolute expiry times must be copied directly, not passed through here.
func (tokens Tokens) Expiry(now time.Time) time.Time {
	const maxSeconds = int64((1<<63 - 1) / time.Second)
	if tokens.ExpiresIn <= 0 || tokens.ExpiresIn > maxSeconds {
		return now.Add(defaultTokenLifetime)
	}
	return now.Add(time.Duration(tokens.ExpiresIn) * time.Second)
}
