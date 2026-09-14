package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"kernel-hw-linkage/internal/config"
)

// ---- 测试用 mock ----

type capture struct {
	mu   sync.Mutex
	body []string
}

func (c *capture) add(b string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.body = append(c.body, b)
}
func (c *capture) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.body)
}
func (c *capture) joined() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.Join(c.body, "\n")
}

func mockES(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/_search") {
			http.Error(w, "bad path", 400)
			return
		}
		body, _ := io.ReadAll(r.Body)
		validateESQuery(t, body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"hits": { "hits": [ { "_source": {
				"@timestamp": "2026-09-13T10:01:00Z",
				"source": "/syslog/system/2026-09-13/10.0.0.11_2026-09-13.log",
				"message": "eth0: NIC Link is Down"
			} } ] },
			"aggregations": { "by_source": { "buckets": [
				{ "key": "/syslog/system/2026-09-13/10.0.0.11_2026-09-13.log", "doc_count": 1 }
			] } }
		}`))
	}))
}

// validateESQuery 校验真实 ES 查询的关键约束，防止 sort/wildcard/size 回归。
// 仅在处理 goroutine 内使用 Errorf（并发安全），不使用 Fatalf。
func validateESQuery(t *testing.T, body []byte) {
	t.Helper()
	var q struct {
		Size int                 `json:"size"`
		Sort []map[string]string `json:"sort"`
	}
	if err := json.Unmarshal(body, &q); err != nil {
		t.Errorf("es query decode failed: %v", err)
		return
	}
	if q.Size <= 0 {
		t.Errorf("es query size should be >0, got %d", q.Size)
	}
	desc := false
	for _, s := range q.Sort {
		for _, v := range s {
			if v == "desc" {
				desc = true
			}
		}
	}
	if !desc {
		t.Errorf("es query should sort by @timestamp desc, body=%s", body)
	}
	if !bytes.Contains(body, []byte(`/syslog/system/*/10.0.0.11_*.log`)) {
		t.Errorf("es query missing source wildcard for target ip, body=%s", body)
	}
}

func mockLLM(t *testing.T) *httptest.Server {
	t.Helper()
	content := `{"结论":"网卡链路异常","可能原因":["物理链路/模块故障"],"影响评估":"业务中断风险","建议动作":["检查物理链路","查看 ethtool -S"],"紧急度":"高","需进一步检查":["dmesg"]}`
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := fmt.Sprintf(`{"choices":[{"message":{"content":%q}}]}`, content)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(resp))
	}))
}

func mockWeCom(t *testing.T, c *capture) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		c.add(string(b))
		w.WriteHeader(http.StatusOK)
	}))
}

// ---- 测试配置 ----

func testConfig(esURL, llmURL, wecomURL string) *config.Config {
	cfg := config.Default()
	cfg.Server.Addr = "127.0.0.1:0"
	cfg.Server.Token = "secret123"
	cfg.ES.Addr = esURL
	cfg.ES.Index = "syslog-system"
	cfg.ES.TypeKeywords = map[string][]string{
		"nic": {"Link is Down", "NIC Link is Down"},
		"mem": {"EDAC", "Machine Check"},
	}
	cfg.LLM.BaseURL = llmURL
	cfg.LLM.Model = "mock"
	cfg.LLM.TimeoutS = 5
	cfg.Resolve.Inventory = map[string]string{"host01": "10.0.0.11"}
	cfg.Window.DedupMinutes = 10
	cfg.Window.MergeSeconds = 1
	cfg.Window.ESLookbackMin = 10
	cfg.Window.MaxLines = 10
	cfg.Window.MaxCharsPerLine = 200
	cfg.Notify.WeCom.WebhookURL = wecomURL
	return cfg
}

func postWebhook(t *testing.T, url, token string, ts int64, ident, typ string) {
	t.Helper()
	payload, _ := json.Marshal(map[string]interface{}{
		"event": map[string]interface{}{
			"is_recovered": false,
			"trigger_time": ts,
			"rule_name":    "hw_type",
			"labels":       map[string]string{"ident": ident, "type": typ},
		},
	})
	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Webhook-Token", token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post webhook: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("webhook status %d", resp.StatusCode)
	}
}

// ---- 用例 ----

func TestEndToEndSingleType(t *testing.T) {
	wecom := &capture{}
	esS := mockES(t)
	defer esS.Close()
	llmS := mockLLM(t)
	defer llmS.Close()
	wcS := mockWeCom(t, wecom)
	defer wcS.Close()

	cfg := testConfig(esS.URL, llmS.URL, wcS.URL)
	a := New(cfg)
	ts := httptest.NewServer(a.Handler())
	defer ts.Close()

	postWebhook(t, ts.URL+"/webhook", cfg.Server.Token, 1700000000, "host01", "nic")

	// 等待合并窗口 + 分析 + 回写
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if wecom.count() >= 1 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if wecom.count() != 1 {
		t.Fatalf("want 1 wecom report, got %d", wecom.count())
	}
	body := wecom.joined()
	for _, want := range []string{"网卡链路异常", "host01", "10.0.0.11", "nic"} {
		if !strings.Contains(body, want) {
			t.Fatalf("report missing %q: %s", want, body)
		}
	}

	// 幂等去重：同一 (ident,type,窗口) 再次触发不应新增报告
	postWebhook(t, ts.URL+"/webhook", cfg.Server.Token, 1700000000, "host01", "nic")
	time.Sleep(1500 * time.Millisecond)
	if n := wecom.count(); n != 1 {
		t.Fatalf("dedup failed: want 1 report, got %d", n)
	}
}

func TestEndToEndMergeTypes(t *testing.T) {
	wecom := &capture{}
	esS := mockES(t)
	defer esS.Close()
	llmS := mockLLM(t)
	defer llmS.Close()
	wcS := mockWeCom(t, wecom)
	defer wcS.Close()

	cfg := testConfig(esS.URL, llmS.URL, wcS.URL)
	a := New(cfg)
	ts := httptest.NewServer(a.Handler())
	defer ts.Close()

	// 同一去重窗口（1700000000 与 1700000100 都在 bucket=2833333）
	postWebhook(t, ts.URL+"/webhook", cfg.Server.Token, 1700000000, "host01", "nic")
	postWebhook(t, ts.URL+"/webhook", cfg.Server.Token, 1700000100, "host01", "mem")

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if wecom.count() >= 1 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if wecom.count() != 1 {
		t.Fatalf("want 1 merged report, got %d", wecom.count())
	}
	body := wecom.joined()
	if !strings.Contains(body, "nic") || !strings.Contains(body, "mem") {
		t.Fatalf("merged report should contain nic and mem: %s", body)
	}
}

func TestAuthRejected(t *testing.T) {
	esS := mockES(t)
	defer esS.Close()
	llmS := mockLLM(t)
	defer llmS.Close()
	cfg := testConfig(esS.URL, llmS.URL, "")
	a := New(cfg)
	ts := httptest.NewServer(a.Handler())
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/webhook", strings.NewReader(`{}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("req: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", resp.StatusCode)
	}
}

// TestAgentDirectTrigger 验证"告警直接交给外部 agent"路径：
// agent 执行器返回 Report，联动服务不再走进程内 ES/LLM。
func TestAgentDirectTrigger(t *testing.T) {
	wecom := &capture{}
	agentReq := &capture{}
	// mock agent 执行器：接收 Incident，返回完整 Report
	agentS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		agentReq.add(string(b))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"hostname":"host01","server_ip":"10.0.0.11","types":["nic"],
			"time_start":"2026-09-13 09:50:00","time_end":"2026-09-13 10:00:00",
			"llm":{"结论":"由 agent 得出：网卡链路中断","可能原因":["光模块故障"],"影响评估":"中断","建议动作":["更换模块"],"紧急度":"高","需进一步检查":[]}
		}`))
	}))
	defer agentS.Close()
	wcS := mockWeCom(t, wecom)
	defer wcS.Close()

	cfg := testConfig("http://127.0.0.1:1", "http://127.0.0.1:1", wcS.URL) // ES/LLM 不应被调用
	cfg.Agent.Enabled = true
	cfg.Agent.ExecutorURL = agentS.URL
	cfg.Agent.Skill = "hw-alert-analyzer"
	a := New(cfg)
	ts := httptest.NewServer(a.Handler())
	defer ts.Close()

	postWebhook(t, ts.URL+"/webhook", cfg.Server.Token, 1700000000, "host01", "nic")

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if wecom.count() >= 1 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if wecom.count() != 1 {
		t.Fatalf("want 1 wecom report, got %d", wecom.count())
	}
	body := wecom.joined()
	if !strings.Contains(body, "由 agent 得出：网卡链路中断") {
		t.Fatalf("report should come from agent: %s", body)
	}
	if !strings.Contains(agentReq.joined(), `"skill":"hw-alert-analyzer"`) {
		t.Fatalf("agent incident should carry skill name: %s", agentReq.joined())
	}
}
