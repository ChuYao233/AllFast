package handler

import (
	"allfast/internal/database"
	"allfast/internal/model"
	"allfast/internal/service"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

// StatsTimeSeries GET /api/stats/timeseries?range=30d
func StatsTimeSeries(c *gin.Context) {
	from, to := parseStatsRange(c.DefaultQuery("range", "30d"))
	longRange := to.Sub(from) > 48*time.Hour

	var pts []model.StatPoint

	if longRange {
		// 长范围：以 daily 为主（按天聚合），再补今天的 raw 实时数据
		rows, err := database.DB.Query(`
			SELECT stat_date, SUM(requests), SUM(bytes), SUM(cached_requests), SUM(cached_bytes)
			FROM cdn_stats_daily
			WHERE stat_date >= $1 AND stat_date <= $2
			GROUP BY stat_date
			ORDER BY stat_date ASC`,
			from.Format("2006-01-02"), to.Format("2006-01-02"),
		)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		defer rows.Close()
		for rows.Next() {
			var p model.StatPoint
			rows.Scan(&p.Time, &p.Requests, &p.Bytes, &p.CachedRequests, &p.CachedBytes)
			pts = append(pts, p)
		}

		// 补今天的 raw 聚合（今天还没有 daily 记录）
		today := time.Now().UTC().Format("2006-01-02")
		var todayPt model.StatPoint
		database.DB.QueryRow(`
			SELECT COALESCE(SUM(requests),0), COALESCE(SUM(bytes),0),
			       COALESCE(SUM(cached_requests),0), COALESCE(SUM(cached_bytes),0)
			FROM cdn_stats_raw
			WHERE DATE(period_start AT TIME ZONE 'UTC') = $1`, today,
		).Scan(&todayPt.Requests, &todayPt.Bytes, &todayPt.CachedRequests, &todayPt.CachedBytes)
		if todayPt.Requests > 0 {
			todayPt.Time, _ = time.Parse("2006-01-02", today)
			// 若 daily 已有今天则覆盖，否则追加
			found := false
			for i, p := range pts {
				if p.Time.Format("2006-01-02") == today {
					pts[i] = todayPt
					found = true
					break
				}
			}
			if !found {
				pts = append(pts, todayPt)
			}
		}
	} else {
		// 短范围：用 raw 小时粒度
		rows, err := database.DB.Query(`
			SELECT period_start, SUM(requests), SUM(bytes), SUM(cached_requests), SUM(cached_bytes)
			FROM cdn_stats_raw
			WHERE period_start >= $1 AND period_start < $2
			GROUP BY period_start
			ORDER BY period_start ASC`, from, to,
		)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		defer rows.Close()
		for rows.Next() {
			var p model.StatPoint
			rows.Scan(&p.Time, &p.Requests, &p.Bytes, &p.CachedRequests, &p.CachedBytes)
			pts = append(pts, p)
		}
	}

	if pts == nil {
		pts = []model.StatPoint{}
	}
	c.JSON(http.StatusOK, gin.H{"data": pts, "from": from, "to": to})
}

// StatsGeo GET /api/stats/geo?range=30d
func StatsGeo(c *gin.Context) {
	from, to := parseStatsRange(c.DefaultQuery("range", "30d"))

	rows, err := database.DB.Query(`
		SELECT country_code, country_name, SUM(requests), SUM(bytes)
		FROM cdn_stats_geo
		WHERE stat_date >= $1 AND stat_date < $2
		GROUP BY country_code, country_name
		ORDER BY SUM(requests) DESC`,
		from.Format("2006-01-02"), to.Format("2006-01-02"),
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	defer rows.Close()

	var pts []model.GeoPoint
	for rows.Next() {
		var p model.GeoPoint
		rows.Scan(&p.CountryCode, &p.CountryName, &p.Requests, &p.Bytes)
		pts = append(pts, p)
	}
	if pts == nil {
		pts = []model.GeoPoint{}
	}
	c.JSON(http.StatusOK, gin.H{"data": pts})
}

// StatsSummary GET /api/stats/summary?range=30d
func StatsSummary(c *gin.Context) {
	from, to := parseStatsRange(c.DefaultQuery("range", "30d"))

	var summary model.StatsSummary

	// 历史天数据（daily 表）
	var dailyReq, dailyBytes, dailyCached int64
	database.DB.QueryRow(`
		SELECT COALESCE(SUM(requests),0), COALESCE(SUM(bytes),0), COALESCE(SUM(cached_requests),0)
		FROM cdn_stats_daily
		WHERE stat_date >= $1 AND stat_date < $2`,
		from.Format("2006-01-02"), to.Format("2006-01-02"),
	).Scan(&dailyReq, &dailyBytes, &dailyCached)

	// 今天实时数据（raw 表），不与 daily 重复
	today := time.Now().UTC().Format("2006-01-02")
	var rawReq, rawBytes, rawCached int64
	database.DB.QueryRow(`
		SELECT COALESCE(SUM(requests),0), COALESCE(SUM(bytes),0), COALESCE(SUM(cached_requests),0)
		FROM cdn_stats_raw
		WHERE DATE(period_start AT TIME ZONE 'UTC') = $1`, today,
	).Scan(&rawReq, &rawBytes, &rawCached)

	summary.TotalRequests = dailyReq + rawReq
	summary.TotalBytes = dailyBytes + rawBytes
	totalCached := dailyCached + rawCached

	// 提供商数：从 provider_configs 取启用数
	database.DB.QueryRow(
		"SELECT COUNT(*) FROM provider_configs WHERE enabled = 1",
	).Scan(&summary.Providers)

	// zone 数：从统计历史表取去重 zone 数（不依赖 DNS 缓存表）
	database.DB.QueryRow(`
		SELECT COUNT(DISTINCT zone_id) FROM (
			SELECT zone_id FROM cdn_stats_daily
			UNION
			SELECT zone_id FROM cdn_stats_raw
		) z`,
	).Scan(&summary.Zones)

	// zone 为 0 时降级：从 dns_cache_zones 查（DNS 同步后才有）
	if summary.Zones == 0 {
		database.DB.QueryRow("SELECT COUNT(DISTINCT zone_id) FROM dns_cache_zones").Scan(&summary.Zones)
	}

	// 缓存命中率
	if summary.TotalRequests > 0 {
		summary.AvgHitRate = float64(totalCached) / float64(summary.TotalRequests)
	}

	c.JSON(http.StatusOK, summary)
}

// SiteStat GET /api/sites/:id/stats?range=30d — 单站点流量统计
func SiteStat(c *gin.Context) {
	siteID := c.Param("id")
	from, to := parseStatsRange(c.DefaultQuery("range", "30d"))
	longRange := to.Sub(from) > 48*time.Hour

	// 获取该站点所有 deployment 的 (config_id, provider_site_id)
	rows, err := database.DB.Query(
		`SELECT config_id, provider, provider_site_id FROM deployments
		 WHERE site_id = $1 AND provider_site_id != '' AND status NOT IN ('pending','error')`,
		siteID,
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	defer rows.Close()

	type zoneKey struct {
		configID         int64
		provider, zoneID string
	}
	var zones []zoneKey
	for rows.Next() {
		var z zoneKey
		rows.Scan(&z.configID, &z.provider, &z.zoneID)
		zones = append(zones, z)
	}

	if len(zones) == 0 {
		c.JSON(http.StatusOK, gin.H{
			"summary": model.StatsSummary{},
			"series":  []model.StatPoint{},
		})
		return
	}

	// 构建 zone_id IN (...) 参数
	zoneIDs := make([]string, 0, len(zones))
	seen := map[string]bool{}
	for _, z := range zones {
		if !seen[z.zoneID] {
			seen[z.zoneID] = true
			zoneIDs = append(zoneIDs, z.zoneID)
		}
	}

	// 构建 $1,$2,... 占位符
	phIdx := 2 // $1 已被日期占用
	placeholders := ""
	args := []interface{}{}
	for i, zid := range zoneIDs {
		if i > 0 {
			placeholders += ","
		}
		placeholders += fmt.Sprintf("$%d", phIdx)
		args = append(args, zid)
		phIdx++
	}

	var pts []model.StatPoint

	if longRange {
		q := fmt.Sprintf(`
			SELECT stat_date, SUM(requests), SUM(bytes), SUM(cached_requests), SUM(cached_bytes)
			FROM cdn_stats_daily
			WHERE stat_date >= $1 AND zone_id IN (%s)
			GROUP BY stat_date ORDER BY stat_date ASC`, placeholders)
		r, err := database.DB.Query(q, append([]interface{}{from.Format("2006-01-02")}, args...)...)
		if err == nil {
			defer r.Close()
			for r.Next() {
				var p model.StatPoint
				r.Scan(&p.Time, &p.Requests, &p.Bytes, &p.CachedRequests, &p.CachedBytes)
				pts = append(pts, p)
			}
		}
		// 补今天 raw
		today := time.Now().UTC().Format("2006-01-02")
		todayQ := fmt.Sprintf(`
			SELECT COALESCE(SUM(requests),0), COALESCE(SUM(bytes),0),
			       COALESCE(SUM(cached_requests),0), COALESCE(SUM(cached_bytes),0)
			FROM cdn_stats_raw
			WHERE DATE(period_start AT TIME ZONE 'UTC') = $1 AND zone_id IN (%s)`, placeholders)
		var tp model.StatPoint
		database.DB.QueryRow(todayQ, append([]interface{}{today}, args...)...).
			Scan(&tp.Requests, &tp.Bytes, &tp.CachedRequests, &tp.CachedBytes)
		if tp.Requests > 0 {
			tp.Time, _ = time.Parse("2006-01-02", today)
			found := false
			for i, p := range pts {
				if p.Time.Format("2006-01-02") == today {
					pts[i] = tp
					found = true
					break
				}
			}
			if !found {
				pts = append(pts, tp)
			}
		}
	} else {
		q := fmt.Sprintf(`
			SELECT period_start, SUM(requests), SUM(bytes), SUM(cached_requests), SUM(cached_bytes)
			FROM cdn_stats_raw
			WHERE period_start >= $1 AND zone_id IN (%s)
			GROUP BY period_start ORDER BY period_start ASC`, placeholders)
		r, err := database.DB.Query(q, append([]interface{}{from}, args...)...)
		if err == nil {
			defer r.Close()
			for r.Next() {
				var p model.StatPoint
				r.Scan(&p.Time, &p.Requests, &p.Bytes, &p.CachedRequests, &p.CachedBytes)
				pts = append(pts, p)
			}
		}
	}

	// 汇总卡片
	var summary model.StatsSummary
	var dailyReq, dailyBytes, dailyCached int64
	dailyQ := fmt.Sprintf(`
		SELECT COALESCE(SUM(requests),0), COALESCE(SUM(bytes),0), COALESCE(SUM(cached_requests),0)
		FROM cdn_stats_daily
		WHERE stat_date >= $1 AND zone_id IN (%s)`, placeholders)
	database.DB.QueryRow(dailyQ, append([]interface{}{from.Format("2006-01-02")}, args...)...).
		Scan(&dailyReq, &dailyBytes, &dailyCached)

	today := time.Now().UTC().Format("2006-01-02")
	var rawReq, rawBytes, rawCached int64
	rawQ := fmt.Sprintf(`
		SELECT COALESCE(SUM(requests),0), COALESCE(SUM(bytes),0), COALESCE(SUM(cached_requests),0)
		FROM cdn_stats_raw
		WHERE DATE(period_start AT TIME ZONE 'UTC') = $1 AND zone_id IN (%s)`, placeholders)
	database.DB.QueryRow(rawQ, append([]interface{}{today}, args...)...).
		Scan(&rawReq, &rawBytes, &rawCached)

	summary.TotalRequests = dailyReq + rawReq
	summary.TotalBytes = dailyBytes + rawBytes
	totalCached := dailyCached + rawCached
	if summary.TotalRequests > 0 {
		summary.AvgHitRate = float64(totalCached) / float64(summary.TotalRequests)
	}
	summary.Providers = len(zones)

	if pts == nil {
		pts = []model.StatPoint{}
	}
	c.JSON(http.StatusOK, gin.H{"summary": summary, "series": pts})
}

// StatsTriggerCollect POST /api/stats/collect — 手动立即采集
func StatsTriggerCollect(c *gin.Context) {
	service.StatsCollect.TriggerNow()
	c.JSON(http.StatusOK, gin.H{"message": "采集任务已触发"})
}

// parseStatsRange 将范围字符串转为 from/to 时间
func parseStatsRange(rangeStr string) (from, to time.Time) {
	to = time.Now().UTC()
	switch model.StatsQueryRange(rangeStr) {
	case model.RangeAllTime:
		from = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	case model.Range1Year:
		from = to.AddDate(-1, 0, 0)
	case model.Range30Day:
		from = to.AddDate(0, 0, -30)
	case model.Range14Day:
		from = to.AddDate(0, 0, -14)
	case model.Range7Day:
		from = to.AddDate(0, 0, -7)
	case model.Range1Day:
		from = to.AddDate(0, 0, -1)
	default:
		from = to.AddDate(0, 0, -30)
	}
	return
}
