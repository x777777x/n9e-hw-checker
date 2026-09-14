// Package llm 调用私有化大模型（openai 兼容 /v1/chat/completions）。
package llm

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"kernel-hw-linkage/internal/model"
)

// defaultSystemPrompt 为分析规则兜底（与 prompts/analyze.txt 保持一致）。
const defaultSystemPrompt = `你是资深 SRE 硬件排障专家，负责根据服务器系统日志对硬件告警进行诊断。

分析规则：
1. 只依据给定的日志行进行判断，严禁臆测或编造日志中未出现的信息；信息不足时在"需进一步检查"中明确列出待确认项。
2. 先对日志行做硬件组件归类（网卡/内存/CPU/磁盘/RAID 卡/PCIe 总线/温度/内核），识别关键错误签名。
3. 综合多条日志判断故障性质：瞬时抖动还是持续故障、单点还是多点、有无级联。
4. 结论要可执行：明确问题组件、影响面、优先处置动作与后续检查项。

输出必须严格为如下 JSON（不得输出 JSON 之外的任何说明）：
{"结论":"","可能原因":[""],"影响评估":"","建议动作":[""],"紧急度":"高|中|低","需进一步检查":[""]}`

// Client 私有化 LLM 客户端。
type Client struct {
	baseURL string
	model   string
	apiKey  string
	system  string
	http    *http.Client
}

// New 创建 LLM 客户端。baseURL 形如 http://llm.example.com/v1。
// system 为分析规则（可来自共享的 prompts/analyze.txt），为空时使用内置兜底。
func New(baseURL, model, apiKey, system string, timeout time.Duration) *Client {
	if system == "" {
		system = defaultSystemPrompt
	}
	return &Client{baseURL: baseURL, model: model, apiKey: apiKey, system: system, http: &http.Client{Timeout: timeout}}
}

type chatReq struct {
	Model    string    `json:"model"`
	Messages []message `json:"messages"`
}

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatResp struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
}

// Analyze 依据聚合日志生成结构化诊断意见。
func (c *Client) Analyze(hostname, ip string, types []string, start, end time.Time, lines []model.AggregatedLine) (model.LLMResult, error) {
	var out model.LLMResult

	logText := ""
	for _, l := range lines {
		if l.Count > 1 {
			logText += fmt.Sprintf("[%s] %s (x%d)\n", l.Type, l.Message, l.Count)
		} else {
			logText += fmt.Sprintf("[%s] %s\n", l.Type, l.Message)
		}
	}
	if logText == "" {
		logText = "(无命中日志)"
	}

	prompt := fmt.Sprintf(`服务器: %s (%s)
硬件告警类型: %v
时间窗: %s ~ %s
命中的日志片段:
-----
%s
-----`, hostname, ip, types, start.Format("2006-01-02 15:04:05"), end.Format("2006-01-02 15:04:05"), logText)

	body, _ := json.Marshal(chatReq{
		Model: c.model,
		Messages: []message{
			{Role: "system", Content: c.system},
			{Role: "user", Content: prompt},
		},
	})
	req, err := http.NewRequest(http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return out, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return out, fmt.Errorf("llm status %d: %s", resp.StatusCode, truncate(string(raw), 300))
	}
	var r chatResp
	if err := json.Unmarshal(raw, &r); err != nil {
		return out, err
	}
	if len(r.Choices) == 0 {
		return out, fmt.Errorf("llm empty choices")
	}
	content := r.Choices[0].Message.Content
	// 先按原始内容解析；失败则剥离 ```json 围栏/多余文本后重试；
	// 仍失败才把原文兜底放入结论。
	if err := json.Unmarshal([]byte(content), &out); err != nil {
		if err2 := json.Unmarshal([]byte(extractJSON(content)), &out); err2 != nil {
			out.Conclusion = content
		}
	}
	return out, nil
}

// extractJSON 从模型输出中提取 JSON 片段：
// 1) 若含 ```json 围栏，取最后一组围栏内容；
// 2) 否则若含 think 块（` response`），截取最后一个 think 结束标签之后的内容；
// 3) 逐 '{' 位置尝试 json.Valid 截取，直到得到合法 JSON；
// 4) 全部失败返回空串（调用方回退原文）。
func extractJSON(s string) string {
	s = strings.TrimSpace(s)

	// 候选 1：```json ... ``` 围栏（取最后一组）
	if strings.Count(s, "```") >= 2 {
		i := strings.LastIndex(s, "```")
		j := strings.LastIndex(s[:i], "```")
		cand := strings.TrimSpace(s[j+3 : i])
		cand = strings.TrimSpace(strings.TrimPrefix(cand, "json"))
		if json.Valid([]byte(cand)) {
			return cand
		}
	}

	// 候选 2：剥离 think 块（deepseek-r1：开头  response，结尾  response）
	t := s
	for _, end := range []string{"  response", "  response", "</think>"} {
		if k := strings.LastIndex(t, end); k >= 0 {
			t = strings.TrimSpace(t[k+len(end):])
			break
		}
	}

	// 候选 3：逐 '{' 尝试合法 JSON（跳过 think 内部花括号）
	for {
		i := strings.Index(t, "{")
		if i < 0 {
			return ""
		}
		j := strings.LastIndex(t, "}")
		if j < 0 || j < i {
			return ""
		}
		cand := t[i : j+1]
		if json.Valid([]byte(cand)) {
			return cand
		}
		t = t[i+1:]
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
