package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"antigravity-proxy/internal/config"
	"antigravity-proxy/internal/oauth"
)

// Proxy serves native Gemini requests and the raw Antigravity model catalog
// for one Google account. Its token and project caches support concurrent handlers.
type Proxy struct {
	cfg        config.Config
	client     *http.Client
	rpcTimeout time.Duration

	tokenMu        sync.Mutex
	cachedToken    string
	tokenExpiresAt time.Time

	projectMu       sync.Mutex
	projectID       string
	projectRevision atomic.Uint64
	projectFlight   *projectDiscovery

	generationSlots chan struct{}
	catalogMu       sync.Mutex
	catalog         modelCatalogCache
	catalogEpoch    uint64

	// StatusHandler is wired before serving; status owns its state and lifecycle.
	StatusHandler http.Handler
}

type upstreamError struct {
	Status         int
	Headers        http.Header
	projectFailure bool
}

// Error deliberately never retains provider bodies, URLs, or credentials.
func (e *upstreamError) Error() string {
	return fmt.Sprintf("Antigravity returned HTTP %d", e.Status)
}

func (e *upstreamError) HTTPStatus() int { return e.Status }

// New creates a Gemini upstream proxy with cfg without contacting Google.
// Wire StatusHandler before serving quota routes. Direct callers receive defaults
// for optional settings normally populated by config.Load.
func New(cfg config.Config) *Proxy {
	if cfg.ClientVersion == "" {
		cfg.ClientVersion = config.DefaultClientVersion
	}
	if cfg.MaxConcurrentGenerations <= 0 {
		cfg.MaxConcurrentGenerations = config.DefaultMaxConcurrentGenerations
	}
	return &Proxy{
		cfg:             cfg,
		client:          newUpstreamClient(5*time.Minute, 5*time.Minute),
		rpcTimeout:      5 * time.Minute,
		projectID:       cfg.ProjectID,
		generationSlots: make(chan struct{}, cfg.MaxConcurrentGenerations),
	}
}

// accessToken uses an explicit access token unchanged, or serializes refreshes
// and caches the result until shortly before its reported expiry.
func (p *Proxy) accessToken(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if p.cfg.AccessToken != "" {
		return p.cfg.AccessToken, nil
	}
	if p.cfg.RefreshToken == "" {
		return "", errors.New("no Google credential configured; run `antigravity-proxy login` or set ANTIGRAVITY_ACCESS_TOKEN")
	}
	p.tokenMu.Lock()
	defer p.tokenMu.Unlock()
	return p.accessTokenLocked(ctx)
}

// accessTokenLocked checks and refreshes the shared cache while tokenMu is held.
func (p *Proxy) accessTokenLocked(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if p.cachedToken != "" && time.Until(p.tokenExpiresAt) > time.Minute {
		return p.cachedToken, nil
	}
	tokens, err := oauth.Refresh(ctx, p.cfg)
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		slog.Warn("upstream authentication failed", "operation", "oauth_refresh")
		return "", errors.New("Google OAuth refresh failed")
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

// generationRequestBody provides replay without encoding the large raw request.
type generationRequestBody interface {
	requestBody() (func() io.Reader, int64, error)
}

// All account traffic uses the configured unified client identity.
func (p *Proxy) postToAntigravity(ctx context.Context, token, path, accept string, payload any) (*http.Response, error) {
	var factory func() io.Reader
	var length int64
	var err error
	streamedBody, generation := payload.(generationRequestBody)
	if generation {
		factory, length, err = streamedBody.requestBody()
	} else {
		var body []byte
		body, err = json.Marshal(payload)
		factory = func() io.Reader { return bytes.NewReader(body) }
		length = int64(len(body))
	}
	if err != nil {
		return nil, errors.New("encode Antigravity request failed")
	}
	cancel := func() {}
	if !generation {
		ctx, cancel = context.WithTimeout(ctx, p.finiteRPCTimeout())
	}
	// The response body owns successful requests' cancellation until EOF/Close.
	// Failed requests release the deadline here, after every fallback attempt.
	transferred := false
	defer func() {
		if !transferred {
			cancel()
		}
	}()
	project := payloadProject(payload)
	revision := p.projectRevision.Load()
	var lastErr error
	for _, endpoint := range p.endpoints() {
		resp, err := p.postEncoded(ctx, endpoint, token, path, accept, factory, length, project)
		if err == nil {
			if !generation {
				resp.Body = &cancelResponseBody{ReadCloser: resp.Body, cancel: cancel}
			}
			transferred = true
			return resp, nil
		}
		lastErr = err
		// A failed daily endpoint may not reject the cached project on production.
		// Only a terminal, explicitly project-scoped failure invalidates the cache.
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if !retryableUpstream(err) {
			break
		}
	}
	if lastErr == nil {
		lastErr = errors.New("no Antigravity endpoints configured")
	}
	var upstream *upstreamError
	if project != "" && errors.As(lastErr, &upstream) && upstream.projectFailure {
		p.invalidateProject(project, revision)
	}
	return nil, lastErr
}

func (p *Proxy) finiteRPCTimeout() time.Duration {
	if p.rpcTimeout > 0 {
		return p.rpcTimeout
	}
	return 5 * time.Minute
}

func payloadProject(payload any) string {
	switch value := payload.(type) {
	case generationEnvelope:
		return value.Project
	case map[string]string:
		return value["project"]
	case map[string]any:
		project, _ := value["project"].(string)
		return project
	default:
		return ""
	}
}

// Transport errors, 5xx, and 404 can indicate an unavailable endpoint. In
// particular 429 is terminal, preserving the first endpoint's backoff guidance.
func retryableUpstream(err error) bool {
	var upstream *upstreamError
	if errors.As(err, &upstream) {
		return upstream.Status == http.StatusNotFound || upstream.Status >= 500 && upstream.Status <= 599
	}
	var failure *upstreamTransportError
	return errors.As(err, &failure)
}

type upstreamTransportError struct{}

func (*upstreamTransportError) Error() string { return "Antigravity transport failed" }

func (p *Proxy) postEncoded(ctx context.Context, endpoint, token, path, accept string, factory func() io.Reader, length int64, project string) (*http.Response, error) {
	requestCtx, cancel := context.WithCancel(ctx)
	transferred := false
	defer func() {
		if !transferred {
			cancel()
		}
	}()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, endpoint+path, factory())
	if err != nil {
		return nil, errors.New("create Antigravity request failed")
	}
	req.ContentLength = length
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(factory()), nil }
	p.setUpstreamHeaders(req, token, accept)
	recordRequestEndpoint(ctx, endpoint)
	started := time.Now()
	resp, err := p.client.Do(req)
	label := "daily"
	if endpoint != p.cfg.DailyEndpoint {
		label = "prod"
	}
	route, _, _ := strings.Cut(path, "?")
	if err != nil {
		slog.Warn("upstream request failed", "endpoint", label, "route", route, "duration", time.Since(started), "failure", "transport")
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &upstreamTransportError{}
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		slog.Debug("upstream response", "endpoint", label, "route", route, "status", resp.StatusCode, "duration", time.Since(started))
		resp.Body = &cancelResponseBody{ReadCloser: resp.Body, cancel: cancel}
		transferred = true
		return resp, nil
	}
	// Inspect only a tiny, time-bounded structured error. Classification retains
	// a boolean, never provider text, resource identifiers or arbitrary metadata.
	projectFailure := false
	if project != "" && (resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusNotFound) {
		timer := time.AfterFunc(100*time.Millisecond, cancel)
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, (16<<10)+1))
		timer.Stop()
		if readErr == nil && len(body) <= 16<<10 {
			projectFailure = rejectedProject(body, project, resp.StatusCode)
		}
	}
	_ = resp.Body.Close()
	slog.Warn("upstream request rejected", "endpoint", label, "route", route, "status", resp.StatusCode, "duration", time.Since(started))
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return nil, &upstreamError{Status: resp.StatusCode, Headers: safeUpstreamHeaders(resp.Header), projectFailure: projectFailure}
}

// rejectedProject accepts only typed Google details that identify this project's
// resource and an explicit rejection reason. Human-readable messages are ignored.
func rejectedProject(body []byte, project string, status int) bool {
	var envelope struct {
		Error struct {
			Details []struct {
				Type         string            `json:"@type"`
				Reason       string            `json:"reason"`
				Domain       string            `json:"domain"`
				Metadata     map[string]string `json:"metadata"`
				ResourceType string            `json:"resourceType"`
				ResourceName string            `json:"resourceName"`
			} `json:"details"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &envelope) != nil {
		return false
	}
	matches := func(resource string) bool {
		return resource == project || resource == "projects/"+project || resource == "//cloudresourcemanager.googleapis.com/projects/"+project
	}
	for _, detail := range envelope.Error.Details {
		switch detail.Type {
		case "type.googleapis.com/google.rpc.ErrorInfo":
			if detail.Domain != "googleapis.com" && detail.Domain != "cloudresourcemanager.googleapis.com" && detail.Domain != "cloudcode-pa.googleapis.com" {
				continue
			}
			switch detail.Reason {
			case "PROJECT_NOT_FOUND", "PROJECT_DELETED", "PROJECT_INVALID", "PROJECT_DISABLED", "CONSUMER_INVALID", "CONSUMER_SUSPENDED":
				if matches(detail.Metadata["project"]) || matches(detail.Metadata["consumer"]) {
					return true
				}
			}
		case "type.googleapis.com/google.rpc.ResourceInfo":
			// Permission denial mentioning a Project does not invalidate its ID.
			if status == http.StatusNotFound && detail.ResourceType == "cloudresourcemanager.googleapis.com/Project" && matches(detail.ResourceName) {
				return true
			}
		}
	}
	return false
}

// safeUpstreamHeaders copies only narrowly validated metadata useful to clients.
// No cookies, authentication challenges, IDs, or arbitrary upstream values escape.
func safeUpstreamHeaders(source http.Header) http.Header {
	headers := make(http.Header)
	if value := strings.TrimSpace(source.Get("Retry-After")); value != "" {
		seconds, err := strconv.ParseUint(value, 10, 63)
		if err == nil {
			headers.Set("Retry-After", strconv.FormatUint(seconds, 10))
		} else if date, err := http.ParseTime(value); err == nil {
			headers.Set("Retry-After", date.UTC().Format(http.TimeFormat))
		}
	}
	if value := source.Get("Cache-Control"); value == "no-store" || value == "no-cache" {
		headers.Set("Cache-Control", value)
	}
	return headers
}

func (p *Proxy) setUpstreamHeaders(req *http.Request, token, accept string) {
	version := p.cfg.ClientVersion
	if version == "" {
		version = config.DefaultClientVersion
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", accept)
	req.Header.Set("User-Agent", fmt.Sprintf("antigravity/%s %s/%s", version, runtime.GOOS, runtime.GOARCH))
	req.Header.Set("X-Client-Name", "antigravity")
	req.Header.Set("X-Client-Version", version)
}

type projectDiscovery struct {
	done    chan struct{}
	id      string
	err     error
	waiters int
	cancel  context.CancelFunc
}

func (p *Proxy) getProjectID(ctx context.Context, token string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	p.projectMu.Lock()
	if p.cfg.ProjectID != "" {
		p.projectMu.Unlock()
		return p.cfg.ProjectID, nil
	}
	if p.projectID != "" {
		id := p.projectID
		p.projectMu.Unlock()
		return id, nil
	}
	flight := p.projectFlight
	var discoveryCtx context.Context
	if flight == nil {
		var cancel context.CancelFunc
		discoveryCtx, cancel = context.WithTimeout(context.WithoutCancel(ctx), p.finiteRPCTimeout())
		flight = &projectDiscovery{done: make(chan struct{}), cancel: cancel}
		p.projectFlight = flight
	}
	flight.waiters++
	p.projectMu.Unlock()
	if discoveryCtx != nil {
		go p.discoverProjectFlight(discoveryCtx, token, flight)
	}
	select {
	case <-flight.done:
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return flight.id, flight.err
	case <-ctx.Done():
		p.projectMu.Lock()
		flight.waiters--
		if flight.waiters == 0 && p.projectFlight == flight {
			p.projectFlight = nil
			flight.cancel()
		}
		p.projectMu.Unlock()
		return "", ctx.Err()
	}
}

func (p *Proxy) discoverProjectFlight(ctx context.Context, token string, flight *projectDiscovery) {
	defer flight.cancel()
	id, err := p.discoverProject(ctx, token)
	p.projectMu.Lock()
	if p.projectFlight == flight {
		if err == nil && id != "" {
			p.projectID = id
			p.projectRevision.Add(1)
		}
		p.projectFlight = nil
	}
	flight.id, flight.err = id, err
	close(flight.done)
	p.projectMu.Unlock()
}

func (p *Proxy) invalidateProject(project string, revision uint64) {
	p.projectMu.Lock()
	invalidated := p.projectID == project && p.projectRevision.Load() == revision
	if invalidated {
		if p.cfg.ProjectID == "" {
			p.projectID = ""
		}
		p.projectRevision.Add(1)
	}
	p.projectMu.Unlock()
	if invalidated {
		p.invalidateGeminiModelCatalog()
	}
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
	ctx, cancel := context.WithTimeout(ctx, p.finiteRPCTimeout())
	defer cancel()
	resp, err := p.postToAntigravity(ctx, token, "/v1internal:loadCodeAssist", "application/json", map[string]any{"metadata": clientMetadata(), "mode": 1})
	if err != nil {
		return "", err
	}
	var load map[string]any
	err = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&load)
	_ = resp.Body.Close()
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", errors.New("decode Antigravity project discovery response failed")
	}
	if id := projectFromLoad(load); id != "" {
		return id, nil
	}
	return p.onboardProject(ctx, token, load)
}

func projectFromLoad(data map[string]any) string {
	value := data["cloudaicompanionProject"]
	if id, ok := value.(string); ok {
		return strings.TrimSpace(id)
	}
	if project, ok := value.(map[string]any); ok {
		if id, ok := project["id"].(string); ok {
			return strings.TrimSpace(id)
		}
	}
	return ""
}

func (p *Proxy) onboardProject(ctx context.Context, token string, load map[string]any) (string, error) {
	return p.pollOnboarding(ctx, token, load, 10, 5*time.Second)
}

func (p *Proxy) pollOnboarding(ctx context.Context, token string, load map[string]any, attempts int, interval time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, p.finiteRPCTimeout())
	defer cancel()
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
	for attempt := range attempts {
		resp, err := p.postToAntigravity(ctx, token, "/v1internal:onboardUser", "application/json", payload)
		if err != nil {
			return "", err
		}
		var data map[string]any
		err = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&data)
		_ = resp.Body.Close()
		if err != nil {
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			return "", errors.New("decode Antigravity onboarding response failed")
		}
		if done, _ := data["done"].(bool); done {
			if response, ok := data["response"].(map[string]any); ok {
				if id := projectFromLoad(response); id != "" {
					return id, nil
				}
			}
			return "", errors.New("Antigravity onboarding completed without a project ID")
		}
		if attempt+1 < attempts {
			timer := time.NewTimer(interval)
			select {
			case <-ctx.Done():
				timer.Stop()
				return "", ctx.Err()
			case <-timer.C:
			}
		}
	}
	return "", errors.New("Antigravity onboarding did not complete within the polling limit")
}
