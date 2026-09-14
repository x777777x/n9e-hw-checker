// Package agent 定义"告警触发后执行诊断"的两种方式：
//   - InProcess：进程内 ES 检索 + LLM 分析（默认）；
//   - HTTP：把事件交给外部 agent 执行器（加载 agent skill 自行检索+分析）。
//
// 目的：告警可以直接触发 agent 工作，简化"查日志 + 分析日志"的定制过程；
// 检索与分析的完整工作流见 skills/hw-alert-analyzer/。
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"kernel-hw-linkage/internal/model"
)

// Incident 交给 agent 的告警上下文。
type Incident struct {
	Hostname  string   `json:"hostname"`
	ServerIP  string   `json:"server_ip"`
	Types     []string `json:"types"`
	TimeStart string   `json:"time_start"`
	TimeEnd   string   `json:"time_end"`
	RuleName  string   `json:"rule_name"`
	Skill     string   `json:"skill,omitempty"`
}

// HTTPInvoker 调用外部 agent 执行器；执行器按 skill 自行检索 ES 并分析，返回 Report。
type HTTPInvoker struct {
	url     string
	skill   string
	timeout time.Duration
	http    *http.Client
}

// NewHTTP 创建外部 agent 执行器。
func NewHTTP(url, skill string, timeout time.Duration) *HTTPInvoker {
	return &HTTPInvoker{url: url, skill: skill, timeout: timeout, http: &http.Client{Timeout: timeout}}
}

// Run 提交事件并等待 Report。
func (h *HTTPInvoker) Run(ctx context.Context, inc Incident) (model.Report, error) {
	inc.Skill = h.skill
	body, _ := json.Marshal(inc)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.url, bytes.NewReader(body))
	if err != nil {
		return model.Report{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.http.Do(req)
	if err != nil {
		return model.Report{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return model.Report{}, fmt.Errorf("agent status %d: %s", resp.StatusCode, truncate(string(raw), 300))
	}
	var r model.Report
	if err := json.Unmarshal(raw, &r); err != nil {
		return model.Report{}, fmt.Errorf("agent decode report: %w", err)
	}
	return r, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
