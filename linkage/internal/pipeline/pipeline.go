// Package pipeline 编排一次完整分析：定位 → ES 检索 → 聚合 → LLM → 通知。
// 支持两种执行方式：
//   - 进程内（默认）：定位 → ES → 聚合 → LLM；
//   - 外部 agent：把事件交给 agent 执行器（加载 skill 自行检索+分析），见 internal/agent。
package pipeline

import (
	"context"
	"log"
	"time"

	"kernel-hw-linkage/internal/agent"
	"kernel-hw-linkage/internal/aggregate"
	"kernel-hw-linkage/internal/buffer"
	"kernel-hw-linkage/internal/config"
	"kernel-hw-linkage/internal/es"
	"kernel-hw-linkage/internal/llm"
	"kernel-hw-linkage/internal/model"
	"kernel-hw-linkage/internal/notify"
)

// Processor 一次完整分析的执行器。
type Processor struct {
	cfg   *config.Config
	es    *es.Client
	llm   *llm.Client
	note  *notify.Multi
	agent *agent.HTTPInvoker // 非空时告警直接交给外部 agent
	sem   chan struct{}      // 并发分析上限
}

// New 构造 Processor。maxConcurrent<=0 表示不限制。
func New(cfg *config.Config, esC *es.Client, llmC *llm.Client, note *notify.Multi, ag *agent.HTTPInvoker, maxConcurrent int) *Processor {
	p := &Processor{cfg: cfg, es: esC, llm: llmC, note: note, agent: ag}
	if maxConcurrent > 0 {
		p.sem = make(chan struct{}, maxConcurrent)
	}
	return p
}

// Handle 由聚合缓冲回调触发，执行整条链路。
// 带 recover（防单次 panic 拖垮整个进程）与并发上限（防告警风暴打爆后端）。
func (p *Processor) Handle(ev buffer.Event, types []string) {
	if p.sem != nil {
		p.sem <- struct{}{}
		defer func() { <-p.sem }()
	}
	defer func() {
		if r := recover(); r != nil {
			log.Printf("E! panic in analysis for %s/%v: %v", ev.Ident, types, r)
		}
	}()

	start := time.Unix(ev.TriggerTime, 0)
	lookback := time.Duration(p.cfg.Window.ESLookbackMin) * time.Minute
	report := model.Report{
		Hostname:  ev.Ident,
		Types:     types,
		RuleName:  ev.RuleName,
		TimeStart: start.Add(-lookback).Format("2006-01-02 15:04:05"),
		TimeEnd:   start.Format("2006-01-02 15:04:05"),
	}

	if p.agent != nil {
		p.runAgent(ev, types, &report)
	} else {
		p.runInProcess(ev, types, start, &report)
	}

	p.finish(ev, types, &report)
}

// runAgent 直接把事件交给外部 agent（加载 skill 自行检索+分析）。
func (p *Processor) runAgent(ev buffer.Event, types []string, report *model.Report) {
	inc := agent.Incident{
		Hostname:  ev.Ident,
		Types:     types,
		TimeStart: report.TimeStart,
		TimeEnd:   report.TimeEnd,
		RuleName:  ev.RuleName,
	}
	r, err := p.agent.Run(context.Background(), inc)
	if err != nil {
		report.AnalysisFailed = true
		report.Err = err.Error()
		return
	}
	// 以 agent 返回的完整报告为准，仅保留触发上下文
	r.Hostname = ev.Ident
	if len(r.Types) == 0 {
		r.Types = types
	}
	if r.TimeStart == "" {
		r.TimeStart = report.TimeStart
		r.TimeEnd = report.TimeEnd
	}
	*report = r
}

// runInProcess 进程内链路：定位 → ES 检索 → 聚合 → LLM，带有限重试。
func (p *Processor) runInProcess(ev buffer.Event, types []string, start time.Time, report *model.Report) {
	var err error
	for attempt := 1; attempt <= 3; attempt++ {
		err = p.run(ev, types, start, report)
		if err == nil {
			break
		}
		log.Printf("W! analysis attempt %d failed for %s/%v: %v", attempt, ev.Ident, types, err)
		if attempt < 3 {
			time.Sleep(time.Duration(attempt) * 5 * time.Second)
		}
	}
	if err != nil {
		report.AnalysisFailed = true
		report.Err = err.Error()
	}
}

func (p *Processor) finish(ev buffer.Event, types []string, report *model.Report) {
	if p.note != nil && p.note.Enabled() {
		if errs := p.note.Send(*report); len(errs) > 0 {
			for _, e := range errs {
				log.Printf("E! notify: %v", e)
			}
		}
	}
	if report.AnalysisFailed {
		log.Printf("E! analysis failed for %s/%v: %v", ev.Ident, types, report.Err)
	} else {
		log.Printf("I! analysis done for %s/%v", ev.Ident, types)
	}
}

func (p *Processor) run(ev buffer.Event, types []string, start time.Time, report *model.Report) error {
	// 1. 定位 IP
	ip, err := p.resolve(ev.Ident, types, start)
	if err != nil {
		return err
	}
	if ip == "" {
		report.Err = "无法定位服务器 IP"
		report.AnalysisFailed = true
		// 无 IP 无法查 ES，直接返回（发送"定位失败"通知）
		return nil
	}
	report.ServerIP = ip

	// 2. ES 检索
	keywords := p.es.Keywords(p.cfg.ES.TypeKeywords, types)
	lookback := time.Duration(p.cfg.Window.ESLookbackMin) * time.Minute
	hits, sources, err := p.es.Search(ip, keywords, start.Add(-lookback), start, p.cfg.Window.MaxLines)
	if err != nil {
		return err
	}
	_ = sources
	if len(hits) == 0 {
		report.Err = "ES 未命中相关日志（IP=" + ip + "）"
		report.AnalysisFailed = true
		return nil
	}

	// 3. 聚合
	agg := aggregate.Aggregate(hits, aggregate.Options{
		MaxLines:     p.cfg.Window.MaxLines,
		MaxCharsLine: p.cfg.Window.MaxCharsPerLine,
	})
	report.Lines = agg.Lines

	// 4. LLM 分析
	res, err := p.llm.Analyze(ev.Ident, ip, types, start.Add(-lookback), start, agg.Lines)
	if err != nil {
		return err
	}
	report.LLM = res
	return nil
}
