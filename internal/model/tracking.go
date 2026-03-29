package model

import "time"

// PageView 单次页面访问记录
type PageView struct {
	ID          int64     `json:"id"`
	SiteID      int64     `json:"site_id"`
	VisitorID   string    `json:"visitor_id"`   // SHA256(IP+UA) 前 16 hex 字符
	SessionID   string    `json:"session_id"`   // 客户端生成的会话 ID
	Path        string    `json:"path"`
	Referrer    string    `json:"referrer"`     // 来源域名
	Browser     string    `json:"browser"`
	OS          string    `json:"os"`
	CountryCode string    `json:"country_code"` // CF-IPCountry header
	Duration    int       `json:"duration"`     // 页面停留秒数
	CreatedAt   time.Time `json:"created_at"`
}

// TrackingMetric 分组统计条目
type TrackingMetric struct {
	Label    string  `json:"label"`
	Visitors int64   `json:"visitors"`
	Pct      float64 `json:"pct"`
}

// TrackingChartPoint 流量趋势点
type TrackingChartPoint struct {
	T         string `json:"t"`
	Visitors  int64  `json:"visitors"`
	Pageviews int64  `json:"pageviews"`
}

// TrackingStats 站点访客统计汇总
type TrackingStats struct {
	Visitors        int64   `json:"visitors"`
	Pageviews       int64   `json:"pageviews"`
	BounceRate      float64 `json:"bounce_rate"`   // 0-1
	AvgDuration     float64 `json:"avg_duration"`  // 秒
	PrevVisitors    int64   `json:"prev_visitors"`
	PrevPageviews   int64   `json:"prev_pageviews"`
	PrevBounceRate  float64 `json:"prev_bounce_rate"`
	PrevAvgDuration float64 `json:"prev_avg_duration"`

	Pages     []TrackingMetric     `json:"pages"`
	Referrers []TrackingMetric     `json:"referrers"`
	Browsers  []TrackingMetric     `json:"browsers"`
	OSes      []TrackingMetric     `json:"oses"`
	Countries []TrackingMetric     `json:"countries"`
	Chart     []TrackingChartPoint `json:"chart"`
}
