package proxy

import "testing"

func BenchmarkAdaptGenerationBody(b *testing.B) {
	payload := []byte(`{"contents":[{"parts":[{"text":"hello benchmark"}]}],"generationConfig":{"temperature":0.7,"maxOutputTokens":1024}}`)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = adaptGenerationBody(payload)
	}
}

func BenchmarkGenerationConfigSpan(b *testing.B) {
	payload := []byte(`{"contents":[{"parts":[{"text":"hello benchmark"}]}],"generationConfig":{"responseJsonSchema":{"type":"object"}}}`)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = generationConfigSpan(payload)
	}
}

func BenchmarkResolveRoute(b *testing.B) {
	routes := []string{
		"/health",
		"/models",
		"/v1beta/models",
		"/v1beta/models/gemini-2.0-flash:generateContent",
		"/v1beta/models/gemini-2.0-flash:streamGenerateContent",
		"/status/limit",
		"/status/usage",
		"/admin/accounts",
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, r := range routes {
			_ = resolveRoute(r)
		}
	}
}
