package edgeone

import (
	"allfast/internal/model"
	"allfast/internal/provider"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

func init() {
	provider.Register(&EdgeOneProvider{})
}

const (
	// EdgeOne API端点
	eoEndpointDomestic = "https://teo.tencentcloudapi.com"
	eoHostDomestic     = "teo.tencentcloudapi.com"
	eoEndpointGlobal   = "https://teo.intl.tencentcloudapi.com"
	eoHostGlobal       = "teo.intl.tencentcloudapi.com"
	eoService          = "teo"
	eoVersion          = "2022-09-01"

	// DNSPod API端点
	dnspodEndpoint = "https://dnspod.tencentcloudapi.com"
	dnspodHost     = "dnspod.tencentcloudapi.com"
	dnspodService  = "dnspod"
	dnspodVersion  = "2021-03-23"

	// zone ID 前缀
	prefixEO     = "eo:"
	prefixDNSPod = "dnspod:"
)

// EdgeOneProvider 腾讯云EdgeOne CDN提供商
type EdgeOneProvider struct{}

func (e *EdgeOneProvider) Name() string { return "edgeone" }

func (e *EdgeOneProvider) Info() model.ProviderInfo {
	return model.ProviderInfo{
		Name:              "edgeone",
		DisplayName:       "腾讯云",
		Description:       "腾讯云边缘安全加速平台，DNS 管理为 EdgeOne NS 接入站点",
		SupportsOptDomain: true,
		ConfigFields: []model.ConfigField{
			{Key: "secret_id", Label: "SecretId", Type: "text", Required: true, Secret: false, Placeholder: "腾讯云 SecretId"},
			{Key: "secret_key", Label: "SecretKey", Type: "secret", Required: true, Secret: true, Placeholder: "腾讯云 SecretKey"},
			{Key: "api_region", Label: "API版本", Type: "select", Required: true, Secret: false, Options: []model.SelectOption{
				{Value: "domestic", Label: "国内版"},
				{Value: "global", Label: "海外版（国际站）"},
			}},
		},
		DeployFields: []model.ConfigField{
			{Key: "zone_id", Label: "站点(Zone)", Type: "select", Required: true, Secret: false, Placeholder: "选择已有站点", Fetchable: true},
		},
	}
}

func (e *EdgeOneProvider) ValidateConfig(cfg map[string]string) error {
	if cfg["secret_id"] == "" {
		return fmt.Errorf("缺少 secret_id 配置")
	}
	if cfg["secret_key"] == "" {
		return fmt.Errorf("缺少 secret_key 配置")
	}
	return nil
}

// FetchFieldOptions 动态获取站点列表
func (e *EdgeOneProvider) FetchFieldOptions(ctx context.Context, cfg map[string]string, fieldKey string) ([]model.SelectOption, error) {
	if fieldKey != "zone_id" {
		return nil, nil
	}

	params := map[string]interface{}{
		"Offset": 0,
		"Limit":  50,
	}

	body, err := e.doRequest(ctx, cfg, "DescribeZones", params)
	if err != nil {
		return nil, fmt.Errorf("查询站点列表失败: %w", err)
	}

	var resp struct {
		Response struct {
			Zones []struct {
				ZoneId   string `json:"ZoneId"`
				ZoneName string `json:"ZoneName"`
				Status   string `json:"Status"`
				Type     string `json:"Type"`
				Area     string `json:"Area"`
			} `json:"Zones"`
			Error *eoAPIError `json:"Error"`
		} `json:"Response"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("解析响应失败: %w", err)
	}
	if resp.Response.Error != nil {
		return nil, resp.Response.Error
	}

	var options []model.SelectOption
	for _, z := range resp.Response.Zones {
		areaLabel := z.Area
		switch z.Area {
		case "mainland":
			areaLabel = "国内"
		case "overseas":
			areaLabel = "海外"
		case "global":
			areaLabel = "全球"
		}
		typeLabel := z.Type
		switch z.Type {
		case "partial":
			typeLabel = "CNAME接入"
		case "full":
			typeLabel = "NS接入"
		}
		label := fmt.Sprintf("%s (%s/%s) [%s]", z.ZoneName, areaLabel, typeLabel, z.Status)
		options = append(options, model.SelectOption{
			Value: z.ZoneId,
			Label: label,
		})
	}

	return options, nil
}

// AddDomain 添加加速域名到已有站点（先查询，已存在则更新回源配置）
func (e *EdgeOneProvider) AddDomain(ctx context.Context, cfg map[string]string, domain string, originCfg model.OriginConfig) (*model.AddDomainResult, error) {
	if err := e.ValidateConfig(cfg); err != nil {
		return nil, err
	}

	zoneID := cfg["zone_id"]

	// 1. 先查询域名是否已存在
	existing, descErr := e.describeAccelerationDomain(ctx, cfg, zoneID, domain)
	if descErr == nil && existing != nil {
		// 已存在，更新回源配置
		log.Printf("[EdgeOne] 域名 %s 已存在（状态: %s），更新回源配置...", domain, existing.DomainStatus)
		if err := e.UpdateOriginConfig(ctx, cfg, domain, zoneID, originCfg); err != nil {
			log.Printf("[EdgeOne] 更新回源配置失败（继续获取 CNAME）: %v", err)
		}
	} else {
		// 查询失败或不存在，先尝试创建
		createErr := e.createAccelerationDomain(ctx, cfg, zoneID, domain, originCfg)
		if createErr != nil {
			// 创建失败：EO 有时对"域名已存在"返回 UnauthorizedOperation 或其他错误码
			// 回退到直接调用 ModifyAccelerationDomain 更新回源配置
			log.Printf("[EdgeOne] 创建加速域名失败 (%v)，尝试更新已有域名的回源配置...", createErr)
			if updateErr := e.UpdateOriginConfig(ctx, cfg, domain, zoneID, originCfg); updateErr != nil {
				// 两者都失败，返回原始创建错误
				return nil, fmt.Errorf("添加加速域名失败: %w", createErr)
			}
			log.Printf("[EdgeOne] 回退更新成功，域名 %s 回源配置已同步", domain)
		}
	}

	// 2. 获取真实 CNAME
	cname, err := e.getAccelerationDomainCname(ctx, cfg, zoneID, domain)
	if err != nil {
		log.Printf("[EdgeOne] 获取CNAME失败，使用默认值: %v", err)
		cname = fmt.Sprintf("%s.eo.dnse3.com", domain)
	}

	log.Printf("[EdgeOne] 域名 %s 处理完成, CNAME: %s", domain, cname)

	return &model.AddDomainResult{
		ProviderSiteID: zoneID,
		CNAME:          cname,
		Status:         "active",
	}, nil
}

// GetDomainStatus 查询加速域名状态
func (e *EdgeOneProvider) GetDomainStatus(ctx context.Context, cfg map[string]string, domain string, providerSiteID string) (*model.DomainStatusResult, error) {
	domainInfo, err := e.describeAccelerationDomain(ctx, cfg, providerSiteID, domain)
	if err != nil {
		return nil, err
	}

	status := "pending"
	msg := "域名配置中"
	switch domainInfo.DomainStatus {
	case "online":
		status = "active"
		msg = "加速域名已上线"
	case "offline":
		status = "failed"
		msg = "加速域名已下线"
	case "process":
		msg = "加速域名部署中"
	case "rejected":
		status = "failed"
		msg = "加速域名审核未通过"
	default:
		msg = fmt.Sprintf("域名状态: %s", domainInfo.DomainStatus)
	}

	return &model.DomainStatusResult{
		Status:  status,
		Message: msg,
	}, nil
}

// DeleteDomain 先停用再删除加速域名（不删整个站点）
// 如域名部署中，每30秒重试一次，最多等待5分钟
func (e *EdgeOneProvider) DeleteDomain(ctx context.Context, cfg map[string]string, domain string, providerSiteID string) error {
	// providerSiteID 优先使用传入值，为空则回退到 deploy_params 里的 zone_id
	zoneID := providerSiteID
	if zoneID == "" {
		zoneID = cfg["zone_id"]
	}
	if zoneID == "" {
		return fmt.Errorf("无法获取 ZoneId：providerSiteID 和 cfg[zone_id] 均为空，无法删除加速域名 %s", domain)
	}

	maxRetries := 10 // 最多重试10次（每次30秒，共5分钟）

	// 1. 先停用加速域名（offline）
	for i := 0; i < maxRetries; i++ {
		log.Printf("[EdgeOne] 停用加速域名 %s（第%d次尝试，ZoneId=%s）...", domain, i+1, zoneID)
		offlineParams := map[string]interface{}{
			"ZoneId":      zoneID,
			"DomainNames": []string{domain},
			"Status":      "offline",
		}
		body, err := e.doRequest(ctx, cfg, "ModifyAccelerationDomainStatuses", offlineParams)
		if err != nil {
			return fmt.Errorf("停用域名失败: %w", err)
		}
		apiErr := e.parseError(body)
		if apiErr == nil {
			log.Printf("[EdgeOne] 停用域名成功，等待生效...")
			break
		}
		if strings.Contains(apiErr.Code, "ResourceNotFound") {
			log.Printf("[EdgeOne] 域名 %s 不存在，无需删除", domain)
			return nil
		}
		if apiErr.Code == "ResourceInUse" || strings.Contains(apiErr.Code, "InvalidDomainStatus") {
			if i < maxRetries-1 {
				log.Printf("[EdgeOne] 域名正在变更中，30秒后重试...")
				time.Sleep(30 * time.Second)
				continue
			}
			return fmt.Errorf("停用域名超时（等待%d分钟仍在变更中）", maxRetries/2)
		}
		// 参数错误或其他不可重试错误，直接返回
		return fmt.Errorf("停用域名失败: %w", apiErr)
	}

	// 等待停用生效
	time.Sleep(3 * time.Second)

	// 2. 删除加速域名
	for i := 0; i < maxRetries; i++ {
		log.Printf("[EdgeOne] 删除加速域名 %s（第%d次尝试）...", domain, i+1)
		deleteParams := map[string]interface{}{
			"ZoneId":      zoneID,
			"DomainNames": []string{domain},
		}
		body, err := e.doRequest(ctx, cfg, "DeleteAccelerationDomains", deleteParams)
		if err != nil {
			return fmt.Errorf("删除加速域名失败: %w", err)
		}
		apiErr := e.parseError(body)
		if apiErr == nil {
			log.Printf("[EdgeOne] 已删除加速域名 %s", domain)
			return nil
		}
		if strings.Contains(apiErr.Code, "ResourceNotFound") {
			log.Printf("[EdgeOne] 域名 %s 已不存在", domain)
			return nil
		}
		// 域名部署中，需等待状态稳定后才能删除
		if apiErr.Code == "ResourceInUse" || strings.Contains(apiErr.Code, "InvalidDomainStatus") {
			if i < maxRetries-1 {
				log.Printf("[EdgeOne] 域名状态不允许删除，30秒后重试...")
				time.Sleep(30 * time.Second)
				continue
			}
			return fmt.Errorf("删除域名超时（等待%d分钟仍无法删除）", maxRetries/2)
		}
		// 参数错误或其他不可重试错误，直接返回
		return fmt.Errorf("删除域名失败: %w", apiErr)
	}

	return nil
}

// UpdateOriginConfig 更新腾讯云 EdgeOne 回源配置（先查后比，不一样才改）
func (e *EdgeOneProvider) UpdateOriginConfig(ctx context.Context, cfg map[string]string, domain string, providerSiteID string, originCfg model.OriginConfig) error {
	zoneID := providerSiteID

	// 期望值
	expectProtocol := "FOLLOW"
	switch originCfg.OriginProtocol {
	case "http":
		expectProtocol = "HTTP"
	case "https":
		expectProtocol = "HTTPS"
	}
	expectHost := originCfg.OriginHost
	if expectHost == "" {
		expectHost = domain
	}

	// 先查询当前配置
	info, err := e.describeAccelerationDomain(ctx, cfg, zoneID, domain)
	if err != nil {
		log.Printf("[EdgeOne] 查询 %s 当前配置失败，直接更新: %v", domain, err)
	} else {
		// 对比：源站、协议、端口一致则跳过
		if info.Origin == originCfg.Origin &&
			info.OriginProtocol == expectProtocol &&
			info.HttpOriginPort == originCfg.HTTPPort &&
			info.HttpsOriginPort == originCfg.HTTPSPort {
			log.Printf("[EdgeOne] 域名 %s 回源配置已一致，跳过", domain)
			return nil
		}
		log.Printf("[EdgeOne] 域名 %s 回源配置不一致，更新中...", domain)
	}

	params := map[string]interface{}{
		"ZoneId":     zoneID,
		"DomainName": domain,
		"OriginInfo": map[string]interface{}{
			"OriginType":    "IP_DOMAIN",
			"Origin":        originCfg.Origin,
			"PrivateAccess": "off",
		},
		"OriginProtocol": expectProtocol,
	}
	if originCfg.HTTPPort > 0 {
		params["HttpOriginPort"] = originCfg.HTTPPort
	}
	if originCfg.HTTPSPort > 0 {
		params["HttpsOriginPort"] = originCfg.HTTPSPort
	}

	body, err := e.doRequest(ctx, cfg, "ModifyAccelerationDomain", params)
	if err != nil {
		return fmt.Errorf("更新回源配置失败: %w", err)
	}
	if apiErr := e.parseError(body); apiErr != nil {
		return fmt.Errorf("更新回源配置失败: %w", apiErr)
	}

	log.Printf("[EdgeOne] 已更新域名 %s 的回源配置 (ZoneId: %s)", domain, zoneID)
	return nil
}

// EnableEdgeCert 启用腾讯云 EdgeOne 免费边缘证书
func (e *EdgeOneProvider) EnableEdgeCert(ctx context.Context, cfg map[string]string, domain string, providerSiteID string) error {
	zoneID := providerSiteID
	params := map[string]interface{}{
		"ZoneId": zoneID,
		"Hosts":  []string{domain},
		"Mode":   "eofreecert",
	}

	body, err := e.doRequest(ctx, cfg, "ModifyHostsCertificate", params)
	if err != nil {
		return fmt.Errorf("启用边缘证书失败: %w", err)
	}
	if apiErr := e.parseError(body); apiErr != nil {
		return fmt.Errorf("启用边缘证书失败: %w", apiErr)
	}

	log.Printf("[EdgeOne] 已启用域名 %s 的免费边缘证书 (ZoneId: %s)", domain, zoneID)
	return nil
}

// DeployCertificate 将自定义证书部署到腾讯云 EdgeOne
// 步骤：1. 上传证书到 SSL 证书服务  2. 用 ModifyHostsCertificate 绑定到加速域名
func (e *EdgeOneProvider) DeployCertificate(ctx context.Context, cfg map[string]string, domain string, providerSiteID string, certPEM string, keyPEM string) error {
	// 1. 上传证书到 SSL 证书服务
	certId, err := e.uploadCertToSSL(ctx, cfg, domain, certPEM, keyPEM)
	if err != nil {
		return fmt.Errorf("上传证书失败: %w", err)
	}
	log.Printf("[EdgeOne] 证书已上传到 SSL 证书服务, CertId: %s", certId)

	// 2. 绑定证书到加速域名
	zoneID := providerSiteID
	params := map[string]interface{}{
		"ZoneId": zoneID,
		"Hosts":  []string{domain},
		"Mode":   "sslcert",
		"ServerCertInfo": []map[string]interface{}{
			{"CertId": certId},
		},
	}

	body, err := e.doRequest(ctx, cfg, "ModifyHostsCertificate", params)
	if err != nil {
		return fmt.Errorf("绑定证书失败: %w", err)
	}
	if apiErr := e.parseError(body); apiErr != nil {
		return fmt.Errorf("绑定证书失败: %w", apiErr)
	}

	log.Printf("[EdgeOne] 证书已绑定到域名 %s (ZoneId: %s)", domain, zoneID)
	return nil
}

// uploadCertToSSL 上传证书到腾讯云 SSL 证书服务，返回 CertId
func (e *EdgeOneProvider) uploadCertToSSL(ctx context.Context, cfg map[string]string, domain string, certPEM string, keyPEM string) (string, error) {
	const (
		sslEndpoint = "https://ssl.tencentcloudapi.com"
		sslHost     = "ssl.tencentcloudapi.com"
		sslService  = "ssl"
		sslVersion  = "2019-12-05"
	)

	alias := fmt.Sprintf("allfast-%s-%s", domain, time.Now().Format("20060102150405"))
	params := map[string]interface{}{
		"CertificatePublicKey":  certPEM,
		"CertificatePrivateKey": keyPEM,
		"Alias":                 alias,
	}

	body, err := e.doTCRequest(ctx, cfg, "UploadCertificate", params, sslEndpoint, sslHost, sslService, sslVersion, "SSL")
	if err != nil {
		return "", err
	}

	var resp struct {
		Response struct {
			CertificateId string      `json:"CertificateId"`
			Error         *eoAPIError `json:"Error"`
		} `json:"Response"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", fmt.Errorf("解析响应失败: %w", err)
	}
	if resp.Response.Error != nil {
		return "", resp.Response.Error
	}
	if resp.Response.CertificateId == "" {
		return "", fmt.Errorf("上传证书成功但未返回 CertificateId")
	}

	return resp.Response.CertificateId, nil
}

// SetupCertificate 不自动申请证书，查询当前证书状态
func (e *EdgeOneProvider) SetupCertificate(ctx context.Context, cfg map[string]string, domain string, providerSiteID string) (*model.CertRequestResult, error) {
	status := e.queryCertStatus(ctx, cfg, providerSiteID, domain)
	return &model.CertRequestResult{
		Status: status,
	}, nil
}

// GetCertificateStatus 通过 DescribeAccelerationDomains 查询证书配置状态
func (e *EdgeOneProvider) GetCertificateStatus(ctx context.Context, cfg map[string]string, domain string, certID string) (*model.CertStatusResult, error) {
	// 需要 zone_id，从 certID 或 cfg 获取
	zoneID := cfg["zone_id"]
	if zoneID == "" {
		return &model.CertStatusResult{Status: "pending", Message: "缺少zone_id"}, nil
	}

	status := e.queryCertStatus(ctx, cfg, zoneID, domain)
	return &model.CertStatusResult{
		Status:  status,
		Message: fmt.Sprintf("证书状态: %s", status),
	}, nil
}

// ===== EdgeOne 内部方法 =====

// eoDomainInfo DescribeAccelerationDomains 返回的域名信息
type eoDomainInfo struct {
	DomainStatus    string
	Cname           string
	CertMode        string
	Origin          string
	HostHeader      string
	OriginProtocol  string
	HttpOriginPort  int
	HttpsOriginPort int
}

// describeAccelerationDomain 查询单个加速域名详情
func (e *EdgeOneProvider) describeAccelerationDomain(ctx context.Context, cfg map[string]string, zoneID, domain string) (*eoDomainInfo, error) {
	params := map[string]interface{}{
		"ZoneId": zoneID,
		"Filters": []map[string]interface{}{
			{
				"Name":   "domain-name",
				"Values": []string{domain},
			},
		},
	}

	body, err := e.doRequest(ctx, cfg, "DescribeAccelerationDomains", params)
	if err != nil {
		return nil, err
	}

	var resp struct {
		Response struct {
			AccelerationDomains []struct {
				DomainName   string `json:"DomainName"`
				DomainStatus string `json:"DomainStatus"`
				Cname        string `json:"Cname"`
				Certificate  struct {
					Mode string `json:"Mode"`
					List []struct {
						CertId string `json:"CertId"`
					} `json:"List"`
				} `json:"Certificate"`
				OriginDetail struct {
					Origin     string `json:"Origin"`
					HostHeader string `json:"HostHeader"`
				} `json:"OriginDetail"`
				OriginProtocol  string `json:"OriginProtocol"`
				HttpOriginPort  int    `json:"HttpOriginPort"`
				HttpsOriginPort int    `json:"HttpsOriginPort"`
			} `json:"AccelerationDomains"`
			Error *eoAPIError `json:"Error"`
		} `json:"Response"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("解析响应失败: %w", err)
	}
	if resp.Response.Error != nil {
		return nil, resp.Response.Error
	}

	for _, d := range resp.Response.AccelerationDomains {
		if d.DomainName == domain {
			return &eoDomainInfo{
				DomainStatus:    d.DomainStatus,
				Cname:           d.Cname,
				CertMode:        d.Certificate.Mode,
				Origin:          d.OriginDetail.Origin,
				HostHeader:      d.OriginDetail.HostHeader,
				OriginProtocol:  d.OriginProtocol,
				HttpOriginPort:  d.HttpOriginPort,
				HttpsOriginPort: d.HttpsOriginPort,
			}, nil
		}
	}

	return nil, fmt.Errorf("未找到加速域名: %s", domain)
}

// getAccelerationDomainCname 获取加速域名的真实CNAME
func (e *EdgeOneProvider) getAccelerationDomainCname(ctx context.Context, cfg map[string]string, zoneID, domain string) (string, error) {
	info, err := e.describeAccelerationDomain(ctx, cfg, zoneID, domain)
	if err != nil {
		return "", err
	}
	if info.Cname == "" {
		return "", fmt.Errorf("CNAME为空")
	}
	return info.Cname, nil
}

// queryCertStatus 查询加速域名的证书配置状态
func (e *EdgeOneProvider) queryCertStatus(ctx context.Context, cfg map[string]string, zoneID, domain string) string {
	info, err := e.describeAccelerationDomain(ctx, cfg, zoneID, domain)
	if err != nil {
		log.Printf("[EdgeOne] 查询 %s 证书状态失败: %v", domain, err)
		return "none"
	}

	// Certificate.Mode: disable / eofreecert / eofreecert_manual / sslcert
	switch info.CertMode {
	case "sslcert", "eofreecert":
		return "active"
	case "eofreecert_manual":
		return "deploying"
	case "disable", "":
		return "none"
	default:
		return "none"
	}
}

func (e *EdgeOneProvider) createAccelerationDomain(ctx context.Context, cfg map[string]string, zoneID, domain string, originCfg model.OriginConfig) error {
	originAddr := originCfg.Origin
	originType := "IP_DOMAIN"

	// 回源协议
	originProtocol := "FOLLOW"
	switch originCfg.OriginProtocol {
	case "http":
		originProtocol = "HTTP"
	case "https":
		originProtocol = "HTTPS"
	}

	originInfo := map[string]interface{}{
		"OriginType":    originType,
		"Origin":        originAddr,
		"PrivateAccess": "off",
	}

	params := map[string]interface{}{
		"ZoneId":         zoneID,
		"DomainName":     domain,
		"OriginInfo":     originInfo,
		"OriginProtocol": originProtocol,
	}

	// 非标准端口
	if originCfg.HTTPPort > 0 && originCfg.HTTPPort != 80 {
		params["HttpOriginPort"] = originCfg.HTTPPort
	}
	if originCfg.HTTPSPort > 0 && originCfg.HTTPSPort != 443 {
		params["HttpsOriginPort"] = originCfg.HTTPSPort
	}

	body, err := e.doRequest(ctx, cfg, "CreateAccelerationDomain", params)
	if err != nil {
		return err
	}

	if apiErr := e.parseError(body); apiErr != nil {
		// 域名已存在不视为错误
		if strings.Contains(apiErr.Code, "ResourceInUse") ||
			strings.Contains(apiErr.Code, "DomainAlreadyExist") {
			log.Printf("[EdgeOne] 域名 %s 已存在，跳过创建", domain)
			return nil
		}
		return apiErr
	}

	return nil
}

// parseError 解析API响应中的错误
func (e *EdgeOneProvider) parseError(body []byte) *eoAPIError {
	var resp struct {
		Response struct {
			Error *eoAPIError `json:"Error"`
		} `json:"Response"`
	}
	json.Unmarshal(body, &resp)
	return resp.Response.Error
}

// getEndpoint 根据配置返回API端点和Host
func (e *EdgeOneProvider) getEndpoint(cfg map[string]string) (endpoint, host string) {
	region := cfg["api_region"]
	if region == "global" {
		return eoEndpointGlobal, eoHostGlobal
	}
	return eoEndpointDomestic, eoHostDomestic
}

// doRequest 执行 EdgeOne API 请求
func (e *EdgeOneProvider) doRequest(ctx context.Context, cfg map[string]string, action string, params interface{}) ([]byte, error) {
	endpoint, host := e.getEndpoint(cfg)
	return e.doTCRequest(ctx, cfg, action, params, endpoint, host, eoService, eoVersion, "EdgeOne")
}

// doDnspodRequest 执行 DNSPod API 请求
func (e *EdgeOneProvider) doDnspodRequest(ctx context.Context, cfg map[string]string, action string, params interface{}) ([]byte, error) {
	return e.doTCRequest(ctx, cfg, action, params, dnspodEndpoint, dnspodHost, dnspodService, dnspodVersion, "DNSPod")
}

// doTCRequest 通用腾讯云API请求（TC3-HMAC-SHA256 签名）
func (e *EdgeOneProvider) doTCRequest(ctx context.Context, cfg map[string]string, action string, params interface{}, endpoint, host, service, version, logTag string) ([]byte, error) {
	secretId := cfg["secret_id"]
	secretKey := cfg["secret_key"]

	payload, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("序列化请求失败: %w", err)
	}

	log.Printf("[%s] %s 请求: %s", logTag, action, string(payload))

	now := time.Now().UTC()
	timestamp := now.Unix()
	dateStr := now.Format("2006-01-02")

	// 1. 拼接规范请求串
	payloadHash := provider.Sha256Hex(payload)
	canonicalHeaders := fmt.Sprintf("content-type:application/json\nhost:%s\nx-tc-action:%s\n",
		host, strings.ToLower(action))
	signedHeaders := "content-type;host;x-tc-action"

	canonicalRequest := strings.Join([]string{
		"POST", "/", "",
		canonicalHeaders,
		signedHeaders,
		payloadHash,
	}, "\n")

	// 2. 拼接待签名字符串
	credentialScope := fmt.Sprintf("%s/%s/tc3_request", dateStr, service)
	stringToSign := fmt.Sprintf("TC3-HMAC-SHA256\n%d\n%s\n%s",
		timestamp, credentialScope, provider.Sha256Hex([]byte(canonicalRequest)))

	// 3. 计算签名
	secretDate := hmacSHA256([]byte("TC3"+secretKey), dateStr)
	secretService := hmacSHA256(secretDate, service)
	secretSigning := hmacSHA256(secretService, "tc3_request")
	signature := hex.EncodeToString(hmacSHA256(secretSigning, stringToSign))

	// 4. Authorization
	authorization := fmt.Sprintf("TC3-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		secretId, credentialScope, signedHeaders, signature)

	// 5. 发送请求
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("构建请求失败: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Host", host)
	req.Header.Set("X-TC-Action", action)
	req.Header.Set("X-TC-Version", version)
	req.Header.Set("X-TC-Timestamp", fmt.Sprintf("%d", timestamp))
	req.Header.Set("Authorization", authorization)

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求%s API失败: %w", logTag, err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("读取响应失败: %w", err)
	}

	log.Printf("[%s] %s 响应: %s", logTag, action, string(respBody))
	return respBody, nil
}

// hmacSHA256 HMAC-SHA256 签名
func hmacSHA256(key []byte, data string) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(data))
	return mac.Sum(nil)
}

// ===== EdgeOne 工具类型 =====

type eoAPIError struct {
	Code    string `json:"Code"`
	Message string `json:"Message"`
}

func (e *eoAPIError) Error() string {
	return fmt.Sprintf("EdgeOne API错误: [%s] %s", e.Code, e.Message)
}
