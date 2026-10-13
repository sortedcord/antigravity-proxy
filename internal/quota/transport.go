package quota

import (
	"context"
	"errors"
	"net/http"
)

func (f *Fetcher) postSummary(ctx context.Context, token, project string) (*http.Response, error) {
	if f.transport.PostSummary != nil {
		return f.transport.PostSummary(ctx, token, project)
	}
	if f.transport.Post == nil {
		return nil, errors.New("quota transport is not configured")
	}
	payload := make(map[string]string, 1)
	if project != "" {
		payload["project"] = project
	}
	return f.transport.Post(ctx, token, SummaryPath, "application/json", payload)
}

func cancellationError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	return nil
}
