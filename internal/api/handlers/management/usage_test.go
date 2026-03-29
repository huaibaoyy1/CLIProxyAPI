package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
)

func TestGetUsageStatisticsStripsSnapshotDetailsAndReturnsPagedRequestDetails(t *testing.T) {
	t.Helper()

	gin.SetMode(gin.TestMode)

	stats := usage.NewRequestStatistics()
	mergeResult := stats.MergeSnapshot(usage.StatisticsSnapshot{
		TotalRequests: 1,
		SuccessCount:  1,
		FailureCount:  0,
		TotalTokens:   42,
		APIs: map[string]usage.APISnapshot{
			"api-1": {
				TotalRequests: 1,
				TotalTokens:   42,
				Models: map[string]usage.ModelSnapshot{
					"gpt-4.1": {
						TotalRequests: 1,
						TotalTokens:   42,
						TokenStats: usage.TokenStats{
							InputTokens:  10,
							OutputTokens: 20,
							TotalTokens:  42,
						},
						Details: []usage.RequestDetail{
							{
								Timestamp: time.Date(2026, 3, 28, 0, 0, 0, 0, time.UTC),
								LatencyMs: 123,
								Source:    "openai",
								AuthIndex: "auth-01",
								Tokens: usage.TokenStats{
									InputTokens:     10,
									OutputTokens:    20,
									ReasoningTokens: 12,
									TotalTokens:     42,
								},
								Failed: false,
							},
						},
					},
				},
			},
		},
	})
	if mergeResult.Added != 1 {
		t.Fatalf("expected one imported detail, got %+v", mergeResult)
	}

	handler := &Handler{}
	handler.SetUsageStatistics(stats)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	req := httptest.NewRequest(http.MethodGet, "/v0/management/usage?page=1&page_size=10", nil)
	ctx.Request = req

	handler.GetUsageStatistics(ctx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("unexpected status code: got %d body=%s", recorder.Code, recorder.Body.String())
	}

	var payload map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if got := intFromAny(payload["page"]); got != 1 {
		t.Fatalf("unexpected page: got %d", got)
	}
	if got := intFromAny(payload["page_size"]); got != 10 {
		t.Fatalf("unexpected page_size: got %d", got)
	}
	if got := intFromAny(payload["total"]); got != 1 {
		t.Fatalf("unexpected total: got %d", got)
	}
	if got := intFromAny(payload["total_pages"]); got != 1 {
		t.Fatalf("unexpected total_pages: got %d", got)
	}

	requestDetails, ok := payload["request_details"].([]any)
	if !ok {
		t.Fatalf("request_details missing or invalid: %#v", payload["request_details"])
	}
	if len(requestDetails) != 1 {
		t.Fatalf("unexpected request_details length: got %d", len(requestDetails))
	}

	usageWrapper, ok := payload["usage"].(map[string]any)
	if !ok {
		t.Fatalf("usage missing or invalid: %#v", payload["usage"])
	}
	apis, ok := usageWrapper["apis"].(map[string]any)
	if !ok {
		t.Fatalf("usage.apis missing or invalid: %#v", usageWrapper["apis"])
	}
	api1, ok := apis["api-1"].(map[string]any)
	if !ok {
		t.Fatalf("usage.apis[api-1] missing or invalid: %#v", apis["api-1"])
	}
	models, ok := api1["models"].(map[string]any)
	if !ok {
		t.Fatalf("usage.apis[api-1].models missing or invalid: %#v", api1["models"])
	}
	model, ok := models["gpt-4.1"].(map[string]any)
	if !ok {
		t.Fatalf("usage.apis[api-1].models[gpt-4.1] missing or invalid: %#v", models["gpt-4.1"])
	}
	if _, exists := model["details"]; exists {
		t.Fatalf("usage snapshot details should be stripped from summary payload: %#v", model["details"])
	}
}

func intFromAny(v any) int {
	switch value := v.(type) {
	case float64:
		return int(value)
	case float32:
		return int(value)
	case int:
		return value
	case int64:
		return int(value)
	default:
		return 0
	}
}