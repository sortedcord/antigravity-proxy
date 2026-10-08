package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"antigravity-proxy/internal/config"
)

func generationTestProxy(endpoint string) *Proxy {
	return New(config.Config{
		AccessToken: "google-token", ProjectID: "project-id",
		DailyEndpoint: endpoint, ProdEndpoint: endpoint,
	})
}

func decodeGenerationJSON(t *testing.T, data []byte) any {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		t.Fatalf("decode JSON: %v", err)
	}
	return value
}

func TestGenerateContentPreservesCallerPolicyAndNativeResponse(t *testing.T) {
	const requestBody = `{
		"contents":[{"role":"model","parts":[{"thought":true,"thoughtSignature":"opaque+/==","text":"earlier"},{"functionCall":{"name":"native_tool","args":{"large":9007199254740993}}}]}],
		"systemInstruction":{"parts":[{"text":"Caller policy <not injected>"}]},
		"generationConfig":{"maxOutputTokens":123456789,"thinkingConfig":{"thinkingBudget":0,"includeThoughts":true},"temperature":0,"responseMimeType":"application/json","responseJsonSchema":{"type":["object","null"],"properties":{"x":{"anyOf":[{"type":"number"},{"type":"string"}]}},"additionalProperties":false}},
		"tools":[{"functionDeclarations":[{"name":"native_tool","parametersJsonSchema":{"type":"object","properties":{"x":{"type":["number","null"]}}}}]}],
		"toolConfig":{"functionCallingConfig":{"mode":"ANY","allowedFunctionNames":["native_tool"]}},
		"safetySettings":[{"category":"HARM_CATEGORY_HATE_SPEECH","threshold":"BLOCK_NONE"}],
		"futureField":{"integer":9007199254740993,"nested":[null,false,"unchanged"]}
	}`
	const inner = `{"candidates":[{"content":{"role":"model","parts":[{"thoughtSignature":"signature-only"},{"inlineData":{"mimeType":"image/png","data":"opaque-data"}}]},"finishReason":"STOP","futureCandidate":9007199254740993}],"usageMetadata":{"thoughtsTokenCount":17},"futureResponse":{"a":true}}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1internal:generateContent" || r.URL.RawQuery != "" || r.Method != http.MethodPost {
			t.Errorf("upstream route = %s %s", r.Method, r.URL)
		}
		if r.Header.Get("Accept") != "application/json" || r.Header.Get("Authorization") != "Bearer google-token" {
			t.Errorf("upstream headers = %v", r.Header)
		}
		var envelope map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&envelope); err != nil {
			t.Errorf("decode envelope: %v", err)
			return
		}
		if len(envelope) != 6 {
			t.Errorf("unexpected envelope fields: %v", envelope)
		}
		for field, expected := range map[string]string{"project": "project-id", "model": "future-native-model", "userAgent": "antigravity", "requestType": "agent"} {
			var got string
			_ = json.Unmarshal(envelope[field], &got)
			if got != expected {
				t.Errorf("%s = %q, want %q", field, got, expected)
			}
		}
		var requestID string
		_ = json.Unmarshal(envelope["requestId"], &requestID)
		if !strings.HasPrefix(requestID, "agent-") || len(requestID) != len("agent-")+32 {
			t.Errorf("requestId = %q", requestID)
		}
		got := decodeGenerationJSON(t, envelope["request"])
		want := decodeGenerationJSON(t, []byte(requestBody))
		// This backend key rename is the sole documented request adaptation.
		wantConfig := want.(map[string]any)["generationConfig"].(map[string]any)
		wantConfig["responseSchema"] = wantConfig["responseJsonSchema"]
		delete(wantConfig, "responseJsonSchema")
		if !reflect.DeepEqual(got, want) {
			t.Errorf("caller policy changed beyond schema-key adaptation: got %#v, want %#v", got, want)
		}
		_, _ = io.WriteString(w, `{"response":`+inner+`,"traceId":"transport-only"}`)
	}))
	defer upstream.Close()
	recorder := httptest.NewRecorder()
	generationTestProxy(upstream.URL).handleGenerateContent(recorder, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(requestBody)), "future-native-model", false)
	if recorder.Code != http.StatusOK || recorder.Body.String() != inner {
		t.Fatalf("response = %d %s, want unchanged native object", recorder.Code, recorder.Body.String())
	}
}

func TestGenerateContentRequestShapeAndLimits(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.WriteString(w, `{"error":{"code":400,"message":"contents required","status":"INVALID_ARGUMENT"}}`)
	}))
	defer upstream.Close()
	proxy := generationTestProxy(upstream.URL)
	for _, body := range []string{"", "null", "[]", `"text"`, "12", "true", "{", `{} {}`, "{} trailing"} {
		t.Run(body, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			proxy.handleGenerateContent(recorder, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body)), "model", false)
			if recorder.Code != http.StatusBadRequest || !json.Valid(recorder.Body.Bytes()) {
				t.Fatalf("invalid request = %d %s", recorder.Code, recorder.Body.String())
			}
		})
	}
	if calls.Load() != 0 {
		t.Fatalf("invalid bodies reached upstream %d times", calls.Load())
	}
	recorder := httptest.NewRecorder()
	proxy.handleGenerateContent(recorder, httptest.NewRequest(http.MethodPost, "/", strings.NewReader("{} \n\t")), "model", false)
	if calls.Load() != 1 || !strings.Contains(recorder.Body.String(), "contents required") {
		t.Fatalf("empty object was not delegated: calls=%d, body=%s", calls.Load(), recorder.Body.String())
	}
	recorder = httptest.NewRecorder()
	oversized := io.MultiReader(strings.NewReader(`{"large":"`), strings.NewReader(strings.Repeat("x", maxGenerationRequestSize)), strings.NewReader(`"}`))
	proxy.handleGenerateContent(recorder, httptest.NewRequest(http.MethodPost, "/", oversized), "model", false)
	if recorder.Code != http.StatusRequestEntityTooLarge || calls.Load() != 1 {
		t.Fatalf("oversized body = %d, upstream calls=%d", recorder.Code, calls.Load())
	}
}

func TestUnwrapGenerationResponseOnlyRemovesTransportBoundary(t *testing.T) {
	for _, body := range []string{
		`{}`,
		`{"usageMetadata":{"totalTokenCount":9007199254740993},"future":true}`,
		`{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","message":"quota"}}`,
		`{"candidates":[{"content":{"parts":[{"thoughtSignature":"opaque"}]}}]}`,
	} {
		got, err := unwrapGenerationResponse([]byte(body))
		if err != nil || string(got) != body {
			t.Fatalf("bare response changed: %s, %v", got, err)
		}
		got, err = unwrapGenerationResponse([]byte(`{"response":` + body + `,"ignoredTransport":true}`))
		if err != nil || string(got) != body {
			t.Fatalf("wrapped response changed: %s, %v", got, err)
		}
	}
	for _, body := range []string{"null", "[]", `{"response":null}`, `{"response":[]}`, `{"response":12}`, `{} {}`, `{"response":{}`} {
		if _, err := unwrapGenerationResponse([]byte(body)); err == nil {
			t.Errorf("accepted invalid response %s", body)
		}
	}
}

type fragmentedGenerationReader struct {
	reader io.Reader
}

func (r fragmentedGenerationReader) Read(p []byte) (int, error) {
	if len(p) > 7 {
		p = p[:7]
	}
	return r.reader.Read(p)
}

func TestGenerationSSELargeFragmentedMultilineAndFinalEvent(t *testing.T) {
	inline := strings.Repeat("a", 256<<10)
	const signature = `{"candidates":[{"content":{"parts":[{"thoughtSignature":"opaque+/=="}]}}]}`
	const finish = `{"candidates":[{"finishReason":"STOP"}]}`
	const usage = `{"usageMetadata":{"totalTokenCount":9007199254740993},"future":true}`
	const nativeError = `{"error":{"code":429,"message":"quota","status":"RESOURCE_EXHAUSTED","details":[{"future":true}]}}`
	prettySignature := strings.ReplaceAll(signature, "{", "{\n  ")
	prettySignature = strings.ReplaceAll(prettySignature, "\n", "\r\ndata: ")
	wire := ": heartbeat\r\n\r\nid: 123\r\nevent: message\r\ndata: {\r\ndata: \"response\": " + prettySignature + ",\r\ndata: \"traceId\": \"outer\"}\r\n\r\n" +
		"data: {\"response\":{\"candidates\":[{\"content\":{\"parts\":[{\"inlineData\":{\"mimeType\":\"image/png\",\"data\":\"" + inline + "\"}}]}}]}}\n\n" +
		"data: {\"response\":" + finish + "}\n\n" + "data: " + nativeError + "\n\n" + "data: [DONE]\n\n" + "data: {\"response\":" + usage + "}"
	recorder := httptest.NewRecorder()
	forwardGenerationStream(recorder, fragmentedGenerationReader{strings.NewReader(wire)})
	if recorder.Code != http.StatusOK || recorder.Header().Get("Content-Type") != "text/event-stream" || !recorder.Flushed {
		t.Fatalf("stream headers = %d %v, flushed=%v", recorder.Code, recorder.Header(), recorder.Flushed)
	}
	if !strings.HasPrefix(recorder.Body.String(), ": heartbeat\n\nid: 123\nevent: message\n") || strings.Contains(recorder.Body.String(), "[DONE]") || strings.Contains(recorder.Body.String(), "traceId") {
		t.Fatal("SSE framing or transport boundary changed unexpectedly")
	}
	dataLines := 0
	for _, line := range strings.Split(recorder.Body.String(), "\n") {
		if payload, found := strings.CutPrefix(line, "data: "); found {
			dataLines++
			if !json.Valid([]byte(payload)) {
				t.Fatalf("Bifrost data line is not complete JSON: %.100s", line)
			}
		}
	}
	if dataLines != 5 {
		t.Fatalf("data lines = %d, want one per native JSON event", dataLines)
	}
	reader := bufio.NewReader(recorder.Body)
	var values [][]byte
	for {
		event, err := readGenerationSSEEvent(reader)
		if event.hasData {
			if !json.Valid(event.data) {
				t.Fatalf("invalid emitted JSON: %.100s", event.data)
			}
			values = append(values, event.data)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(values) != 5 || string(values[0]) != signature || string(values[2]) != finish || string(values[3]) != nativeError || string(values[4]) != usage || !bytes.Contains(values[1], []byte(inline)) {
		t.Fatalf("SSE payloads not lossless: count=%d", len(values))
	}
}

type failingGenerationReader struct{}

func (failingGenerationReader) Read([]byte) (int, error) {
	return 0, errors.New("upstream interrupted")
}

func TestGenerationSSEFailureBoundaries(t *testing.T) {
	for name, reader := range map[string]io.Reader{
		"empty":                    strings.NewReader(""),
		"read failure":             failingGenerationReader{},
		"comment then malformed":   strings.NewReader(": comment\n\ndata: {broken}\n\n"),
		"invalid wrapped response": strings.NewReader("data: {\"response\":null}\n\n"),
		"done only":                strings.NewReader("data: [DONE]\n\n"),
	} {
		t.Run(name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			forwardGenerationStream(recorder, reader)
			if recorder.Code != http.StatusBadGateway || recorder.Header().Get("Content-Type") != "application/json" || !json.Valid(recorder.Body.Bytes()) {
				t.Fatalf("pre-event failure = %d %v %s", recorder.Code, recorder.Header(), recorder.Body.String())
			}
		})
	}
	for name, tail := range map[string]io.Reader{"malformed": strings.NewReader("data: invalid\n\n"), "read failure": failingGenerationReader{}} {
		t.Run("committed "+name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			defer func() {
				if got := recover(); got != http.ErrAbortHandler {
					t.Errorf("post-event failure panic = %v, want http.ErrAbortHandler", got)
				}
				if recorder.Body.String() != "data: {\"usageMetadata\":{}}\n\n" {
					t.Errorf("manufactured stream error or end: %q", recorder.Body.String())
				}
			}()
			forwardGenerationStream(recorder, io.MultiReader(strings.NewReader("data: {\"response\":{\"usageMetadata\":{}}}\n\n"), tail))
		})
	}
}

func TestGenerationSSEDeliversBeforeUpstreamClosesAndCancels(t *testing.T) {
	upstreamStarted := make(chan struct{})
	upstreamCanceled := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1internal:streamGenerateContent" || r.URL.Query().Get("alt") != "sse" || r.Header.Get("Accept") != "text/event-stream" {
			t.Errorf("stream request = %s, accept=%q", r.URL, r.Header.Get("Accept"))
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"response\":{\"candidates\":[{\"content\":{\"parts\":[{\"thoughtSignature\":\"first\"}]}}]}}\n\n")
		w.(http.Flusher).Flush()
		close(upstreamStarted)
		<-r.Context().Done()
		close(upstreamCanceled)
	}))
	defer upstream.Close()
	proxy := generationTestProxy(upstream.URL)
	downstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxy.handleGenerateContent(w, r, "model-without-thinking-heuristics", true)
	}))
	defer downstream.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, downstream.URL, strings.NewReader(`{"contents":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	event, err := readGenerationSSEEvent(bufio.NewReader(response.Body))
	if err != nil || !bytes.Contains(event.data, []byte(`"thoughtSignature":"first"`)) {
		t.Fatalf("incremental event = %s, %v", event.data, err)
	}
	select {
	case <-upstreamStarted:
	case <-ctx.Done():
		t.Fatal("upstream did not signal event delivery")
	}
	select {
	case <-upstreamCanceled:
		t.Fatal("upstream already closed before event delivery")
	default:
	}
	cancel()
	select {
	case <-upstreamCanceled:
	case <-time.After(5 * time.Second):
		t.Fatal("downstream cancellation did not cancel upstream context")
	}
}

func TestGenerationUpstreamHTTPErrorDoesNotCommitSSE(t *testing.T) {
	const body = `{"error":{"code":429,"message":"native quota detail","status":"RESOURCE_EXHAUSTED","details":[{"reason":"QUOTA"}]}}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, body)
	}))
	defer upstream.Close()
	recorder := httptest.NewRecorder()
	generationTestProxy(upstream.URL).handleGenerateContent(recorder, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{}`)), "model", true)
	if recorder.Code != http.StatusTooManyRequests || recorder.Header().Get("Content-Type") != "application/json" || strings.Contains(recorder.Body.String(), "native quota detail") {
		t.Fatalf("upstream error = %d %v %s", recorder.Code, recorder.Header(), recorder.Body.String())
	}
	assertCatalogTestError(t, recorder, http.StatusTooManyRequests, "RESOURCE_EXHAUSTED")
}

func TestGenerateContentRejectsInvalidUpstreamJSONBeforeCommit(t *testing.T) {
	for _, body := range []string{"", "null", "[]", "{} {}", `{"response":null}`, `{"response":{"candidates":`} {
		t.Run(body, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, body)
			}))
			defer upstream.Close()
			recorder := httptest.NewRecorder()
			generationTestProxy(upstream.URL).handleGenerateContent(recorder, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{}`)), "model", false)
			if recorder.Code != http.StatusBadGateway || recorder.Header().Get("Content-Type") != "application/json" || !json.Valid(recorder.Body.Bytes()) {
				t.Fatalf("invalid upstream response = %d %v %s", recorder.Code, recorder.Header(), recorder.Body.String())
			}
		})
	}
}

func TestGenerateContentDoesNotDefaultOrClampThinking(t *testing.T) {
	for _, body := range []string{
		`{"contents":[]}`,
		`{"generationConfig":{"thinkingConfig":{"thinkingBudget":-1}}}`,
		`{"generationConfig":{"thinkingConfig":{"thinkingBudget":9007199254740993,"includeThoughts":false},"maxOutputTokens":0}}`,
	} {
		t.Run(body, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var envelope generationEnvelope
				if err := json.NewDecoder(r.Body).Decode(&envelope); err != nil {
					t.Errorf("decode generation envelope: %v", err)
					return
				}
				encoded, err := json.Marshal(envelope.Request)
				if err != nil {
					t.Errorf("encode preserved request: %v", err)
					return
				}
				if got, want := decodeGenerationJSON(t, encoded), decodeGenerationJSON(t, []byte(body)); !reflect.DeepEqual(got, want) {
					t.Errorf("defaulted or clamped request: got %#v, want %#v", got, want)
				}
				_, _ = io.WriteString(w, `{"usageMetadata":{"totalTokenCount":0}}`)
			}))
			defer upstream.Close()
			recorder := httptest.NewRecorder()
			generationTestProxy(upstream.URL).handleGenerateContent(recorder, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body)), "opaque-native-model", false)
			if recorder.Code != http.StatusOK || recorder.Body.String() != `{"usageMetadata":{"totalTokenCount":0}}` {
				t.Fatalf("bare native response = %d %s", recorder.Code, recorder.Body.String())
			}
		})
	}
}

func TestGenerateContentAdaptsOnlyModernSchemaKey(t *testing.T) {
	const schema = `{"type":["object","null"],"$schema":"https://json-schema.org/draft/2020-12/schema","properties":{"x":{"anyOf":[{"type":"number","minimum":9007199254740993},{"type":"string","pattern":"^[a-z]+$"}]}},"additionalProperties":false,"unevaluatedProperties":false,"futureConstraint":{"opaque":"signature+/==","limit":9007199254740993}}`
	for _, test := range []struct {
		name string
		key  string
	}{
		{"modern", "responseJsonSchema"},
		{"explicit legacy", "responseSchema"},
	} {
		for _, stream := range []bool{false, true} {
			name := test.name + " nonstream"
			if stream {
				name = test.name + " stream"
			}
			t.Run(name, func(t *testing.T) {
				body := `{"contents":[],"futureTopLevel":9007199254740993,"generationConfig":{"responseMimeType":"application/json","temperature":0,"futureConfig":{"nested":true},"` + test.key + `":` + schema + `}}`
				want := `{"contents":[],"futureTopLevel":9007199254740993,"generationConfig":{"responseMimeType":"application/json","temperature":0,"futureConfig":{"nested":true},"responseSchema":` + schema + `}}`
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					var envelope generationEnvelope
					if err := json.NewDecoder(r.Body).Decode(&envelope); err != nil {
						t.Errorf("decode envelope: %v", err)
						return
					}
					encoded, err := json.Marshal(envelope.Request)
					if err != nil {
						t.Errorf("encode request: %v", err)
						return
					}
					if got, expected := decodeGenerationJSON(t, encoded), decodeGenerationJSON(t, []byte(want)); !reflect.DeepEqual(got, expected) {
						t.Errorf("schema or other caller fields changed: got %#v, want %#v", got, expected)
					}
					if stream {
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = io.WriteString(w, "data: {\"response\":{\"candidates\":[{\"finishReason\":\"STOP\"}]}}\n\ndata: {\"response\":{\"usageMetadata\":{\"totalTokenCount\":0}}}\n\n")
					} else {
						_, _ = io.WriteString(w, `{"response":{"usageMetadata":{"totalTokenCount":0}}}`)
					}
				}))
				defer upstream.Close()
				recorder := httptest.NewRecorder()
				generationTestProxy(upstream.URL).handleGenerateContent(recorder, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body)), "native-model", stream)
				if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"totalTokenCount":0`) {
					t.Fatalf("generation response = %d %s", recorder.Code, recorder.Body.String())
				}
			})
		}
	}
}

func TestGenerateContentRejectsConflictingSchemaFields(t *testing.T) {
	for _, body := range []string{
		`{"generationConfig":{"responseJsonSchema":{},"responseSchema":{}}}`,
		`{"generationConfig":{"responseJsonSchema":null,"responseSchema":{}}}`,
		`{"generationConfig":{"responseJsonSchema":{},"responseSchema":null}}`,
	} {
		for _, stream := range []bool{false, true} {
			recorder := httptest.NewRecorder()
			// Conflicts must be rejected before discovery or any upstream transport is attempted.
			New(config.Config{AccessToken: "mock-token"}).handleGenerateContent(recorder, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body)), "native-model", stream)
			if recorder.Code != http.StatusBadRequest || recorder.Header().Get("Content-Type") != "application/json" || !json.Valid(recorder.Body.Bytes()) || !strings.Contains(recorder.Body.String(), "mutually exclusive") {
				t.Fatalf("conflicting schema fields = %d %v %s", recorder.Code, recorder.Header(), recorder.Body.String())
			}
		}
	}
}

func TestGenerationSSECompletionProofHTTPRuntime(t *testing.T) {
	event := func(native string) string {
		return "data: {\"response\":" + native + "}\n\n"
	}
	const partial = `{"candidates":[{"content":{"parts":[{"text":"partial"}]}}]}`
	const signature = `{"candidates":[{"content":{"parts":[{"thoughtSignature":"opaque+/=="}]}}]}`
	const finish = `{"candidates":[{"finishReason":"STOP"}]}`
	const usage = `{"usageMetadata":{"totalTokenCount":9007199254740993}}`
	const nativeError = `{"error":{"code":429,"message":"quota","status":"RESOURCE_EXHAUSTED"}}`
	const blocked = `{"promptFeedback":{"blockReason":"SAFETY","futureFeedback":true}}`
	for _, test := range []struct {
		name        string
		wire        string
		wantAbort   bool
		wantPayload string
	}{
		{"clean EOF after nonterminal content", event(partial), true, partial},
		{"clean EOF after signature only", event(signature), true, signature},
		{"DONE before terminal", event(partial) + "data: [DONE]\n\n" + event(finish), true, partial},
		{"two explicit candidates one unfinished", event(`{"candidates":[{"index":0,"content":{"parts":[{"text":"a"}]}},{"index":1,"content":{"parts":[{"text":"b"}]}}]}`) + event(finish), true, finish},
		{"two implicit candidates one unfinished", event(`{"candidates":[{"content":{"parts":[{"text":"a"}]}},{"content":{"parts":[{"text":"b"}]}}]}`) + event(finish), true, finish},
		{"usage only has no terminal proof", event(usage), true, usage},
		{"empty finish reason is not terminal", event(partial) + event(`{"candidates":[{"finishReason":""}]}`), true, `"finishReason":""`},
		{"blocked prompt does not finish started candidate", event(partial) + event(blocked), true, blocked},
		{"null native error is not terminal", event(partial) + event(`{"error":null}`), true, `{"error":null}`},
		{"native finish followed by usage", event(partial) + event(finish) + event(usage), false, usage},
		{"signature followed by native finish", event(signature) + event(finish), false, signature},
		{"explicit index native finish", event(`{"candidates":[{"index":7,"content":{"parts":[{"text":"seven"}]}}]}`) + event(`{"candidates":[{"index":7,"finishReason":"MAX_TOKENS"}]}`) + event(usage), false, usage},
		{"two explicit candidates both finished separately", event(`{"candidates":[{"index":3,"content":{"parts":[{"text":"three"}]}},{"index":8,"content":{"parts":[{"text":"eight"}]}}]}`) + event(`{"candidates":[{"index":8,"finishReason":"STOP"}]}`) + event(`{"candidates":[{"index":3,"finishReason":"STOP"}]}`), false, `"index":3,"finishReason":"STOP"`},
		{"two implicit candidates both finished", event(`{"candidates":[{"content":{"parts":[{"text":"a"}]}},{"content":{"parts":[{"text":"b"}]}}]}`) + event(`{"candidates":[{"finishReason":"STOP"},{"finishReason":"SAFETY"}]}`), false, `"finishReason":"SAFETY"`},
		{"DONE after terminal preserves later usage", event(partial) + event(finish) + "data: [DONE]\n\n" + event(usage), false, usage},
		{"native error without generation", event(nativeError), false, nativeError},
		{"native error after partial generation", event(partial) + event(nativeError), false, nativeError},
		{"native blocked prompt without generation", event(blocked) + event(usage), false, blocked},
		{"native blocked prompt then DONE", event(blocked) + "data: [DONE]", false, blocked},
		{"finished candidate then final unterminated DONE", event(partial) + event(finish) + "data: [DONE]", false, finish},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				forwardGenerationStream(w, strings.NewReader(test.wire))
			}))
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			request, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
			if err != nil {
				t.Fatal(err)
			}
			response, err := server.Client().Do(request)
			if err != nil {
				t.Fatalf("read committed stream headers: %v", err)
			}
			defer response.Body.Close()
			body, readErr := io.ReadAll(response.Body)
			if test.wantAbort {
				if !errors.Is(readErr, io.ErrUnexpectedEOF) {
					t.Fatalf("incomplete native stream ended successfully: body=%s, error=%v", body, readErr)
				}
			} else if readErr != nil {
				t.Fatalf("complete native outcome was aborted: %s, %v", body, readErr)
			}
			if response.StatusCode != http.StatusOK || response.Header.Get("Content-Type") != "text/event-stream" || !bytes.Contains(body, []byte(test.wantPayload)) {
				t.Fatalf("forwarded stream = %d %v %s", response.StatusCode, response.Header, body)
			}
			if bytes.Contains(body, []byte("[DONE]")) || bytes.Contains(body, []byte(`"response":`)) {
				t.Fatalf("transport marker or wrapper leaked: %s", body)
			}
			if test.name == "native finish followed by usage" && bytes.Index(body, []byte(`"finishReason"`)) > bytes.Index(body, []byte(`"usageMetadata"`)) {
				t.Fatalf("usage moved ahead of terminal event: %s", body)
			}
		})
	}
}
