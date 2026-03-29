package handler

import (
	"allfast/internal/database"
	"allfast/internal/model"
	"allfast/internal/service"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
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
		WHERE stat_date >= $1 AND stat_date <= $2
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

	providerFilter := c.Query("provider") // 可选：cloudflare / edgeone / aliyun / ""

	// 获取站点域名（CF zone 匹配需要）
	var siteDomain string
	database.DB.QueryRow("SELECT domain FROM sites WHERE id = $1", siteID).Scan(&siteDomain)

	// 获取该站点所有 deployment
	var rows *sql.Rows
	var err error
	if providerFilter != "" {
		rows, err = database.DB.Query(
			`SELECT config_id, provider, provider_site_id FROM deployments
			 WHERE site_id = $1 AND provider = $2 AND status NOT IN ('pending','error')`,
			siteID, providerFilter,
		)
	} else {
		rows, err = database.DB.Query(
			`SELECT config_id, provider, provider_site_id FROM deployments
			 WHERE site_id = $1 AND status NOT IN ('pending','error')`,
			siteID,
		)
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	type deplRow struct {
		configID         int64
		provider, siteID string
	}
	var depls []deplRow
	for rows.Next() {
		var d deplRow
		rows.Scan(&d.configID, &d.provider, &d.siteID)
		depls = append(depls, d)
	}
	rows.Close()

	// 解析各提供商真实的统计 zone ID
	// - CF CNAME 模式：provider_site_id 是 hostname UUID（含短横线），需从 dns_cache_zones 按 zone_name 匹配
	// - CF NS 模式：provider_site_id 即为 zone ID（32位纯十六进制）
	// - EO / Aliyun：provider_site_id 即为 CDN 站点 ID
	type zoneKey struct {
		configID         int64
		provider, zoneID string
	}
	seen := map[string]bool{}
	var zones []zoneKey
	addZone := func(configID int64, provider, zoneID string) {
		k := fmt.Sprintf("%d:%s", configID, zoneID)
		if zoneID != "" && !seen[k] {
			seen[k] = true
			zones = append(zones, zoneKey{configID, provider, zoneID})
		}
	}

	for _, d := range depls {
		switch d.provider {
		case "cloudflare":
			if !strings.Contains(d.siteID, "-") && d.siteID != "" {
				// NS 模式：provider_site_id 就是 zone ID
				addZone(d.configID, "cloudflare", d.siteID)
			} else {
				// CNAME 模式：采集时 zone_id 格式为 "zoneID|hostname"
				// 从 deploy_params 读取 zone_id，拼接成 zoneID|siteDomain
				if siteDomain != "" {
					var paramsJSON string
					database.DB.QueryRow(
						`SELECT deploy_params FROM deployments WHERE site_id = $1 AND config_id = $2 AND provider = 'cloudflare'`,
						siteID, d.configID,
					).Scan(&paramsJSON)
					var params map[string]string
					json.Unmarshal([]byte(paramsJSON), &params)
					if zid := params["zone_id"]; zid != "" {
						addZone(d.configID, "cloudflare", zid+"|"+siteDomain)
					}
				}
			}
		case "edgeone":
			// EdgeOne：采集时 zone_id 格式为 "zoneID:domain"
			if d.siteID != "" && siteDomain != "" {
				addZone(d.configID, "edgeone", d.siteID+":"+siteDomain)
			} else if d.siteID != "" {
				addZone(d.configID, "edgeone", d.siteID)
			}
		case "aliyun", "aliyun_esa":
			// Aliyun ESA：采集时 zone_id 格式为 "siteID:domain"
			if d.siteID != "" && siteDomain != "" {
				addZone(d.configID, d.provider, d.siteID+":"+siteDomain)
			} else if d.siteID != "" {
				addZone(d.configID, d.provider, d.siteID)
			}
		}
	}

	if len(zones) == 0 {
		c.JSON(http.StatusOK, gin.H{
			"summary": model.StatsSummary{},
			"series":  []model.StatPoint{},
		})
		return
	}

	// 构建 zone_id IN (...) 参数（addZone 已保证唯一）
	zoneIDs := make([]string, 0, len(zones))
	for _, z := range zones {
		zoneIDs = append(zoneIDs, z.zoneID)
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

// StatsClear POST /api/stats/clear — 清空所有统计缓存（临时调试用）
func StatsClear(c *gin.Context) {
	database.DB.Exec("DELETE FROM cdn_stats_daily")
	database.DB.Exec("DELETE FROM cdn_stats_raw")
	database.DB.Exec("DELETE FROM cdn_stats_geo")
	c.JSON(http.StatusOK, gin.H{"message": "统计数据已清空"})
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
