package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"antigravity-proxy/internal/config"
	"antigravity-proxy/internal/oauth"
)

const defaultProjectID = "rising-fact-p41fc"

type Proxy struct {
	cfg    config.Config
	client *http.Client

	tokenMu        sync.Mutex
	cachedToken    string
	tokenExpiresAt time.Time

	projectMu sync.Mutex
	projectID string
}

type upstreamError struct {
	Status int
	Body   string
}

func (e *upstreamError) Error() string {
	return fmt.Sprintf("Antigravity returned HTTP %d: %s", e.Status, e.Body)
}

// New creates a Gemini upstream proxy with the supplied configuration.
func New(cfg config.Config) *Proxy {
	return &Proxy{
		cfg:       cfg,
		client:    &http.Client{Timeout: 5 * time.Minute},
		projectID: cfg.ProjectID,
	}
}

func (p *Proxy) accessToken(ctx context.Context) (string, error) {
	if p.cfg.AccessToken != "" {
		return p.cfg.AccessToken, nil
	}
	if p.cfg.RefreshToken == "" {
		return "", errors.New("no Google credential configured; run `antigravity-proxy login` or set ANTIGRAVITY_ACCESS_TOKEN")
	}

	p.tokenMu.Lock()
	defer p.tokenMu.Unlock()
	if p.cachedToken != "" && time.Until(p.tokenExpiresAt) > time.Minute {
		return p.cachedToken, nil
	}
	tokens, err := oauth.Refresh(ctx, p.cfg.RefreshToken)
	if err != nil {
		return "", err
	}
	if tokens.AccessToken == "" {
		return "", errors.New("Google OAuth refresh response contained no access token")
	}
	p.cachedToken = tokens.AccessToken
	expiresIn := tokens.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = 3600
	}
	p.tokenExpiresAt = time.Now().Add(time.Duration(expiresIn) * time.Second)
	return p.cachedToken, nil
}

func (p *Proxy) endpoints() []string {
	if p.cfg.DailyEndpoint == p.cfg.ProdEndpoint {
		return []string{p.cfg.DailyEndpoint}
	}
	return []string{p.cfg.DailyEndpoint, p.cfg.ProdEndpoint}
}

func (p *Proxy) postToAntigravity(ctx context.Context, token, path, accept string, payload any) (*http.Response, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encode Antigravity request: %w", err)
	}
	var lastErr error
	for _, endpoint := range p.endpoints() {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+path, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		p.setUpstreamHeaders(req, token, accept)
		resp, err := p.client.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			lastErr = err
			continue
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return resp, nil
		}
		responseBody, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
		if readErr != nil {
			lastErr = readErr
		} else {
			lastErr = &upstreamError{Status: resp.StatusCode, Body: strings.TrimSpace(string(responseBody))}
		}
	}
	if lastErr == nil {
		lastErr = errors.New("no Antigravity endpoints configured")
	}
	return nil, lastErr
}

func (p *Proxy) setUpstreamHeaders(req *http.Request, token, accept string) {
	version := "1.15.8"
	if value := strings.TrimSpace(getenv("ANTIGRAVITY_CLIENT_VERSION")); value != "" {
		version = value
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", accept)
	req.Header.Set("User-Agent", fmt.Sprintf("antigravity/%s %s/%s", version, runtime.GOOS, runtime.GOARCH))
	req.Header.Set("X-Client-Name", "antigravity")
	req.Header.Set("X-Client-Version", version)
	req.Header.Set("x-goog-api-client", "gl-node/18.18.2 fire/0.8.6 grpc/1.10.x")
}

func getenv(name string) string {
	return strings.TrimSpace(strings.Trim(os.Getenv(name), "\x00"))
}

func (p *Proxy) getProjectID(ctx context.Context, token string) (string, error) {
	p.projectMu.Lock()
	defer p.projectMu.Unlock()
	if p.projectID != "" {
		return p.projectID, nil
	}
	projectID, err := p.discoverProject(ctx, token)
	if err != nil {
		return "", err
	}
	p.projectID = projectID
	return projectID, nil
}

func platformNumber() int {
	switch runtime.GOOS {
	case "darwin":
		if runtime.GOARCH == "arm64" {
			return 2
		}
		return 1
	case "linux":
		if runtime.GOARCH == "arm64" {
			return 4
		}
		return 3
	case "windows":
		return 5
	default:
		return 0
	}
}

func clientMetadata() map[string]any {
	return map[string]any{"ideType": 9, "platform": platformNumber(), "pluginType": 2}
}

func (p *Proxy) discoverProject(ctx context.Context, token string) (string, error) {
	metadata := clientMetadata()
	var lastErr error
	var loadResponse map[string]any
	for _, endpoint := range []string{p.cfg.ProdEndpoint, p.cfg.DailyEndpoint} {
		payload := map[string]any{"metadata": metadata, "mode": 1}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+"/v1internal:loadCodeAssist", mustJSON(payload))
		if err != nil {
			return "", err
		}
		p.setUpstreamHeaders(req, token, "application/json")
		resp, err := p.client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			_ = resp.Body.Close()
			lastErr = &upstreamError{Status: resp.StatusCode, Body: strings.TrimSpace(string(body))}
			continue
		}
		loadResponse = make(map[string]any)
		err = json.NewDecoder(resp.Body).Decode(&loadResponse)
		_ = resp.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}
		if id := projectFromLoad(loadResponse); id != "" {
			return id, nil
		}
		break
	}
	if loadResponse != nil {
		if id, err := p.onboardProject(ctx, token, loadResponse); err == nil && id != "" {
			return id, nil
		} else if err != nil {
			lastErr = err
		}
	}
	if lastErr != nil {
		return "", fmt.Errorf("discover Antigravity project: %w", lastErr)
	}
	return defaultProjectID, nil
}

func projectFromLoad(data map[string]any) string {
	value := data["cloudaicompanionProject"]
	if id, ok := value.(string); ok {
		return id
	}
	if project, ok := value.(map[string]any); ok {
		if id, ok := project["id"].(string); ok {
			return id
		}
	}
	return ""
}

func (p *Proxy) onboardProject(ctx context.Context, token string, load map[string]any) (string, error) {
	tierID := ""
	if tiers, ok := load["allowedTiers"].([]any); ok {
		for _, value := range tiers {
			if tier, ok := value.(map[string]any); ok {
				id, _ := tier["id"].(string)
				if id == "" {
					continue
				}
				if tierID == "" {
					tierID = id
				}
				if isDefault, _ := tier["isDefault"].(bool); isDefault {
					tierID = id
					break
				}
			}
		}
	}
	if tierID == "" {
		tierID = "free-tier"
	}
	payload := map[string]any{"tierId": tierID, "metadata": clientMetadata()}
	for _, endpoint := range p.endpoints() {
		for attempt := range 10 {
			resp, err := p.postToAntigravityEndpoint(ctx, endpoint, token, "/v1internal:onboardUser", "application/json", payload)
			if err != nil {
				break
			}
			var data map[string]any
			decodeErr := json.NewDecoder(resp.Body).Decode(&data)
			_ = resp.Body.Close()
			if decodeErr != nil {
				return "", decodeErr
			}
			if done, _ := data["done"].(bool); done {
				if response, ok := data["response"].(map[string]any); ok {
					if id := projectFromLoad(response); id != "" {
						return id, nil
					}
				}
				return "", nil
			}
			if attempt < 9 {
				timer := time.NewTimer(5 * time.Second)
				select {
				case <-ctx.Done():
					timer.Stop()
					return "", ctx.Err()
				case <-timer.C:
				}
			}
		}
	}
	return "", nil
}

func (p *Proxy) postToAntigravityEndpoint(ctx context.Context, endpoint, token, path, accept string, payload any) (*http.Response, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	p.setUpstreamHeaders(req, token, accept)
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
		return nil, &upstreamError{Status: resp.StatusCode, Body: strings.TrimSpace(string(body))}
	}
	return resp, nil
}

func mustJSON(value any) *bytes.Reader {
	data, _ := json.Marshal(value)
	return bytes.NewReader(data)
}
