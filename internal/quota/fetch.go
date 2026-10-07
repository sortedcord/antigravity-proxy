package quota

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

const summaryPath = "/v1internal:retrieveUserQuotaSummary"

// ErrAuthenticationBusy means the shared account cache is owned by another
// request. Polling must fail this attempt rather than wait for that request.
var ErrAuthenticationBusy = errors.New("quota authentication is busy")

// Transport connects quota fetching to the account's existing credentials,
// project cache, and HTTP transport. ProjectID must only inspect an immediately
// available cached or configured project; it must not discover or provision one.
// Post retains the account's unified identity and endpoint fallback.
// Errors may implement HTTPStatus() int.
type Transport struct {
	AccessToken func(context.Context) (string, error)
	ProjectID   func() string
	Post        func(ctx context.Context, token, path, accept string, payload any) (*http.Response, error)
}

// Fetcher obtains authoritative quota without owning OAuth or transport caches.
type Fetcher struct {
	transport Transport
}

// NewFetcher connects quota polling to a shared account transport.
func NewFetcher(transport Transport) *Fetcher {
	return &Fetcher{transport: transport}
}

// Fetch builds and parses one bounded quota request. Only credential-safe errors
// escape this boundary; cancellation remains recognizable by callers.
func (f *Fetcher) Fetch(ctx context.Context) (Snapshot, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	token, err := f.transport.AccessToken(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return Snapshot{}, ctx.Err()
		}
		if errors.Is(err, ErrAuthenticationBusy) {
			return Snapshot{}, ErrAuthenticationBusy
		}
		return Snapshot{}, errors.New("authenticate quota request failed")
	}
	payload := make(map[string]string, 1)
	if projectID := f.transport.ProjectID(); projectID != "" {
		payload["project"] = projectID
	}
	resp, err := f.transport.Post(ctx, token, summaryPath, "application/json", payload)
	if err != nil {
		if ctx.Err() != nil {
			return Snapshot{}, ctx.Err()
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return Snapshot{}, context.DeadlineExceeded
		}
		if errors.Is(err, context.Canceled) {
			return Snapshot{}, context.Canceled
		}
		var upstream interface{ HTTPStatus() int }
		if errors.As(err, &upstream) {
			return Snapshot{}, fmt.Errorf("quota request returned HTTP %d", upstream.HTTPStatus())
		}
		return Snapshot{}, errors.New("quota request failed")
	}
	defer resp.Body.Close()
	const maxQuotaSize = 8 << 20
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxQuotaSize+1))
	if err != nil {
		if ctx.Err() != nil {
			return Snapshot{}, ctx.Err()
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return Snapshot{}, context.DeadlineExceeded
		}
		if errors.Is(err, context.Canceled) {
			return Snapshot{}, context.Canceled
		}
		return Snapshot{}, errors.New("read quota response failed")
	}
	if len(body) > maxQuotaSize {
		return Snapshot{}, errors.New("quota response exceeds size limit")
	}
	return Parse(body, time.Now().UTC())
}
