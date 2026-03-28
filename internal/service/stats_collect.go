package service

import (
	"allfast/internal/database"
	"allfast/internal/model"
	"allfast/internal/provider"
	"context"
	"encoding/json"
	"log"
	"sync"
	"time"
)

// StatsCollectService CDN 流量统计采集服务
type StatsCollectService struct {
	mu sync.Mutex
}

var StatsCollect = &StatsCollectService{}

// StartBackgroundCollect 启动后台采集（每15分钟一次）+ 每天0点聚合
func (s *StatsCollectService) StartBackgroundCollect() {
	go func() {
		time.Sleep(20 * time.Second)
		log.Println("[StatsCollect] 后台采集任务启动")
		s.CollectAll()

		ticker := time.NewTicker(15 * time.Minute)
		defer ticker.Stop()

		for range ticker.C {
			now := time.Now()
			// 0点附近（0:00~0:15）先执行跨天聚合
			if now.Hour() == 0 && now.Minute() < 15 {
				if err := s.AggregatePreviousDay(); err != nil {
					log.Printf("[StatsCollect] 跨天聚合失败: %v", err)
				}
			}
			s.CollectAll()
		}
	}()
}

// CollectAll 并发采集所有启用提供商
func (s *StatsCollectService) CollectAll() {
	tasks := s.loadTasks()
	var wg sync.WaitGroup
	for _, t := range tasks {
		wg.Add(1)
		go func(t collectTask) {
			defer wg.Done()
			if err := s.collectForConfig(t.configID, t.provider, t.cfg); err != nil {
				log.Printf("[StatsCollect] 配置 ID=%d 采集失败: %v", t.configID, err)
			}
		}(t)
	}
	wg.Wait()
}

// TriggerNow 手动立即采集 + 触发今天 geo 采集
func (s *StatsCollectService) TriggerNow() {
	go func() {
		s.CollectAll()
		today := time.Now().UTC().Format("2006-01-02")
		s.collectGeoForDateRange(today, today)
	}()
}

type collectTask struct {
	configID int64
	provider string
	cfg      map[string]string
}

func (s *StatsCollectService) loadTasks() []collectTask {
	rows, err := database.DB.Query(
		"SELECT id, provider, config FROM provider_configs WHERE enabled = 1",
	)
	if err != nil {
		log.Printf("[StatsCollect] 查询提供商配置失败: %v", err)
		return nil
	}
	defer rows.Close()

	var tasks []collectTask
	for rows.Next() {
		var id int64
		var prov, cfgJSON string
		if err := rows.Scan(&id, &prov, &cfgJSON); err != nil {
			continue
		}
		cfg := map[string]string{}
		json.Unmarshal([]byte(cfgJSON), &cfg)
		tasks = append(tasks, collectTask{id, prov, cfg})
	}
	return tasks
}

// collectForConfig 采集单个配置下所有 zone
// 首次采集自动补拉 30 天历史，写入 cdn_stats_daily；后续增量写 cdn_stats_raw
func (s *StatsCollectService) collectForConfig(configID int64, providerName string, cfg map[string]string) error {
	sp := provider.GetStats(providerName)
	if sp == nil {
		return nil
	}

	zoneIDs := s.getStatsZoneIDs(configID, providerName)
	if len(zoneIDs) == 0 {
		return nil
	}

	now := time.Now().UTC()

	for _, zoneID := range zoneIDs {
		// 判断是否首次采集（daily 表无记录）
		var dailyCount int
		database.DB.QueryRow(
			"SELECT COUNT(*) FROM cdn_stats_daily WHERE config_id=$1 AND zone_id=$2",
			configID, zoneID,
		).Scan(&dailyCount)

		if dailyCount == 0 {
			// 首次：补拉 30 天历史，写入 daily
			log.Printf("[StatsCollect] zone %s 首次采集，补拉30天历史数据", zoneID)
			s.backfillHistory(sp, configID, providerName, cfg, zoneID, now.AddDate(0, 0, -30), now)
		} else {
			// 增量：拉最近 2 小时，写入 raw
			from := now.Truncate(time.Hour).Add(-2 * time.Hour)
			to := now.Truncate(time.Hour)
			s.collectRaw(sp, configID, providerName, cfg, zoneID, from, to)
		}
	}
	return nil
}

// backfillHistory 拉取历史数据，按天聚合写入 cdn_stats_daily
// 先尝试完整范围，失败则依次降级（兼容 ESA 免费版仅支持 7 天数据）
func (s *StatsCollectService) backfillHistory(sp provider.StatsProvider, configID int64, providerName string, cfg map[string]string, zoneID string, from, to time.Time) {
	// 按天数降级：原始范围 → 14 天 → 7 天
	attempts := []time.Duration{0, 14 * 24 * time.Hour, 7 * 24 * time.Hour}
	var pts []model.StatPoint

	success := false
	for _, limit := range attempts {
		queryFrom := from
		if limit > 0 {
			capped := to.Add(-limit)
			if capped.After(from) {
				queryFrom = capped
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		var err error
		pts, err = sp.GetTimeSeries(ctx, cfg, zoneID, queryFrom, to)
		cancel()
		if err == nil {
			success = true
			if limit > 0 && limit < to.Sub(from) {
				log.Printf("[StatsCollect] zone %s 降级至 %dd 历史补拉成功", zoneID, int(limit.Hours()/24))
			}
			break
		}
		log.Printf("[StatsCollect] backfill zone %s (from=%s) 失败: %v，尝试降级", zoneID, queryFrom.Format("2006-01-02"), err)
	}
	if !success {
		log.Printf("[StatsCollect] zone %s 历史补拉全部降级后仍失败，跳过", zoneID)
		return
	}
	if len(pts) == 0 {
		// API 成功但该 zone 无流量数据，标记已采集避免重复尝试
		log.Printf("[StatsCollect] zone %s 历史无流量数据，跳过写入", zoneID)
		database.DB.Exec(`
			INSERT INTO cdn_stats_collect_status (config_id, zone_id, last_collected_at)
			VALUES ($1, $2, NOW())
			ON CONFLICT (config_id, zone_id) DO UPDATE SET last_collected_at = NOW()`,
			configID, zoneID,
		)
		// 写一条 0 记录标记已采集，让 dailyCount > 0 避免下次重启再补拉
		database.DB.Exec(`
			INSERT INTO cdn_stats_daily (config_id, provider, zone_id, stat_date, requests, bytes, cached_requests, cached_bytes)
			VALUES ($1, $2, $3, $4, 0, 0, 0, 0)
			ON CONFLICT (config_id, zone_id, stat_date) DO NOTHING`,
			configID, providerName, zoneID, time.Now().UTC().Format("2006-01-02"),
		)
		return
	}

	// 按天聚合
	type dayKey = string
	type dayAgg struct {
		requests, bytes, cached_req, cached_bytes int64
	}
	daily := map[dayKey]*dayAgg{}
	for _, p := range pts {
		day := p.Time.UTC().Format("2006-01-02")
		if daily[day] == nil {
			daily[day] = &dayAgg{}
		}
		daily[day].requests += p.Requests
		daily[day].bytes += p.Bytes
		daily[day].cached_req += p.CachedRequests
		daily[day].cached_bytes += p.CachedBytes
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	for day, agg := range daily {
		database.DB.Exec(`
			INSERT INTO cdn_stats_daily (config_id, provider, zone_id, stat_date, requests, bytes, cached_requests, cached_bytes)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			ON CONFLICT (config_id, zone_id, stat_date) DO UPDATE
			SET requests=$5, bytes=$6, cached_requests=$7, cached_bytes=$8`,
			configID, providerName, zoneID, day,
			agg.requests, agg.bytes, agg.cached_req, agg.cached_bytes,
		)
	}
	database.DB.Exec(`
		INSERT INTO cdn_stats_collect_status (config_id, zone_id, last_collected_at)
		VALUES ($1, $2, NOW())
		ON CONFLICT (config_id, zone_id) DO UPDATE SET last_collected_at = NOW()`,
		configID, zoneID,
	)
	log.Printf("[StatsCollect] zone %s 历史补拉完成，共 %d 天", zoneID, len(daily))
}

// collectRaw 增量拉取写入 cdn_stats_raw
func (s *StatsCollectService) collectRaw(sp provider.StatsProvider, configID int64, providerName string, cfg map[string]string, zoneID string, from, to time.Time) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pts, err := sp.GetTimeSeries(ctx, cfg, zoneID, from, to)
	if err != nil {
		log.Printf("[StatsCollect] zone %s 增量采集失败: %v", zoneID, err)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range pts {
		database.DB.Exec(`
			INSERT INTO cdn_stats_raw (config_id, provider, zone_id, period_start, requests, bytes, cached_requests, cached_bytes)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			ON CONFLICT (config_id, zone_id, period_start) DO UPDATE
			SET requests=$5, bytes=$6, cached_requests=$7, cached_bytes=$8, collected_at=NOW()`,
			configID, providerName, zoneID, p.Time, p.Requests, p.Bytes, p.CachedRequests, p.CachedBytes,
		)
	}
	database.DB.Exec(`
		INSERT INTO cdn_stats_collect_status (config_id, zone_id, last_collected_at)
		VALUES ($1, $2, NOW())
		ON CONFLICT (config_id, zone_id) DO UPDATE SET last_collected_at = NOW()`,
		configID, zoneID,
	)
}

// AggregatePreviousDay 0点聚合：raw→daily，采集昨天 geo，清理旧 raw
func (s *StatsCollectService) AggregatePreviousDay() error {
	yesterday := time.Now().UTC().AddDate(0, 0, -1).Format("2006-01-02")
	log.Printf("[StatsCollect] 开始聚合 %s 的数据", yesterday)

	s.mu.Lock()
	_, err := database.DB.Exec(`
		INSERT INTO cdn_stats_daily (config_id, provider, zone_id, stat_date, requests, bytes, cached_requests, cached_bytes)
		SELECT config_id, provider, zone_id,
		       DATE(period_start AT TIME ZONE 'UTC') AS stat_date,
		       SUM(requests), SUM(bytes), SUM(cached_requests), SUM(cached_bytes)
		FROM cdn_stats_raw
		WHERE DATE(period_start AT TIME ZONE 'UTC') = $1
		GROUP BY config_id, provider, zone_id, stat_date
		ON CONFLICT (config_id, zone_id, stat_date) DO UPDATE
		SET requests=EXCLUDED.requests, bytes=EXCLUDED.bytes,
		    cached_requests=EXCLUDED.cached_requests, cached_bytes=EXCLUDED.cached_bytes`,
		yesterday,
	)
	database.DB.Exec("DELETE FROM cdn_stats_raw WHERE period_start < NOW() - INTERVAL '7 days'")
	s.mu.Unlock()

	if err != nil {
		return err
	}

	// 采集昨天的 geo
	s.collectGeoForDateRange(yesterday, yesterday)
	log.Printf("[StatsCollect] %s 数据聚合完成", yesterday)
	return nil
}

// collectGeoForDateRange 采集指定日期范围的地区分布（startDate/endDate 格式 YYYY-MM-DD）
func (s *StatsCollectService) collectGeoForDateRange(startDate, endDate string) {
	start, _ := time.Parse("2006-01-02", startDate)
	end, _ := time.Parse("2006-01-02", endDate)
	from := start
	to := end.Add(24 * time.Hour)

	tasks := s.loadTasks()
	for _, t := range tasks {
		sp := provider.GetStats(t.provider)
		if sp == nil {
			continue
		}
		zoneIDs := s.getStatsZoneIDs(t.configID, t.provider)

		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		for _, zoneID := range zoneIDs {
			pts, err := sp.GetGeoDistribution(ctx, t.cfg, zoneID, from, to)
			if err != nil {
				log.Printf("[StatsCollect] zone %s geo 采集失败: %v", zoneID, err)
				continue
			}
			s.saveGeo(t.configID, t.provider, zoneID, start, pts)
		}
		cancel()
	}
}

func (s *StatsCollectService) saveGeo(configID int64, providerName, zoneID string, date time.Time, pts []model.GeoPoint) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range pts {
		database.DB.Exec(`
			INSERT INTO cdn_stats_geo (config_id, provider, zone_id, stat_date, country_code, country_name, requests, bytes)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			ON CONFLICT (config_id, zone_id, stat_date, country_code) DO UPDATE
			SET requests=EXCLUDED.requests, bytes=EXCLUDED.bytes`,
			configID, providerName, zoneID, date, p.CountryCode, p.CountryName, p.Requests, p.Bytes,
		)
	}
}

// getStatsZoneIDs 获取用于流量统计的 zone/site ID 列表
// - 过滤掉 alidns:xxx 等 DNS 专用 zone（含冒号的非标准 ID）
// - Aliyun ESA 和 EdgeOne 的 CDN 站点 ID 从 deployments 补充（DNS 同步未必覆盖）
func (s *StatsCollectService) getStatsZoneIDs(configID int64, providerName string) []string {
	seen := map[string]bool{}
	var ids []string

	addUniq := func(id string) {
		if id != "" && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}

	// 从 DNS 缓存表取（仅保留无冒号的标准 zone ID，过滤 alidns:xxx 格式）
	rows, err := database.DB.Query(
		"SELECT zone_id FROM dns_cache_zones WHERE config_id = $1 AND zone_id NOT LIKE '%:%'", configID,
	)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var z string
			rows.Scan(&z)
			addUniq(z)
		}
	}

	// 对于 Aliyun ESA 和 EdgeOne：CDN 站点 ID 存在 deployments.provider_site_id 中
	// Cloudflare NS 模式 provider_site_id = zone ID（与 dns_cache_zones 重复），CNAME 模式为 hostname ID（不适合统计），跳过
	if providerName == "aliyun" || providerName == "edgeone" {
		deplRows, err := database.DB.Query(
			`SELECT DISTINCT provider_site_id FROM deployments
			 WHERE config_id = $1 AND provider = $2 AND provider_site_id != '' AND status NOT IN ('pending','error')`,
			configID, providerName,
		)
		if err == nil {
			defer deplRows.Close()
			for deplRows.Next() {
				var z string
				deplRows.Scan(&z)
				addUniq(z)
			}
		}
	}

	return ids
}
