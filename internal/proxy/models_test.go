package proxy

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"antigravity-proxy/internal/config"
)

type catalogTestList struct {
	Models        []map[string]json.RawMessage `json:"models"`
	NextPageToken string                       `json:"nextPageToken"`
}

func newCatalogTestProxy(t *testing.T, response func() string) *Proxy {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1internal:fetchAvailableModels" {
			t.Errorf("upstream request = %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer catalog-token" || r.Header.Get("Accept") != "application/json" {
			t.Errorf("upstream headers = %v", r.Header)
		}
		var body struct {
			Project string `json:"project"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Project != "catalog-project" {
			t.Errorf("upstream project = %q, decode error = %v", body.Project, err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, response())
	}))
	t.Cleanup(upstream.Close)
	return New(config.Config{
		AccessToken:   "catalog-token",
		ProjectID:     "catalog-project",
		DailyEndpoint: upstream.URL,
		ProdEndpoint:  upstream.URL,
	})
}

func catalogTestRequest(p *Proxy, path string) *httptest.ResponseRecorder {
	response := httptest.NewRecorder()
	p.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
	return response
}

// catalogTestClock drives the production TTL without sleeping or mutating
// cached entries. Install it before serving any requests.
func catalogTestClock(p *Proxy) func(time.Duration) {
	now := time.Now()
	var elapsed atomic.Int64
	p.catalog.clock = func() time.Time { return now.Add(time.Duration(elapsed.Load())) }
	return func(delta time.Duration) { elapsed.Add(int64(delta)) }
}

func readCatalogTestList(t *testing.T, p *Proxy, query string) catalogTestList {
	t.Helper()
	response := catalogTestRequest(p, "/v1beta/models"+query)
	if response.Code != http.StatusOK {
		t.Fatalf("list status = %d: %s", response.Code, response.Body.String())
	}
	if response.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("list content type = %q", response.Header().Get("Content-Type"))
	}
	var list catalogTestList
	if err := json.Unmarshal(response.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	return list
}

func catalogTestString(t *testing.T, model map[string]json.RawMessage, field string) string {
	t.Helper()
	var value string
	if err := json.Unmarshal(model[field], &value); err != nil {
		t.Fatalf("decode %s: %v", field, err)
	}
	return value
}

func catalogTestNames(t *testing.T, list catalogTestList) []string {
	t.Helper()
	names := make([]string, 0, len(list.Models))
	for _, model := range list.Models {
		names = append(names, catalogTestString(t, model, "name"))
	}
	return names
}

func assertCatalogTestError(t *testing.T, response *httptest.ResponseRecorder, code int, status string) {
	t.Helper()
	if response.Code != code {
		t.Fatalf("HTTP status = %d, want %d: %s", response.Code, code, response.Body.String())
	}
	var body struct {
		Error struct {
			Code    int    `json:"code"`
			Status  string `json:"status"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Code != code || body.Error.Status != status || body.Error.Message == "" {
		t.Fatalf("native error = %+v", body.Error)
	}
}

func TestGeminiCatalogSelectsMetadataAndPreservesLimits(t *testing.T) {
	const catalog = `{"models":{
		"future-native-id":{"apiProvider":"API_PROVIDER_GOOGLE_GEMINI","displayName":"Upstream Display","description":"Upstream description","maxTokens":9007199254740993,"maxOutputTokens":1729,"futureMetadata":{"enabled":true}},
		"another-native-id":{"apiProvider":"API_PROVIDER_GOOGLE_GEMINI"},
		"claude-example":{"apiProvider":"API_PROVIDER_ANTHROPIC_VERTEX","maxTokens":1234},
		"openai-example":{"apiProvider":"API_PROVIDER_OPENAI_VERTEX"},
		"gemini-no-provider":{"displayName":"No upstream provider metadata"},
		"internal-native":{"apiProvider":"API_PROVIDER_GOOGLE_GEMINI","isInternal":true},
		"internal-provider":{"apiProvider":"API_PROVIDER_INTERNAL"},
		"lead-in-native":{"apiProvider":"API_PROVIDER_GOOGLE_GEMINI","requiresLeadInGeneration":true},
		"tab-native":{"apiProvider":"API_PROVIDER_GOOGLE_GEMINI"}
	},"tabModelIds":["tab-native"]}`
	p := newCatalogTestProxy(t, func() string { return catalog })
	list := readCatalogTestList(t, p, "")
	if got, want := catalogTestNames(t, list), []string{"models/another-native-id", "models/claude-example", "models/future-native-id", "models/openai-example"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("selected model names = %v, want %v", got, want)
	}
	if list.NextPageToken != "" {
		t.Fatalf("unexpected next page token = %q", list.NextPageToken)
	}
	model := list.Models[2]
	if got := catalogTestString(t, model, "displayName"); got != "Upstream Display" {
		t.Fatalf("displayName = %q", got)
	}
	if got := catalogTestString(t, model, "description"); got != "Upstream description" {
		t.Fatalf("description = %q", got)
	}
	if got := string(model["inputTokenLimit"]); got != "9007199254740993" {
		t.Fatalf("inputTokenLimit = %s, want exact upstream integer", got)
	}
	if got := string(model["outputTokenLimit"]); got != "1729" {
		t.Fatalf("outputTokenLimit = %s, want upstream limit", got)
	}
	for _, model := range list.Models {
		var methods []string
		if err := json.Unmarshal(model["supportedGenerationMethods"], &methods); err != nil {
			t.Fatal(err)
		}
		if want := []string{"generateContent", "streamGenerateContent"}; !reflect.DeepEqual(methods, want) {
			t.Fatalf("supportedGenerationMethods = %v, want %v", methods, want)
		}
	}
	fallback := list.Models[0]
	if got := catalogTestString(t, fallback, "displayName"); got != "another-native-id" {
		t.Fatalf("fallback displayName = %q", got)
	}
	for _, field := range []string{"description", "inputTokenLimit", "outputTokenLimit", "version", "baseModelId"} {
		if _, exists := fallback[field]; exists {
			t.Fatalf("absent upstream field was fabricated: %s = %s", field, fallback[field])
		}
	}
}

func TestGeminiCatalogPaginationRoundTripAndCatalogChanges(t *testing.T) {
	var catalog atomic.Value
	catalog.Store(`{"models":{
		"model-005":{"apiProvider":"API_PROVIDER_GOOGLE_GEMINI"},
		"model-003":{"apiProvider":"API_PROVIDER_GOOGLE_GEMINI"},
		"model-001":{"apiProvider":"API_PROVIDER_GOOGLE_GEMINI"},
		"model-004":{"apiProvider":"API_PROVIDER_GOOGLE_GEMINI"},
		"model-002":{"apiProvider":"API_PROVIDER_GOOGLE_GEMINI"}
	}}`)
	p := newCatalogTestProxy(t, func() string { return catalog.Load().(string) })
	advance := catalogTestClock(p)
	first := readCatalogTestList(t, p, "?pageSize=2")
	if got, want := catalogTestNames(t, first), []string{"models/model-001", "models/model-002"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("first page = %v, want %v", got, want)
	}
	if first.NextPageToken == "" {
		t.Fatal("first page has no nextPageToken")
	}
	if repeated := readCatalogTestList(t, p, "?pageSize=2"); repeated.NextPageToken != first.NextPageToken {
		t.Fatalf("same page produced a different opaque cursor: %q vs %q", repeated.NextPageToken, first.NextPageToken)
	}
	// Inserting before the cursor must not make a positional offset repeat model-002.
	catalog.Store(`{"models":{
		"model-000":{"apiProvider":"API_PROVIDER_GOOGLE_GEMINI"},
		"model-001":{"apiProvider":"API_PROVIDER_GOOGLE_GEMINI"},
		"model-002":{"apiProvider":"API_PROVIDER_GOOGLE_GEMINI"},
		"model-003":{"apiProvider":"API_PROVIDER_GOOGLE_GEMINI"},
		"model-004":{"apiProvider":"API_PROVIDER_GOOGLE_GEMINI"},
		"model-005":{"apiProvider":"API_PROVIDER_GOOGLE_GEMINI"}
	}}`)
	advance(modelCatalogTTL)
	second := readCatalogTestList(t, p, "?pageSize=2&pageToken="+url.QueryEscape(first.NextPageToken))
	if got, want := catalogTestNames(t, second), []string{"models/model-003", "models/model-004"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("second page = %v, want %v", got, want)
	}
	if second.NextPageToken == "" {
		t.Fatal("second page has no nextPageToken")
	}
	last := readCatalogTestList(t, p, "?pageSize=2&pageToken="+url.QueryEscape(second.NextPageToken))
	if got, want := catalogTestNames(t, last), []string{"models/model-005"}; !reflect.DeepEqual(got, want) || last.NextPageToken != "" {
		t.Fatalf("last page = %v, nextPageToken = %q", got, last.NextPageToken)
	}
	assertCatalogTestError(t, catalogTestRequest(p, "/v1beta/models?pageSize=3&pageToken="+url.QueryEscape(first.NextPageToken)), http.StatusBadRequest, "INVALID_ARGUMENT")
}

func TestGeminiCatalogPaginationDefaultsAndMaximum(t *testing.T) {
	models := make(map[string]any, 1005)
	for i := range 1005 {
		models[fmt.Sprintf("model-%04d", i)] = map[string]string{"apiProvider": "API_PROVIDER_GOOGLE_GEMINI"}
	}
	body, err := json.Marshal(map[string]any{"models": models})
	if err != nil {
		t.Fatal(err)
	}
	p := newCatalogTestProxy(t, func() string { return string(body) })
	for _, query := range []string{"", "?pageSize=0"} {
		list := readCatalogTestList(t, p, query)
		if len(list.Models) != 50 || list.NextPageToken == "" {
			t.Fatalf("default page %q has %d models, nextPageToken = %q", query, len(list.Models), list.NextPageToken)
		}
	}
	for _, size := range []string{"1000", "2000"} {
		first := readCatalogTestList(t, p, "?pageSize="+size)
		if len(first.Models) != 1000 || first.NextPageToken == "" {
			t.Fatalf("pageSize=%s returned %d models, nextPageToken = %q", size, len(first.Models), first.NextPageToken)
		}
		last := readCatalogTestList(t, p, "?pageSize="+size+"&pageToken="+url.QueryEscape(first.NextPageToken))
		if len(last.Models) != 5 || last.NextPageToken != "" {
			t.Fatalf("remaining page has %d models, nextPageToken = %q", len(last.Models), last.NextPageToken)
		}
	}
}

func TestGeminiCatalogRejectsMalformedPagination(t *testing.T) {
	p := New(config.Config{})
	queries := []string{
		"pageSize=-1",
		"pageSize=1.5",
		"pageSize=2;3",
		"pageSize=not-an-integer",
		"pageSize=999999999999999999999999999999",
		"pageSize=",
		"pageSize=2&pageSize=2",
		"pageToken=not-a-valid-cursor",
		"pageToken=%25",
		"pageToken=one&pageToken=two",
	}
	for _, payload := range []string{
		`{}`,
		`{"version":2,"after":"models/model-001","pageSize":50}`,
		`{"version":1,"after":"models/","pageSize":50}`,
		`{"version":1,"after":"model-001","pageSize":50}`,
		`{"version":1,"after":"models/model-001","pageSize":50,"extra":true}`,
	} {
		queries = append(queries, "pageToken="+base64.RawURLEncoding.EncodeToString([]byte(payload)))
	}
	for _, query := range queries {
		t.Run(query, func(t *testing.T) {
			assertCatalogTestError(t, catalogTestRequest(p, "/v1beta/models?"+query), http.StatusBadRequest, "INVALID_ARGUMENT")
		})
	}
}

func TestGeminiCatalogGetUsesCurrentMetadataAndMissingIs404(t *testing.T) {
	var catalog atomic.Value
	catalog.Store(`{"models":{"future-native-id":{"apiProvider":"API_PROVIDER_GOOGLE_GEMINI","displayName":"First catalog","maxTokens":8123},"claude-example":{"apiProvider":"API_PROVIDER_ANTHROPIC_VERTEX"},"internal-model":{"apiProvider":"API_PROVIDER_INTERNAL"}}}`)
	p := newCatalogTestProxy(t, func() string { return catalog.Load().(string) })
	advance := catalogTestClock(p)
	claudeResponse := catalogTestRequest(p, "/v1beta/models/claude-example")
	if claudeResponse.Code != http.StatusOK {
		t.Fatalf("native Claude get status = %d: %s", claudeResponse.Code, claudeResponse.Body.String())
	}
	for _, id := range []string{"missing-model", "internal-model"} {
		assertCatalogTestError(t, catalogTestRequest(p, "/v1beta/models/"+id), http.StatusNotFound, "NOT_FOUND")
	}
	for _, expected := range []struct {
		displayName string
		inputLimit  string
	}{
		{"First catalog", "8123"},
		{"Updated catalog", "9147"},
	} {
		response := catalogTestRequest(p, "/v1beta/models/future-native-id")
		if response.Code != http.StatusOK {
			t.Fatalf("get status = %d: %s", response.Code, response.Body.String())
		}
		var model map[string]json.RawMessage
		if err := json.Unmarshal(response.Body.Bytes(), &model); err != nil {
			t.Fatal(err)
		}
		if got := catalogTestString(t, model, "name"); got != "models/future-native-id" {
			t.Fatalf("get model name = %q", got)
		}
		if got := catalogTestString(t, model, "displayName"); got != expected.displayName {
			t.Fatalf("get displayName = %q, want %q", got, expected.displayName)
		}
		if got := string(model["inputTokenLimit"]); got != expected.inputLimit {
			t.Fatalf("get inputTokenLimit = %s, want %s", got, expected.inputLimit)
		}
		catalog.Store(`{"models":{"future-native-id":{"apiProvider":"API_PROVIDER_GOOGLE_GEMINI","displayName":"Updated catalog","maxTokens":9147}}}`)
		advance(modelCatalogTTL)
	}
}

func TestGeminiCatalogEmptyListIsAnArray(t *testing.T) {
	p := newCatalogTestProxy(t, func() string { return `{"models":{}}` })
	list := readCatalogTestList(t, p, "")
	if list.Models == nil || len(list.Models) != 0 || list.NextPageToken != "" {
		t.Fatalf("empty catalog = %+v", list)
	}
}
