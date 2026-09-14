// Package config 加载联动服务配置（JSON 文件 + 环境变量覆盖密钥）。
package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
)

// Config 联动服务全部配置。
// 说明：设计文档原定 YAML，本实现为标准库 JSON（无第三方依赖）。
// 接入网络后可将此处替换为 yaml/viper 解析，结构不变。
type Config struct {
	Server struct {
		Addr  string `json:"addr"`
		Token string `json:"token"` // n9e 回调共享密钥（可被环境变量 LINKAGE_TOKEN 覆盖）
	} `json:"server"`

	ES struct {
		Addr  string `json:"addr"`
		User  string `json:"user"`
		Pass  string `json:"pass"`
		Index string `json:"index"`
		// TypeKeywords: type(nic/mem/...) -> ES match_phrase 关键词列表。
		// 需与采集端 patterns.conf 的 include 类别对齐。
		TypeKeywords map[string][]string `json:"type_keywords"`
	} `json:"es"`

	LLM struct {
		BaseURL  string `json:"base_url"` // 私有化 openai 兼容端点
		Model    string `json:"model"`
		APIKey   string `json:"api_key"`
		TimeoutS int    `json:"timeout_s"`
		// PromptFile 分析规则文件（与 agent skill 共用，默认内置兜底）
		PromptFile string `json:"prompt_file"`
	} `json:"llm"`

	// Agent 告警触发时是否直接交给外部 agent（而非进程内 ES+LLM）。
	Agent struct {
		Enabled     bool   `json:"enabled"`
		ExecutorURL string `json:"executor_url"` // agent 执行器端点，接收 Incident JSON，返回 Report JSON
		Skill       string `json:"skill"`        // 使用的 agent skill 名（如 hw-alert-analyzer）
		TimeoutS    int    `json:"timeout_s"`
	} `json:"agent"`

	Resolve struct {
		// Inventory: hostname(ident) -> server IP。
		// 优先使用；缺失时用 ES 反查兜底。
		Inventory map[string]string `json:"inventory"`
	} `json:"resolve"`

	Window struct {
		DedupMinutes    int `json:"dedup_minutes"`      // 去重窗口（默认 10）
		MergeSeconds    int `json:"merge_seconds"`      // 多类型合并等待（默认 60）
		ESLookbackMin   int `json:"es_lookback_min"`    // ES 查询向前回溯（默认 10）
		MaxLines        int `json:"max_lines"`          // 交给 LLM 的日志行上限（默认 50）
		MaxCharsPerLine int `json:"max_chars_per_line"` // 单行截断（默认 200）
		MaxConcurrent   int `json:"max_concurrent"`     // 并发分析上限（默认 5）
	} `json:"window"`

	Notify struct {
		WeCom struct {
			WebhookURL string `json:"webhook_url"`
		} `json:"wecom"`
		Mail struct {
			SMTPHost string   `json:"smtp_host"`
			SMTPPort int      `json:"smtp_port"`
			From     string   `json:"from"`
			Pass     string   `json:"pass"`
			To       []string `json:"to"`
		} `json:"mail"`
		UniPro struct {
			BaseURL   string `json:"base_url"`
			Token     string `json:"token"`
			ProjectID string `json:"project_id"`
		} `json:"unipro"`
	} `json:"notify"`
}

// Default 返回带默认值的配置。
func Default() *Config {
	c := &Config{}
	c.Server.Addr = ":8080"
	c.ES.Index = "syslog-system"
	c.LLM.TimeoutS = 30
	c.Agent.TimeoutS = 120
	c.Window.DedupMinutes = 10
	c.Window.MergeSeconds = 60
	c.Window.ESLookbackMin = 10
	c.Window.MaxLines = 50
	c.Window.MaxCharsPerLine = 200
	c.Window.MaxConcurrent = 5
	return c
}

// sanitize 校验并修正配置：防止除零/空 token 关闭鉴权/关键依赖缺失等部署事故。
func (c *Config) sanitize() error {
	if c.Server.Token == "" {
		return fmt.Errorf("server.token 不能为空（webhook 鉴权依赖它）")
	}
	if c.Window.DedupMinutes <= 0 {
		c.Window.DedupMinutes = 10
	}
	if c.Window.MergeSeconds <= 0 {
		c.Window.MergeSeconds = 60
	}
	if c.Window.ESLookbackMin < 0 {
		c.Window.ESLookbackMin = 10
	}
	if c.Window.MaxLines <= 0 {
		c.Window.MaxLines = 50
	}
	if c.Window.MaxCharsPerLine <= 0 {
		c.Window.MaxCharsPerLine = 200
	}
	if c.Window.MaxConcurrent <= 0 {
		c.Window.MaxConcurrent = 5
	}
	// 超时钳制：0/负数会变成 http.Client{Timeout:0}（无限超时）
	if c.LLM.TimeoutS <= 0 {
		c.LLM.TimeoutS = 30
	}
	if c.Agent.TimeoutS <= 0 {
		c.Agent.TimeoutS = 120
	}
	if c.Agent.Enabled {
		if c.Agent.ExecutorURL == "" {
			return fmt.Errorf("agent.enabled=true 但 agent.executor_url 为空")
		}
	} else {
		if c.ES.Addr == "" || c.ES.Index == "" {
			return fmt.Errorf("es.addr/es.index 不能为空（或启用 agent 模式）")
		}
		if c.LLM.BaseURL == "" || c.LLM.Model == "" {
			return fmt.Errorf("llm.base_url/llm.model 不能为空（或启用 agent 模式）")
		}
	}
	return nil
}

// Load 从 JSON 文件加载配置，并用环境变量覆盖密钥类字段。
func Load(path string) (*Config, error) {
	c := Default()
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read config: %w", err)
		}
		// DisallowUnknownFields：配置 typo 直接报错，而不是静默失效
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.DisallowUnknownFields()
		if err := dec.Decode(c); err != nil {
			return nil, fmt.Errorf("parse config: %w", err)
		}
	}
	overrideFromEnv(c)
	if err := c.sanitize(); err != nil {
		return nil, err
	}
	return c, nil
}

func overrideFromEnv(c *Config) {
	if v := os.Getenv("LINKAGE_TOKEN"); v != "" {
		c.Server.Token = v
	}
	if v := os.Getenv("LINKAGE_ES_PASS"); v != "" {
		c.ES.Pass = v
	}
	if v := os.Getenv("LINKAGE_LLM_KEY"); v != "" {
		c.LLM.APIKey = v
	}
	if v := os.Getenv("LINKAGE_WECOM_URL"); v != "" {
		c.Notify.WeCom.WebhookURL = v
	}
	if v := os.Getenv("LINKAGE_SMTP_PASS"); v != "" {
		c.Notify.Mail.Pass = v
	}
	if v := os.Getenv("LINKAGE_UNIPRO_TOKEN"); v != "" {
		c.Notify.UniPro.Token = v
	}
}
