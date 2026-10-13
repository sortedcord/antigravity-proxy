package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"testing"
)

// Differentially check the zero-copy span scanner against encoding/json. A
// Decoder/RawMessage walk would copy large inlineData values merely to skip them;
// the scanner is retained for that memory contract, not as a general JSON parser.
func FuzzGenerationConfigSpan(f *testing.F) {
	for _, seed := range []string{
		`{}`, `{"generationConfig":null}`, `{"generationConfig":[]}`,
		`{"contents":[{"parts":[{"text":"escaped \\\" } , :"}]}],"generationConfig":{"responseJsonSchema":{"type":"object"}}}`,
		`{"generationConfig":{"temperature":1},"generationConfig":{"responseJsonSchema":{}}}`,
		`{"generation\u0043onfig":{"responseJsonSchema":null,"other":12345678901234567890}}`,
		`{"generationConfig":{"responseJsonSchema":{},"responseSchema":{}}}`,
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		body = bytes.TrimSpace(body)
		if len(body) == 0 || body[0] != '{' || !json.Valid(body) {
			return
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(body, &fields); err != nil {
			t.Fatal(err)
		}
		start, end := generationConfigSpan(body)
		raw, found := fields["generationConfig"]
		if !found {
			if start != -1 || end != -1 {
				t.Fatal("scanner found a missing field")
			}
		} else {
			if start < 0 || end < start || end > len(body) {
				t.Fatal("invalid span")
			}
			var expected, actual bytes.Buffer
			_ = json.Compact(&expected, raw)
			_ = json.Compact(&actual, body[start:end])
			if !bytes.Equal(expected.Bytes(), actual.Bytes()) {
				t.Fatal("span does not match the last decoded field")
			}
		}
		adapted, err := adaptGenerationBody(body)
		var config map[string]json.RawMessage
		_ = json.Unmarshal(raw, &config)
		_, modern := config["responseJsonSchema"]
		_, legacy := config["responseSchema"]
		if modern && legacy {
			if err == nil {
				t.Fatal("conflicting schema fields accepted")
			}
			return
		}
		if err != nil || !json.Valid(adapted) {
			t.Fatalf("invalid adapted JSON: %v", err)
		}
		if !modern && !bytes.Equal(body, adapted) {
			t.Fatal("unrelated request rewritten")
		}
	})
}

func TestGenerationResponseAndStreamLineBounds(t *testing.T) {
	for _, test := range []struct {
		body  string
		limit int64
		fail  bool
	}{
		{body: "12345678", limit: 8}, {body: "123456789", limit: 8, fail: true},
	} {
		_, err := readGenerationResponse(bytes.NewBufferString(test.body), test.limit)
		if (err != nil) != test.fail {
			t.Fatalf("response bound error = %v", err)
		}
		remaining := test.limit
		_, err = readGenerationSSELine(bufio.NewReaderSize(bytes.NewBufferString(test.body+"\n"), 16), &remaining)
		if err == nil {
			t.Fatal("line delimiter escaped the byte bound")
		}
	}
	remaining := int64(5)
	reader := bufio.NewReader(bytes.NewBufferString("one\nxx\n"))
	if line, err := readGenerationSSELine(reader, &remaining); err != nil || line != "one\n" {
		t.Fatal("valid first line", line, err)
	}
	if _, err := readGenerationSSELine(reader, &remaining); err == nil {
		t.Fatal("event total bound was not enforced across lines")
	}
}
