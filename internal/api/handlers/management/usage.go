package management

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
)

type usageExportPayload struct {
	Version    int                      `json:"version"`
	ExportedAt time.Time                `json:"exported_at"`
	Usage      usage.StatisticsSnapshot `json:"usage"`
}

type usageImportPayload struct {
	Version int                      `json:"version"`
	Usage   usage.StatisticsSnapshot `json:"usage"`
}

type usageRequestEntry struct {
	API       string           `json:"api"`
	Model     string           `json:"model"`
	Timestamp time.Time        `json:"timestamp"`
	LatencyMs int64            `json:"latency_ms"`
	Source    string           `json:"source"`
	AuthIndex string           `json:"auth_index"`
	Tokens    usage.TokenStats `json:"tokens"`
	Failed    bool             `json:"failed"`
}

// GetUsageStatistics returns the in-memory request statistics snapshot.
func (h *Handler) GetUsageStatistics(c *gin.Context) {
	var snapshot usage.StatisticsSnapshot
	if h != nil && h.usageStats != nil {
		snapshot = h.usageStats.Snapshot()
	}

	page, pageSize := parseUsagePagination(c)
	requests := flattenUsageRequestDetails(snapshot)
	pagedRequests, total, totalPages := paginateUsageRequestEntries(requests, page, pageSize)

	c.JSON(http.StatusOK, gin.H{
		"usage":           snapshot,
		"failed_requests": snapshot.FailureCount,
		"request_details": pagedRequests,
		"page":            page,
		"page_size":       pageSize,
		"total":           total,
		"total_pages":     totalPages,
	})
}

// ExportUsageStatistics returns a complete usage snapshot for backup/migration.
func (h *Handler) ExportUsageStatistics(c *gin.Context) {
	var snapshot usage.StatisticsSnapshot
	if h != nil && h.usageStats != nil {
		snapshot = h.usageStats.Snapshot()
	}
	c.JSON(http.StatusOK, usageExportPayload{
		Version:    1,
		ExportedAt: time.Now().UTC(),
		Usage:      snapshot,
	})
}

// ImportUsageStatistics merges a previously exported usage snapshot into memory.
func (h *Handler) ImportUsageStatistics(c *gin.Context) {
	if h == nil || h.usageStats == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "usage statistics unavailable"})
		return
	}

	data, err := c.GetRawData()
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "failed to read request body"})
		return
	}

	var payload usageImportPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid json"})
		return
	}
	if payload.Version != 0 && payload.Version != 1 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "unsupported version"})
		return
	}

	result := h.usageStats.MergeSnapshot(payload.Usage)
	snapshot := h.usageStats.Snapshot()
	c.JSON(http.StatusOK, gin.H{
		"added":           result.Added,
		"skipped":         result.Skipped,
		"total_requests":  snapshot.TotalRequests,
		"failed_requests": snapshot.FailureCount,
	})
}

func parseUsagePagination(c *gin.Context) (page int, pageSize int) {
	page = 1
	pageSize = 50

	if c == nil {
		return page, pageSize
	}

	if rawPage := strings.TrimSpace(c.Query("page")); rawPage != "" {
		if parsed, err := strconvAtoi(rawPage); err == nil && parsed > 0 {
			page = parsed
		}
	}

	if rawPageSize := strings.TrimSpace(c.Query("page_size")); rawPageSize != "" {
		if parsed, err := strconvAtoi(rawPageSize); err == nil && parsed > 0 {
			pageSize = parsed
		}
	}

	switch {
	case pageSize <= 0:
		pageSize = 50
	case pageSize > 200:
		pageSize = 200
	}

	return page, pageSize
}

func flattenUsageRequestDetails(snapshot usage.StatisticsSnapshot) []usageRequestEntry {
	if len(snapshot.APIs) == 0 {
		return []usageRequestEntry{}
	}

	entries := make([]usageRequestEntry, 0)
	for apiName, apiSnapshot := range snapshot.APIs {
		for modelName, modelSnapshot := range apiSnapshot.Models {
			for _, detail := range modelSnapshot.Details {
				entries = append(entries, usageRequestEntry{
					API:       apiName,
					Model:     modelName,
					Timestamp: detail.Timestamp,
					LatencyMs: detail.LatencyMs,
					Source:    detail.Source,
					AuthIndex: detail.AuthIndex,
					Tokens:    detail.Tokens,
					Failed:    detail.Failed,
				})
			}
		}
	}

	sort.Slice(entries, func(i, j int) bool {
		left := entries[i].Timestamp
		right := entries[j].Timestamp
		if left.Equal(right) {
			if entries[i].API == entries[j].API {
				return entries[i].Model < entries[j].Model
			}
			return entries[i].API < entries[j].API
		}
		return left.After(right)
	})

	return entries
}

func stripUsageSnapshotDetails(snapshot *usage.StatisticsSnapshot) {
	if snapshot == nil || len(snapshot.APIs) == 0 {
		return
	}

	for apiName, apiSnapshot := range snapshot.APIs {
		if len(apiSnapshot.Models) == 0 {
			continue
		}
		models := make(map[string]usage.ModelSnapshot, len(apiSnapshot.Models))
		for modelName, modelSnapshot := range apiSnapshot.Models {
			modelSnapshot.Details = nil
			models[modelName] = modelSnapshot
		}
		apiSnapshot.Models = models
		snapshot.APIs[apiName] = apiSnapshot
	}
}

func paginateUsageRequestEntries(entries []usageRequestEntry, page int, pageSize int) ([]usageRequestEntry, int, int) {
	total := len(entries)
	if page <= 0 {
		page = 1
	}
	if pageSize <= 0 {
		pageSize = 50
	}

	if total == 0 {
		return []usageRequestEntry{}, 0, 0
	}

	totalPages := (total + pageSize - 1) / pageSize
	if page > totalPages {
		return []usageRequestEntry{}, total, totalPages
	}

	start := (page - 1) * pageSize
	if start >= total {
		return []usageRequestEntry{}, total, totalPages
	}

	end := start + pageSize
	if end > total {
		end = total
	}

	return entries[start:end], total, totalPages
}

func strconvAtoi(raw string) (int, error) {
	sign := 1
	value := 0
	for i, ch := range raw {
		if i == 0 && ch == '-' {
			sign = -1
			continue
		}
		if ch < '0' || ch > '9' {
			return 0, errInvalidInteger
		}
		value = value*10 + int(ch-'0')
	}
	return sign * value, nil
}

var errInvalidInteger = &usageParseError{message: "invalid integer"}

type usageParseError struct {
	message string
}

func (e *usageParseError) Error() string {
	if e == nil {
		return "invalid integer"
	}
	return e.message
}