package management

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

const (
	authStatusProbeDefaultIntervalHours = 8
	authStatusProbeTimeout              = 45 * time.Second
	authStatusProbeConcurrency          = 50
	authStatusProbeMaxAttempts          = 3
	authStatusProbeRetryBackoff         = 1500 * time.Millisecond
	codexUsageProbeURL                  = "https://chatgpt.com/backend-api/wham/usage"
	codexUsageProbeUserAgent            = "codex_cli_rs/0.76.0 (Debian 13.0.0; x86_64) WindowsTerminal"
	authStatusProbeStateFileName        = ".auth-status-probe-state"
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
	RetryCount    int    `json:"retry_count,omitempty"`
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

type authStatusProbeState struct {
	Status            string                  `json:"status"`
	Running           bool                    `json:"running"`
	Trigger           string                  `json:"trigger,omitempty"`
	StartedAt         time.Time               `json:"started_at,omitempty"`
	CompletedAt       time.Time               `json:"completed_at,omitempty"`
	LastCompletedAt   time.Time               `json:"last_completed_at,omitempty"`
	LastSuccessAt     time.Time               `json:"last_success_at,omitempty"`
	RequestedCount    int                     `json:"requested_count"`
	CheckedCount      int                     `json:"checked_count,omitempty"`
	HealthyCount      int                     `json:"healthy_count,omitempty"`
	WarningCount      int                     `json:"warning_count,omitempty"`
	UnauthorizedCount int                     `json:"unauthorized_count,omitempty"`
	FailedCount       int                     `json:"failed_count,omitempty"`
	SkippedCount      int                     `json:"skipped_count,omitempty"`
	LastError         string                  `json:"last_error,omitempty"`
	Summary           *authStatusProbeSummary `json:"summary,omitempty"`
}

type codexUsageWindowPayload struct {
	UsedPercent        any `json:"used_percent"`
	UsedPercentAlt     any `json:"usedPercent"`
	LimitReached       any `json:"limit_reached"`
	LimitReachedAlt    any `json:"limitReached"`
	ResetAfterSeconds  any `json:"reset_after_seconds"`
	ResetAfterAlt      any `json:"resetAfterSeconds"`
	LimitWindowSeconds any `json:"limit_window_seconds"`
	LimitWindowAlt     any `json:"limitWindowSeconds"`
	WindowSeconds      any `json:"window_seconds"`
	WindowSecondsAlt   any `json:"windowSeconds"`
}

type codexRateLimitPayload struct {
	Allowed             any                      `json:"allowed"`
	LimitReached        any                      `json:"limit_reached"`
	LimitReachedAlt     any                      `json:"limitReached"`
	PrimaryWindow       *codexUsageWindowPayload `json:"primary_window"`
	PrimaryWindowAlt    *codexUsageWindowPayload `json:"primaryWindow"`
	SecondaryWindow     *codexUsageWindowPayload `json:"secondary_window"`
	SecondaryWindowAlt  *codexUsageWindowPayload `json:"secondaryWindow"`
	IndividualWindow    *codexUsageWindowPayload `json:"individual_window"`
	IndividualWindowAlt *codexUsageWindowPayload `json:"individualWindow"`
}

type codexUsagePayload struct {
	PlanType     string                 `json:"plan_type"`
	PlanTypeAlt  string                 `json:"planType"`
	RateLimit    *codexRateLimitPayload `json:"rate_limit"`
	RateLimitAlt *codexRateLimitPayload `json:"rateLimit"`
}

type parsedCodexUsageWindow struct {
	name                 string
	usedPercent          float64
	hasUsedPercent       bool
	limitReached         bool
	hasLimitReached      bool
	resetAfterSeconds    int64
	hasResetAfterSeconds bool
	windowSeconds        int64
	hasWindowSeconds     bool
}

func (h *Handler) startAuthStatusProbeLoop() {
	go func() {
		for {
			timer := time.NewTimer(h.authStatusProbeNextDelay(time.Now()))
			<-timer.C

			if !h.isAuthStatusProbeSchedulerEnabled() {
				log.Debug("auth status probe scheduler skipped: disabled by configuration")
				continue
			}

			_, started, err := h.scheduleAuthStatusProbe(nil, false, "scheduled")
			if err != nil {
				log.WithError(err).Warn("failed to schedule auth status probe")
				continue
			}
			if !started {
				log.Debug("auth status probe skipped: previous run still in progress")
			}
		}
	}()
}

func (h *Handler) isAuthStatusProbeSchedulerEnabled() bool {
	if h == nil || h.cfg == nil {
		return false
	}
	return h.cfg.RemoteManagement.EnableAuthStatusProbeScheduler
}

func (h *Handler) authStatusProbeInterval() time.Duration {
	if h == nil || h.cfg == nil {
		return time.Duration(authStatusProbeDefaultIntervalHours) * time.Hour
	}

	hours := h.cfg.RemoteManagement.AuthStatusProbeIntervalHours
	if hours <= 0 {
		hours = authStatusProbeDefaultIntervalHours
	}

	return time.Duration(hours) * time.Hour
}

func (h *Handler) authStatusProbePersistencePath() string {
	if h == nil {
		return ""
	}
	if trimmed := strings.TrimSpace(h.configFilePath); trimmed != "" {
		return trimmed + "." + strings.TrimPrefix(authStatusProbeStateFileName, ".")
	}
	if h.cfg != nil {
		if authDir := strings.TrimSpace(h.cfg.AuthDir); authDir != "" {
			return filepath.Join(authDir, authStatusProbeStateFileName)
		}
	}
	return ""
}

func sanitizePersistedAuthStatusProbeState(state authStatusProbeState) authStatusProbeState {
	state.Running = false
	if state.Status == "running" {
		if !state.LastCompletedAt.IsZero() || !state.CompletedAt.IsZero() {
			state.Status = "completed"
		} else if strings.TrimSpace(state.LastError) != "" {
			state.Status = "failed"
		} else {
			state.Status = ""
		}
	}
	if state.LastCompletedAt.IsZero() && !state.CompletedAt.IsZero() {
		state.LastCompletedAt = state.CompletedAt
	}
	state.Summary = copyAuthStatusProbeSummary(state.Summary)
	return state
}

func (h *Handler) loadPersistedAuthStatusProbeState() {
	if h == nil {
		return
	}
	path := h.authStatusProbePersistencePath()
	if path == "" {
		return
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.WithError(err).Warn("failed to read persisted auth status probe state")
		}
		return
	}

	var state authStatusProbeState
	if err := json.Unmarshal(raw, &state); err != nil {
		log.WithError(err).Warn("failed to decode persisted auth status probe state")
		return
	}
	state = sanitizePersistedAuthStatusProbeState(state)

	h.authStatusProbeMu.Lock()
	if !h.authStatusProbeRunning {
		h.authStatusProbeState = state
	}
	h.authStatusProbeMu.Unlock()
}

func (h *Handler) persistAuthStatusProbeState(state authStatusProbeState) {
	if h == nil {
		return
	}
	path := h.authStatusProbePersistencePath()
	if path == "" {
		return
	}

	state = sanitizePersistedAuthStatusProbeState(state)
	raw, err := json.Marshal(state)
	if err != nil {
		log.WithError(err).Warn("failed to encode auth status probe state")
		return
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		log.WithError(err).Warn("failed to prepare auth status probe state directory")
		return
	}

	tempPath := path + ".tmp"
	if err := os.WriteFile(tempPath, raw, 0o600); err != nil {
		log.WithError(err).Warn("failed to write auth status probe state")
		return
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		log.WithError(err).Warn("failed to replace auth status probe state")
		_ = os.Remove(tempPath)
		return
	}
	if err := os.Rename(tempPath, path); err != nil {
		log.WithError(err).Warn("failed to finalize auth status probe state")
		_ = os.Remove(tempPath)
	}
}

func (h *Handler) authStatusProbeNextDelay(now time.Time) time.Duration {
	interval := h.authStatusProbeInterval()
	if interval <= 0 {
		interval = time.Duration(authStatusProbeDefaultIntervalHours) * time.Hour
	}

	if h == nil {
		return interval
	}
	if !h.isAuthStatusProbeSchedulerEnabled() {
		return interval
	}

	h.authStatusProbeMu.Lock()
	state := h.authStatusProbeState
	running := h.authStatusProbeRunning || state.Running
	h.authStatusProbeMu.Unlock()

	if running {
		if !state.StartedAt.IsZero() {
			next := state.StartedAt.Add(interval)
			if next.After(now) {
				return next.Sub(now)
			}
		}
		return interval
	}

	anchor := state.LastCompletedAt
	if anchor.IsZero() {
		anchor = state.CompletedAt
	}
	if anchor.IsZero() {
		return interval
	}

	next := anchor.Add(interval)
	if !next.After(now) {
		return 0
	}
	return next.Sub(now)
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

	allowDisabled := len(req.Names) > 0
	state, _, err := h.scheduleAuthStatusProbe(req.Names, allowDisabled, "manual")
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusAccepted, state)
}

func (h *Handler) GetAuthStatusProbe(c *gin.Context) {
	if h == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "handler not initialized"})
		return
	}
	if h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "core auth manager unavailable"})
		return
	}

	c.JSON(http.StatusOK, h.currentAuthStatusProbeState())
}

func (h *Handler) scheduleAuthStatusProbe(
	names []string,
	allowDisabled bool,
	trigger string,
) (authStatusProbeState, bool, error) {
	if h == nil {
		return authStatusProbeState{}, false, fmt.Errorf("handler not initialized")
	}
	if h.authManager == nil {
		return authStatusProbeState{}, false, fmt.Errorf("core auth manager unavailable")
	}

	normalizedNames := normalizeAuthStatusProbeNames(names)
	startedAt := time.Now()

	h.authStatusProbeMu.Lock()
	if h.authStatusProbeRunning {
		state := h.copyAuthStatusProbeStateLocked()
		h.authStatusProbeMu.Unlock()
		return state, false, nil
	}

	h.authStatusProbeRunning = true
	h.authStatusProbeState = authStatusProbeState{
		Status:         "running",
		Running:        true,
		Trigger:        strings.TrimSpace(trigger),
		StartedAt:      startedAt,
		RequestedCount: len(normalizedNames),
		LastError:      "",
		Summary:        nil,
	}
	state := h.copyAuthStatusProbeStateLocked()
	h.authStatusProbeMu.Unlock()

	h.persistAuthStatusProbeState(state)

	go h.executeAuthStatusProbe(normalizedNames, allowDisabled, trigger, startedAt)

	return state, true, nil
}

func normalizeAuthStatusProbeNames(names []string) []string {
	if len(names) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(names))
	normalized := make([]string, 0, len(names))
	for _, name := range names {
		trimmed := strings.TrimSpace(name)
		if trimmed == "" {
			continue
		}
		if _, ok := seen[trimmed]; ok {
			continue
		}
		seen[trimmed] = struct{}{}
		normalized = append(normalized, trimmed)
	}
	return normalized
}

func (h *Handler) executeAuthStatusProbe(
	names []string,
	allowDisabled bool,
	trigger string,
	startedAt time.Time,
) {
	timeoutMultiplier := authStatusProbeConcurrency
	if timeoutMultiplier <= 0 {
		timeoutMultiplier = 1
	}

	ctx, cancel := context.WithTimeout(context.Background(), authStatusProbeTimeout*time.Duration(timeoutMultiplier))
	defer cancel()

	summary, err := h.runAuthStatusProbe(ctx, names, allowDisabled)
	h.completeAuthStatusProbe(startedAt, trigger, summary, err)

	if err != nil {
		log.WithError(err).Warn("auth status probe failed")
		return
	}

	if summary != nil {
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
}

func (h *Handler) currentAuthStatusProbeState() authStatusProbeState {
	if h == nil {
		return authStatusProbeState{}
	}
	h.authStatusProbeMu.Lock()
	defer h.authStatusProbeMu.Unlock()
	return h.copyAuthStatusProbeStateLocked()
}

func (h *Handler) copyAuthStatusProbeStateLocked() authStatusProbeState {
	state := h.authStatusProbeState
	state.Summary = copyAuthStatusProbeSummary(state.Summary)
	return state
}

func copyAuthStatusProbeSummary(summary *authStatusProbeSummary) *authStatusProbeSummary {
	if summary == nil {
		return nil
	}
	cloned := *summary
	if len(summary.Results) > 0 {
		cloned.Results = append([]authStatusProbeResult(nil), summary.Results...)
	}
	return &cloned
}

func (h *Handler) completeAuthStatusProbe(
	startedAt time.Time,
	trigger string,
	summary *authStatusProbeSummary,
	err error,
) {
	if h == nil {
		return
	}

	completedAt := time.Now()

	h.authStatusProbeMu.Lock()

	state := h.authStatusProbeState
	state.Running = false
	state.Trigger = strings.TrimSpace(trigger)
	state.StartedAt = startedAt
	state.CompletedAt = completedAt
	state.LastCompletedAt = completedAt

	if summary != nil {
		if summary.StartedAt.IsZero() {
			summary.StartedAt = startedAt
		}
		if summary.CompletedAt.IsZero() {
			summary.CompletedAt = completedAt
		} else {
			state.CompletedAt = summary.CompletedAt
			state.LastCompletedAt = summary.CompletedAt
		}

		state.RequestedCount = summary.RequestedCount
		state.CheckedCount = summary.CheckedCount
		state.HealthyCount = summary.HealthyCount
		state.WarningCount = summary.WarningCount
		state.UnauthorizedCount = summary.Unauthorized
		state.FailedCount = summary.FailedCount
		state.SkippedCount = summary.SkippedCount
		state.Summary = copyAuthStatusProbeSummary(summary)
	} else {
		state.Summary = nil
	}

	if err != nil {
		state.Status = "failed"
		state.LastError = err.Error()
	} else {
		state.Status = "completed"
		state.LastError = ""
		state.LastSuccessAt = state.LastCompletedAt
	}

	h.authStatusProbeRunning = false
	h.authStatusProbeState = state
	persistedState := h.copyAuthStatusProbeStateLocked()
	h.authStatusProbeMu.Unlock()

	h.persistAuthStatusProbeState(persistedState)
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
	if auth == nil {
		return authStatusProbeResult{
			Status:        "skipped",
			StatusMessage: "auth unavailable",
		}
	}

	result := authStatusProbeResult{
		Name:     authStatusProbeName(auth),
		Provider: strings.TrimSpace(auth.Provider),
	}

	if h == nil || h.authManager == nil {
		result.Status = string(coreauth.StatusError)
		result.Error = "core auth manager unavailable"
		return result
	}

	maxAttempts := authStatusProbeMaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = 1
	}

	var lastErr error
	var lastStatusMessage string
	var retryCount int

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		requestCtx, cancel := context.WithTimeout(ctx, authStatusProbeTimeout)

		req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, codexUsageProbeURL, nil)
		if err != nil {
			cancel()
			result.Status = string(coreauth.StatusError)
			result.Error = err.Error()
			result.StatusMessage = "probe_request_build_failed"
			h.applyAuthProbeFailure(ctx, auth, result.StatusMessage)
			return result
		}

		req.Header.Set("User-Agent", codexUsageProbeUserAgent)
		req.Header.Set("Content-Type", "application/json")
		if accountID := authStatusProbeAccountID(auth); accountID != "" {
			req.Header.Set("Chatgpt-Account-Id", accountID)
		}

		resp, err := h.authManager.HttpRequest(requestCtx, auth, req)
		if err != nil {
			cancel()
			lastErr = err
			lastStatusMessage = "probe_request_failed"
			if attempt < maxAttempts && shouldRetryAuthProbeError(err) && waitBeforeAuthStatusProbeRetry(ctx, attempt) {
				retryCount++
				continue
			}
			result.Status = string(coreauth.StatusError)
			result.Error = err.Error()
			result.StatusMessage = lastStatusMessage
			result.RetryCount = retryCount
			h.applyAuthProbeFailure(ctx, auth, result.StatusMessage)
			return result
		}

		bodyBytes, readErr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		cancel()

		if readErr != nil {
			lastErr = readErr
			lastStatusMessage = "probe_response_read_failed"
			if attempt < maxAttempts && shouldRetryAuthProbeError(readErr) && waitBeforeAuthStatusProbeRetry(ctx, attempt) {
				retryCount++
				continue
			}
			result.Status = string(coreauth.StatusError)
			result.Error = readErr.Error()
			result.StatusMessage = lastStatusMessage
			result.RetryCount = retryCount
			h.applyAuthProbeFailure(ctx, auth, result.StatusMessage)
			return result
		}

		if attempt < maxAttempts && shouldRetryAuthProbeHTTPStatus(resp.StatusCode) && waitBeforeAuthStatusProbeRetry(ctx, attempt) {
			retryCount++
			lastStatusMessage = fmt.Sprintf("probe_http_%d", resp.StatusCode)
			lastErr = fmt.Errorf("probe http status %d", resp.StatusCode)
			continue
		}

		result.HTTPStatus = resp.StatusCode
		result.RetryCount = retryCount
		quotaOverview := parseCodexQuotaOverview(bodyBytes)

		switch {
		case resp.StatusCode == http.StatusUnauthorized:
			result.Status = string(coreauth.StatusError)
			result.StatusMessage = "unauthorized"
			h.applyAuthProbeUnauthorized(ctx, auth)
		case resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices:
			result.Status = string(coreauth.StatusActive)
			h.applyAuthProbeHealthy(ctx, auth, quotaOverview)
		default:
			result.Status = string(coreauth.StatusError)
			result.StatusMessage = fmt.Sprintf("probe_http_%d", resp.StatusCode)
			h.applyAuthProbeHTTPError(ctx, auth, resp.StatusCode)
		}

		return result
	}

	result.Status = string(coreauth.StatusError)
	result.StatusMessage = lastStatusMessage
	result.RetryCount = retryCount
	if lastErr != nil {
		result.Error = lastErr.Error()
	}
	h.applyAuthProbeFailure(ctx, auth, result.StatusMessage)
	return result
}

func shouldRetryAuthProbeError(err error) bool {
	if err == nil {
		return false
	}
	lowered := strings.ToLower(strings.TrimSpace(err.Error()))
	switch {
	case lowered == "":
		return false
	case strings.Contains(lowered, "timeout"),
		strings.Contains(lowered, "tempor"),
		strings.Contains(lowered, "connection reset"),
		strings.Contains(lowered, "connection refused"),
		strings.Contains(lowered, "connection aborted"),
		strings.Contains(lowered, "broken pipe"),
		strings.Contains(lowered, "unexpected eof"),
		strings.Contains(lowered, "eof"),
		strings.Contains(lowered, "no such host"),
		strings.Contains(lowered, "network"),
		strings.Contains(lowered, "tls"),
		strings.Contains(lowered, "proxyconnect"):
		return true
	default:
		return false
	}
}

func shouldRetryAuthProbeHTTPStatus(statusCode int) bool {
	switch {
	case statusCode == http.StatusRequestTimeout,
		statusCode == http.StatusTooManyRequests,
		statusCode == http.StatusBadGateway,
		statusCode == http.StatusServiceUnavailable,
		statusCode == http.StatusGatewayTimeout:
		return true
	case statusCode >= http.StatusInternalServerError:
		return true
	default:
		return false
	}
}

func waitBeforeAuthStatusProbeRetry(ctx context.Context, attempt int) bool {
	if attempt <= 0 {
		attempt = 1
	}
	delay := time.Duration(attempt) * authStatusProbeRetryBackoff
	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
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
	next.NextRetryAfter = now.Add(h.authStatusProbeInterval())
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
	next.StatusMessage = fmt.Sprintf("probe_http_%d", statusCode)
	next.Unavailable = true
	next.NextRetryAfter = now.Add(30 * time.Minute)
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

	planType := strings.TrimSpace(payload.PlanType)
	if planType == "" {
		planType = strings.TrimSpace(payload.PlanTypeAlt)
	}

	windows := collectCodexUsageWindows(rateLimit)
	if len(windows) == 0 {
		return nil
	}

	shortWindow, longWindow := pickCodexQuotaWindows(windows)

	primaryWindow := shortWindow
	if primaryWindow == nil {
		primaryWindow = longWindow
	}

	secondaryWindow := longWindow
	if primaryWindow != nil && secondaryWindow != nil && primaryWindow.name == secondaryWindow.name {
		secondaryWindow = nil
	}

	overview := map[string]any{}
	if planType != "" {
		overview["plan_type"] = planType
	}

	addCodexQuotaOverviewWindow(overview, "primary", primaryWindow, planType)
	addCodexQuotaOverviewWindow(overview, "secondary", secondaryWindow, planType)

	if len(overview) == 0 {
		return nil
	}
	return overview
}

func collectCodexUsageWindows(rateLimit *codexRateLimitPayload) []*parsedCodexUsageWindow {
	if rateLimit == nil {
		return nil
	}

	windows := make([]*parsedCodexUsageWindow, 0, 3)
	if parsed := parseNamedCodexUsageWindow("primary", firstNonNilWindow(rateLimit.PrimaryWindow, rateLimit.PrimaryWindowAlt)); parsed != nil {
		windows = append(windows, parsed)
	}
	if parsed := parseNamedCodexUsageWindow("secondary", firstNonNilWindow(rateLimit.SecondaryWindow, rateLimit.SecondaryWindowAlt)); parsed != nil {
		windows = append(windows, parsed)
	}
	if parsed := parseNamedCodexUsageWindow("individual", firstNonNilWindow(rateLimit.IndividualWindow, rateLimit.IndividualWindowAlt)); parsed != nil {
		windows = append(windows, parsed)
	}
	return windows
}

func firstNonNilWindow(values ...*codexUsageWindowPayload) *codexUsageWindowPayload {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}

func parseNamedCodexUsageWindow(name string, window *codexUsageWindowPayload) *parsedCodexUsageWindow {
	if window == nil {
		return nil
	}

	parsed := &parsedCodexUsageWindow{name: strings.TrimSpace(name)}
	if parsed.usedPercent, parsed.hasUsedPercent = quotaWindowUsedPercent(window); !parsed.hasUsedPercent {
		parsed.usedPercent = 0
	}
	if parsed.limitReached, parsed.hasLimitReached = quotaWindowLimitReached(window); !parsed.hasLimitReached {
		parsed.limitReached = false
	}
	if parsed.resetAfterSeconds, parsed.hasResetAfterSeconds = quotaWindowResetAfterSeconds(window); !parsed.hasResetAfterSeconds {
		parsed.resetAfterSeconds = 0
	}
	if parsed.windowSeconds, parsed.hasWindowSeconds = quotaWindowLimitWindowSeconds(window); !parsed.hasWindowSeconds {
		parsed.windowSeconds = 0
	}

	if !parsed.hasUsedPercent && !parsed.hasLimitReached && !parsed.hasResetAfterSeconds && !parsed.hasWindowSeconds {
		return nil
	}
	return parsed
}

func pickCodexQuotaWindows(windows []*parsedCodexUsageWindow) (*parsedCodexUsageWindow, *parsedCodexUsageWindow) {
	if len(windows) == 0 {
		return nil, nil
	}

	var shortWindow *parsedCodexUsageWindow
	var longWindow *parsedCodexUsageWindow

	for _, window := range windows {
		if window == nil {
			continue
		}
		switch window.name {
		case "individual":
			longWindow = window
		case "secondary":
			if shortWindow == nil {
				shortWindow = window
			}
		}
	}

	if longWindow == nil {
		for _, window := range windows {
			if window == nil || !window.hasWindowSeconds {
				continue
			}
			if longWindow == nil || window.windowSeconds > longWindow.windowSeconds {
				longWindow = window
			}
		}
	}

	if shortWindow == nil {
		for _, window := range windows {
			if window == nil || !window.hasWindowSeconds {
				continue
			}
			if longWindow != nil && window.name == longWindow.name {
				continue
			}
			if shortWindow == nil || window.windowSeconds < shortWindow.windowSeconds {
				shortWindow = window
			}
		}
	}

	if shortWindow == nil && longWindow != nil && longWindow.hasWindowSeconds && longWindow.windowSeconds <= 6*3600 {
		shortWindow = longWindow
		longWindow = nil
	}

	if shortWindow == nil && len(windows) > 0 {
		shortWindow = windows[0]
		if longWindow != nil && shortWindow.name == longWindow.name {
			shortWindow = nil
		}
	}

	return shortWindow, longWindow
}

func addCodexQuotaOverviewWindow(
	overview map[string]any,
	prefix string,
	window *parsedCodexUsageWindow,
	planType string,
) {
	if len(overview) == 0 || prefix == "" || window == nil {
		return
	}

	if label := codexQuotaWindowLabel(window, planType); label != "" {
		overview[prefix+"_label"] = label
	}
	if window.hasUsedPercent {
		overview[prefix+"_used_percent"] = window.usedPercent
	}
	if window.hasLimitReached {
		overview[prefix+"_limit_reached"] = window.limitReached
	}
	if window.hasResetAfterSeconds {
		overview[prefix+"_reset_after_seconds"] = window.resetAfterSeconds
	}
	if window.hasWindowSeconds {
		overview[prefix+"_window_seconds"] = window.windowSeconds
	}
}

func codexQuotaWindowLabel(window *parsedCodexUsageWindow, planType string) string {
	if window == nil {
		return ""
	}
	if window.hasWindowSeconds {
		return formatCodexQuotaWindowDuration(window.windowSeconds)
	}

	lowerPlanType := strings.ToLower(strings.TrimSpace(planType))
	switch window.name {
	case "individual":
		return "7)"
	case "secondary":
		return "5h"
	case "primary":
		if strings.Contains(lowerPlanType, "free") {
			return "7)"
		}
		return "5h"
	default:
		if strings.Contains(lowerPlanType, "free") {
			return "7)"
		}
	}
	return ""
}

func formatCodexQuotaWindowDuration(seconds int64) string {
	switch {
	case seconds <= 0:
		return ""
	case seconds >= 6*24*3600 && seconds <= 8*24*3600:
		return "7)"
	case seconds%(24*3600) == 0 && seconds >= 24*3600:
		return fmt.Sprintf("%d)", seconds/(24*3600))
	case seconds == 5*3600:
		return "5h"
	case seconds%3600 == 0 && seconds >= 3600:
		return fmt.Sprintf("%dh", seconds/3600)
	case seconds >= 24*3600:
		return fmt.Sprintf("%.1f)", float64(seconds)/86400)
	case seconds >= 3600:
		return fmt.Sprintf("%.1fh", float64(seconds)/3600)
	case seconds >= 60:
		return fmt.Sprintf("%d分钟", seconds/60)
	default:
		return fmt.Sprintf("%d秒", seconds)
	}
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

func quotaWindowLimitWindowSeconds(window *codexUsageWindowPayload) (int64, bool) {
	if window == nil {
		return 0, false
	}
	return probeInt64(
		window.LimitWindowSeconds,
		window.LimitWindowAlt,
		window.WindowSeconds,
		window.WindowSecondsAlt,
	)
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