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

const (
	// DNS 缓存有效期 15 分钟
	dnsCacheTTL = 15 * time.Minute
	// 后台同步间隔
	dnsSyncInterval = 15 * time.Minute
)

// DnsSyncService DNS 缓存同步服务
type DnsSyncService struct {
	mu sync.Mutex
}

var DnsSync = &DnsSyncService{}

// StartBackgroundSync 启动后台定时同步
func (s *DnsSyncService) StartBackgroundSync() {
	go func() {
		// 启动后先等 10 秒让数据库就绪
		time.Sleep(10 * time.Second)
		log.Println("[DnsSync] 后台同步任务启动，间隔15分钟")
		s.SyncAllConfigs()

		ticker := time.NewTicker(dnsSyncInterval)
		defer ticker.Stop()
		for range ticker.C {
			s.SyncAllConfigs()
		}
	}()
}

// SyncAllConfigs 同步所有启用的提供商配置的 DNS Zone（并发请求 API，串行写 SQLite）
func (s *DnsSyncService) SyncAllConfigs() {
	rows, err := database.DB.Query("SELECT id, provider, config FROM provider_configs WHERE enabled = 1")
	if err != nil {
		log.Printf("[DnsSync] 查询提供商配置失败: %v", err)
		return
	}
	defer rows.Close()

	type syncTask struct {
		configID     int64
		providerName string
		cfg          map[string]string
		dnsProv      provider.DNSProvider
	}
	var tasks []syncTask

	for rows.Next() {
		var configID int64
		var providerName, configJSON string
		if err := rows.Scan(&configID, &providerName, &configJSON); err != nil {
			continue
		}
		cfg := map[string]string{}
		if err := json.Unmarshal([]byte(configJSON), &cfg); err != nil {
			continue
		}
		dnsProv, err := provider.GetDNS(providerName)
		if err != nil {
			continue
		}
		tasks = append(tasks, syncTask{configID, providerName, cfg, dnsProv})
	}

	// 并发同步所有配置
	var wg sync.WaitGroup
	for _, t := range tasks {
		wg.Add(1)
		go func(t syncTask) {
			defer wg.Done()
			log.Printf("[DnsSync] 同步配置 ID=%d (%s) 的域名列表...", t.configID, t.providerName)
			if err := s.syncZones(t.configID, t.dnsProv, t.cfg); err != nil {
				log.Printf("[DnsSync] 同步配置 ID=%d Zone列表失败: %v", t.configID, err)
				return
			}

			// 预拉取每个 Zone 的记录
			zones, err := s.GetCachedZones(t.configID)
			if err != nil {
				return
			}
			for _, z := range zones {
				if err := s.syncRecords(t.configID, z.ID, t.dnsProv, t.cfg); err != nil {
					log.Printf("[DnsSync] 预拉取 %s 记录失败: %v", z.Name, err)
				}
			}
			log.Printf("[DnsSync] 配置 ID=%d 全部记录预拉取完成", t.configID)
		}(t)
	}
	wg.Wait()
}

// SyncZonesForConfig 手动触发单个配置的 Zone 同步
func (s *DnsSyncService) SyncZonesForConfig(configID int64, providerName string, cfg map[string]string) error {
	dnsProv, err := provider.GetDNS(providerName)
	if err != nil {
		return err
	}
	return s.syncZones(configID, dnsProv, cfg)
}

// SyncRecordsForZone 手动触发单个 Zone 的记录同步
func (s *DnsSyncService) SyncRecordsForZone(configID int64, providerName string, cfg map[string]string, zoneID string) error {
	dnsProv, err := provider.GetDNS(providerName)
	if err != nil {
		return err
	}
	return s.syncRecords(configID, zoneID, dnsProv, cfg)
}

// GetCachedZones 获取缓存的 Zone 列表
func (s *DnsSyncService) GetCachedZones(configID int64) ([]model.DnsZone, error) {
	rows, err := database.DnsCacheDB.Query(
		`SELECT z.zone_id, z.zone_name, z.zone_status,
		        COALESCE(rc.cnt, z.record_count) AS record_count,
		        z.plan_name
		 FROM dns_cache_zones z
		 LEFT JOIN (
		   SELECT zone_id, COUNT(*) AS cnt FROM dns_cache_records WHERE config_id = $1 GROUP BY zone_id
		 ) rc ON rc.zone_id = z.zone_id
		 WHERE z.config_id = $2 ORDER BY z.zone_name`,
		configID, configID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var zones []model.DnsZone
	for rows.Next() {
		var z model.DnsZone
		if err := rows.Scan(&z.ID, &z.Name, &z.Status, &z.RecordCount, &z.PlanName); err != nil {
			continue
		}
		zones = append(zones, z)
	}
	return zones, nil
}

// GetCachedRecords 获取缓存的记录列表（支持分页）
func (s *DnsSyncService) GetCachedRecords(configID int64, zoneID string) ([]model.DnsRecord, error) {
	return s.GetCachedRecordsPaged(configID, zoneID, 0, 0)
}

// GetCachedRecordsPaged 分页获取缓存记录，page/pageSize 为 0 时返回全部
func (s *DnsSyncService) GetCachedRecordsPaged(configID int64, zoneID string, page, pageSize int) ([]model.DnsRecord, error) {
	query := `SELECT record_id, record_type, host_record, name, value, ttl, priority, weight, proxied, line, line_label, status, remark
		 FROM dns_cache_records WHERE config_id = $1 AND zone_id = $2 ORDER BY record_type, name`
	args := []interface{}{configID, zoneID}
	if pageSize > 0 && page > 0 {
		offset := (page - 1) * pageSize
		query += " LIMIT $3 OFFSET $4"
		args = append(args, pageSize, offset)
	}
	rows, err := database.DnsCacheDB.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var records []model.DnsRecord
	for rows.Next() {
		var r model.DnsRecord
		var proxied int
		if err := rows.Scan(&r.ID, &r.Type, &r.HostRecord, &r.Name, &r.Value, &r.TTL, &r.Priority, &r.Weight, &proxied, &r.Line, &r.LineLabel, &r.Status, &r.Remark); err != nil {
			continue
		}
		r.ZoneID = zoneID
		if proxied >= 0 {
			v := proxied == 1
			r.Proxied = &v
		}
		records = append(records, r)
	}
	return records, nil
}

// GetCachedRecordCount 获取缓存记录总数
func (s *DnsSyncService) GetCachedRecordCount(configID int64, zoneID string) int {
	var count int
	database.DnsCacheDB.QueryRow(
		"SELECT COUNT(*) FROM dns_cache_records WHERE config_id = $1 AND zone_id = $2",
		configID, zoneID,
	).Scan(&count)
	return count
}

// IsZoneCacheFresh 检查 Zone 缓存是否新鲜（< 15 分钟）
func (s *DnsSyncService) IsZoneCacheFresh(configID int64) bool {
	var lastSync *time.Time
	err := database.DnsCacheDB.QueryRow(
		"SELECT last_zone_sync FROM dns_sync_status WHERE config_id = $1", configID,
	).Scan(&lastSync)
	if err != nil || lastSync == nil {
		return false
	}
	return time.Since(*lastSync) < dnsCacheTTL
}

// GetLastSyncTime 获取上次同步时间
func (s *DnsSyncService) GetLastSyncTime(configID int64) *time.Time {
	var lastSync *time.Time
	database.DnsCacheDB.QueryRow(
		"SELECT last_zone_sync FROM dns_sync_status WHERE config_id = $1", configID,
	).Scan(&lastSync)
	return lastSync
}

// ===== 内部同步方法 =====

func (s *DnsSyncService) syncZones(configID int64, dnsProv provider.DNSProvider, cfg map[string]string) error {
	// 更新同步状态为 syncing（加锁写 DB）
	s.mu.Lock()
	database.DnsCacheDB.Exec(
		"INSERT INTO dns_sync_status (config_id, status) VALUES ($1, 'syncing') ON CONFLICT(config_id) DO UPDATE SET status = 'syncing'",
		configID,
	)
	s.mu.Unlock()

	// API 调用不持锁
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	zones, err := dnsProv.ListZones(ctx, cfg)
	if err != nil {
		log.Printf("[DnsSync] 同步配置 ID=%d Zone列表失败: %v", configID, err)
		s.mu.Lock()
		database.DnsCacheDB.Exec(
			"UPDATE dns_sync_status SET status = 'error' WHERE config_id = $1", configID,
		)
		s.mu.Unlock()
		return err
	}

	// 加锁写入缓存
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := database.DnsCacheDB.Begin()
	if err != nil {
		return err
	}

	tx.Exec("DELETE FROM dns_cache_zones WHERE config_id = $1", configID)
	for _, z := range zones {
		tx.Exec(
			"INSERT INTO dns_cache_zones (config_id, zone_id, zone_name, zone_status, record_count, plan_name, synced_at) VALUES ($1, $2, $3, $4, $5, $6, $7)",
			configID, z.ID, z.Name, z.Status, z.RecordCount, z.PlanName, time.Now(),
		)
	}

	if err := tx.Commit(); err != nil {
		return err
	}

	// 更新同步状态
	database.DnsCacheDB.Exec(
		"INSERT INTO dns_sync_status (config_id, last_zone_sync, status) VALUES ($1, $2, 'idle') ON CONFLICT(config_id) DO UPDATE SET last_zone_sync = EXCLUDED.last_zone_sync, status = 'idle'",
		configID, time.Now(),
	)

	log.Printf("[DnsSync] 配置 ID=%d 同步完成，共 %d 个域名", configID, len(zones))
	return nil
}

func (s *DnsSyncService) syncRecords(configID int64, zoneID string, dnsProv provider.DNSProvider, cfg map[string]string) error {
	// API 调用不持锁
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	records, err := dnsProv.ListRecords(ctx, cfg, zoneID)
	if err != nil {
		return err
	}

	// 加锁写入 SQLite
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := database.DnsCacheDB.Begin()
	if err != nil {
		return err
	}

	tx.Exec("DELETE FROM dns_cache_records WHERE config_id = $1 AND zone_id = $2", configID, zoneID)
	for _, r := range records {
		proxied := -1
		if r.Proxied != nil {
			if *r.Proxied {
				proxied = 1
			} else {
				proxied = 0
			}
		}
		tx.Exec(
			`INSERT INTO dns_cache_records (config_id, zone_id, record_id, record_type, host_record, name, value, ttl, priority, weight, proxied, line, line_label, status, remark, synced_at)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)`,
			configID, zoneID, r.ID, r.Type, r.HostRecord, r.Name, r.Value, r.TTL, r.Priority, r.Weight, proxied, r.Line, r.LineLabel, r.Status, r.Remark, time.Now(),
		)
	}

	if err := tx.Commit(); err != nil {
		return err
	}

	// 同步完成后更新 zone 的记录数量
	database.DnsCacheDB.Exec(
		"UPDATE dns_cache_zones SET record_count = $1 WHERE config_id = $2 AND zone_id = $3",
		len(records), configID, zoneID,
	)
	return nil
}
