package model

import "time"

// StatPoint 单小时流量数据点
type StatPoint struct {
	Time           time.Time `json:"time"`
	Requests       int64     `json:"requests"`
	Bytes          int64     `json:"bytes"`
	CachedRequests int64     `json:"cached_requests"`
	CachedBytes    int64     `json:"cached_bytes"`
}

// GeoPoint 地区流量分布数据点
type GeoPoint struct {
	CountryCode string `json:"country_code"`
	CountryName string `json:"country_name"`
	Requests    int64  `json:"requests"`
	Bytes       int64  `json:"bytes"`
}

// StatsSummary 汇总卡片数据
type StatsSummary struct {
	TotalRequests int64   `json:"total_requests"`
	TotalBytes    int64   `json:"total_bytes"`
	AvgHitRate    float64 `json:"avg_hit_rate"` // 缓存命中率 0-1
	Providers     int     `json:"providers"`
	Zones         int     `json:"zones"`
}

// StatsQueryRange 查询时间范围标识
type StatsQueryRange string

const (
	RangeAllTime StatsQueryRange = "all"
	Range1Year   StatsQueryRange = "1y"
	Range30Day   StatsQueryRange = "30d"
	Range14Day   StatsQueryRange = "14d"
	Range7Day    StatsQueryRange = "7d"
	Range1Day    StatsQueryRange = "1d"
)
