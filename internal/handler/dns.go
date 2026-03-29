package handler

import (
	"allfast/internal/database"
	"allfast/internal/model"
	"allfast/internal/provider"
	"allfast/internal/service"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
)

// DnsListZones 列出指定提供商配置下的所有 DNS 域名区域（优先读缓存）
// GET /api/dns/zones?config_id=1
func DnsListZones(c *gin.Context) {
	configID := c.Query("config_id")
	if configID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "缺少 config_id 参数"})
		return
	}
	cid, _ := strconv.ParseInt(configID, 10, 64)

	// 缓存新鲜则直接返回（包括空列表）
	if service.DnsSync.IsZoneCacheFresh(cid) {
		zones, err := service.DnsSync.GetCachedZones(cid)
		if err == nil {
			if zones == nil {
				zones = []model.DnsZone{}
			}
			lastSync := service.DnsSync.GetLastSyncTime(cid)
			c.JSON(http.StatusOK, gin.H{"zones": zones, "last_sync": lastSync, "from_cache": true})
			return
		}
	}

	// 缓存过期，同步拉取并写入缓存（DNS缓存用独立DB，不阻塞主库）
	cfg, providerName, err := loadProviderConfig(configID)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if err := service.DnsSync.SyncZonesForConfig(cid, providerName, cfg); err != nil {
		// 同步失败返回空列表，不返回 500，防止前端卡死
		c.JSON(http.StatusOK, gin.H{
			"zones":      []model.DnsZone{},
			"last_sync":  nil,
			"from_cache": false,
			"error":      fmt.Sprintf("同步域名列表失败: %v", err),
		})
		return
	}

	zones, _ := service.DnsSync.GetCachedZones(cid)
	if zones == nil {
		zones = []model.DnsZone{}
	}
	lastSync := service.DnsSync.GetLastSyncTime(cid)
	c.JSON(http.StatusOK, gin.H{"zones": zones, "last_sync": lastSync, "from_cache": false})
}

// DnsListRecords 列出指定 Zone 的 DNS 记录（优先读缓存，支持分页）
// GET /api/dns/records?config_id=1&zone_id=xxx&page=1&page_size=20
func DnsListRecords(c *gin.Context) {
	configID := c.Query("config_id")
	zoneID := c.Query("zone_id")
	if configID == "" || zoneID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "缺少 config_id 或 zone_id 参数"})
		return
	}
	cid, _ := strconv.ParseInt(configID, 10, 64)
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("page_size", "10"))
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = 20
	} else if pageSize > 500 {
		pageSize = 500
	}

	cfg, providerName, err := loadProviderConfig(configID)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	dnsProv, err := provider.GetDNS(providerName)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	features := dnsProv.Features(zoneID)
	// 根据 zone 套餐限制最小 TTL
	if providerName == "aliyun" && strings.HasPrefix(zoneID, "alidns:") {
		zones, _ := service.DnsSync.GetCachedZones(cid)
		for _, z := range zones {
			if z.ID == zoneID {
				switch z.PlanName {
				case "企业标准版", "企业旗舰版", "企业至尊版":
					features.MinTTL = 60
				default:
					features.MinTTL = 600
				}
				break
			}
		}
		if features.MinTTL == 0 {
			features.MinTTL = 600 // 未能匹配时默认免费版限制
		}
	}
	meta := gin.H{
		"record_types": dnsProv.SupportedRecordTypes(),
		"lines":        dnsProv.SupportedLines(),
		"features":     features,
	}

	// 尝试读缓存
	total := service.DnsSync.GetCachedRecordCount(cid, zoneID)
	if total > 0 {
		cached, _ := service.DnsSync.GetCachedRecordsPaged(cid, zoneID, page, pageSize)
		result := gin.H{"records": cached, "total": total, "page": page, "page_size": pageSize, "from_cache": true}
		for k, v := range meta {
			result[k] = v
		}
		c.JSON(http.StatusOK, result)
		return
	}

	// 缓存无数据，同步拉取并写入缓存
	if err := service.DnsSync.SyncRecordsForZone(cid, providerName, cfg, zoneID); err != nil {
		result := gin.H{"records": []model.DnsRecord{}, "total": 0, "page": page, "page_size": pageSize, "from_cache": false, "error": fmt.Sprintf("同步记录失败: %v", err)}
		for k, v := range meta {
			result[k] = v
		}
		c.JSON(http.StatusOK, result)
		return
	}

	total = service.DnsSync.GetCachedRecordCount(cid, zoneID)
	records, _ := service.DnsSync.GetCachedRecordsPaged(cid, zoneID, page, pageSize)
	result := gin.H{"records": records, "total": total, "page": page, "page_size": pageSize, "from_cache": false}
	for k, v := range meta {
		result[k] = v
	}
	c.JSON(http.StatusOK, result)
}

// DnsAddRecord 添加 DNS 记录
// POST /api/dns/records { config_id, zone_id, record }
func DnsAddRecord(c *gin.Context) {
	var body struct {
		ConfigID string                 `json:"config_id" binding:"required"`
		ZoneID   string                 `json:"zone_id" binding:"required"`
		Record   model.DnsRecordRequest `json:"record" binding:"required"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求参数错误: " + err.Error()})
		return
	}

	cfg, providerName, err := loadProviderConfig(body.ConfigID)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	dnsProv, err := provider.GetDNS(providerName)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	record, err := dnsProv.AddRecord(c.Request.Context(), cfg, body.ZoneID, body.Record)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("添加DNS记录失败: %v", err)})
		return
	}

	// 写操作后同步刷新记录缓存
	cid, _ := strconv.ParseInt(body.ConfigID, 10, 64)
	_ = service.DnsSync.SyncRecordsForZone(cid, providerName, cfg, body.ZoneID)

	c.JSON(http.StatusOK, gin.H{"record": record})
}

// DnsUpdateRecord 更新 DNS 记录
// PUT /api/dns/records/:id { config_id, zone_id, record }
func DnsUpdateRecord(c *gin.Context) {
	recordID := c.Param("id")

	var body struct {
		ConfigID string                 `json:"config_id" binding:"required"`
		ZoneID   string                 `json:"zone_id" binding:"required"`
		Record   model.DnsRecordRequest `json:"record" binding:"required"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求参数错误: " + err.Error()})
		return
	}

	cfg, providerName, err := loadProviderConfig(body.ConfigID)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	dnsProv, err := provider.GetDNS(providerName)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if err := dnsProv.UpdateRecord(c.Request.Context(), cfg, body.ZoneID, recordID, body.Record); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("更新DNS记录失败: %v", err)})
		return
	}

	// 写操作后同步刷新记录缓存
	cid, _ := strconv.ParseInt(body.ConfigID, 10, 64)
	_ = service.DnsSync.SyncRecordsForZone(cid, providerName, cfg, body.ZoneID)

	c.JSON(http.StatusOK, gin.H{"success": true})
}

// DnsDeleteRecord 删除 DNS 记录
// DELETE /api/dns/records/:id?config_id=1&zone_id=xxx
func DnsDeleteRecord(c *gin.Context) {
	recordID := c.Param("id")
	configID := c.Query("config_id")
	zoneID := c.Query("zone_id")
	if configID == "" || zoneID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "缺少 config_id 或 zone_id 参数"})
		return
	}

	cfg, providerName, err := loadProviderConfig(configID)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	dnsProv, err := provider.GetDNS(providerName)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if err := dnsProv.DeleteRecord(c.Request.Context(), cfg, zoneID, recordID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("删除DNS记录失败: %v", err)})
		return
	}

	// 写操作后同步刷新记录缓存
	cid, _ := strconv.ParseInt(configID, 10, 64)
	_ = service.DnsSync.SyncRecordsForZone(cid, providerName, cfg, zoneID)

	c.JSON(http.StatusOK, gin.H{"success": true})
}

// DnsSetRecordStatus 启用/禁用 DNS 记录
// PUT /api/dns/records/:id/status?config_id=1&zone_id=xxx
func DnsSetRecordStatus(c *gin.Context) {
	recordID := c.Param("id")
	configID := c.Query("config_id")
	zoneID := c.Query("zone_id")
	if configID == "" || zoneID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "缺少 config_id 或 zone_id 参数"})
		return
	}

	var body struct {
		Enable bool `json:"enable"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "参数错误"})
		return
	}

	cfg, providerName, err := loadProviderConfig(configID)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	dnsProv, err := provider.GetDNS(providerName)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if err := dnsProv.SetRecordStatus(c.Request.Context(), cfg, zoneID, recordID, body.Enable); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("设置记录状态失败: %v", err)})
		return
	}

	cid, _ := strconv.ParseInt(configID, 10, 64)
	// Cloudflare 程序级禁用：保留缓存并标记为暂停，避免记录从列表消失
	if providerName == "cloudflare" && !body.Enable {
		database.DnsCacheDB.Exec(
			"UPDATE dns_cache_records SET status = $1 WHERE config_id = $2 AND zone_id = $3 AND record_id = $4",
			"disable", cid, zoneID, recordID,
		)
	} else {
		// 其他场景统一同步刷新记录缓存
		_ = service.DnsSync.SyncRecordsForZone(cid, providerName, cfg, zoneID)
	}

	c.JSON(http.StatusOK, gin.H{"success": true})
}

// DnsSyncZones 手动触发 Zone 同步
// POST /api/dns/sync?config_id=1
func DnsSyncZones(c *gin.Context) {
	configID := c.Query("config_id")
	if configID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "缺少 config_id 参数"})
		return
	}
	cid, _ := strconv.ParseInt(configID, 10, 64)

	cfg, providerName, err := loadProviderConfig(configID)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if err := service.DnsSync.SyncZonesForConfig(cid, providerName, cfg); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("同步失败: %v", err)})
		return
	}

	zones, _ := service.DnsSync.GetCachedZones(cid)
	lastSync := service.DnsSync.GetLastSyncTime(cid)
	c.JSON(http.StatusOK, gin.H{"zones": zones, "last_sync": lastSync})
}

// DnsSyncRecords 手动触发单个 Zone 记录同步
// POST /api/dns/sync-records?config_id=1&zone_id=xxx
func DnsSyncRecords(c *gin.Context) {
	configID := c.Query("config_id")
	zoneID := c.Query("zone_id")
	if configID == "" || zoneID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "缺少 config_id 或 zone_id 参数"})
		return
	}
	cid, _ := strconv.ParseInt(configID, 10, 64)

	cfg, providerName, err := loadProviderConfig(configID)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if err := service.DnsSync.SyncRecordsForZone(cid, providerName, cfg, zoneID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("同步记录失败: %v", err)})
		return
	}

	dnsProv, _ := provider.GetDNS(providerName)
	records, _ := service.DnsSync.GetCachedRecords(cid, zoneID)

	result := gin.H{"records": records, "from_cache": false}
	if dnsProv != nil {
		result["record_types"] = dnsProv.SupportedRecordTypes()
		result["lines"] = dnsProv.SupportedLines()
		result["features"] = dnsProv.Features(zoneID)
	}
	c.JSON(http.StatusOK, result)
}

// DnsAllCachedZones 仅从数据库缓存返回所有配置的 Zone 列表（不触发外部同步）
// GET /api/dns/all-cached-zones
func DnsAllCachedZones(c *gin.Context) {
	type entry struct {
		ConfigID   int64  `json:"config_id"`
		ConfigName string `json:"config_name"`
		ZoneID     string `json:"zone_id"`
		ZoneName   string `json:"zone_name"`
	}
	rows, err := database.DnsCacheDB.Query(`
		SELECT z.config_id, COALESCE(p.name, ''), z.zone_id, z.zone_name
		FROM dns_cache_zones z
		LEFT JOIN provider_configs p ON p.id = z.config_id
		ORDER BY z.config_id, z.zone_name
	`)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{"zones": []entry{}})
		return
	}
	defer rows.Close()
	zones := []entry{}
	for rows.Next() {
		var e entry
		if err := rows.Scan(&e.ConfigID, &e.ConfigName, &e.ZoneID, &e.ZoneName); err == nil {
			zones = append(zones, e)
		}
	}
	c.JSON(http.StatusOK, gin.H{"zones": zones})
}

// cleanupDnsRecordsForCnames 从所有匹配 domain 的缓存 Zone 中删除指向 cnames 列表的 CNAME 记录（非致命）
func cleanupDnsRecordsForCnames(domain string, cnames []string) {
	if len(cnames) == 0 {
		return
	}
	cnameSet := make(map[string]bool)
	for _, c := range cnames {
		if c != "" {
			cnameSet[c] = true
		}
	}
	if len(cnameSet) == 0 {
		return
	}
	ctx := context.Background()

	// 从缓存查找与 domain 匹配的 Zone
	rows, err := database.DnsCacheDB.Query(`
		SELECT z.config_id, z.zone_id, z.zone_name
		FROM dns_cache_zones z ORDER BY z.zone_name DESC`)
	if err != nil {
		return
	}
	type zoneEntry struct {
		ConfigID int64
		ZoneID   string
		ZoneName string
	}
	var matched []zoneEntry
	for rows.Next() {
		var e zoneEntry
		if rows.Scan(&e.ConfigID, &e.ZoneID, &e.ZoneName) == nil {
			if domain == e.ZoneName || strings.HasSuffix(domain, "."+e.ZoneName) {
				matched = append(matched, e)
			}
		}
	}
	rows.Close()

	for _, ze := range matched {
		cfg, providerName, err := loadProviderConfig(fmt.Sprintf("%d", ze.ConfigID))
		if err != nil {
			continue
		}
		dnsProv, err := provider.GetDNS(providerName)
		if err != nil {
			continue
		}

		// 从缓存取记录
		records, err := service.DnsSync.GetCachedRecords(ze.ConfigID, ze.ZoneID)
		if err != nil || len(records) == 0 {
			// 缓存为空则先同步一次再取
			_ = service.DnsSync.SyncRecordsForZone(ze.ConfigID, providerName, cfg, ze.ZoneID)
			records, _ = service.DnsSync.GetCachedRecords(ze.ConfigID, ze.ZoneID)
		}

		hostname := domain
		if strings.HasSuffix(domain, "."+ze.ZoneName) {
			hostname = domain[:len(domain)-len(ze.ZoneName)-1]
		}

		var deleted int
		for _, r := range records {
			if r.Type != "CNAME" {
				continue
			}
			if r.HostRecord != hostname && r.Name != domain && r.Name != hostname {
				continue
			}
			if !cnameSet[r.Value] {
				continue
			}
			if err := dnsProv.DeleteRecord(ctx, cfg, ze.ZoneID, r.ID); err != nil {
				log.Printf("[DnsCleanup] 删除记录失败 [%s/%s]: %v", ze.ZoneName, r.Value, err)
			} else {
				log.Printf("[DnsCleanup] 已删除 CNAME %s → %s", domain, r.Value)
				deleted++
			}
		}
		if deleted > 0 {
			_ = service.DnsSync.SyncRecordsForZone(ze.ConfigID, providerName, cfg, ze.ZoneID)
		}
	}
}

// loadProviderConfig 加载提供商配置，返回解析后的 cfg map 和提供商名称
func loadProviderConfig(configID string) (map[string]string, string, error) {
	var providerName, configJSON string
	err := database.DB.QueryRow(
		"SELECT provider, config FROM provider_configs WHERE id = $1", configID,
	).Scan(&providerName, &configJSON)
	if err != nil {
		return nil, "", fmt.Errorf("未找到配置 ID=%s", configID)
	}

	cfg := map[string]string{}
	if err := json.Unmarshal([]byte(configJSON), &cfg); err != nil {
		return nil, "", fmt.Errorf("解析配置失败: %v", err)
	}

	return cfg, providerName, nil
}
