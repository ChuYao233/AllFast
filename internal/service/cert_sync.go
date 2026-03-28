package service

import (
	"allfast/internal/database"
	"allfast/internal/provider"
	"context"
	"encoding/json"
	"log"
	"time"
)

// CertSyncService 证书状态后台同步服务
type CertSyncService struct{}

var CertSync = &CertSyncService{}

const certSyncInterval = 5 * time.Minute

// StartBackgroundSync 启动后台定时同步证书状态
func (s *CertSyncService) StartBackgroundSync() {
	go func() {
		// 启动后等 15 秒让数据库和其他服务就绪
		time.Sleep(15 * time.Second)
		log.Println("[CertSync] 后台证书状态同步任务启动，间隔5分钟")
		s.SyncAll()

		ticker := time.NewTicker(certSyncInterval)
		defer ticker.Stop()
		for range ticker.C {
			s.SyncAll()
		}
	}()
}

// SyncAll 同步所有部署的证书状态
func (s *CertSyncService) SyncAll() {
	// 查询所有有部署记录的证书
	rows, err := database.DB.Query(`
		SELECT c.id, c.site_id, c.deployment_id, c.provider, c.status, c.domain, c.cert_id,
		       d.config_id, d.provider_site_id, d.deploy_params,
		       s.config_auto_sync
		FROM certificates c
		JOIN deployments d ON d.id = c.deployment_id
		JOIN sites s ON s.id = c.site_id
		WHERE d.status = 'active'
	`)
	if err != nil {
		log.Printf("[CertSync] 查询证书记录失败: %v", err)
		return
	}
	defer rows.Close()

	type certTask struct {
		certID         int64
		siteID         int64
		deploymentID   int64
		providerName   string
		certStatus     string
		domain         string
		certIDStr      string
		configID       int64
		providerSiteID string
		deployParams   string
		autoSync       int
	}

	var tasks []certTask
	for rows.Next() {
		var t certTask
		if err := rows.Scan(&t.certID, &t.siteID, &t.deploymentID, &t.providerName, &t.certStatus,
			&t.domain, &t.certIDStr, &t.configID, &t.providerSiteID, &t.deployParams, &t.autoSync); err != nil {
			continue
		}
		tasks = append(tasks, t)
	}

	if len(tasks) == 0 {
		return
	}

	ctx := context.Background()
	now := time.Now()

	for _, t := range tasks {
		p, err := provider.Get(t.providerName)
		if err != nil {
			continue
		}

		cfg := s.getProviderCfg(t.configID)
		if cfg == nil {
			continue
		}
		s.mergeDeployParams(cfg, t.deployParams)

		// 查询 CDN 侧实际证书状态
		result, err := p.GetCertificateStatus(ctx, cfg, t.domain, t.certIDStr)
		if err != nil {
			log.Printf("[CertSync] 查询 %s 证书状态失败: %v", t.domain, err)
			continue
		}

		// 更新本地状态
		if result.Status != t.certStatus {
			log.Printf("[CertSync] %s 证书状态变更: %s -> %s", t.domain, t.certStatus, result.Status)
			database.DB.Exec(
				"UPDATE certificates SET status = $1, updated_at = $2 WHERE id = $3",
				result.Status, now, t.certID,
			)
			if result.ExpiresAt != nil {
				database.DB.Exec(
					"UPDATE certificates SET expires_at = $1 WHERE id = $2",
					result.ExpiresAt, t.certID,
				)
			}
		}

		// 如果站点开启了自动同步，且 CDN 侧证书状态为 none/disabled，自动重新启用
		if t.autoSync == 1 && (result.Status == "none" || result.Status == "pending") {
			// 证书之前是 active 但现在被关了，自动重新启用
			if t.certStatus == "active" {
				log.Printf("[CertSync] %s 证书被提供商侧关闭，自动重新启用边缘证书", t.domain)
				if err := p.EnableEdgeCert(ctx, cfg, t.domain, t.providerSiteID); err != nil {
					log.Printf("[CertSync] 自动重新启用 %s 边缘证书失败: %v", t.domain, err)
				} else {
					database.DB.Exec(
						"UPDATE certificates SET status = 'deploying', updated_at = $1 WHERE id = $2",
						now, t.certID,
					)
				}
			}
		}
	}
}

// getProviderCfg 获取提供商配置
func (s *CertSyncService) getProviderCfg(configID int64) map[string]string {
	var configJSON string
	err := database.DB.QueryRow("SELECT config FROM provider_configs WHERE id = $1 AND enabled = 1", configID).Scan(&configJSON)
	if err != nil {
		return nil
	}
	cfg := make(map[string]string)
	if err := json.Unmarshal([]byte(configJSON), &cfg); err != nil {
		return nil
	}
	return cfg
}

// mergeDeployParams 合并部署参数
func (s *CertSyncService) mergeDeployParams(cfg map[string]string, deployParamsJSON string) {
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
