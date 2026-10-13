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
	"time"
)

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

func (p *Proxy) endpoints() []string {
	if p.cfg.DailyEndpoint == p.cfg.ProdEndpoint {
		return []string{p.cfg.DailyEndpoint}
	}
	return []string{p.cfg.DailyEndpoint, p.cfg.ProdEndpoint}
}

// upstreamCall makes replay, project invalidation and timeout ownership explicit.
// Finite JSON RPCs use a total deadline; generation uses header/body inactivity
// deadlines so a healthy stream can remain active for longer than one RPC.
type upstreamCall struct {
	path    string
	accept  string
	project string
	body    func() io.Reader
	length  int64
	finite  bool
}

func (p *Proxy) postJSON(ctx context.Context, token, path, project string, payload any) (*http.Response, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, errors.New("encode Antigravity request failed")
	}
	return p.post(ctx, token, jsonUpstreamCall(path, "application/json", project, body))
}

func jsonUpstreamCall(path, accept, project string, body []byte) upstreamCall {
	return upstreamCall{
		path: path, accept: accept, project: project, finite: true,
		body: func() io.Reader { return bytes.NewReader(body) }, length: int64(len(body)),
	}
}

func (p *Proxy) post(ctx context.Context, token string, call upstreamCall) (*http.Response, error) {
	cancel := func() {}
	if call.finite {
		ctx, cancel = context.WithTimeout(ctx, p.finiteRPCTimeout())
	}
	transferred := false
	defer func() {
		if !transferred {
			cancel()
		}
	}()
	revision := p.projectRevision.Load()
	var lastErr error
	for _, endpoint := range p.endpoints() {
		resp, err := p.postEncoded(ctx, endpoint, token, call)
		if err == nil {
			if call.finite {
				resp.Body = &cancelResponseBody{ReadCloser: resp.Body, cancel: cancel}
			}
			transferred = true
			return resp, nil
		}
		lastErr = err
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
	// Only a terminal project-scoped failure invalidates the cache. Daily failure
	// followed by a healthy production response must retain the project.
	var upstream *upstreamError
	if call.project != "" && errors.As(lastErr, &upstream) && upstream.projectFailure {
		p.invalidateProject(call.project, revision)
	}
	return nil, lastErr
}

func (p *Proxy) finiteRPCTimeout() time.Duration {
	if p.rpcTimeout > 0 {
		return p.rpcTimeout
	}
	return defaultRPCTimeout
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

func (p *Proxy) postEncoded(ctx context.Context, endpoint, token string, call upstreamCall) (*http.Response, error) {
	requestCtx, cancel := context.WithCancel(ctx)
	transferred := false
	defer func() {
		if !transferred {
			cancel()
		}
	}()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, endpoint+call.path, call.body())
	if err != nil {
		return nil, errors.New("create Antigravity request failed")
	}
	req.ContentLength = call.length
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(call.body()), nil }
	p.setUpstreamHeaders(req, token, call.accept)
	recordRequestEndpoint(ctx, endpoint)
	started := time.Now()
	resp, err := p.client.Do(req)
	label := "daily"
	if endpoint != p.cfg.DailyEndpoint {
		label = "prod"
	}
	route, _, _ := strings.Cut(call.path, "?")
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
	if call.project != "" && (resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusNotFound) {
		timer := time.AfterFunc(100*time.Millisecond, cancel)
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, (16<<10)+1))
		timer.Stop()
		if readErr == nil && len(body) <= 16<<10 {
			projectFailure = rejectedProject(body, call.project, resp.StatusCode)
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
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", accept)
	req.Header.Set("User-Agent", fmt.Sprintf("antigravity/%s %s/%s", version, runtime.GOOS, runtime.GOARCH))
	req.Header.Set("X-Client-Name", "antigravity")
	req.Header.Set("X-Client-Version", version)
}
