package management

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

const (
	authStatusProbeInterval    = 2 * time.Hour
	authStatusProbeTimeout     = 45 * time.Second
	authStatusProbeConcurrency = 50
	codexUsageProbeURL         = "https://chatgpt.com/backend-api/wham/usage"
	codexUsageProbeUserAgent   = "codex_cli_rs/0.76.0 (Debian 13.0.0; x86_64) WindowsTerminal"
)

type authStatusProbeRequest struct {
	Names []string `json:"names"`
}

type authStatusProbeResult struct {
	Name          string `json:"name"`
	Provider      string `json:"provider"`
	Status        string `json:"status"`
	StatusMessage string `json:"status_message,omitempty"`
	HTTPStatus    int    `json:"http_status,omitempty"`
	Error         string `json:"error,omitempty"`
}

type authStatusProbeSummary struct {
	Status         string                  `json:"status"`
	StartedAt      time.Time               `json:"started_at"`
	CompletedAt    time.Time               `json:"completed_at"`
	RequestedCount int                     `json:"requested_count"`
	CheckedCount   int                     `json:"checked_count"`
	HealthyCount   int                     `json:"healthy_count"`
	WarningCount   int                     `json:"warning_count"`
	Unauthorized   int                     `json:"unauthorized_count"`
	FailedCount    int                     `json:"failed_count"`
	SkippedCount   int                     `json:"skipped_count"`
	Results        []authStatusProbeResult `json:"results,omitempty"`
}

type codexUsageWindowPayload struct {
	UsedPercent       any `json:"used_percent"`
	UsedPercentAlt    any `json:"usedPercent"`
	LimitReached      any `json:"limit_reached"`
	LimitReachedAlt   any `json:"limitReached"`
	ResetAfterSeconds any `json:"reset_after_seconds"`
	ResetAfterAlt     any `json:"resetAfterSeconds"`
}

type codexRateLimitPayload struct {
	Allowed          any                     `json:"allowed"`
	LimitReached     any                     `json:"limit_reached"`
	LimitReachedAlt  any                     `json:"limitReached"`
	PrimaryWindow    *codexUsageWindowPayload `json:"primary_window"`
	PrimaryWindowAlt *codexUsageWindowPayload `json:"primaryWindow"`
	SecondaryWindow    *codexUsageWindowPayload `json:"secondary_window"`
	SecondaryWindowAlt *codexUsageWindowPayload `json:"secondaryWindow"`
}

type codexUsagePayload struct {
	PlanType      string                `json:"plan_type"`
	PlanTypeAlt   string                `json:"planType"`
	RateLimit     *codexRateLimitPayload `json:"rate_limit"`
	RateLimitAlt  *codexRateLimitPayload `json:"rateLimit"`
}

func (h *Handler) startAuthStatusProbeLoop() {
	go func() {
		ticker := time.NewTicker(authStatusProbeInterval)
		defer ticker.Stop()

		for range ticker.C {
			ctx, cancel := context.WithTimeout(context.Background(), authStatusProbeTimeout*time.Duration(authStatusProbeConcurrency))
			summary, err := h.runAuthStatusProbe(ctx, nil, false)
			cancel()
			if err != nil {
				if strings.Contains(strings.ToLower(err.Error()), "already running") {
					log.Debug("auth status probe skipped: previous run still in progress")
					continue
				}
				log.WithError(err).Warn("auth status probe failed")
				continue
			}
			log.Infof(
				"auth status probe finished: checked=%d healthy=%d warning=%d unauthorized=%d failed=%d skipped=%d",
				summary.CheckedCount,
				summary.HealthyCount,
				summary.WarningCount,
				summary.Unauthorized,
				summary.FailedCount,
				summary.SkippedCount,
			)
		}
	}()
}

func (h *Handler) TriggerAuthStatusProbe(c *gin.Context) {
	if h == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "handler not initialized"})
		return
	}
	if h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "core auth manager unavailable"})
		return
	}

	var req authStatusProbeRequest
	if c.Request.Body != nil && c.Request.ContentLength != 0 {
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
			return
		}
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), authStatusProbeTimeout*time.Duration(authStatusProbeConcurrency))
	defer cancel()

	allowDisabled := len(req.Names) > 0
	summary, err := h.runAuthStatusProbe(ctx, req.Names, allowDisabled)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "already running") {
			c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, summary)
}

func (h *Handler) runAuthStatusProbe(
	ctx context.Context,
	names []string,
	allowDisabled bool,
) (*authStatusProbeSummary, error) {
	if h == nil {
		return nil, fmt.Errorf("handler not initialized")
	}
	if h.authManager == nil {
		return nil, fmt.Errorf("core auth manager unavailable")
	}

	if !h.beginAuthStatusProbe() {
		return nil, fmt.Errorf("auth status probe already running")
	}
	defer h.endAuthStatusProbe()

	startedAt := time.Now()
	candidates := h.authStatusProbeCandidates(names, allowDisabled)
	results := h.probeAuthStatusBatch(ctx, candidates)

	summary := &authStatusProbeSummary{
		Status:         "ok",
		StartedAt:      startedAt,
		CompletedAt:    time.Now(),
		RequestedCount: len(candidates),
		CheckedCount:   len(results),
		Results:        results,
	}

	for _, result := range results {
		switch {
		case result.Error != "":
			summary.FailedCount++
		case result.Status == string(coreauth.StatusActive):
			summary.HealthyCount++
		case result.HTTPStatus == http.StatusUnauthorized:
			summary.WarningCount++
			summary.Unauthorized++
		case result.Status == "skipped":
			summary.SkippedCount++
		case result.Status != "":
			summary.WarningCount++
		default:
			summary.SkippedCount++
		}
	}

	return summary, nil
}

func (h *Handler) beginAuthStatusProbe() bool {
	h.authStatusProbeMu.Lock()
	defer h.authStatusProbeMu.Unlock()
	if h.authStatusProbeRunning {
		return false
	}
	h.authStatusProbeRunning = true
	return true
}

func (h *Handler) endAuthStatusProbe() {
	h.authStatusProbeMu.Lock()
	h.authStatusProbeRunning = false
	h.authStatusProbeMu.Unlock()
}

func (h *Handler) authStatusProbeCandidates(names []string, allowDisabled bool) []*coreauth.Auth {
	if h == nil || h.authManager == nil {
		return nil
	}

	requested := make(map[string]struct{})
	for _, name := range names {
		trimmed := strings.TrimSpace(name)
		if trimmed == "" {
			continue
		}
		requested[trimmed] = struct{}{}
	}

	auths := h.authManager.List()
	candidates := make([]*coreauth.Auth, 0, len(auths))
	for _, auth := range auths {
		if !shouldProbeAuthStatus(auth, requested, allowDisabled) {
			continue
		}
		candidates = append(candidates, auth)
	}
	return candidates
}

func shouldProbeAuthStatus(
	auth *coreauth.Auth,
	requested map[string]struct{},
	allowDisabled bool,
) bool {
	if auth == nil {
		return false
	}
	if isRuntimeOnlyAuth(auth) {
		return false
	}
	if strings.ToLower(strings.TrimSpace(auth.Provider)) != "codex" {
		return false
	}
	if !allowDisabled && auth.Disabled {
		return false
	}
	if len(requested) == 0 {
		return true
	}

	candidates := []string{
		strings.TrimSpace(auth.ID),
		strings.TrimSpace(auth.FileName),
		filepath.Base(strings.TrimSpace(auth.FileName)),
	}

	if path := strings.TrimSpace(authAttribute(auth, "path")); path != "" {
		candidates = append(candidates, path, filepath.Base(path))
	}

	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		if _, ok := requested[candidate]; ok {
			return true
		}
	}

	return false
}

func (h *Handler) probeAuthStatusBatch(
	ctx context.Context,
	auths []*coreauth.Auth,
) []authStatusProbeResult {
	if len(auths) == 0 {
		return []authStatusProbeResult{}
	}

	workerCount := authStatusProbeConcurrency
	if workerCount <= 0 {
		workerCount = 1
	}
	if len(auths) < workerCount {
		workerCount = len(auths)
	}

	jobs := make(chan *coreauth.Auth)
	results := make(chan authStatusProbeResult, len(auths))

	var wg sync.WaitGroup
	for i := 0; i < workerCount; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for auth := range jobs {
				results <- h.probeSingleAuthStatus(ctx, auth)
			}
		}()
	}

	go func() {
		defer close(jobs)
		for _, auth := range auths {
			select {
			case <-ctx.Done():
				return
			case jobs <- auth:
			}
		}
	}()

	go func() {
		wg.Wait()
		close(results)
	}()

	collected := make([]authStatusProbeResult, 0, len(auths))
	for result := range results {
		collected = append(collected, result)
	}

	return collected
}

func (h *Handler) probeSingleAuthStatus(ctx context.Context, auth *coreauth.Auth) authStatusProbeResult {
	result := authStatusProbeResult{
		Name:     authStatusProbeName(auth),
		Provider: strings.TrimSpace(auth.Provider),
	}

	if auth == nil {
		result.Status = "skipped"
		result.StatusMessage = "auth unavailable"
		return result
	}
	if h == nil || h.authManager == nil {
		result.Status = string(coreauth.StatusError)
		result.Error = "core auth manager unavailable"
		return result
	}

	requestCtx, cancel := context.WithTimeout(ctx, authStatusProbeTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, codexUsageProbeURL, nil)
	if err != nil {
		result.Status = string(coreauth.StatusError)
		result.Error = err.Error()
		h.applyAuthProbeFailure(requestCtx, auth, "failed to build probe request")
		return result
	}
	req.Header.Set("User-Agent", codexUsageProbeUserAgent)
	req.Header.Set("Content-Type", "application/json")
	if accountID := authStatusProbeAccountID(auth); accountID != "" {
		req.Header.Set("Chatgpt-Account-Id", accountID)
	}

	resp, err := h.authManager.HttpRequest(requestCtx, auth, req)
	if err != nil {
		result.Status = string(coreauth.StatusError)
		result.Error = err.Error()
		h.applyAuthProbeFailure(requestCtx, auth, "status probe failed")
		return result
	}
	bodyBytes, readErr := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if readErr != nil {
		result.Status = string(coreauth.StatusError)
		result.Error = readErr.Error()
		h.applyAuthProbeFailure(requestCtx, auth, "failed to read usage payload")
		return result
	}

	result.HTTPStatus = resp.StatusCode
	quotaOverview := parseCodexQuotaOverview(bodyBytes)

	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		result.Status = string(coreauth.StatusError)
		result.StatusMessage = "unauthorized"
		h.applyAuthProbeUnauthorized(requestCtx, auth)
	case resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices:
		result.Status = string(coreauth.StatusActive)
		h.applyAuthProbeHealthy(requestCtx, auth, quotaOverview)
	default:
		result.Status = string(coreauth.StatusError)
		result.StatusMessage = fmt.Sprintf("http_%d", resp.StatusCode)
		h.applyAuthProbeHTTPError(requestCtx, auth, resp.StatusCode)
	}

	return result
}

func authStatusProbeName(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	if name := strings.TrimSpace(auth.FileName); name != "" {
		return filepath.Base(name)
	}
	if path := strings.TrimSpace(authAttribute(auth, "path")); path != "" {
		return filepath.Base(path)
	}
	return strings.TrimSpace(auth.ID)
}

func authStatusProbeAccountID(auth *coreauth.Auth) string {
	if auth == nil || auth.Metadata == nil {
		return ""
	}
	for _, key := range []string{
		"account_id",
		"accountId",
		"chatgpt_account_id",
		"chatgptAccountId",
	} {
		if value, ok := auth.Metadata[key].(string); ok {
			trimmed := strings.TrimSpace(value)
			if trimmed != "" {
				return trimmed
			}
		}
	}
	return ""
}

func (h *Handler) applyAuthProbeHealthy(
	ctx context.Context,
	auth *coreauth.Auth,
	quotaOverview map[string]any,
) {
	if auth == nil || h == nil || h.authManager == nil {
		return
	}
	now := time.Now()
	next := auth.Clone()
	next.Status = coreauth.StatusActive
	next.StatusMessage = ""
	next.Unavailable = false
	next.LastError = nil
	next.NextRetryAfter = time.Time{}
	next.Quota = coreauth.QuotaState{}
	next.LastRefreshedAt = now
	next.UpdatedAt = now
	if next.Metadata == nil {
		next.Metadata = make(map[string]any)
	}
	if len(quotaOverview) > 0 {
		next.Metadata["quota_overview"] = quotaOverview
	} else {
		delete(next.Metadata, "quota_overview")
	}
	_, _ = h.authManager.Update(ctx, next)
}

func (h *Handler) applyAuthProbeUnauthorized(ctx context.Context, auth *coreauth.Auth) {
	if auth == nil || h == nil || h.authManager == nil {
		return
	}
	now := time.Now()
	next := auth.Clone()
	next.Status = coreauth.StatusError
	next.StatusMessage = "unauthorized"
	next.Unavailable = true
	next.NextRetryAfter = now.Add(authStatusProbeInterval)
	next.LastRefreshedAt = now
	next.UpdatedAt = now
	_, _ = h.authManager.Update(ctx, next)
}

func (h *Handler) applyAuthProbeHTTPError(ctx context.Context, auth *coreauth.Auth, statusCode int) {
	if auth == nil || h == nil || h.authManager == nil {
		return
	}
	now := time.Now()
	next := auth.Clone()
	next.Status = coreauth.StatusError
	next.StatusMessage = fmt.Sprintf("http_%d", statusCode)
	next.Unavailable = true
	next.NextRetryAfter = now.Add(authStatusProbeInterval)
	next.LastRefreshedAt = now
	next.UpdatedAt = now
	_, _ = h.authManager.Update(ctx, next)
}

func (h *Handler) applyAuthProbeFailure(ctx context.Context, auth *coreauth.Auth, message string) {
	if auth == nil || h == nil || h.authManager == nil {
		return
	}
	now := time.Now()
	next := auth.Clone()
	next.Status = coreauth.StatusError
	next.StatusMessage = message
	next.Unavailable = true
	next.NextRetryAfter = now.Add(30 * time.Minute)
	next.LastRefreshedAt = now
	next.UpdatedAt = now
	_, _ = h.authManager.Update(ctx, next)
}

func parseCodexQuotaOverview(body []byte) map[string]any {
	if len(body) == 0 {
		return nil
	}

	var payload codexUsagePayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil
	}

	rateLimit := payload.RateLimit
	if rateLimit == nil {
		rateLimit = payload.RateLimitAlt
	}
	if rateLimit == nil {
		return nil
	}

	primaryWindow := rateLimit.PrimaryWindow
	if primaryWindow == nil {
		primaryWindow = rateLimit.PrimaryWindowAlt
	}
	secondaryWindow := rateLimit.SecondaryWindow
	if secondaryWindow == nil {
		secondaryWindow = rateLimit.SecondaryWindowAlt
	}

	overview := map[string]any{}
	planType := strings.TrimSpace(payload.PlanType)
	if planType == "" {
		planType = strings.TrimSpace(payload.PlanTypeAlt)
	}
	if planType != "" {
		overview["plan_type"] = planType
	}
	if primaryUsed, ok := quotaWindowUsedPercent(primaryWindow); ok {
		overview["primary_used_percent"] = primaryUsed
	}
	if secondaryUsed, ok := quotaWindowUsedPercent(secondaryWindow); ok {
		overview["secondary_used_percent"] = secondaryUsed
	}
	if primaryReached, ok := quotaWindowLimitReached(primaryWindow); ok {
		overview["primary_limit_reached"] = primaryReached
	}
	if secondaryReached, ok := quotaWindowLimitReached(secondaryWindow); ok {
		overview["secondary_limit_reached"] = secondaryReached
	}
	if primaryReset, ok := quotaWindowResetAfterSeconds(primaryWindow); ok {
		overview["primary_reset_after_seconds"] = primaryReset
	}
	if secondaryReset, ok := quotaWindowResetAfterSeconds(secondaryWindow); ok {
		overview["secondary_reset_after_seconds"] = secondaryReset
	}

	if len(overview) == 0 {
		return nil
	}
	return overview
}

func quotaWindowUsedPercent(window *codexUsageWindowPayload) (float64, bool) {
	if window == nil {
		return 0, false
	}
	return probeNumber(window.UsedPercent, window.UsedPercentAlt)
}

func quotaWindowLimitReached(window *codexUsageWindowPayload) (bool, bool) {
	if window == nil {
		return false, false
	}
	return probeBool(window.LimitReached, window.LimitReachedAlt)
}

func quotaWindowResetAfterSeconds(window *codexUsageWindowPayload) (int64, bool) {
	if window == nil {
		return 0, false
	}
	return probeInt64(window.ResetAfterSeconds, window.ResetAfterAlt)
}

func probeNumber(values ...any) (float64, bool) {
	for _, value := range values {
		switch typed := value.(type) {
		case float64:
			return typed, true
		case float32:
			return float64(typed), true
		case int:
			return float64(typed), true
		case int32:
			return float64(typed), true
		case int64:
			return float64(typed), true
		case json.Number:
			if parsed, err := typed.Float64(); err == nil {
				return parsed, true
			}
		}
	}
	return 0, false
}

func probeInt64(values ...any) (int64, bool) {
	for _, value := range values {
		switch typed := value.(type) {
		case int:
			return int64(typed), true
		case int32:
			return int64(typed), true
		case int64:
			return typed, true
		case float64:
			return int64(typed), true
		case float32:
			return int64(typed), true
		case json.Number:
			if parsed, err := typed.Int64(); err == nil {
				return parsed, true
			}
		}
	}
	return 0, false
}

func probeBool(values ...any) (bool, bool) {
	for _, value := range values {
		switch typed := value.(type) {
		case bool:
			return typed, true
		case string:
			trimmed := strings.TrimSpace(strings.ToLower(typed))
			if trimmed == "true" {
				return true, true
			}
			if trimmed == "false" {
				return false, true
			}
		}
	}
	return false, false
}