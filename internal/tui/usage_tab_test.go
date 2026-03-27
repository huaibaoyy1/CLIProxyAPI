package tui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestClientGetUsagePageAddsPaginationQuery(t *testing.T) {
	t.Helper()

	var gotPath string
	var gotAuth string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.RequestURI()
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"usage": {"total_requests": 1},
			"page": 2,
			"page_size": 25,
			"total": 1,
			"total_pages": 1,
			"request_details": []
		}`))
	}))
	defer server.Close()

	client := &Client{
		baseURL:   server.URL,
		secretKey: "secret-token",
		http:      server.Client(),
	}

	result, err := client.GetUsagePage(2, 25)
	if err != nil {
		t.Fatalf("GetUsagePage returned error: %v", err)
	}

	if gotPath != "/v0/management/usage?page=2&page_size=25" {
		t.Fatalf("unexpected request path: got %q", gotPath)
	}

	if gotAuth != "Bearer secret-token" {
		t.Fatalf("unexpected authorization header: got %q", gotAuth)
	}

	if got := getIntValue(result, "page", 0); got != 2 {
		t.Fatalf("unexpected page: got %d", got)
	}
	if got := getIntValue(result, "page_size", 0); got != 25 {
		t.Fatalf("unexpected page_size: got %d", got)
	}
}

func TestExtractUsageRequestDetailsParsesPayload(t *testing.T) {
	t.Helper()

	wrapper := map[string]any{
		"request_details": []map[string]any{
			{
				"api":        "test-api-key",
				"model":      "gpt-4.1",
				"timestamp":  "2026-03-27T15:04:05Z",
				"latency_ms": 321,
				"source":     "openai",
				"auth_index": "auth-01",
				"tokens": map[string]any{
					"input_tokens":     10,
					"output_tokens":    20,
					"cached_tokens":    3,
					"reasoning_tokens": 4,
				},
				"failed": true,
			},
		},
	}

	details := extractUsageRequestDetails(wrapper)
	if len(details) != 1 {
		t.Fatalf("unexpected details length: got %d", len(details))
	}

	detail := details[0]
	if detail.API != "test-api-key" {
		t.Fatalf("unexpected api: got %q", detail.API)
	}
	if detail.Model != "gpt-4.1" {
		t.Fatalf("unexpected model: got %q", detail.Model)
	}
	if detail.LatencyMs != 321 {
		t.Fatalf("unexpected latency: got %d", detail.LatencyMs)
	}
	if detail.Source != "openai" {
		t.Fatalf("unexpected source: got %q", detail.Source)
	}
	if detail.AuthIndex != "auth-01" {
		t.Fatalf("unexpected auth index: got %q", detail.AuthIndex)
	}
	if !detail.Failed {
		t.Fatalf("expected failed detail to be true")
	}
	if detail.Timestamp.IsZero() {
		t.Fatalf("expected timestamp to be parsed")
	}
	if detail.Tokens.InputTokens != 10 || detail.Tokens.OutputTokens != 20 || detail.Tokens.CachedTokens != 3 || detail.Tokens.ReasoningTokens != 4 {
		t.Fatalf("unexpected token breakdown: %+v", detail.Tokens)
	}
}

func TestUsageTabRenderContentIncludesPaginationAndRequestDetails(t *testing.T) {
	t.Helper()

	originalLocale := CurrentLocale()
	defer SetLocale(originalLocale)
	SetLocale("en")

	model := newUsageTabModel(nil)
	model.width = 120
	model.page = 2
	model.pageSize = 25
	model.total = 60
	model.totalPages = 3
	model.usage = map[string]any{
		"usage": map[string]any{
			"total_requests":  5,
			"success_count":   4,
			"failure_count":   1,
			"total_tokens":    1234,
			"requests_by_day": map[string]any{},
		},
	}
	model.requestDetails = []usageRequestDetail{
		{
			API:       "sk-example-key",
			Model:     "gpt-4.1",
			Timestamp: time.Date(2026, 3, 27, 15, 4, 5, 0, time.UTC),
			LatencyMs: 456,
			Source:    "openai",
			AuthIndex: "auth-02",
			Tokens: usageTokenBreakdown{
				InputTokens:     11,
				OutputTokens:    22,
				CachedTokens:    0,
				ReasoningTokens: 7,
			},
			Failed: false,
		},
	}

	content := model.renderContent()

	expectedSnippets := []string{
		"Page 2/3",
		"Page Size 25",
		"Total 60",
		"Request Details",
		"gpt-4.1",
		"456ms",
		"Source: openai",
		"Auth: auth-02",
		"Tokens: Input:11  Output:22  Reasoning:7",
	}

	for _, snippet := range expectedSnippets {
		if !strings.Contains(content, snippet) {
			t.Fatalf("rendered content missing snippet %q\ncontent:\n%s", snippet, content)
		}
	}
}