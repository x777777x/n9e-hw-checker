// Package notify 分析结果的多路回写通道。
package notify

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/smtp"
	"strconv"
	"strings"
	"sync"
	"time"

	"kernel-hw-linkage/internal/config"
	"kernel-hw-linkage/internal/model"
)

// Notifier 单路回写通道。
type Notifier interface {
	Name() string
	Send(r model.Report) error
}

// Multi 串行执行全部通道，单路失败不影响其它。
type Multi struct {
	chans []Notifier
}

// NewMulti 组装所有已配置的通道。
func NewMulti(cfg *config.Config) *Multi {
	m := &Multi{}
	if cfg.Notify.WeCom.WebhookURL != "" {
		m.chans = append(m.chans, &WeCom{webhook: cfg.Notify.WeCom.WebhookURL})
	}
	if cfg.Notify.Mail.SMTPHost != "" {
		port := cfg.Notify.Mail.SMTPPort
		if port == 0 {
			port = 587 // 归一化放这里，避免并发 Send 内写字段（data race）
		}
		m.chans = append(m.chans, &Mail{
			host: cfg.Notify.Mail.SMTPHost,
			port: port,
			from: cfg.Notify.Mail.From,
			pass: cfg.Notify.Mail.Pass,
			to:   cfg.Notify.Mail.To,
		})
	}
	if cfg.Notify.UniPro.BaseURL != "" {
		m.chans = append(m.chans, &UniPro{
			baseURL: cfg.Notify.UniPro.BaseURL,
			token:   cfg.Notify.UniPro.Token,
			project: cfg.Notify.UniPro.ProjectID,
		})
	}
	return m
}

// Send 并发发送全部通道并汇总错误。
// 并发保证单路挂起（超时）不会拖死其它通道；每路自身带超时。
func (m *Multi) Send(r model.Report) []error {
	var mu sync.Mutex
	var errs []error
	var wg sync.WaitGroup
	for _, c := range m.chans {
		wg.Add(1)
		go func(ch Notifier) {
			defer wg.Done()
			if err := ch.Send(r); err != nil {
				mu.Lock()
				errs = append(errs, fmt.Errorf("%s: %w", ch.Name(), err))
				mu.Unlock()
			}
		}(c)
	}
	wg.Wait()
	return errs
}

// Enabled 返回是否配置了至少一个通道。
func (m *Multi) Enabled() bool { return len(m.chans) > 0 }

func renderMarkdown(r model.Report) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("## 硬件告警诊断报告\n\n"))
	b.WriteString(fmt.Sprintf("- 服务器: %s (%s)\n", r.Hostname, r.ServerIP))
	b.WriteString(fmt.Sprintf("- 类型: %v\n", r.Types))
	b.WriteString(fmt.Sprintf("- 时间窗: %s ~ %s\n", r.TimeStart, r.TimeEnd))
	if r.AnalysisFailed {
		b.WriteString("\n> 分析失败: " + r.Err + "\n")
		return b.String()
	}
	llm := r.LLM
	b.WriteString(fmt.Sprintf("\n**结论**: %s\n", llm.Conclusion))
	if len(llm.Causes) > 0 {
		b.WriteString("\n**可能原因**:\n")
		for _, c := range llm.Causes {
			b.WriteString("- " + c + "\n")
		}
	}
	if llm.Impact != "" {
		b.WriteString(fmt.Sprintf("\n**影响评估**: %s\n", llm.Impact))
	}
	if len(llm.Actions) > 0 {
		b.WriteString("\n**建议动作**:\n")
		for _, a := range llm.Actions {
			b.WriteString("- " + a + "\n")
		}
	}
	b.WriteString(fmt.Sprintf("\n**紧急度**: %s\n", llm.Urgency))
	if len(llm.Checks) > 0 {
		b.WriteString("\n**需进一步检查**:\n")
		for _, c := range llm.Checks {
			b.WriteString("- " + c + "\n")
		}
	}
	return b.String()
}

// ---------------- 企业微信 ----------------

// WeCom 企业微信群机器人。
type WeCom struct{ webhook string }

func (w *WeCom) Name() string { return "wecom" }

func (w *WeCom) Send(r model.Report) error {
	// 企微 markdown 上限约 4096 字节，超限截断（按 rune 避免切碎 UTF-8）
	content := renderMarkdown(r)
	content = truncateRunes(content, 4000)
	payload, _ := json.Marshal(map[string]interface{}{
		"msgtype":  "markdown",
		"markdown": map[string]string{"content": content},
	})
	hc := &http.Client{Timeout: 10 * time.Second}
	resp, err := hc.Post(w.webhook, "application/json", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("wecom status %d: %s", resp.StatusCode, truncate(string(raw), 300))
	}
	return nil
}

// ---------------- 邮件 ----------------

// Mail 通过 SMTP 发送分析报告。
// 使用带 deadline 的 TCP 连接，避免 SMTP 对端停滞导致永久挂起。
type Mail struct {
	host string
	port int
	from string
	pass string
	to   []string
}

func (m *Mail) Name() string { return "mail" }

func (m *Mail) Send(r model.Report) error {
	addr := net.JoinHostPort(m.host, strconv.Itoa(m.port))
	conn, err := net.DialTimeout("tcp", addr, 10*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(30 * time.Second)); err != nil {
		return err
	}
	c, err := smtp.NewClient(conn, m.host)
	if err != nil {
		return err
	}
	if m.pass != "" {
		if err := c.Auth(smtp.PlainAuth("", m.from, m.pass, m.host)); err != nil {
			return err
		}
	}
	if err := c.Mail(m.from); err != nil {
		return err
	}
	for _, to := range m.to {
		if err := c.Rcpt(to); err != nil {
			return err
		}
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write([]byte(buildMail(r, m.from, m.to))); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}

func buildMail(r model.Report, from string, to []string) string {
	// Subject 含中文需 RFC2047 编码，否则部分 MTA 乱码/拒绝
	subject := mime.QEncoding.Encode("utf-8", "[硬件告警] "+r.Hostname+" "+strings.Join(r.Types, ","))
	return "From: " + from + "\r\n" +
		"To: " + strings.Join(to, ",") + "\r\n" +
		"Subject: " + subject + "\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: text/plain; charset=UTF-8\r\n" +
		"\r\n" + renderMarkdown(r)
}

// ---------------- UniPro 工单 ----------------

// UniPro 工单系统（REST 建单）。
// 注意：UniPro 具体建单 API 路径/入参需按实际部署补全，当前为通用骨架。
type UniPro struct {
	baseURL string
	token   string
	project string
}

func (u *UniPro) Name() string { return "unipro" }

func (u *UniPro) Send(r model.Report) error {
	// TODO: 按实际 UniPro OpenAPI 调整路径与字段
	payload, _ := json.Marshal(map[string]interface{}{
		"title":       "[硬件告警] " + r.Hostname + " " + strings.Join(r.Types, ","),
		"description": renderMarkdown(r),
		"project_id":  u.project,
		"source":      "kernel-hw-linkage",
	})
	req, err := http.NewRequest(http.MethodPost, u.baseURL+"/api/v1/tickets", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if u.token != "" {
		req.Header.Set("Authorization", "Bearer "+u.token)
	}
	hc := &http.Client{Timeout: 10 * time.Second}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("unipro status %d: %s", resp.StatusCode, truncate(string(raw), 300))
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}
