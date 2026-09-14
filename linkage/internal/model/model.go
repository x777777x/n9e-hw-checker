// Package model 定义联动服务的核心数据结构。
package model

// WebhookEvent 为 n9e 告警回调 payload 的容错解析模型。
// 兼容嵌套 event.* 与扁平字段两种形态（不同 n9e 版本字段略有差异）。
type WebhookEvent struct {
	// 嵌套形态
	Event struct {
		IsRecovered bool              `json:"is_recovered"`
		TriggerTime int64             `json:"trigger_time"` // unix 秒
		RuleName    string            `json:"rule_name"`
		Labels      map[string]string `json:"labels"`
	} `json:"event"`
	// 扁平形态（兜底）
	IsRecovered bool              `json:"is_recovered"`
	TriggerTime int64             `json:"trigger_time"`
	RuleName    string            `json:"rule_name"`
	Labels      map[string]string `json:"labels"`
}

// Ident 从 payload 中容错提取主机标识。
func (w *WebhookEvent) Ident() string {
	labels := w.Labels
	if len(w.Event.Labels) > 0 {
		labels = w.Event.Labels
	}
	for _, k := range []string{"ident", "hostname", "host"} {
		if v := labels[k]; v != "" {
			return v
		}
	}
	return ""
}

// Type 从 payload 中容错提取硬件类型标签。
func (w *WebhookEvent) Type() string {
	labels := w.Labels
	if len(w.Event.Labels) > 0 {
		labels = w.Event.Labels
	}
	return labels["type"]
}

// Recovered 返回是否恢复事件。
func (w *WebhookEvent) Recovered() bool {
	return w.Event.IsRecovered || w.IsRecovered
}

// Time 返回触发时间（unix 秒），优先嵌套字段。
func (w *WebhookEvent) Time() int64 {
	if w.Event.TriggerTime > 0 {
		return w.Event.TriggerTime
	}
	return w.TriggerTime
}

// Rule 返回告警规则名。
func (w *WebhookEvent) Rule() string {
	if w.Event.RuleName != "" {
		return w.Event.RuleName
	}
	return w.RuleName
}

// AggregatedLine 聚合后的日志行。
type AggregatedLine struct {
	Message string `json:"message"`
	Count   int    `json:"count"`
	Type    string `json:"type,omitempty"`
}

// LLMResult 大模型结构化分析结果。
type LLMResult struct {
	Conclusion string   `json:"结论"`
	Causes     []string `json:"可能原因"`
	Impact     string   `json:"影响评估"`
	Actions    []string `json:"建议动作"`
	Urgency    string   `json:"紧急度"`
	Checks     []string `json:"需进一步检查"`
}

// Report 一次完整分析的结果，供各通知通道使用。
type Report struct {
	Hostname       string           `json:"hostname"`
	ServerIP       string           `json:"server_ip"`
	Types          []string         `json:"types"`
	RuleName       string           `json:"rule_name"`
	TimeStart      string           `json:"time_start"`
	TimeEnd        string           `json:"time_end"`
	Lines          []AggregatedLine `json:"lines"`
	LLM            LLMResult        `json:"llm"`
	AnalysisFailed bool             `json:"analysis_failed"`
	Err            string           `json:"err,omitempty"`
}
