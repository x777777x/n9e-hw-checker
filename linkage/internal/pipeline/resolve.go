package pipeline

import (
	"time"
)

// resolve 定位服务器 IP：优先清单映射，缺失时用 ES 反查兜底。
func (p *Processor) resolve(hostname string, types []string, start time.Time) (string, error) {
	if ip, ok := p.cfg.Resolve.Inventory[hostname]; ok && ip != "" {
		return ip, nil
	}
	keywords := p.es.Keywords(p.cfg.ES.TypeKeywords, types)
	lookback := time.Duration(p.cfg.Window.ESLookbackMin) * time.Minute
	ips, err := p.es.ReverseSearch(hostname, keywords, start.Add(-lookback), start)
	if err != nil {
		return "", err
	}
	if len(ips) > 0 {
		return ips[0], nil
	}
	return "", nil
}
