package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const maxGenerationRequestSize = 50 << 20

type generationEnvelope struct {
	Project     string          `json:"project"`
	Model       string          `json:"model"`
	Request     json.RawMessage `json:"request"`
	UserAgent   string          `json:"userAgent"`
	RequestType string          `json:"requestType"`
	RequestID   string          `json:"requestId"`
}

// requestBody encodes only envelope metadata. Each replay reads the same raw
// request directly, preserving numeric literals and opaque signatures without
// allocating another request-sized JSON buffer.
func (payload generationEnvelope) requestBody() (func() io.Reader, int64, error) {
	metadata, err := json.Marshal(struct {
		Project     string `json:"project"`
		Model       string `json:"model"`
		UserAgent   string `json:"userAgent"`
		RequestType string `json:"requestType"`
		RequestID   string `json:"requestId"`
	}{payload.Project, payload.Model, payload.UserAgent, payload.RequestType, payload.RequestID})
	if err != nil {
		return nil, 0, err
	}
	prefix := append(metadata[:len(metadata)-1], `,"request":`...)
	return func() io.Reader {
		return io.MultiReader(bytes.NewReader(prefix), bytes.NewReader(payload.Request), strings.NewReader("}"))
	}, int64(len(prefix)) + int64(len(payload.Request)) + 1, nil
}

func (p *Proxy) handleGenerateContent(w http.ResponseWriter, r *http.Request, model string, stream bool) {
	if r.Context().Err() != nil {
		writeGeminiError(w, http.StatusServiceUnavailable, "generation canceled")
		return
	}
	if !p.hasCredentials() {
		if r.ProtoMajor == 1 {
			w.Header().Set("Connection", "close")
			_ = http.NewResponseController(w).SetReadDeadline(time.Now())
		}
		writeGeminiError(w, http.StatusServiceUnavailable, "Google authentication unavailable")
		return
	}
	select {
	case p.generationSlots <- struct{}{}:
		defer func() { <-p.generationSlots }()
	default:
		if r.ProtoMajor == 1 {
			// HTTP/1 normally drains small unread request bodies before responding.
			// Close this connection instead, and prevent its final Body.Close from
			// waiting for bytes a saturated caller may never send.
			w.Header().Set("Connection", "close")
			_ = http.NewResponseController(w).SetReadDeadline(time.Now())
		}
		w.Header().Set("Retry-After", "1")
		writeGeminiError(w, http.StatusTooManyRequests, "generation capacity exhausted")
		return
	}
	var err error
	r, err = ensureRequestID(w, r)
	if err != nil {
		writeGeminiError(w, http.StatusInternalServerError, "create generation request ID")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxGenerationRequestSize)
	defer r.Body.Close()
	request, err := io.ReadAll(r.Body)
	if err != nil {
		writeGenerationRequestError(w, err)
		return
	}
	request, err = adaptGenerationBody(request)
	if err != nil {
		writeGeminiError(w, http.StatusBadRequest, err.Error())
		return
	}
	token, err := p.accessToken(r.Context())
	if err != nil {
		writeGeminiError(w, http.StatusServiceUnavailable, "Google authentication unavailable")
		return
	}
	project, err := p.getProjectID(r.Context(), token)
	if err != nil {
		writeGeminiUpstreamError(w, err)
		return
	}
	payload := generationEnvelope{
		Project: project, Model: model, Request: request,
		UserAgent: "antigravity", RequestType: "agent",
		RequestID: w.Header().Get("X-Request-ID"),
	}
	path, accept := "/v1internal:generateContent", "application/json"
	if stream {
		path, accept = "/v1internal:streamGenerateContent?alt=sse", "text/event-stream"
	}
	// Carry caller cancellation into the upstream transport, including body reads.
	resp, err := p.postToAntigravity(r.Context(), token, path, accept, payload)
	if err != nil {
		writeGeminiUpstreamError(w, err)
		return
	}
	defer resp.Body.Close()
	if stream {
		forwardGenerationStream(w, resp.Body)
		return
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		writeGeminiError(w, http.StatusBadGateway, "read generation response from Google upstream")
		return
	}
	body, err = unwrapGenerationResponse(body)
	if err != nil {
		writeGeminiError(w, http.StatusBadGateway, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(body)
}

// adaptGenerationBody retains a single raw request buffer. Only generationConfig
// is decoded and rewritten, avoiding copies of large inlineData/content fields.
func adaptGenerationBody(body []byte) ([]byte, error) {
	body = bytes.TrimSpace(body)
	if len(body) == 0 || body[0] != '{' || !json.Valid(body) {
		return nil, errors.New("request body must contain exactly one JSON object")
	}
	start, end := generationConfigSpan(body)
	if start < 0 {
		return body, nil
	}
	var config map[string]json.RawMessage
	if err := json.Unmarshal(body[start:end], &config); err != nil || config == nil {
		return body, nil
	}
	schema, modern := config["responseJsonSchema"]
	if !modern {
		return body, nil
	}
	if _, legacy := config["responseSchema"]; legacy {
		return nil, errors.New("generationConfig.responseJsonSchema and responseSchema are mutually exclusive")
	}
	config["responseSchema"] = schema
	delete(config, "responseJsonSchema")
	encoded, err := json.Marshal(config)
	if err != nil {
		return nil, fmt.Errorf("encode generationConfig: %w", err)
	}
	result := make([]byte, 0, len(body)-(end-start)+len(encoded))
	result = append(result, body[:start]...)
	result = append(result, encoded...)
	return append(result, body[end:]...), nil
}

// generationConfigSpan scans already validated JSON without decoding/copying
// large values. The last duplicate field wins, matching encoding/json.
func generationConfigSpan(body []byte) (start, end int) {
	start, end = -1, -1
	space := func(i int) int {
		for i < len(body) && (body[i] == ' ' || body[i] == '\n' || body[i] == '\r' || body[i] == '\t') {
			i++
		}
		return i
	}
	stringEnd := func(i int) int {
		i++
		for i < len(body) {
			if body[i] == '\\' {
				i += 2
				continue
			}
			if body[i] == '"' {
				return i + 1
			}
			i++
		}
		return i
	}
	for i := space(1); i < len(body) && body[i] != '}'; {
		keyEnd := stringEnd(i)
		var key string
		_ = json.Unmarshal(body[i:keyEnd], &key)
		i = space(space(keyEnd) + 1)
		valueStart, depth := i, 0
		for i < len(body) {
			if body[i] == '"' {
				i = stringEnd(i)
				continue
			}
			if depth == 0 && (body[i] == ',' || body[i] == '}') {
				break
			}
			switch body[i] {
			case '{', '[':
				depth++
			case '}', ']':
				depth--
			}
			i++
		}
		if key == "generationConfig" {
			start, end = valueStart, i
		}
		if i < len(body) && body[i] == ',' {
			i = space(i + 1)
		}
	}
	return start, end
}

func writeGenerationRequestError(w http.ResponseWriter, err error) {
	status := http.StatusBadRequest
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		status = http.StatusRequestEntityTooLarge
	}
	writeGeminiError(w, status, "invalid generation request")
}

// unwrapGenerationResponse removes only the Cloud Code transport envelope.
// RawMessage preserves numbers, signatures, unknown fields and nested payloads.
func unwrapGenerationResponse(body []byte) ([]byte, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(body, &object); err != nil || object == nil {
		return nil, errors.New("invalid JSON object in generation response")
	}
	if response, wrapped := object["response"]; wrapped {
		trimmed := bytes.TrimSpace(response)
		if len(trimmed) == 0 || trimmed[0] != '{' {
			return nil, errors.New("invalid response object in generation envelope")
		}
		return response, nil
	}
	return body, nil
}

type generationSSEEvent struct {
	lines   []string
	data    []byte
	hasData bool
}

// readGenerationSSEEvent handles arbitrarily long lines and events; Scanner's
// token limit would truncate Gemini inlineData and signature-bearing responses.
func readGenerationSSEEvent(reader *bufio.Reader) (generationSSEEvent, error) {
	var event generationSSEEvent
	for {
		line, err := reader.ReadString('\n')
		if err != nil && err != io.EOF {
			return generationSSEEvent{}, err
		}
		if line != "" {
			line = strings.TrimSuffix(line, "\n")
			line = strings.TrimSuffix(line, "\r")
			if line == "" {
				return event, nil
			}
			field, value, _ := strings.Cut(line, ":")
			if field == "data" {
				event.lines = append(event.lines, "data")
				value = strings.TrimPrefix(value, " ")
				if event.hasData {
					event.data = append(event.data, '\n')
				}
				event.data = append(event.data, value...)
				event.hasData = true
			} else {
				event.lines = append(event.lines, line)
			}
		}
		if err == io.EOF {
			return event, io.EOF
		}
	}
}

func encodeGenerationSSEEvent(event generationSSEEvent, data []byte) []byte {
	var frame bytes.Buffer
	dataWritten := false
	for _, line := range event.lines {
		field, _, _ := strings.Cut(line, ":")
		if field != "data" {
			frame.WriteString(line)
			frame.WriteByte('\n')
			continue
		}
		if dataWritten {
			continue
		}
		frame.WriteString("data: ")
		frame.Write(data)
		frame.WriteByte('\n')
		dataWritten = true
	}
	frame.WriteByte('\n')
	return frame.Bytes()
}

// generationStreamCompletion observes native metadata without rewriting payloads.
// Completion requires all observed candidates to finish, a native error, or a
// blocked prompt with no candidates; EOF and [DONE] are transport-only markers.
type generationStreamCompletion struct {
	candidates  map[int]bool
	nativeError bool
	blocked     bool
}

func (completion *generationStreamCompletion) observe(data []byte) error {
	var event struct {
		Candidates []struct {
			Index        *int   `json:"index"`
			FinishReason string `json:"finishReason"`
		} `json:"candidates"`
		Error          json.RawMessage `json:"error"`
		PromptFeedback struct {
			BlockReason string `json:"blockReason"`
		} `json:"promptFeedback"`
	}
	if err := json.Unmarshal(data, &event); err != nil {
		return fmt.Errorf("read native stream completion metadata: %w", err)
	}
	if errorObject := bytes.TrimSpace(event.Error); len(errorObject) != 0 && errorObject[0] == '{' {
		completion.nativeError = true
	}
	if event.PromptFeedback.BlockReason != "" {
		completion.blocked = true
	}
	if len(event.Candidates) != 0 && completion.candidates == nil {
		completion.candidates = make(map[int]bool, len(event.Candidates))
	}
	for position, candidate := range event.Candidates {
		index := position
		if candidate.Index != nil {
			index = *candidate.Index
		}
		completion.candidates[index] = completion.candidates[index] || candidate.FinishReason != ""
	}
	return nil
}

func (completion *generationStreamCompletion) complete() bool {
	if completion.nativeError {
		return true
	}
	if len(completion.candidates) == 0 {
		return completion.blocked
	}
	for _, finished := range completion.candidates {
		if !finished {
			return false
		}
	}
	return true
}

func forwardGenerationStream(w http.ResponseWriter, body io.Reader) {
	reader := bufio.NewReader(body)
	controller := http.NewResponseController(w)
	// Delay SSE headers until the first valid JSON event so early failures retain
	// an HTTP error status.
	committed := false
	var completion generationStreamCompletion
	var pending bytes.Buffer
	fail := func(err error) {
		if committed {
			// net/http aborts the response (HTTP/2 resets the stream); never
			// convert a truncated or malformed upstream stream to success.
			panic(http.ErrAbortHandler)
		}
		writeGeminiError(w, http.StatusBadGateway, "invalid generation stream from Google upstream")
	}
	for {
		event, readErr := readGenerationSSEEvent(reader)
		if readErr != nil && readErr != io.EOF {
			fail(readErr)
			return
		}
		var data []byte
		if event.hasData {
			if bytes.Equal(bytes.TrimSpace(event.data), []byte("[DONE]")) {
				if !committed || !completion.complete() {
					fail(io.ErrUnexpectedEOF)
					return
				}
				if readErr == io.EOF {
					return
				}
				continue
			}
			var err error
			data, err = unwrapGenerationResponse(event.data)
			if err != nil {
				fail(err)
				return
			}
			if err := completion.observe(data); err != nil {
				fail(err)
				return
			}
			// Bifrost expects complete JSON on one conventional SSE data line.
			// Compact preserves numeric literals, signatures and object ordering.
			var compact bytes.Buffer
			if err := json.Compact(&compact, data); err != nil {
				fail(err)
				return
			}
			data = compact.Bytes()
			if !committed {
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set("Cache-Control", "no-cache")
				w.WriteHeader(http.StatusOK)
				committed = true
			}
		}
		if len(event.lines) != 0 {
			frame := encodeGenerationSSEEvent(event, data)
			if !committed {
				pending.Write(frame)
			} else {
				if pending.Len() != 0 {
					if _, err := w.Write(pending.Bytes()); err != nil {
						fail(err)
						return
					}
					pending.Reset()
				}
				if _, err := w.Write(frame); err != nil {
					fail(err)
					return
				}
				if err := controller.Flush(); err != nil {
					fail(fmt.Errorf("flush generation event: %w", err))
					return
				}
			}
		}
		if readErr == io.EOF {
			if !committed || !completion.complete() {
				fail(io.ErrUnexpectedEOF)
			}
			return
		}
	}
}
