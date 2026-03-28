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
	"time"

	"github.com/gin-gonic/gin"
)

var deploySvc = service.NewDeployService()

// DeploySite 一键部署站点到多个CDN账户
func DeploySite(c *gin.Context) {
	siteID := c.Param("id")

	var req model.DeployRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请选择至少一个CDN账户"})
		return
	}

	if len(req.ConfigIDs) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请选择至少一个CDN账户"})
		return
	}

	// 解析站点ID
	var id int64
	if _, err := parseID(siteID, &id); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的站点ID"})
		return
	}

	// 约束：同一站点同一提供商只能接入一个账户
	if err := validateProviderUniquePerSite(id, req.ConfigIDs); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// 执行部署
	deployments, err := deploySvc.DeploySite(c.Request.Context(), id, req.ConfigIDs, req.DeployParams)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"message":     "部署完成",
		"deployments": deployments,
	})
}

// RemoveSiteDeployment 移除站点某个接入（删除提供商侧资源 + 本地记录）
// DELETE /api/sites/:id/deployments/:dep_id
func RemoveSiteDeployment(c *gin.Context) {
	siteID := c.Param("id")
	depID := c.Param("dep_id")

	var dep model.Deployment
	err := database.DB.QueryRow(
		`SELECT id, site_id, provider, config_id, config_name, status, provider_site_id, cdn_cname, deploy_params, error_message, deploy_log, created_at, updated_at
		 FROM deployments WHERE id = $1 AND site_id = $2`, depID, siteID,
	).Scan(&dep.ID, &dep.SiteID, &dep.Provider, &dep.ConfigID, &dep.ConfigName, &dep.Status, &dep.ProviderSiteID, &dep.CDNCname, &dep.DeployParams, &dep.ErrorMessage, &dep.DeployLog, &dep.CreatedAt, &dep.UpdatedAt)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "接入记录不存在"})
		return
	}

	var domain string
	if err := database.DB.QueryRow("SELECT domain FROM sites WHERE id = $1", siteID).Scan(&domain); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "站点不存在"})
		return
	}

	// 删除提供商侧资源（非致命）
	if dep.ProviderSiteID != "" {
		if p, err := provider.Get(dep.Provider); err == nil {
			cfg := getProviderCfgByID(dep.ConfigID)
			if cfg != nil {
				mergeDeployParamsToMap(cfg, dep.DeployParams)
				if err := p.DeleteDomain(context.Background(), cfg, domain, dep.ProviderSiteID); err != nil {
					log.Printf("[RemoveAccess] 删除远端资源失败 [%s/%d]: %v", dep.Provider, dep.ID, err)
				}
			}
		}
	}

	// 删除本地关联记录
	database.DB.Exec("DELETE FROM dns_records WHERE deployment_id = $1", dep.ID)
	database.DB.Exec("DELETE FROM certificates WHERE deployment_id = $1", dep.ID)
	if _, err := database.DB.Exec("DELETE FROM deployments WHERE id = $1", dep.ID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "删除接入记录失败"})
		return
	}

	// 回写站点状态
	refreshSiteStatus(dep.SiteID)

	// 异步清理该接入对应的 DNS 解析记录
	if dep.CDNCname != "" {
		go cleanupDnsRecordsForCnames(domain, []string{dep.CDNCname})
	}

	c.JSON(http.StatusOK, gin.H{"message": "接入已移除"})
}

func validateProviderUniquePerSite(siteID int64, configIDs []int64) error {
	// 本次批量请求内不允许同一 config_id 重复
	seen := map[int64]bool{}
	for _, cfgID := range configIDs {
		if seen[cfgID] {
			return fmt.Errorf("同一次部署请求中 config_id=%d 重复", cfgID)
		}
		seen[cfgID] = true
		// 确认配置存在且启用
		var exists int
		if err := database.DB.QueryRow("SELECT 1 FROM provider_configs WHERE id = $1 AND enabled = 1", cfgID).Scan(&exists); err != nil {
			return fmt.Errorf("提供商配置不存在或已禁用 (ID=%d)", cfgID)
		}
	}

	// 同一站点同一 config_id 只能有一条部署记录
	rows, err := database.DB.Query("SELECT config_id FROM deployments WHERE site_id = $1", siteID)
	if err != nil {
		return fmt.Errorf("校验站点接入失败")
	}
	defer rows.Close()

	for rows.Next() {
		var existCfgID int64
		if err := rows.Scan(&existCfgID); err != nil {
			continue
		}
		if seen[existCfgID] {
			return fmt.Errorf("该账户已接入此站点 (config_id=%d)，如需重新部署请使用「重新部署」按钮", existCfgID)
		}
	}
	return nil
}

func refreshSiteStatus(siteID int64) {
	var total, active, failed int
	database.DB.QueryRow(
		"SELECT COUNT(*), COALESCE(SUM(CASE WHEN status='active' THEN 1 ELSE 0 END),0), COALESCE(SUM(CASE WHEN status='failed' THEN 1 ELSE 0 END),0) FROM deployments WHERE site_id = $1",
		siteID,
	).Scan(&total, &active, &failed)

	status := "pending"
	if total > 0 {
		status = "deploying"
		if active == total {
			status = "active"
		} else if failed == total {
			status = "failed"
		} else if active > 0 {
			status = "partial"
		}
	}

	database.DB.Exec("UPDATE sites SET status = $1, updated_at = $2 WHERE id = $3", status, time.Now(), siteID)
}

// ListDeployments 获取站点的部署列表
func ListDeployments(c *gin.Context) {
	siteID := c.Param("id")

	rows, err := database.DB.Query(
		`SELECT id, site_id, provider, config_id, config_name, status, provider_site_id, cdn_cname, deploy_params, error_message, deploy_log, created_at, updated_at
		 FROM deployments WHERE site_id = $1 ORDER BY id DESC`, siteID,
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "查询部署列表失败"})
		return
	}
	defer rows.Close()

	deployments := []model.Deployment{}
	for rows.Next() {
		var d model.Deployment
		if err := rows.Scan(&d.ID, &d.SiteID, &d.Provider, &d.ConfigID, &d.ConfigName, &d.Status, &d.ProviderSiteID, &d.CDNCname, &d.DeployParams, &d.ErrorMessage, &d.DeployLog, &d.CreatedAt, &d.UpdatedAt); err != nil {
			continue
		}
		deployments = append(deployments, d)
	}

	c.JSON(http.StatusOK, gin.H{"deployments": deployments})
}

// GetDeployment 获取单个部署详情（含实时状态刷新）
func GetDeployment(c *gin.Context) {
	deployID := c.Param("id")

	var d model.Deployment
	err := database.DB.QueryRow(
		`SELECT id, site_id, provider, config_id, config_name, status, provider_site_id, cdn_cname, deploy_params, error_message, deploy_log, created_at, updated_at
		 FROM deployments WHERE id = $1`, deployID,
	).Scan(&d.ID, &d.SiteID, &d.Provider, &d.ConfigID, &d.ConfigName, &d.Status, &d.ProviderSiteID, &d.CDNCname, &d.DeployParams, &d.ErrorMessage, &d.DeployLog, &d.CreatedAt, &d.UpdatedAt)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "部署记录不存在"})
		return
	}

	// 如果状态不是终态，实时刷新
	if d.Status != "active" && d.Status != "failed" && d.ProviderSiteID != "" {
		statusResult, err := deploySvc.RefreshDeploymentStatus(c.Request.Context(), &d)
		if err == nil {
			d.Status = statusResult.Status
		}
	}

	c.JSON(http.StatusOK, gin.H{"deployment": d})
}

// ListDNSRecords 获取站点的DNS记录
func ListDNSRecords(c *gin.Context) {
	siteID := c.Param("id")

	rows, err := database.DB.Query(
		`SELECT id, site_id, deployment_id, record_type, name, value, purpose, status, created_at
		 FROM dns_records WHERE site_id = $1 ORDER BY id`, siteID,
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "查询DNS记录失败"})
		return
	}
	defer rows.Close()

	records := []model.DNSRecord{}
	for rows.Next() {
		var r model.DNSRecord
		if err := rows.Scan(&r.ID, &r.SiteID, &r.DeploymentID, &r.RecordType, &r.Name, &r.Value, &r.Purpose, &r.Status, &r.CreatedAt); err != nil {
			continue
		}
		records = append(records, r)
	}

	c.JSON(http.StatusOK, gin.H{"dns_records": records})
}

// ListCertificates 获取站点的证书列表
func ListCertificates(c *gin.Context) {
	siteID := c.Param("id")

	rows, err := database.DB.Query(
		`SELECT id, site_id, deployment_id, provider, status, domain, cert_id, expires_at, error_message, created_at, updated_at
		 FROM certificates WHERE site_id = $1 ORDER BY id`, siteID,
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "查询证书列表失败"})
		return
	}
	defer rows.Close()

	certs := []model.Certificate{}
	for rows.Next() {
		var cert model.Certificate
		if err := rows.Scan(&cert.ID, &cert.SiteID, &cert.DeploymentID, &cert.Provider, &cert.Status, &cert.Domain, &cert.CertID, &cert.ExpiresAt, &cert.ErrorMessage, &cert.CreatedAt, &cert.UpdatedAt); err != nil {
			continue
		}
		certs = append(certs, cert)
	}

	c.JSON(http.StatusOK, gin.H{"certificates": certs})
}

// DeployHTTPS POST /api/deployments/:id/https — 配置 HTTPS 证书
func DeployHTTPS(c *gin.Context) {
	deployID := c.Param("id")
	var id int64
	if _, err := parseID(deployID, &id); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的部署ID"})
		return
	}

	var req struct {
		Mode   string `json:"mode" binding:"required"` // edge / ssl / self_sign
		CertID int64  `json:"cert_id"`                 // mode 为 ssl 或 self_sign 时必填
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请指定证书模式 (mode)"})
		return
	}

	if (req.Mode == "ssl" || req.Mode == "self_sign") && req.CertID == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请选择要部署的证书"})
		return
	}

	if err := deploySvc.DeployCertToDeployment(c.Request.Context(), id, req.Mode, req.CertID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "HTTPS 证书配置成功"})
}

// ListProviders 获取所有CDN提供商信息
func ListProviders(c *gin.Context) {
	providers := provider.ListAll()
	infos := make([]model.ProviderInfo, len(providers))
	for i, p := range providers {
		info := p.Info()
		// 检测该提供商是否同时支持 DNS 管理
		if _, err := provider.GetDNS(info.Name); err == nil {
			info.SupportsDNS = true
		}
		infos[i] = info
	}
	c.JSON(http.StatusOK, gin.H{"providers": infos})
}

// RedeployDeployment 重新部署某个失败/异常的接入
// POST /api/sites/:id/deployments/:dep_id/redeploy
func RedeployDeployment(c *gin.Context) {
	siteID := c.Param("id")
	depID := c.Param("dep_id")

	var siteIDInt, depIDInt int64
	if _, err := parseID(siteID, &siteIDInt); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的站点ID"})
		return
	}
	if _, err := parseID(depID, &depIDInt); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的部署ID"})
		return
	}

	// 读取已有部署记录获取 config_id 和 deploy_params
	var dep model.Deployment
	err := database.DB.QueryRow(
		`SELECT id, site_id, provider, config_id, config_name, status, provider_site_id, cdn_cname, deploy_params, error_message, deploy_log, created_at, updated_at
		 FROM deployments WHERE id = $1 AND site_id = $2`, depIDInt, siteIDInt,
	).Scan(&dep.ID, &dep.SiteID, &dep.Provider, &dep.ConfigID, &dep.ConfigName, &dep.Status,
		&dep.ProviderSiteID, &dep.CDNCname, &dep.DeployParams, &dep.ErrorMessage, &dep.DeployLog,
		&dep.CreatedAt, &dep.UpdatedAt)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "部署记录不存在"})
		return
	}

	// 解析 deploy_params JSON → map
	params := map[string]string{}
	if dep.DeployParams != "" && dep.DeployParams != "{}" {
		if err := json.Unmarshal([]byte(dep.DeployParams), &params); err != nil {
			log.Printf("[Redeploy] 解析 deploy_params 失败: %v", err)
		}
	}

	// 立即将状态置为 deploying
	database.DB.Exec(
		"UPDATE deployments SET status = 'deploying', error_message = '', deploy_log = '', updated_at = $1 WHERE id = $2",
		time.Now(), dep.ID,
	)
	database.DB.Exec(
		"UPDATE sites SET status = 'deploying', updated_at = $1 WHERE id = $2",
		time.Now(), siteIDInt,
	)

	// 后台异步重新部署
	deployParamsMap := map[string]map[string]string{
		fmt.Sprintf("%d", dep.ConfigID): params,
	}
	go deploySvc.DeploySite(context.Background(), siteIDInt, []int64{dep.ConfigID}, deployParamsMap)

	c.JSON(http.StatusOK, gin.H{"message": "已开始重新部署"})
}

// parseID 解析字符串ID为int64
func parseID(s string, id *int64) (bool, error) {
	var n int64
	for _, ch := range s {
		if ch < '0' || ch > '9' {
			return false, fmt.Errorf("无效ID")
		}
		n = n*10 + int64(ch-'0')
	}
	*id = n
	return true, nil
}
