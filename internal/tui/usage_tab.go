package tui

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

type usageTokenBreakdown struct {
	InputTokens     int64 `json:"input_tokens"`
	OutputTokens    int64 `json:"output_tokens"`
	CachedTokens    int64 `json:"cached_tokens"`
	ReasoningTokens int64 `json:"reasoning_tokens"`
}

// usageRequestDetail mirrors the paginated request_details payload returned by the management API.
type usageRequestDetail struct {
	API       string              `json:"api"`
	Model     string              `json:"model"`
	Timestamp time.Time           `json:"timestamp"`
	LatencyMs int64               `json:"latency_ms"`
	Source    string              `json:"source"`
	AuthIndex string              `json:"auth_index"`
	Tokens    usageTokenBreakdown `json:"tokens"`
	Failed    bool                `json:"failed"`
}

// usageTabModel displays usage statistics with charts, breakdowns, and paginated request details.
type usageTabModel struct {
	client         *Client
	viewport       viewport.Model
	usage          map[string]any
	requestDetails []usageRequestDetail
	err            error
	width          int
	height         int
	ready          bool
	page           int
	pageSize       int
	total          int
	totalPages     int
}

type usageDataMsg struct {
	usage map[string]any
	err   error
}

func newUsageTabModel(client *Client) usageTabModel {
	return usageTabModel{
		client:   client,
		page:     1,
		pageSize: 50,
	}
}

func (m usageTabModel) Init() tea.Cmd {
	return m.fetchData
}

func (m usageTabModel) fetchData() tea.Msg {
	page := m.page
	if page <= 0 {
		page = 1
	}
	pageSize := m.pageSize
	if pageSize <= 0 {
		pageSize = 50
	}

	usage, err := m.client.GetUsagePage(page, pageSize)
	return usageDataMsg{usage: usage, err: err}
}

func (m usageTabModel) Update(msg tea.Msg) (usageTabModel, tea.Cmd) {
	switch msg := msg.(type) {
	case localeChangedMsg:
		m.viewport.SetContent(m.renderContent())
		return m, nil

	case usageDataMsg:
		if msg.err != nil {
			m.err = msg.err
		} else {
			m.err = nil
			m.usage = msg.usage
			m.page = getIntValue(msg.usage, "page", m.page)
			if m.page <= 0 {
				m.page = 1
			}
			m.pageSize = getIntValue(msg.usage, "page_size", m.pageSize)
			if m.pageSize <= 0 {
				m.pageSize = 50
			}
			m.total = getIntValue(msg.usage, "total", 0)
			m.totalPages = getIntValue(msg.usage, "total_pages", 0)
			m.requestDetails = extractUsageRequestDetails(msg.usage)
		}
		m.viewport.SetContent(m.renderContent())
		return m, nil

	case tea.KeyMsg:
		switch msg.String() {
		case "r":
			return m, m.fetchData
		case "n", "right", "l":
			if m.totalPages > 0 && m.page < m.totalPages {
				m.page++
				return m, m.fetchData
			}
			return m, nil
		case "p", "left", "h":
			if m.page > 1 {
				m.page--
				return m, m.fetchData
			}
			return m, nil
		case "]", "=", "+":
			nextPageSize := nextUsagePageSize(m.pageSize, 1)
			if nextPageSize != m.pageSize {
				m.pageSize = nextPageSize
				m.page = 1
				return m, m.fetchData
			}
			return m, nil
		case "[", "-":
			nextPageSize := nextUsagePageSize(m.pageSize, -1)
			if nextPageSize != m.pageSize {
				m.pageSize = nextPageSize
				m.page = 1
				return m, m.fetchData
			}
			return m, nil
		}

		var cmd tea.Cmd
		m.viewport, cmd = m.viewport.Update(msg)
		return m, cmd
	}

	var cmd tea.Cmd
	m.viewport, cmd = m.viewport.Update(msg)
	return m, cmd
}

func (m *usageTabModel) SetSize(w, h int) {
	m.width = w
	m.height = h
	if !m.ready {
		m.viewport = viewport.New(w, h)
		m.viewport.SetContent(m.renderContent())
		m.ready = true
	} else {
		m.viewport.Width = w
		m.viewport.Height = h
	}
}

func (m usageTabModel) View() string {
	if !m.ready {
		return T("loading")
	}
	return m.viewport.View()
}

func (m usageTabModel) renderContent() string {
	var sb strings.Builder

	sb.WriteString(titleStyle.Render(T("usage_title")))
	sb.WriteString("\n")
	sb.WriteString(helpStyle.Render(T("usage_help")))
	sb.WriteString("\n")

	currentPage := m.page
	if currentPage <= 0 {
		currentPage = 1
	}
	pageCount := m.totalPages
	if pageCount < 0 {
		pageCount = 0
	}
	pageSummary := fmt.Sprintf(
		"%s %d/%d • %s %d • %s %d",
		T("usage_page"),
		currentPage,
		pageCount,
		T("usage_page_size"),
		m.pageSize,
		T("usage_total_items"),
		m.total,
	)
	sb.WriteString(helpStyle.Render(" " + pageSummary))
	sb.WriteString("\n\n")

	if m.err != nil {
		sb.WriteString(errorStyle.Render("⚠ Error: " + m.err.Error()))
		sb.WriteString("\n")
		return sb.String()
	}

	if m.usage == nil {
		sb.WriteString(subtitleStyle.Render(T("usage_no_data")))
		sb.WriteString("\n")
		return sb.String()
	}

	usageMap, _ := m.usage["usage"].(map[string]any)
	if usageMap == nil {
		sb.WriteString(subtitleStyle.Render(T("usage_no_data")))
		sb.WriteString("\n")
		return sb.String()
	}

	totalReqs := int64(getFloat(usageMap, "total_requests"))
	successCnt := int64(getFloat(usageMap, "success_count"))
	failureCnt := int64(getFloat(usageMap, "failure_count"))
	totalTokens := int64(getFloat(usageMap, "total_tokens"))

	// ━━━ Overview Cards ━━━
	cardWidth := 20
	if m.width > 0 {
		cardWidth = (m.width - 6) / 4
		if cardWidth < 16 {
			cardWidth = 16
		}
	}
	cardStyle := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("240")).
		Padding(0, 1).
		Width(cardWidth).
		Height(3)

	// Total Requests
	card1 := cardStyle.Copy().BorderForeground(lipgloss.Color("111")).Render(fmt.Sprintf(
		"%s\n%s\n%s",
		lipgloss.NewStyle().Foreground(colorMuted).Render(T("usage_total_reqs")),
		lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("111")).Render(fmt.Sprintf("%d", totalReqs)),
		lipgloss.NewStyle().Foreground(colorMuted).Render(fmt.Sprintf("● %s: %d  ● %s: %d", T("usage_success"), successCnt, T("usage_failure"), failureCnt)),
	))

	// Total Tokens
	card2 := cardStyle.Copy().BorderForeground(lipgloss.Color("214")).Render(fmt.Sprintf(
		"%s\n%s\n%s",
		lipgloss.NewStyle().Foreground(colorMuted).Render(T("usage_total_tokens")),
		lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("214")).Render(formatLargeNumber(totalTokens)),
		lipgloss.NewStyle().Foreground(colorMuted).Render(fmt.Sprintf("%s: %s", T("usage_total_token_l"), formatLargeNumber(totalTokens))),
	))

	// RPM
	rpm := float64(0)
	if totalReqs > 0 {
		if rByH, ok := usageMap["requests_by_hour"].(map[string]any); ok && len(rByH) > 0 {
			rpm = float64(totalReqs) / float64(len(rByH)) / 60.0
		}
	}
	card3 := cardStyle.Copy().BorderForeground(lipgloss.Color("76")).Render(fmt.Sprintf(
		"%s\n%s\n%s",
		lipgloss.NewStyle().Foreground(colorMuted).Render(T("usage_rpm")),
		lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("76")).Render(fmt.Sprintf("%.2f", rpm)),
		lipgloss.NewStyle().Foreground(colorMuted).Render(fmt.Sprintf("%s: %d", T("usage_total_reqs"), totalReqs)),
	))

	// TPM
	tpm := float64(0)
	if totalTokens > 0 {
		if tByH, ok := usageMap["tokens_by_hour"].(map[string]any); ok && len(tByH) > 0 {
			tpm = float64(totalTokens) / float64(len(tByH)) / 60.0
		}
	}
	card4 := cardStyle.Copy().BorderForeground(lipgloss.Color("170")).Render(fmt.Sprintf(
		"%s\n%s\n%s",
		lipgloss.NewStyle().Foreground(colorMuted).Render(T("usage_tpm")),
		lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("170")).Render(fmt.Sprintf("%.2f", tpm)),
		lipgloss.NewStyle().Foreground(colorMuted).Render(fmt.Sprintf("%s: %s", T("usage_total_tokens"), formatLargeNumber(totalTokens))),
	))

	sb.WriteString(lipgloss.JoinHorizontal(lipgloss.Top, card1, " ", card2, " ", card3, " ", card4))
	sb.WriteString("\n\n")

	// ━━━ Requests by Hour (ASCII bar chart) ━━━
	if rByH, ok := usageMap["requests_by_hour"].(map[string]any); ok && len(rByH) > 0 {
		sb.WriteString(lipgloss.NewStyle().Bold(true).Foreground(colorHighlight).Render(T("usage_req_by_hour")))
		sb.WriteString("\n")
		sb.WriteString(strings.Repeat("─", minInt(m.width, 60)))
		sb.WriteString("\n")
		sb.WriteString(renderBarChart(rByH, m.width-6, lipgloss.Color("111")))
		sb.WriteString("\n")
	}

	// ━━━ Tokens by Hour ━━━
	if tByH, ok := usageMap["tokens_by_hour"].(map[string]any); ok && len(tByH) > 0 {
		sb.WriteString(lipgloss.NewStyle().Bold(true).Foreground(colorHighlight).Render(T("usage_tok_by_hour")))
		sb.WriteString("\n")
		sb.WriteString(strings.Repeat("─", minInt(m.width, 60)))
		sb.WriteString("\n")
		sb.WriteString(renderBarChart(tByH, m.width-6, lipgloss.Color("214")))
		sb.WriteString("\n")
	}

	// ━━━ Requests by Day ━━━
	if rByD, ok := usageMap["requests_by_day"].(map[string]any); ok && len(rByD) > 0 {
		sb.WriteString(lipgloss.NewStyle().Bold(true).Foreground(colorHighlight).Render(T("usage_req_by_day")))
		sb.WriteString("\n")
		sb.WriteString(strings.Repeat("─", minInt(m.width, 60)))
		sb.WriteString("\n")
		sb.WriteString(renderBarChart(rByD, m.width-6, lipgloss.Color("76")))
		sb.WriteString("\n")
	}

	// ━━━ API Detail Stats ━━━
	if apis, ok := usageMap["apis"].(map[string]any); ok && len(apis) > 0 {
		sb.WriteString(lipgloss.NewStyle().Bold(true).Foreground(colorHighlight).Render(T("usage_api_detail")))
		sb.WriteString("\n")
		sb.WriteString(strings.Repeat("─", minInt(m.width, 80)))
		sb.WriteString("\n")

		header := fmt.Sprintf("  %-30s %10s %12s", "API", T("requests"), T("tokens"))
		sb.WriteString(tableHeaderStyle.Render(header))
		sb.WriteString("\n")

		for apiName, apiSnap := range apis {
			if apiMap, ok := apiSnap.(map[string]any); ok {
				apiReqs := int64(getFloat(apiMap, "total_requests"))
				apiToks := int64(getFloat(apiMap, "total_tokens"))

				row := fmt.Sprintf("  %-30s %10d %12s",
					truncate(maskKey(apiName), 30), apiReqs, formatLargeNumber(apiToks))
				sb.WriteString(lipgloss.NewStyle().Bold(true).Render(row))
				sb.WriteString("\n")

				// Per-model breakdown
				if models, ok := apiMap["models"].(map[string]any); ok {
					for model, v := range models {
						if stats, ok := v.(map[string]any); ok {
							mReqs := int64(getFloat(stats, "total_requests"))
							mToks := int64(getFloat(stats, "total_tokens"))
							mRow := fmt.Sprintf("    ├─ %-28s %10d %12s",
								truncate(model, 28), mReqs, formatLargeNumber(mToks))
							sb.WriteString(tableCellStyle.Render(mRow))
							sb.WriteString("\n")

							// Token type breakdown from details
							sb.WriteString(m.renderTokenBreakdown(stats))
						}
					}
				}
			}
		}
	}

	// ━━━ Request Details ━━━
	sb.WriteString("\n")
	sb.WriteString(lipgloss.NewStyle().Bold(true).Foreground(colorHighlight).Render(T("usage_request_details")))
	sb.WriteString("\n")
	sb.WriteString(strings.Repeat("─", minInt(m.width, 100)))
	sb.WriteString("\n")

	if len(m.requestDetails) == 0 {
		sb.WriteString(subtitleStyle.Render(T("usage_no_request_details")))
		sb.WriteString("\n")
		return sb.String()
	}

	header := fmt.Sprintf(
		"  %-14s %-14s %-24s %-7s %8s %10s",
		T("usage_request_time"),
		T("usage_request_api"),
		T("usage_request_model"),
		T("usage_request_status"),
		T("usage_request_latency"),
		T("usage_request_tokens"),
	)
	sb.WriteString(tableHeaderStyle.Render(header))
	sb.WriteString("\n")

	for _, detail := range m.requestDetails {
		timestamp := ""
		if !detail.Timestamp.IsZero() {
			timestamp = detail.Timestamp.Local().Format("01-02 15:04:05")
		}
		status := T("usage_request_ok")
		if detail.Failed {
			status = T("usage_request_failed")
		}
		tokenTotal := usageRequestTokenTotal(detail.Tokens)

		row := fmt.Sprintf(
			"  %-14s %-14s %-24s %-7s %8dms %10s",
			truncate(timestamp, 14),
			truncate(maskKey(detail.API), 14),
			truncate(detail.Model, 24),
			status,
			detail.LatencyMs,
			formatLargeNumber(tokenTotal),
		)
		sb.WriteString(tableCellStyle.Render(row))
		sb.WriteString("\n")

		extraParts := make([]string, 0, 3)
		if detail.Source != "" {
			extraParts = append(extraParts, fmt.Sprintf("%s: %s", T("usage_request_source"), detail.Source))
		}
		if detail.AuthIndex != "" {
			extraParts = append(extraParts, fmt.Sprintf("%s: %s", T("usage_request_auth"), detail.AuthIndex))
		}
		tokenBreakdown := summarizeUsageRequestTokens(detail.Tokens)
		if tokenBreakdown != "" {
			extraParts = append(extraParts, fmt.Sprintf("%s: %s", T("usage_request_tokens"), tokenBreakdown))
		}
		if len(extraParts) > 0 {
			sb.WriteString(lipgloss.NewStyle().Foreground(colorMuted).Render("    " + strings.Join(extraParts, "  •  ")))
			sb.WriteString("\n")
		}
	}

	sb.WriteString("\n")
	return sb.String()
}

// renderTokenBreakdown aggregates input/output/cached/reasoning tokens from model details.
func (m usageTabModel) renderTokenBreakdown(modelStats map[string]any) string {
	tokenStats, ok := modelStats["token_stats"].(map[string]any)
	if !ok || len(tokenStats) == 0 {
		return ""
	}

	inputTotal := int64(getFloat(tokenStats, "input_tokens"))
	outputTotal := int64(getFloat(tokenStats, "output_tokens"))
	cachedTotal := int64(getFloat(tokenStats, "cached_tokens"))
	reasoningTotal := int64(getFloat(tokenStats, "reasoning_tokens"))

	if inputTotal == 0 && outputTotal == 0 && cachedTotal == 0 && reasoningTotal == 0 {
		return ""
	}

	parts := []string{}
	if inputTotal > 0 {
		parts = append(parts, fmt.Sprintf("%s:%s", T("usage_input"), formatLargeNumber(inputTotal)))
	}
	if outputTotal > 0 {
		parts = append(parts, fmt.Sprintf("%s:%s", T("usage_output"), formatLargeNumber(outputTotal)))
	}
	if cachedTotal > 0 {
		parts = append(parts, fmt.Sprintf("%s:%s", T("usage_cached"), formatLargeNumber(cachedTotal)))
	}
	if reasoningTotal > 0 {
		parts = append(parts, fmt.Sprintf("%s:%s", T("usage_reasoning"), formatLargeNumber(reasoningTotal)))
	}

	return fmt.Sprintf("    │  %s\n",
		lipgloss.NewStyle().Foreground(colorMuted).Render(strings.Join(parts, "  ")))
}

// renderBarChart renders a simple ASCII horizontal bar chart.
func renderBarChart(data map[string]any, maxBarWidth int, barColor lipgloss.Color) string {
	if maxBarWidth < 10 {
		maxBarWidth = 10
	}

	// Sort keys
	keys := make([]string, 0, len(data))
	for k := range data {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	// Find max value
	maxVal := float64(0)
	for _, k := range keys {
		v := getFloat(data, k)
		if v > maxVal {
			maxVal = v
		}
	}
	if maxVal == 0 {
		return ""
	}

	barStyle := lipgloss.NewStyle().Foreground(barColor)
	var sb strings.Builder

	labelWidth := 12
	barAvail := maxBarWidth - labelWidth - 12
	if barAvail < 5 {
		barAvail = 5
	}

	for _, k := range keys {
		v := getFloat(data, k)
		barLen := int(v / maxVal * float64(barAvail))
		if barLen < 1 && v > 0 {
			barLen = 1
		}
		bar := strings.Repeat("█", barLen)
		label := k
		if len(label) > labelWidth {
			label = label[:labelWidth]
		}
		sb.WriteString(fmt.Sprintf("  %-*s %s %s\n",
			labelWidth, label,
			barStyle.Render(bar),
			lipgloss.NewStyle().Foreground(colorMuted).Render(fmt.Sprintf("%.0f", v)),
		))
	}

	return sb.String()
}

func getIntValue(m map[string]any, key string, fallback int) int {
	if m == nil {
		return fallback
	}
	v, ok := m[key]
	if !ok || v == nil {
		return fallback
	}

	switch value := v.(type) {
	case float64:
		return int(value)
	case float32:
		return int(value)
	case int:
		return value
	case int64:
		return int(value)
	case int32:
		return int(value)
	case json.Number:
		if parsed, err := value.Int64(); err == nil {
			return int(parsed)
		}
	}

	return fallback
}

func extractUsageRequestDetails(wrapper map[string]any) []usageRequestDetail {
	if wrapper == nil {
		return nil
	}
	rawDetails, ok := wrapper["request_details"]
	if !ok || rawDetails == nil {
		return nil
	}

	rawJSON, err := json.Marshal(rawDetails)
	if err != nil {
		return nil
	}

	var details []usageRequestDetail
	if err := json.Unmarshal(rawJSON, &details); err != nil {
		return nil
	}
	return details
}

func usageRequestTokenTotal(tokens usageTokenBreakdown) int64 {
	return tokens.InputTokens + tokens.OutputTokens + tokens.CachedTokens + tokens.ReasoningTokens
}

func summarizeUsageRequestTokens(tokens usageTokenBreakdown) string {
	parts := []string{}
	if tokens.InputTokens > 0 {
		parts = append(parts, fmt.Sprintf("%s:%s", T("usage_input"), formatLargeNumber(tokens.InputTokens)))
	}
	if tokens.OutputTokens > 0 {
		parts = append(parts, fmt.Sprintf("%s:%s", T("usage_output"), formatLargeNumber(tokens.OutputTokens)))
	}
	if tokens.CachedTokens > 0 {
		parts = append(parts, fmt.Sprintf("%s:%s", T("usage_cached"), formatLargeNumber(tokens.CachedTokens)))
	}
	if tokens.ReasoningTokens > 0 {
		parts = append(parts, fmt.Sprintf("%s:%s", T("usage_reasoning"), formatLargeNumber(tokens.ReasoningTokens)))
	}
	return strings.Join(parts, "  ")
}

func nextUsagePageSize(current int, direction int) int {
	options := []int{10, 25, 50, 100, 200}
	if current <= 0 {
		current = 50
	}

	index := 0
	for i, option := range options {
		if option >= current {
			index = i
			if option == current {
				break
			}
		}
	}
	if direction > 0 {
		if index < len(options)-1 {
			return options[index+1]
		}
		return options[len(options)-1]
	}
	if direction < 0 {
		if options[index] > current && index > 0 {
			return options[index-1]
		}
		if index > 0 {
			return options[index-1]
		}
		return options[0]
	}
	return current
}