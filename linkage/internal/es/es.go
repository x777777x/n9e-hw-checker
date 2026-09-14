// Package es 封装对 Elasticsearch 的检索（标准库 net/http）。
// 查询策略：不拼接文件名日期，用通配 source + 时间窗 + 类别关键词，
// 从命中文档的 source.keyword 反向定位文件与 IP。
package es

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// Hit 一条命中的日志行。
type Hit struct {
	Timestamp time.Time
	Source    string // source.keyword（日志文件名）
	Message   string
}

// SourceCount source.keyword 聚合计数。
type SourceCount struct {
	Source   string `json:"key"`
	DocCount int    `json:"doc_count"`
}

// Client ES 只读检索客户端。
type Client struct {
	addr  string
	user  string
	pass  string
	index string
	http  *http.Client
	patIP *regexp.Regexp
}

// New 创建 ES 客户端。
func New(addr, user, pass, index string, timeout time.Duration) *Client {
	return &Client{
		addr:  strings.TrimRight(addr, "/"),
		user:  user,
		pass:  pass,
		index: index,
		http:  &http.Client{Timeout: timeout},
		// /syslog/system/<date>/<IP>_<date>.log
		patIP: regexp.MustCompile(`/syslog/system/[^/]+/([^_/]+)_`),
	}
}

type query struct {
	Size  int           `json:"size"`
	Query qBool         `json:"query"`
	Aggs  map[string]ag `json:"aggs,omitempty"`
	Sort  []interface{} `json:"sort,omitempty"`
}

type qBool struct {
	Bool struct {
		Filter []interface{} `json:"filter"`
	} `json:"bool"`
}

type ag struct {
	Terms struct {
		Field string `json:"field"`
		Size  int    `json:"size"`
	} `json:"terms"`
}

func newSourceAgg() ag {
	a := ag{}
	a.Terms.Field = "source.keyword"
	a.Terms.Size = 10
	return a
}

// Keywords 返回该类型对应的 ES match_phrase 关键词（来自配置）。
// keywords 为空时返回全部类型的关键词并集。
func (c *Client) Keywords(typeKeywords map[string][]string, types []string) []string {
	if len(types) == 0 {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, t := range types {
		for _, kw := range typeKeywords[t] {
			if kw != "" && !seen[kw] {
				seen[kw] = true
				out = append(out, kw)
			}
		}
	}
	return out
}

// Search 按类型关键词 + 时间窗 + source 通配检索。
// ip 为空则不限 IP。返回按时间升序的命中行与 source 聚合。
func (c *Client) Search(ip string, keywords []string, start, end time.Time, maxLines int) ([]Hit, []SourceCount, error) {
	q := query{Size: maxLines}
	q.Query.Bool.Filter = append(q.Query.Bool.Filter,
		map[string]interface{}{
			"range": map[string]interface{}{
				"@timestamp": map[string]string{"gte": start.UTC().Format(time.RFC3339), "lte": end.UTC().Format(time.RFC3339)},
			},
		})
	if ip != "" {
		q.Query.Bool.Filter = append(q.Query.Bool.Filter,
			map[string]interface{}{
				"wildcard": map[string]interface{}{"source.keyword": "/syslog/system/*/" + ip + "_*.log"},
			})
	}
	if len(keywords) > 0 {
		should := make([]interface{}, 0, len(keywords))
		for _, kw := range keywords {
			should = append(should,
				map[string]interface{}{"match_phrase": map[string]string{"message": kw}})
		}
		q.Query.Bool.Filter = append(q.Query.Bool.Filter, map[string]interface{}{"bool": map[string]interface{}{"should": should}})
	}
	q.Aggs = map[string]ag{}
	q.Aggs["by_source"] = newSourceAgg()
	// 按时间倒序取最新行：告警时段日志量大时，最新/最严重的错误（往往更接近根因）优先保留
	q.Sort = []interface{}{map[string]string{"@timestamp": "desc"}}

	body, err := json.Marshal(q)
	if err != nil {
		return nil, nil, err
	}
	resp, err := c.do("_search", body)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("es status %d: %s", resp.StatusCode, truncate(string(raw), 300))
	}

	var parsed struct {
		Hits struct {
			Hits []struct {
				Source struct {
					Timestamp string `json:"@timestamp"`
					Source    string `json:"source"`
					Message   string `json:"message"`
				} `json:"_source"`
			} `json:"hits"`
		} `json:"hits"`
		Aggregations struct {
			BySource struct {
				Buckets []SourceCount `json:"buckets"`
			} `json:"by_source"`
		} `json:"aggregations"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, nil, fmt.Errorf("es decode: %w", err)
	}

	hits := make([]Hit, 0, len(parsed.Hits.Hits))
	for _, h := range parsed.Hits.Hits {
		ts, _ := time.Parse(time.RFC3339, h.Source.Timestamp)
		hits = append(hits, Hit{
			Timestamp: ts,
			Source:    h.Source.Source,
			Message:   h.Source.Message,
		})
	}
	return hits, parsed.Aggregations.BySource.Buckets, nil
}

// ReverseSearch 按主机名反查 IP：在时间窗内匹配 message 含 hostname 且命中关键词，
// 从命中的 source.keyword 解析出服务器 IP（清单映射缺失时的兜底）。
func (c *Client) ReverseSearch(hostname string, keywords []string, start, end time.Time) ([]string, error) {
	q := query{Size: 50}
	q.Query.Bool.Filter = append(q.Query.Bool.Filter,
		map[string]interface{}{
			"range": map[string]interface{}{
				"@timestamp": map[string]string{"gte": start.UTC().Format(time.RFC3339), "lte": end.UTC().Format(time.RFC3339)},
			},
		})
	should := []interface{}{
		map[string]interface{}{"match_phrase": map[string]string{"message": hostname}},
	}
	for _, kw := range keywords {
		should = append(should,
			map[string]interface{}{"match_phrase": map[string]string{"message": kw}})
	}
	q.Query.Bool.Filter = append(q.Query.Bool.Filter, map[string]interface{}{"bool": map[string]interface{}{"should": should}})
	q.Aggs = map[string]ag{}
	q.Aggs["by_source"] = newSourceAgg()

	body, _ := json.Marshal(q)
	resp, err := c.do("_search", body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("es reverse status %d: %s", resp.StatusCode, truncate(string(raw), 300))
	}
	var parsed struct {
		Aggregations struct {
			BySource struct {
				Buckets []SourceCount `json:"buckets"`
			} `json:"by_source"`
		} `json:"aggregations"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var ips []string
	for _, b := range parsed.Aggregations.BySource.Buckets {
		if ip := c.IPFromSource(b.Source); ip != "" && !seen[ip] {
			seen[ip] = true
			ips = append(ips, ip)
		}
	}
	return ips, nil
}

// IPFromSource 从文件名 /syslog/system/<date>/<IP>_<date>.log 提取 IP。
func (c *Client) IPFromSource(source string) string {
	m := c.patIP.FindStringSubmatch(source)
	if len(m) == 2 {
		return m[1]
	}
	return ""
}

func (c *Client) do(path string, body []byte) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodPost, c.addr+"/"+c.index+"/"+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.user != "" {
		req.SetBasicAuth(c.user, c.pass)
	}
	return c.http.Do(req)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
