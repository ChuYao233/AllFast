package service

import (
	"allfast/internal/database"
	"allfast/internal/model"
	"allfast/internal/provider"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
)

// DeployService 部署编排服务
type DeployService struct{}

// NewDeployService 创建部署服务实例
func NewDeployService() *DeployService {
	return &DeployService{}
}

// DeploySite 一键部署站点到多个CDN账户
// deployParams: config_id(string) -> 部署参数(map)
func (s *DeployService) DeploySite(ctx context.Context, siteID int64, configIDs []int64, deployParams map[string]map[string]string) ([]model.Deployment, error) {
	// 1. 查询站点信息
	var site model.Site
	err := database.DB.QueryRow(
		"SELECT id, domain, origin, origin_protocol, http_port, https_port, origin_host, status FROM sites WHERE id = $1", siteID,
	).Scan(&site.ID, &site.Domain, &site.Origin, &site.OriginProtocol, &site.HTTPPort, &site.HTTPSPort, &site.OriginHost, &site.Status)
	if err != nil {
		return nil, fmt.Errorf("站点不存在: %w", err)
	}

	// 2. 更新站点状态为部署中
	database.DB.Exec("UPDATE sites SET status = 'deploying', updated_at = $1 WHERE id = $2", time.Now(), siteID)

	// 3. 并发部署到各CDN账户
	var wg sync.WaitGroup
	var mu sync.Mutex
	deployments := make([]model.Deployment, 0, len(configIDs))

	for _, cfgID := range configIDs {
		wg.Add(1)
		go func(configID int64) {
			defer wg.Done()

			// 获取该 configID 对应的部署参数
			params := deployParams[fmt.Sprintf("%d", configID)]

			deployment := s.deployToConfig(ctx, &site, configID, params)

			mu.Lock()
			deployments = append(deployments, deployment)
			mu.Unlock()
		}(cfgID)
	}

	wg.Wait()

	// 4. 根据各部署结果更新站点总状态
	s.updateSiteStatus(siteID)

	return deployments, nil
}

// deployToConfig 部署到单个CDN账户
func (s *DeployService) deployToConfig(ctx context.Context, site *model.Site, configID int64, deployParams map[string]string) model.Deployment {
	now := time.Now()
	// 内存日志缓冲区，部署结束后写入 deploy_log 字段
	var logBuf strings.Builder
	dlog := func(format string, args ...any) {
		line := fmt.Sprintf(format, args...)
		log.Print(line)
		logBuf.WriteString(line)
		logBuf.WriteByte('\n')
	}

	// 获取提供商配置（认证凭证）
	providerName, cfg, configName, err := s.getProviderConfigByID(configID)
	if err != nil {
		log.Printf("[Deploy] 获取配置失败 [ID=%d]: %v", configID, err)
		return model.Deployment{
			SiteID:       site.ID,
			ConfigID:     configID,
			Status:       "failed",
			ErrorMessage: err.Error(),
		}
	}

	// 合并部署参数到 cfg（deploy_params 覆盖同名 key）
	for k, v := range deployParams {
		cfg[k] = v
	}

	// 序列化 deployParams 为 JSON
	deployParamsJSON := "{}"
	if len(deployParams) > 0 {
		if b, err := json.Marshal(deployParams); err == nil {
			deployParamsJSON = string(b)
		}
	}

	// 查找已有部署记录（同一站点+同一配置只保留一条）
	var deploymentID int64
	err = database.DB.QueryRow(
		"SELECT id FROM deployments WHERE site_id = $1 AND config_id = $2", site.ID, configID,
	).Scan(&deploymentID)
	if err != nil {
		// 没有已有记录，新增
		err = database.DB.QueryRow(
			`INSERT INTO deployments (site_id, provider, config_id, config_name, status, deploy_params, created_at, updated_at)
			 VALUES ($1, $2, $3, $4, 'deploying', $5, $6, $7) RETURNING id`,
			site.ID, providerName, configID, configName, deployParamsJSON, now, now,
		).Scan(&deploymentID)
		if err != nil {
			log.Printf("[Deploy] 创建部署记录失败 [%s]: %v", configName, err)
			return model.Deployment{
				SiteID:       site.ID,
				Provider:     providerName,
				ConfigID:     configID,
				ConfigName:   configName,
				Status:       "failed",
				ErrorMessage: fmt.Sprintf("创建部署记录失败: %v", err),
			}
		}
	} else {
		// 已有记录，更新状态和部署参数，清空旧日志
		database.DB.Exec(
			"UPDATE deployments SET status = 'deploying', deploy_params = $1, error_message = '', deploy_log = '', updated_at = $2 WHERE id = $3",
			deployParamsJSON, now, deploymentID,
		)
	}

	// 获取CDN提供商实例
	p, err := provider.Get(providerName)
	if err != nil {
		errMsg := fmt.Sprintf("未知的CDN提供商: %s", providerName)
		dlog("[Deploy] %s", errMsg)
		return s.failDeploymentWithLog(deploymentID, site.ID, providerName, configID, configName, errMsg, logBuf.String())
	}

	// 构建回源配置
	originCfg := model.OriginConfig{
		Origin:         site.Origin,
		OriginProtocol: site.OriginProtocol,
		HTTPPort:       site.HTTPPort,
		HTTPSPort:      site.HTTPSPort,
		OriginHost:     site.OriginHost,
	}

	// 调用提供商API添加域名
	dlog("[Deploy] 正在将 %s 添加到 %s (%s)...", site.Domain, configName, providerName)
	addResult, err := p.AddDomain(ctx, cfg, site.Domain, originCfg)
	if err != nil {
		errMsg := fmt.Sprintf("添加域名失败: %v", err)
		dlog("[Deploy] 部署失败 [%s]: %s", configName, errMsg)
		return s.failDeploymentWithLog(deploymentID, site.ID, providerName, configID, configName, errMsg, logBuf.String())
	}

	dlog("[Deploy] %s 部署到 %s (%s) 完成, CNAME: %s", site.Domain, configName, providerName, addResult.CNAME)

	// 更新部署记录（含日志）
	database.DB.Exec(
		`UPDATE deployments SET status = 'active', provider_site_id = $1, cdn_cname = $2, deploy_log = $3, updated_at = $4
		 WHERE id = $5`,
		addResult.ProviderSiteID, addResult.CNAME, logBuf.String(), time.Now(), deploymentID,
	)

	// 生成DNS记录
	s.generateDNSRecords(site.ID, deploymentID, site.Domain, addResult)

	// 查询证书状态（不自动申请，只记录当前状态）
	s.checkCertificateStatus(ctx, site, deploymentID, providerName, p, cfg, addResult.ProviderSiteID)

	return model.Deployment{
		ID:             deploymentID,
		SiteID:         site.ID,
		Provider:       providerName,
		ConfigID:       configID,
		ConfigName:     configName,
		Status:         "active",
		ProviderSiteID: addResult.ProviderSiteID,
		CDNCname:       addResult.CNAME,
		DeployLog:      logBuf.String(),
		CreatedAt:      now,
		UpdatedAt:      time.Now(),
	}
}

// generateDNSRecords 根据部署结果生成DNS记录
func (s *DeployService) generateDNSRecords(siteID, deploymentID int64, domain string, result *model.AddDomainResult) {
	now := time.Now()

	// CNAME 流量记录
	if result.CNAME != "" {
		database.DB.Exec(
			`INSERT INTO dns_records (site_id, deployment_id, record_type, name, value, purpose, status, created_at)
			 VALUES ($1, $2, 'CNAME', $3, $4, 'traffic', 'pending', $5)`,
			siteID, deploymentID, domain, result.CNAME, now,
		)
	}

	// NS 记录（如 Cloudflare 需要）
	for _, ns := range result.NameServers {
		database.DB.Exec(
			`INSERT INTO dns_records (site_id, deployment_id, record_type, name, value, purpose, status, created_at)
			 VALUES ($1, $2, 'NS', $3, $4, 'validation', 'pending', $5)`,
			siteID, deploymentID, domain, ns, now,
		)
	}
}

// checkCertificateStatus 查询证书状态（不自动申请），记录到数据库
func (s *DeployService) checkCertificateStatus(ctx context.Context, site *model.Site, deploymentID int64, providerName string, p provider.CDNProvider, cfg map[string]string, providerSiteID string) {
	now := time.Now()

	// 查询CDN平台的证书状态
	certReqResult, err := p.SetupCertificate(ctx, cfg, site.Domain, providerSiteID)
	if err != nil {
		log.Printf("[Cert] 查询 %s 证书状态失败: %v", site.Domain, err)
		return
	}

	// 查找已有证书记录（同一 deployment 只保留一条）
	var certID int64
	err = database.DB.QueryRow(
		"SELECT id FROM certificates WHERE deployment_id = $1", deploymentID,
	).Scan(&certID)
	if err != nil {
		// 新增
		database.DB.Exec(
			`INSERT INTO certificates (site_id, deployment_id, provider, status, domain, cert_id, created_at, updated_at)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
			site.ID, deploymentID, providerName, certReqResult.Status, site.Domain, certReqResult.CertID, now, now,
		)
	} else {
		// 更新
		database.DB.Exec(
			"UPDATE certificates SET status = $1, cert_id = $2, updated_at = $3 WHERE id = $4",
			certReqResult.Status, certReqResult.CertID, now, certID,
		)
	}

	// 自动写入证书验证所需 DNS 记录（如 Cloudflare TXT 验证）
	for _, vr := range certReqResult.DNSRecords {
		if vr.Type == "" || vr.Name == "" || vr.Value == "" {
			continue
		}
		database.DB.Exec(
			`INSERT INTO dns_records (site_id, deployment_id, record_type, name, value, purpose, status, created_at)
			 VALUES ($1, $2, $3, $4, $5, 'validation', 'pending', $6)`,
			site.ID, deploymentID, vr.Type, vr.Name, vr.Value, now,
		)
	}

	log.Printf("[Cert] %s 在 %s 证书状态: %s", site.Domain, providerName, certReqResult.Status)
}

// failDeployment 标记部署失败
func (s *DeployService) failDeployment(deploymentID, siteID int64, providerName string, configID int64, configName, errMsg string) model.Deployment {
	return s.failDeploymentWithLog(deploymentID, siteID, providerName, configID, configName, errMsg, "")
}

// failDeploymentWithLog 标记部署失败，同时保存日志
func (s *DeployService) failDeploymentWithLog(deploymentID, siteID int64, providerName string, configID int64, configName, errMsg, deployLog string) model.Deployment {
	database.DB.Exec(
		`UPDATE deployments SET status = 'failed', error_message = $1, deploy_log = $2, updated_at = $3 WHERE id = $4`,
		errMsg, deployLog, time.Now(), deploymentID,
	)

	return model.Deployment{
		ID:           deploymentID,
		SiteID:       siteID,
		Provider:     providerName,
		ConfigID:     configID,
		ConfigName:   configName,
		Status:       "failed",
		ErrorMessage: errMsg,
		DeployLog:    deployLog,
	}
}

// updateSiteStatus 根据各部署记录更新站点总状态
func (s *DeployService) updateSiteStatus(siteID int64) {
	var total, active, failed int
	database.DB.QueryRow(
		"SELECT COUNT(*), COALESCE(SUM(CASE WHEN status='active' THEN 1 ELSE 0 END),0), COALESCE(SUM(CASE WHEN status='failed' THEN 1 ELSE 0 END),0) FROM deployments WHERE site_id = $1",
		siteID,
	).Scan(&total, &active, &failed)

	status := "deploying"
	if active == total && total > 0 {
		status = "active"
	} else if failed == total && total > 0 {
		status = "failed"
	} else if active > 0 {
		status = "partial"
	}

	database.DB.Exec("UPDATE sites SET status = $1, updated_at = $2 WHERE id = $3", status, time.Now(), siteID)
}

// getProviderConfigByID 按ID获取提供商配置
func (s *DeployService) getProviderConfigByID(id int64) (providerName string, cfg map[string]string, configName string, err error) {
	var configJSON string
	var enabled int
	err = database.DB.QueryRow(
		"SELECT name, provider, config, enabled FROM provider_configs WHERE id = $1", id,
	).Scan(&configName, &providerName, &configJSON, &enabled)
	if err != nil {
		return "", nil, "", fmt.Errorf("提供商配置不存在 (ID=%d)", id)
	}
	if enabled == 0 {
		return "", nil, "", fmt.Errorf("提供商配置已禁用: %s", configName)
	}

	cfg = make(map[string]string)
	if err := json.Unmarshal([]byte(configJSON), &cfg); err != nil {
		return "", nil, "", fmt.Errorf("配置格式错误")
	}

	return providerName, cfg, configName, nil
}

// mergeDeployParams 将部署记录中的 deploy_params 合并到 cfg
func (s *DeployService) mergeDeployParams(cfg map[string]string, deployParamsJSON string) {
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

// RefreshDeploymentStatus 刷新部署状态（从CDN提供商实时查询）
func (s *DeployService) RefreshDeploymentStatus(ctx context.Context, deployment *model.Deployment) (*model.DomainStatusResult, error) {
	p, err := provider.Get(deployment.Provider)
	if err != nil {
		return nil, err
	}

	_, cfg, _, err := s.getProviderConfigByID(deployment.ConfigID)
	if err != nil {
		return nil, err
	}
	s.mergeDeployParams(cfg, deployment.DeployParams)

	result, err := p.GetDomainStatus(ctx, cfg, "", deployment.ProviderSiteID)
	if err != nil {
		return nil, err
	}

	// 更新本地状态
	if result.Status != deployment.Status {
		database.DB.Exec(
			"UPDATE deployments SET status = $1, updated_at = $2 WHERE id = $3",
			result.Status, time.Now(), deployment.ID,
		)
	}

	return result, nil
}

// DeployCertToDeployment 将证书部署到指定的 CDN 部署
// mode: "edge"(使用服务商边缘证书) / "ssl"(证书管理中的证书) / "self_sign"(自签证书)
// certID: 当 mode 为 ssl 或 self_sign 时，对应的证书 ID
func (s *DeployService) DeployCertToDeployment(ctx context.Context, deploymentID int64, mode string, certID int64) error {
	// 1. 查询部署记录
	var d model.Deployment
	err := database.DB.QueryRow(
		`SELECT id, site_id, provider, config_id, status, provider_site_id, deploy_params
		 FROM deployments WHERE id = $1`, deploymentID,
	).Scan(&d.ID, &d.SiteID, &d.Provider, &d.ConfigID, &d.Status, &d.ProviderSiteID, &d.DeployParams)
	if err != nil {
		return fmt.Errorf("部署记录不存在")
	}

	// 2. 查询站点域名
	var domain string
	err = database.DB.QueryRow("SELECT domain FROM sites WHERE id = $1", d.SiteID).Scan(&domain)
	if err != nil {
		return fmt.Errorf("站点不存在")
	}

	// 3. 获取提供商配置
	providerName, cfg, _, err := s.getProviderConfigByID(d.ConfigID)
	if err != nil {
		return err
	}
	s.mergeDeployParams(cfg, d.DeployParams)

	p, err := provider.Get(providerName)
	if err != nil {
		return err
	}

	// 4. 根据模式处理
	now := time.Now()
	switch mode {
	case "edge":
		// 使用服务商边缘证书：调用 EnableEdgeCert 实际启用免费证书
		if err := p.EnableEdgeCert(ctx, cfg, domain, d.ProviderSiteID); err != nil {
			return fmt.Errorf("启用边缘证书失败: %w", err)
		}
		// 启用后查询实际证书状态
		certResult, queryErr := p.SetupCertificate(ctx, cfg, domain, d.ProviderSiteID)
		certStatus := "deploying" // 默认申请中
		if queryErr == nil && certResult.Status != "" {
			certStatus = certResult.Status
		}
		s.upsertCertRecord(d.SiteID, deploymentID, providerName, domain, certStatus, now)
		return nil

	case "ssl":
		// 从证书管理加载证书 PEM
		var certPEM, keyPEM string
		err := database.DB.QueryRow(
			"SELECT certificate, private_key FROM ssl_certificates WHERE id = $1", certID,
		).Scan(&certPEM, &keyPEM)
		if err != nil {
			return fmt.Errorf("证书不存在 (ID=%d)", certID)
		}
		if certPEM == "" || keyPEM == "" {
			return fmt.Errorf("证书数据不完整，可能正在申请中")
		}
		// 部署到 CDN
		if err := p.DeployCertificate(ctx, cfg, domain, d.ProviderSiteID, certPEM, keyPEM); err != nil {
			return err
		}
		s.upsertCertRecord(d.SiteID, deploymentID, providerName, domain, "active", now)
		return nil

	case "self_sign":
		// 从自签证书加载
		var certPEM, keyPEM string
		err := database.DB.QueryRow(
			"SELECT certificate, private_key FROM self_signed_certs WHERE id = $1", certID,
		).Scan(&certPEM, &keyPEM)
		if err != nil {
			return fmt.Errorf("自签证书不存在 (ID=%d)", certID)
		}
		if certPEM == "" || keyPEM == "" {
			return fmt.Errorf("自签证书数据不完整")
		}
		// 部署到 CDN
		if err := p.DeployCertificate(ctx, cfg, domain, d.ProviderSiteID, certPEM, keyPEM); err != nil {
			return err
		}
		s.upsertCertRecord(d.SiteID, deploymentID, providerName, domain, "active", now)
		return nil

	default:
		return fmt.Errorf("不支持的证书模式: %s", mode)
	}
}

// upsertCertRecord 插入或更新本地证书记录
func (s *DeployService) upsertCertRecord(siteID, deploymentID int64, providerName, domain, status string, now time.Time) {
	var existID int64
	err := database.DB.QueryRow(
		"SELECT id FROM certificates WHERE deployment_id = $1", deploymentID,
	).Scan(&existID)
	if err != nil {
		database.DB.Exec(
			`INSERT INTO certificates (site_id, deployment_id, provider, status, domain, created_at, updated_at)
			 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			siteID, deploymentID, providerName, status, domain, now, now,
		)
	} else {
		database.DB.Exec(
			"UPDATE certificates SET status = $1, updated_at = $2 WHERE id = $3",
			status, now, existID,
		)
	}
}

// RefreshCertificateStatus 刷新证书状态
func (s *DeployService) RefreshCertificateStatus(ctx context.Context, cert *model.Certificate, configID int64, deployParamsJSON string) (*model.CertStatusResult, error) {
	p, err := provider.Get(cert.Provider)
	if err != nil {
		return nil, err
	}

	_, cfg, _, err := s.getProviderConfigByID(configID)
	if err != nil {
		return nil, err
	}
	s.mergeDeployParams(cfg, deployParamsJSON)

	result, err := p.GetCertificateStatus(ctx, cfg, cert.Domain, cert.CertID)
	if err != nil {
		return nil, err
	}

	// 更新本地状态
	if result.Status != cert.Status {
		database.DB.Exec(
			"UPDATE certificates SET status = $1, updated_at = $2 WHERE id = $3",
			result.Status, time.Now(), cert.ID,
		)
		if result.ExpiresAt != nil {
			database.DB.Exec(
				"UPDATE certificates SET expires_at = $1 WHERE id = $2",
				result.ExpiresAt, cert.ID,
			)
		}
	}

	return result, nil
}
