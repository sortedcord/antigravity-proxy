package proxy

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// antigravityModelMetadata supplies public-model eligibility; model ID names do
// not determine inclusion. Token limits stay raw and descriptive, not request caps.
type antigravityModelMetadata struct {
	APIProvider              string          `json:"apiProvider"`
	IsInternal               bool            `json:"isInternal"`
	RequiresLeadInGeneration bool            `json:"requiresLeadInGeneration"`
	DisplayName              string          `json:"displayName"`
	Description              string          `json:"description"`
	MaxTokens                json.RawMessage `json:"maxTokens"`
	MaxOutputTokens          json.RawMessage `json:"maxOutputTokens"`
}

type geminiModel struct {
	Name                       string          `json:"name"`
	DisplayName                string          `json:"displayName"`
	Description                string          `json:"description,omitempty"`
	InputTokenLimit            json.RawMessage `json:"inputTokenLimit,omitempty"`
	OutputTokenLimit           json.RawMessage `json:"outputTokenLimit,omitempty"`
	SupportedGenerationMethods [2]string       `json:"supportedGenerationMethods"`
}

type geminiModelList struct {
	Models        []geminiModel `json:"models"`
	NextPageToken string        `json:"nextPageToken,omitempty"`
}

// geminiModelCursor stores the last sorted name and effective page size, not an
// offset. Each page refreshes the catalog, which may add or remove entries;
// keyset pagination gives forward-only progress, not a stable snapshot.
type geminiModelCursor struct {
	Version  int    `json:"version"`
	After    string `json:"after"`
	PageSize int    `json:"pageSize"`
}

func (p *Proxy) fetchGeminiModels(ctx context.Context, token, projectID string) ([]geminiModel, error) {
	resp, err := p.postToAntigravity(ctx, token, "/v1internal:fetchAvailableModels", "application/json", "", map[string]string{"project": projectID})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	const maxCatalogSize = 8 << 20
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxCatalogSize+1))
	if err != nil {
		return nil, fmt.Errorf("read model catalog from Cloud Code: %w", err)
	}
	if len(body) > maxCatalogSize {
		return nil, fmt.Errorf("model catalog from Cloud Code exceeds size limit")
	}
	var catalog struct {
		Models      map[string]antigravityModelMetadata `json:"models"`
		TabModelIDs []string                            `json:"tabModelIds"`
	}
	if err := json.Unmarshal(body, &catalog); err != nil || catalog.Models == nil {
		return nil, fmt.Errorf("invalid model catalog from Cloud Code")
	}
	tabModels := make(map[string]bool, len(catalog.TabModelIDs))
	for _, id := range catalog.TabModelIDs {
		tabModels[id] = true
	}
	models := make([]geminiModel, 0, len(catalog.Models))
	for id, metadata := range catalog.Models {
		if metadata.APIProvider == "" || metadata.APIProvider == "API_PROVIDER_INTERNAL" || metadata.IsInternal || metadata.RequiresLeadInGeneration || tabModels[id] {
			continue
		}
		displayName := metadata.DisplayName
		if displayName == "" {
			displayName = id
		}
		models = append(models, geminiModel{
			Name:                       "models/" + id,
			DisplayName:                displayName,
			Description:                metadata.Description,
			InputTokenLimit:            metadata.MaxTokens,
			OutputTokenLimit:           metadata.MaxOutputTokens,
			SupportedGenerationMethods: [2]string{"generateContent", "streamGenerateContent"},
		})
	}
	sort.Slice(models, func(i, j int) bool { return models[i].Name < models[j].Name })
	return models, nil
}

func parseGeminiModelPagination(r *http.Request) (int, string, error) {
	pageSize := 50
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return 0, "", fmt.Errorf("invalid pagination query")
	}
	if values, present := query["pageSize"]; present {
		if len(values) != 1 || values[0] == "" {
			return 0, "", fmt.Errorf("invalid pageSize")
		}
		value, err := strconv.Atoi(values[0])
		if err != nil || value < 0 {
			return 0, "", fmt.Errorf("pageSize must be a nonnegative integer")
		}
		if value != 0 {
			pageSize = value
			if pageSize > 1000 {
				pageSize = 1000
			}
		}
	}
	values, present := query["pageToken"]
	if !present {
		return pageSize, "", nil
	}
	if len(values) != 1 {
		return 0, "", fmt.Errorf("invalid pageToken")
	}
	if values[0] == "" {
		return pageSize, "", nil
	}
	encoded := values[0]
	body, err := base64.RawURLEncoding.Strict().DecodeString(encoded)
	if err != nil {
		return 0, "", fmt.Errorf("invalid pageToken")
	}
	var cursor geminiModelCursor
	if err := json.Unmarshal(body, &cursor); err != nil || cursor.Version != 1 || cursor.PageSize != pageSize || !strings.HasPrefix(cursor.After, "models/") || cursor.After == "models/" {
		return 0, "", fmt.Errorf("invalid pageToken")
	}
	// Canonical encoding rejects unknown fields and alternate token representations.
	canonical, err := json.Marshal(cursor)
	if err != nil || base64.RawURLEncoding.EncodeToString(canonical) != encoded {
		return 0, "", fmt.Errorf("invalid pageToken")
	}
	return pageSize, cursor.After, nil
}

func (p *Proxy) geminiModelCatalog(w http.ResponseWriter, r *http.Request) ([]geminiModel, bool) {
	token, err := p.accessToken(r.Context())
	if err != nil {
		writeGeminiError(w, http.StatusServiceUnavailable, err.Error())
		return nil, false
	}
	projectID, err := p.getProjectID(r.Context(), token)
	if err != nil {
		writeGeminiUpstreamError(w, err)
		return nil, false
	}
	models, err := p.fetchGeminiModels(r.Context(), token, projectID)
	if err != nil {
		writeGeminiUpstreamError(w, err)
		return nil, false
	}
	return models, true
}

func (p *Proxy) handleGeminiModels(w http.ResponseWriter, r *http.Request) {
	pageSize, after, err := parseGeminiModelPagination(r)
	if err != nil {
		writeGeminiError(w, http.StatusBadRequest, err.Error())
		return
	}
	models, ok := p.geminiModelCatalog(w, r)
	if !ok {
		return
	}
	start := sort.Search(len(models), func(i int) bool { return models[i].Name > after })
	end := start + pageSize
	if end > len(models) {
		end = len(models)
	}
	response := geminiModelList{Models: models[start:end]}
	if end < len(models) {
		cursor, _ := json.Marshal(geminiModelCursor{Version: 1, After: models[end-1].Name, PageSize: pageSize})
		response.NextPageToken = base64.RawURLEncoding.EncodeToString(cursor)
	}
	writeJSON(w, http.StatusOK, response)
}

func (p *Proxy) handleGeminiModel(w http.ResponseWriter, r *http.Request, model string) {
	models, ok := p.geminiModelCatalog(w, r)
	if !ok {
		return
	}
	name := "models/" + model
	index := sort.Search(len(models), func(i int) bool { return models[i].Name >= name })
	if index == len(models) || models[index].Name != name {
		writeGeminiError(w, http.StatusNotFound, "model not found: "+name)
		return
	}
	writeJSON(w, http.StatusOK, models[index])
}
