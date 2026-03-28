package handler

import (
	"allfast/internal/database"
	"allfast/internal/model"
	"allfast/internal/provider"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/lib/pq"
)

// SiteSummary 站点列表条目（含部署摘要）
type SiteSummary struct {
	model.Site
	Deployments []DeploymentSummary `json:"deployments"`
}

// DeploymentSummary 部署摘要（列表页用）
type DeploymentSummary struct {
	Provider   string `json:"provider"`
	ConfigName string `json:"config_name"`
	Status     string `json:"status"`
	CDNCname   string `json:"cdn_cname"`
}

// 获取站点列表
func ListSites(c *gin.Context) {
	rows, err := database.DB.Query(
		"SELECT id, domain, origin, origin_protocol, http_port, https_port, origin_host, status, config_auto_sync, created_at, updated_at FROM sites ORDER BY id DESC",
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "查询站点列表失败"})
		return
	}
	defer rows.Close()

	var sites []SiteSummary
	var siteIDs []int64
	for rows.Next() {
		var s model.Site
		if err := rows.Scan(&s.ID, &s.Domain, &s.Origin, &s.OriginProtocol, &s.HTTPPort, &s.HTTPSPort, &s.OriginHost, &s.Status, &s.ConfigAutoSync, &s.CreatedAt, &s.UpdatedAt); err != nil {
			continue
		}
		sites = append(sites, SiteSummary{Site: s, Deployments: []DeploymentSummary{}})
		siteIDs = append(siteIDs, s.ID)
	}

	// 批量拉取所有站点的部署摘要，避免 N+1 查询
	if len(siteIDs) > 0 {
		depRows, depErr := database.DB.Query(
			"SELECT site_id, provider, config_name, status, cdn_cname FROM deployments WHERE site_id = ANY($1) ORDER BY id",
			pq.Array(siteIDs),
		)
		if depErr == nil {
			defer depRows.Close()
			depMap := map[int64][]DeploymentSummary{}
			for depRows.Next() {
				var siteID int64
				var d DeploymentSummary
				if err := depRows.Scan(&siteID, &d.Provider, &d.ConfigName, &d.Status, &d.CDNCname); err == nil {
					depMap[siteID] = append(depMap[siteID], d)
				}
			}
			for i := range sites {
				if deps, ok := depMap[sites[i].ID]; ok {
					sites[i].Deployments = deps
				}
			}
		}
	}

	if sites == nil {
		sites = []SiteSummary{}
	}
	c.JSON(http.StatusOK, gin.H{"sites": sites})
}

// 创建站点
func CreateSite(c *gin.Context) {
	var req model.CreateSiteRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请提供域名和源站地址"})
		return
	}

	// 默认值
	if req.OriginProtocol == "" {
		req.OriginProtocol = "follow"
	}
	if req.HTTPPort <= 0 {
		req.HTTPPort = 80
	}
	if req.HTTPSPort <= 0 {
		req.HTTPSPort = 443
	}
	if req.OriginHost == "" {
		req.OriginHost = req.Domain
	}

	// 检查域名是否已存在
	var count int
	database.DB.QueryRow("SELECT COUNT(*) FROM sites WHERE domain = $1", req.Domain).Scan(&count)
	if count > 0 {
		c.JSON(http.StatusConflict, gin.H{"error": "该域名已存在"})
		return
	}

	now := time.Now()
	var id int64
	err := database.DB.QueryRow(
		`INSERT INTO sites (domain, origin, origin_protocol, http_port, https_port, origin_host, status, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, 'pending', $7, $8) RETURNING id`,
		req.Domain, req.Origin, req.OriginProtocol, req.HTTPPort, req.HTTPSPort, req.OriginHost, now, now,
	).Scan(&id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "创建站点失败"})
		return
	}
	site := model.Site{
		ID:             id,
		Domain:         req.Domain,
		Origin:         req.Origin,
		OriginProtocol: req.OriginProtocol,
		HTTPPort:       req.HTTPPort,
		HTTPSPort:      req.HTTPSPort,
		OriginHost:     req.OriginHost,
		Status:         "pending",
		CreatedAt:      now,
		UpdatedAt:      now,
	}

	// 如果选择了CDN账户，后台异步部署（立即返回站点ID，不阻塞等待）
	if len(req.ConfigIDs) > 0 {
		if err := validateProviderUniquePerSite(id, req.ConfigIDs); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		// 先将站点状态置为部署中
		database.DB.Exec("UPDATE sites SET status = 'deploying', updated_at = $1 WHERE id = $2", now, id)
		site.Status = "deploying"
		// 后台并发部署，不阻塞当前请求
		go deploySvc.DeploySite(context.Background(), id, req.ConfigIDs, req.DeployParams)
	}

	c.JSON(http.StatusCreated, gin.H{"site": site})
}

// mergeDeployParamsToMap 将 deploy_params JSON 合并到 cfg map
func mergeDeployParamsToMap(cfg map[string]string, deployParamsJSON string) {
	if deployParamsJSON == "" || deployParamsJSON == "{}" {
		return
	}
	var params map[string]string
	if err := json.Unmarshal([]byte(deployParamsJSON), &params); err != nil {
		return
	}
	for k, v := range params {
		cfg[k] = v
	}
}

// 获取站点详情（包含部署、DNS、证书信息）
func GetSite(c *gin.Context) {
	siteID := c.Param("id")

	var site model.Site
	err := database.DB.QueryRow(
		"SELECT id, domain, origin, origin_protocol, http_port, https_port, origin_host, status, config_auto_sync, created_at, updated_at FROM sites WHERE id = $1",
		siteID,
	).Scan(&site.ID, &site.Domain, &site.Origin, &site.OriginProtocol, &site.HTTPPort, &site.HTTPSPort, &site.OriginHost, &site.Status, &site.ConfigAutoSync, &site.CreatedAt, &site.UpdatedAt)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "站点不存在"})
		return
	}

	// 查询部署记录
	deployments := []model.Deployment{}
	dRows, err := database.DB.Query(
		"SELECT id, site_id, provider, config_id, config_name, status, provider_site_id, cdn_cname, deploy_params, error_message, deploy_log, created_at, updated_at FROM deployments WHERE site_id = $1",
		siteID,
	)
	if err == nil {
		defer dRows.Close()
		for dRows.Next() {
			var d model.Deployment
			if err := dRows.Scan(&d.ID, &d.SiteID, &d.Provider, &d.ConfigID, &d.ConfigName, &d.Status, &d.ProviderSiteID, &d.CDNCname, &d.DeployParams, &d.ErrorMessage, &d.DeployLog, &d.CreatedAt, &d.UpdatedAt); err != nil {
				continue
			}
			deployments = append(deployments, d)
		}
	}

	// 对非终态部署实时从 CDN平台刷新状态
	for i, d := range deployments {
		if d.Status == "active" || d.Status == "failed" {
			continue
		}
		// provider_site_id 为空说明部署尚未完成，跳过刷新避免空 ID 传到 CDN API
		if d.ProviderSiteID == "" {
			continue
		}
		p, err := provider.Get(d.Provider)
		if err != nil {
			continue
		}
		cfg := getProviderCfgByID(d.ConfigID)
		if cfg == nil {
			continue
		}
		mergeDeployParamsToMap(cfg, d.DeployParams)
		result, err := p.GetDomainStatus(c.Request.Context(), cfg, site.Domain, d.ProviderSiteID)
		if err != nil {
			log.Printf("[Deploy] 刷新部署状态失败 [%s]: %v", d.Provider, err)
			continue
		}
		if result.Status != d.Status {
			deployments[i].Status = result.Status
			database.DB.Exec(
				"UPDATE deployments SET status = $1, updated_at = $2 WHERE id = $3",
				result.Status, time.Now(), d.ID,
			)
			log.Printf("[Deploy] 部署 %d 状态更新: %s -> %s", d.ID, d.Status, result.Status)
		}
	}

	// 查询 DNS 记录
	dnsRecords := []model.DNSRecord{}
	nRows, err := database.DB.Query(
		"SELECT id, site_id, deployment_id, record_type, name, value, purpose, status, created_at FROM dns_records WHERE site_id = $1",
		siteID,
	)
	if err == nil {
		defer nRows.Close()
		for nRows.Next() {
			var r model.DNSRecord
			if err := nRows.Scan(&r.ID, &r.SiteID, &r.DeploymentID, &r.RecordType, &r.Name, &r.Value, &r.Purpose, &r.Status, &r.CreatedAt); err != nil {
				continue
			}
			dnsRecords = append(dnsRecords, r)
		}
	}

	// 查询证书
	certs := []model.Certificate{}
	cRows, err := database.DB.Query(
		"SELECT id, site_id, deployment_id, provider, status, domain, cert_id, expires_at, error_message, created_at, updated_at FROM certificates WHERE site_id = $1",
		siteID,
	)
	if err == nil {
		defer cRows.Close()
		for cRows.Next() {
			var cert model.Certificate
			if err := cRows.Scan(&cert.ID, &cert.SiteID, &cert.DeploymentID, &cert.Provider, &cert.Status, &cert.Domain, &cert.CertID, &cert.ExpiresAt, &cert.ErrorMessage, &cert.CreatedAt, &cert.UpdatedAt); err != nil {
				continue
			}
			certs = append(certs, cert)
		}
	}

	// 对非终态证书实时从CDN平台刷新状态
	for i, cert := range certs {
		if cert.Status == "active" || cert.Status == "failed" {
			continue
		}
		// 找到对应的部署记录获取 config_id 和 deploy_params
		for _, d := range deployments {
			if d.ID == cert.DeploymentID {
				p, err := provider.Get(d.Provider)
				if err != nil {
					break
				}
				cfg := getProviderCfgByID(d.ConfigID)
				if cfg == nil {
					break
				}
				// 合并部署参数
				mergeDeployParamsToMap(cfg, d.DeployParams)
				result, err := p.GetCertificateStatus(c.Request.Context(), cfg, cert.Domain, cert.CertID)
				if err != nil {
					log.Printf("[Cert] 刷新证书状态失败 [%s]: %v", cert.Domain, err)
					break
				}
				if result.Status != cert.Status {
					certs[i].Status = result.Status
					database.DB.Exec(
						"UPDATE certificates SET status = $1, updated_at = $2 WHERE id = $3",
						result.Status, time.Now(), cert.ID,
					)
					if result.ExpiresAt != nil {
						certs[i].ExpiresAt = result.ExpiresAt
						database.DB.Exec(
							"UPDATE certificates SET expires_at = $1 WHERE id = $2",
							result.ExpiresAt, cert.ID,
						)
					}
				}
				break
			}
		}
	}

	detail := model.SiteDetail{
		Site:         site,
		Deployments:  deployments,
		DNSRecords:   dnsRecords,
		Certificates: certs,
	}

	c.JSON(http.StatusOK, gin.H{"site": detail})
}

// UpdateSite PUT /api/sites/:id — 更新站点设置
func UpdateSite(c *gin.Context) {
	siteID := c.Param("id")

	var req struct {
		Origin         string `json:"origin"`
		OriginProtocol string `json:"origin_protocol"`
		HTTPPort       int    `json:"http_port"`
		HTTPSPort      int    `json:"https_port"`
		OriginHost     string `json:"origin_host"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "参数错误"})
		return
	}
	if req.Origin == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "源站地址不能为空"})
		return
	}
	if req.OriginProtocol == "" {
		req.OriginProtocol = "follow"
	}
	if req.HTTPPort <= 0 {
		req.HTTPPort = 80
	}
	if req.HTTPSPort <= 0 {
		req.HTTPSPort = 443
	}

	result, err := database.DB.Exec(
		`UPDATE sites SET origin = $1, origin_protocol = $2, http_port = $3, https_port = $4, origin_host = $5, updated_at = $6 WHERE id = $7`,
		req.Origin, req.OriginProtocol, req.HTTPPort, req.HTTPSPort, req.OriginHost, time.Now(), siteID,
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "更新失败"})
		return
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "站点不存在"})
		return
	}

	// 异步同步回源配置到各 CDN 提供商
	go syncOriginToProviders(siteID, model.OriginConfig{
		Origin:         req.Origin,
		OriginProtocol: req.OriginProtocol,
		HTTPPort:       req.HTTPPort,
		HTTPSPort:      req.HTTPSPort,
		OriginHost:     req.OriginHost,
	})

	c.JSON(http.StatusOK, gin.H{"message": "站点设置已更新"})
}

// syncOriginToProviders 将回源配置同步到所有 CDN 提供商
func syncOriginToProviders(siteID string, originCfg model.OriginConfig) {
	// 查询站点域名
	var domain string
	if err := database.DB.QueryRow("SELECT domain FROM sites WHERE id = $1", siteID).Scan(&domain); err != nil {
		log.Printf("[SyncOrigin] 查询站点域名失败: %v", err)
		return
	}

	// 查询所有 active 的部署记录
	rows, err := database.DB.Query(
		"SELECT id, provider, config_id, provider_site_id, deploy_params FROM deployments WHERE site_id = $1 AND status = 'active'", siteID,
	)
	if err != nil {
		log.Printf("[SyncOrigin] 查询部署记录失败: %v", err)
		return
	}
	defer rows.Close()

	ctx := context.Background()
	for rows.Next() {
		var depID, configID int64
		var providerName, providerSiteID, deployParamsJSON string
		if err := rows.Scan(&depID, &providerName, &configID, &providerSiteID, &deployParamsJSON); err != nil {
			continue
		}

		p, err := provider.Get(providerName)
		if err != nil {
			log.Printf("[SyncOrigin] 获取提供商 %s 失败: %v", providerName, err)
			continue
		}
		cfg := getProviderCfgByID(configID)
		if cfg == nil {
			continue
		}
		mergeDeployParamsToMap(cfg, deployParamsJSON)

		log.Printf("[SyncOrigin] 正在同步 %s 到 %s 的回源配置...", domain, providerName)
		if err := p.UpdateOriginConfig(ctx, cfg, domain, providerSiteID, originCfg); err != nil {
			log.Printf("[SyncOrigin] 同步 %s 回源配置失败: %v", providerName, err)
		} else {
			log.Printf("[SyncOrigin] %s 回源配置同步成功", providerName)
			// 同步后查询域名实际状态，更新本地 deployment 记录
			statusResult, err := p.GetDomainStatus(ctx, cfg, domain, providerSiteID)
			if err == nil && statusResult.Status != "" {
				database.DB.Exec(
					"UPDATE deployments SET status = $1, updated_at = $2 WHERE id = $3",
					statusResult.Status, time.Now(), depID,
				)
				log.Printf("[SyncOrigin] %s 部署状态更新为: %s", providerName, statusResult.Status)
			}
		}
	}
}

// 删除站点（同步清理各CDN平台资源）
func DeleteSite(c *gin.Context) {
	siteID := c.Param("id")

	// 查询站点域名
	var domain string
	err := database.DB.QueryRow("SELECT domain FROM sites WHERE id = $1", siteID).Scan(&domain)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "站点不存在"})
		return
	}

	// 查询所有部署记录，调用各CDN provider 清理远端资源
	var allCnames []string
	rows, err := database.DB.Query(
		"SELECT id, provider, config_id, provider_site_id, cdn_cname, deploy_params FROM deployments WHERE site_id = $1", siteID,
	)
	if err == nil {
		defer rows.Close()
		ctx := context.Background()
		for rows.Next() {
			var depID, configID int64
			var providerName, providerSiteID, cdnCname, deployParamsJSON string
			if err := rows.Scan(&depID, &providerName, &configID, &providerSiteID, &cdnCname, &deployParamsJSON); err != nil {
				continue
			}
			if cdnCname != "" {
				allCnames = append(allCnames, cdnCname)
			}
			// 获取 provider 配置
			p, err := provider.Get(providerName)
			if err != nil {
				log.Printf("[Delete] 获取提供商 %s 失败: %v", providerName, err)
				continue
			}
			cfg := getProviderCfgByID(configID)
			if cfg == nil {
				log.Printf("[Delete] 获取配置 %d 失败", configID)
				continue
			}
			// 合并部署参数
			mergeDeployParamsToMap(cfg, deployParamsJSON)
			// 调用 CDN 删除
			log.Printf("[Delete] 正在从 %s 删除 %s (SiteID=%s)...", providerName, domain, providerSiteID)
			if err := p.DeleteDomain(ctx, cfg, domain, providerSiteID); err != nil {
				log.Printf("[Delete] 从 %s 删除失败(非致命): %v", providerName, err)
			}
		}
	}

	// 清理 DNS 解析记录（非致命，异步执行）
	go cleanupDnsRecordsForCnames(domain, allCnames)

	// 删除数据库记录（CASCADE 会删除 deployments/dns_records/certificates）
	result, err := database.DB.Exec("DELETE FROM sites WHERE id = $1", siteID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "删除站点失败"})
		return
	}

	affected, _ := result.RowsAffected()
	if affected == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "站点不存在"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "站点已删除"})
}

// ToggleAutoSync PUT /api/sites/:id/auto-sync — 切换自动同步配置开关
func ToggleAutoSync(c *gin.Context) {
	siteID := c.Param("id")
	var req struct {
		Enabled int `json:"enabled"` // 0 或 1
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "参数错误"})
		return
	}
	if req.Enabled != 0 && req.Enabled != 1 {
		req.Enabled = 0
	}
	result, err := database.DB.Exec(
		"UPDATE sites SET config_auto_sync = $1, updated_at = $2 WHERE id = $3",
		req.Enabled, time.Now(), siteID,
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "更新失败"})
		return
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "站点不存在"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "已更新", "config_auto_sync": req.Enabled})
}

// getProviderCfgByID 按ID获取配置map
func getProviderCfgByID(id int64) map[string]string {
	var configJSON string
	err := database.DB.QueryRow("SELECT config FROM provider_configs WHERE id = $1", id).Scan(&configJSON)
	if err != nil {
		return nil
	}
	cfg := make(map[string]string)
	if err := json.Unmarshal([]byte(configJSON), &cfg); err != nil {
		return nil
	}
	return cfg
}
