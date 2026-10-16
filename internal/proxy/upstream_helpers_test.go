package proxy

import (
	"antigravity-proxy/internal/testutil"
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

func awaitSurface(t *testing.T, done <-chan struct{}) {
	t.Helper()
	testutil.Await(t, done, "timed out waiting for HTTP request")
}

func (p *Proxy) postToAntigravity(ctx context.Context, token, path, accept string, payload any) (*http.Response, error) {
	if envelope, ok := payload.(generationEnvelope); ok {
		body, length, err := envelope.requestBody()
		if err != nil {
			return nil, err
		}
		return p.post(ctx, token, upstreamCall{path: path, accept: accept, project: envelope.Project, body: body, length: length})
	}
	project := ""
	switch value := payload.(type) {
	case map[string]string:
		project = value["project"]
	case map[string]any:
		project, _ = value["project"].(string)
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return p.post(ctx, token, jsonUpstreamCall(path, accept, project, body))
}
