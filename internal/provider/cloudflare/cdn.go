package cloudflare

import (
	"allfast/internal/model"
	"allfast/internal/provider"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

func init() {
	provider.Register(&CloudflareProvider{})
}

const cfAPIBase = "https://api.cloudflare.com/client/v4"

// CloudflareProvider Cloudflare CDN 真实 API v4 集成
// 支持两种认证方式：API Token 或 Global API Key + Email
// 支持两种接入模式：
//   - ns:   传统 NS 接入，为域名创建独立 Zone，需修改域名 NS 记录
//   - saas: Custom Hostnames (Cloudflare for SaaS)，通过 CNAME 接入已有 Zone，无需修改 NS
type CloudflareProvider struct{}

func (c *CloudflareProvider) Name() string { return "cloudflare" }

func (c *CloudflareProvider) Info() model.ProviderInfo {
	return model.ProviderInfo{
		Name:              "cloudflare",
		DisplayName:       "Cloudflare",
		Description:       "全球领先的CDN和安全服务提供商（支持NS接入和SaaS/CNAME接入）",
		SupportsOptDomain: true,
		ConfigFields: []model.ConfigField{
			{Key: "auth_type", Label: "认证方式", Type: "select", Required: true, Secret: false, Options: []model.SelectOption{
				{Value: "token", Label: "API Token（推荐）"},
				{Value: "global_key", Label: "Global API Key + Email"},
			}},
			{Key: "api_token", Label: "API Token", Type: "secret", Required: false, Secret: true, Placeholder: "Cloudflare API Token（选Token认证时填写）"},
			{Key: "global_api_key", Label: "Global API Key", Type: "secret", Required: false, Secret: true, Placeholder: "Global API Key（选Global Key认证时填写）"},
			{Key: "email", Label: "账户邮箱", Type: "text", Required: false, Secret: false, Placeholder: "Cloudflare 账户邮箱（Global Key时填写）"},
		},
		DeployFields: []model.ConfigField{
			{Key: "mode", Label: "接入模式", Type: "select", Required: true, Secret: false, Options: []model.SelectOption{
				{Value: "cname", Label: "CNAME接入（自定义主机名）"},
				{Value: "ns", Label: "NS接入（托管DNS）"},
			}},
			{Key: "zone_id", Label: "Zone（CNAME接入必填）", Type: "select", Required: false, Secret: false, Placeholder: "选择SaaS主站Zone", Fetchable: true},
			{Key: "ssl_validation_method", Label: "证书验证方式（CNAME接入）", Type: "select", Required: false, Secret: false, Options: []model.SelectOption{
				{Value: "http", Label: "HTTP 验证"},
				{Value: "txt", Label: "TXT 验证"},
			}},
			{Key: "fallback_origin", Label: "回退源父域名", Type: "select", Required: false, Secret: false, Placeholder: "选择同账号顶级域名（将自动创建子域名记录）", Fetchable: true},
		},
	}
}

func (c *CloudflareProvider) FetchFieldOptions(ctx context.Context, cfg map[string]string, fieldKey string) ([]model.SelectOption, error) {
	if fieldKey != "zone_id" && fieldKey != "fallback_origin" {
		return nil, nil
	}
	if err := c.validateAuth(cfg); err != nil {
		return nil, err
	}

	// 分页获取所有 Zone
	var allZones []model.SelectOption
	var fallbackOrigins []model.SelectOption
	page := 1
	for {
		url := fmt.Sprintf("%s/zones?page=%d&per_page=50", cfAPIBase, page)
		body, err := c.doRequest(ctx, cfg, "GET", url, nil)
		if err != nil {
			return nil, fmt.Errorf("获取Zone列表失败: %w", err)
		}

		var resp struct {
			Success    bool `json:"success"`
			ResultInfo struct {
				TotalPages int `json:"total_pages"`
			} `json:"result_info"`
			Result []struct {
				ID     string `json:"id"`
				Name   string `json:"name"`
				Status string `json:"status"`
				Plan   struct {
					Name string `json:"name"`
				} `json:"plan"`
			} `json:"result"`
			Errors []cfError `json:"errors"`
		}
		if err := json.Unmarshal(body, &resp); err != nil {
			return nil, fmt.Errorf("解析响应失败: %w", err)
		}
		if !resp.Success {
			return nil, cfErrorMsg(resp.Errors)
		}

		for _, z := range resp.Result {
			label := fmt.Sprintf("%s (%s) [%s]", z.Name, z.Plan.Name, z.Status)
			allZones = append(allZones, model.SelectOption{Value: z.ID, Label: label})
			fallbackOrigins = append(fallbackOrigins, model.SelectOption{Value: z.Name, Label: z.Name})
		}

		if page >= resp.ResultInfo.TotalPages {
			break
		}
		page++
	}

	if len(allZones) == 0 {
		return nil, fmt.Errorf("当前账号下没有可用的Zone")
	}
	if fieldKey == "fallback_origin" {
		// 回退源域名：使用同账号顶级域名列表
		return fallbackOrigins, nil
	}
	return allZones, nil
}

// validateAuth 校验认证配置
func (c *CloudflareProvider) validateAuth(cfg map[string]string) error {
	authType := cfg["auth_type"]
	if authType == "global_key" {
		if cfg["global_api_key"] == "" {
			return fmt.Errorf("请填写 Global API Key")
		}
		if cfg["email"] == "" {
			return fmt.Errorf("请填写账户邮箱")
		}
		return nil
	}
	// 默认 token 模式
	if cfg["api_token"] == "" {
		return fmt.Errorf("请填写 API Token")
	}
	return nil
}

func (c *CloudflareProvider) ValidateConfig(cfg map[string]string) error {
	return c.validateAuth(cfg)
}

// validateDeployParams 校验部署参数（在 AddDomain 时调用）
func (c *CloudflareProvider) validateDeployParams(cfg map[string]string) error {
	mode := c.getMode(cfg)
	if mode != "ns" && mode != "cname" {
		return fmt.Errorf("mode 必须为 ns 或 cname，当前值: %s", cfg["mode"])
	}
	if mode == "cname" && cfg["zone_id"] == "" {
		return fmt.Errorf("CNAME接入模式下 zone_id 为必填项")
	}
	if mode == "cname" {
		fallback := strings.TrimSpace(cfg["fallback_origin"])
		if fallback == "" {
			return fmt.Errorf("CNAME接入模式下回退源父域名(fallback_origin)为必填项")
		}
	}
	return nil
}

// getMode 获取接入模式，默认 ns
func (c *CloudflareProvider) getMode(cfg map[string]string) string {
	mode := strings.TrimSpace(strings.ToLower(cfg["mode"]))
	if mode == "saas" {
		return "cname"
	}
	if mode == "" {
		return "ns"
	}
	return mode
}

// AddDomain 根据接入模式添加域名
func (c *CloudflareProvider) AddDomain(ctx context.Context, cfg map[string]string, domain string, originCfg model.OriginConfig) (*model.AddDomainResult, error) {
	if err := c.validateAuth(cfg); err != nil {
		return nil, err
	}
	if err := c.validateDeployParams(cfg); err != nil {
		return nil, err
	}
	if c.getMode(cfg) == "cname" {
		return c.addDomainSaaS(ctx, cfg, domain, originCfg)
	}
	return c.addDomainNS(ctx, cfg, domain, originCfg)
}

// GetDomainStatus 根据接入模式查询域名状态
func (c *CloudflareProvider) GetDomainStatus(ctx context.Context, cfg map[string]string, domain string, providerSiteID string) (*model.DomainStatusResult, error) {
	if c.getMode(cfg) == "cname" {
		return c.getDomainStatusSaaS(ctx, cfg, providerSiteID)
	}
	return c.getDomainStatusNS(ctx, cfg, providerSiteID)
}

// DeleteDomain 根据接入模式删除域名
func (c *CloudflareProvider) DeleteDomain(ctx context.Context, cfg map[string]string, domain string, providerSiteID string) error {
	if c.getMode(cfg) == "cname" {
		return c.deleteDomainSaaS(ctx, cfg, domain, providerSiteID)
	}
	return c.deleteDomainNS(ctx, cfg, providerSiteID)
}

// SetupCertificate 根据接入模式配置证书
func (c *CloudflareProvider) SetupCertificate(ctx context.Context, cfg map[string]string, domain string, providerSiteID string) (*model.CertRequestResult, error) {
	if c.getMode(cfg) == "cname" {
		return c.setupCertSaaS(ctx, cfg, providerSiteID)
	}
	return c.setupCertNS(ctx, cfg, providerSiteID)
}

// UpdateOriginConfig CNAME 模式下更新回退源 DNS 记录和 Origin Rule
func (c *CloudflareProvider) UpdateOriginConfig(ctx context.Context, cfg map[string]string, domain string, providerSiteID string, originCfg model.OriginConfig) error {
	if c.getMode(cfg) != "cname" {
		log.Printf("[Cloudflare] NS 模式跳过回源配置同步 (域名: %s)", domain)
		return nil
	}

	zoneID := cfg["zone_id"]
	if zoneID == "" {
		return nil
	}

	// 计算回退源子域名
	fallbackHost := ""
	fallbackZone := strings.TrimSpace(cfg["fallback_origin"])
	if fallbackZone != "" {
		fallbackHost = fmt.Sprintf("%s.%s", strings.ReplaceAll(domain, ".", "-"), fallbackZone)
		// 同步回退源 DNS 记录（源站地址变更时更新）
		if fallbackZoneID, err := c.getZoneIDByName(ctx, cfg, fallbackZone); err == nil {
			if err := c.ensureFallbackRecord(ctx, cfg, fallbackZoneID, fallbackHost, originCfg.Origin); err != nil {
				log.Printf("[Cloudflare] 更新回退源记录失败 [%s]: %v", fallbackHost, err)
			}
		}
	}

	// 同步 Origin Rule（端口重写）
	if err := c.ensureOriginRule(ctx, cfg, zoneID, domain, fallbackHost, originCfg); err != nil {
		log.Printf("[Cloudflare] 更新 Origin Rule 失败 [%s]: %v", domain, err)
	}
	return nil
}

// EnableEdgeCert Cloudflare 默认自动管理边缘证书（Universal SSL），无需额外操作
func (c *CloudflareProvider) EnableEdgeCert(ctx context.Context, cfg map[string]string, domain string, providerSiteID string) error {
	log.Printf("[Cloudflare] Cloudflare 默认启用 Universal SSL，无需额外配置 (域名: %s)", domain)
	return nil
}

// DeployCertificate Cloudflare 不支持通过 API 上传自定义证书（免费版），返回不支持提示
func (c *CloudflareProvider) DeployCertificate(ctx context.Context, cfg map[string]string, domain string, providerSiteID string, certPEM string, keyPEM string) error {
	return fmt.Errorf("Cloudflare 使用边缘证书，不支持上传自定义证书")
}

// GetCertificateStatus 根据接入模式查询证书状态
func (c *CloudflareProvider) GetCertificateStatus(ctx context.Context, cfg map[string]string, domain string, certID string) (*model.CertStatusResult, error) {
	if c.getMode(cfg) == "cname" {
		return c.getCertStatusSaaS(ctx, cfg, certID)
	}
	return c.getCertStatusNS(ctx, cfg, certID)
}

// =============================================================================
// SaaS 模式 (Custom Hostnames / Cloudflare for SaaS) — CNAME 接入
// =============================================================================

// addDomainSaaS 通过 Custom Hostnames API 添加域名
func (c *CloudflareProvider) addDomainSaaS(ctx context.Context, cfg map[string]string, domain string, originCfg model.OriginConfig) (*model.AddDomainResult, error) {
	zoneID := cfg["zone_id"]

	sslMethod := strings.TrimSpace(strings.ToLower(cfg["ssl_validation_method"]))
	if sslMethod != "txt" {
		sslMethod = "http"
	}

	// 根据 fallback_origin 父域名自动生成二级回退源子域名
	// 格式：{domain所有点替换为横线}.{fallback-zone}
	// 例如：ssl.yaooa.cn + 081806.xyz → ssl-yaooa-cn.081806.xyz
	originHost := originCfg.Origin
	fallbackHost := ""
	fallbackZone := strings.TrimSpace(cfg["fallback_origin"])
	if fallbackZone != "" {
		fallbackSubdomain := strings.ReplaceAll(domain, ".", "-")
		fallbackHost = fmt.Sprintf("%s.%s", fallbackSubdomain, fallbackZone)

		// 查找回退源父域名对应的 Zone ID
		fallbackZoneID, err := c.getZoneIDByName(ctx, cfg, fallbackZone)
		if err != nil {
			return nil, fmt.Errorf("查找回退源Zone失败 [%s]: %w", fallbackZone, err)
		}

		// 在父域名 Zone 中自动创建二级回退源 DNS 记录（开启 CF 代理）
		if err := c.ensureFallbackRecord(ctx, cfg, fallbackZoneID, fallbackHost, originCfg.Origin); err != nil {
			return nil, fmt.Errorf("创建回退源DNS记录失败 [%s]: %w", fallbackHost, err)
		}

		originHost = fallbackHost
		log.Printf("[Cloudflare] 回退源子域名: %s → 源站: %s", fallbackHost, originCfg.Origin)
	}

	// 1. 创建 Custom Hostname
	payload := map[string]interface{}{
		"hostname": domain,
		"ssl": map[string]interface{}{
			"method": sslMethod,
			"type":   "dv",
			"settings": map[string]interface{}{
				"min_tls_version": "1.0",
			},
		},
		"custom_origin_server": originHost,
	}

	url := fmt.Sprintf("%s/zones/%s/custom_hostnames", cfAPIBase, zoneID)
	body, err := c.doRequest(ctx, cfg, "POST", url, payload)
	if err != nil {
		return nil, fmt.Errorf("创建Custom Hostname失败: %w", err)
	}

	var resp struct {
		Success bool `json:"success"`
		Result  struct {
			ID                    string `json:"id"`
			Hostname              string `json:"hostname"`
			CustomOriginServer    string `json:"custom_origin_server"`
			Status                string `json:"status"`
			OwnershipVerification struct {
				Type  string `json:"type"`
				Name  string `json:"name"`
				Value string `json:"value"`
			} `json:"ownership_verification"`
			OwnershipVerificationHTTP struct {
				HTTPUrl  string `json:"http_url"`
				HTTPBody string `json:"http_body"`
			} `json:"ownership_verification_http"`
			SSL struct {
				Status            string `json:"status"`
				ValidationRecords []struct {
					TxtName     string `json:"txt_name"`
					TxtValue    string `json:"txt_value"`
					CnameName   string `json:"cname"`
					CnameTarget string `json:"cname_target"`
				} `json:"validation_records"`
			} `json:"ssl"`
		} `json:"result"`
		Errors []cfError `json:"errors"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("解析响应失败: %w", err)
	}

	var hostnameID, hostnameStatus string
	if resp.Success {
		hostnameID = resp.Result.ID
		hostnameStatus = resp.Result.Status
	} else {
		// 已存在（code 1406）时查找并更新已有记录
		isDuplicate := false
		for _, e := range resp.Errors {
			if e.Code == 1406 {
				isDuplicate = true
				break
			}
		}
		if !isDuplicate {
			return nil, cfErrorMsg(resp.Errors)
		}
		log.Printf("[Cloudflare] Custom Hostname %s 已存在，查找并更新...", domain)
		existing, err := c.findCustomHostname(ctx, cfg, zoneID, domain)
		if err != nil {
			return nil, fmt.Errorf("查找已有 Custom Hostname 失败: %w", err)
		}
		// 更新 origin server
		updateURL := fmt.Sprintf("%s/zones/%s/custom_hostnames/%s", cfAPIBase, zoneID, existing.ID)
		updatePayload := map[string]interface{}{
			"custom_origin_server": originHost,
			"ssl": map[string]interface{}{
				"method": sslMethod, "type": "dv",
				"settings": map[string]interface{}{"min_tls_version": "1.0"},
			},
		}
		c.doRequest(ctx, cfg, "PATCH", updateURL, updatePayload)
		hostnameID = existing.ID
		hostnameStatus = existing.Status
		log.Printf("[Cloudflare] Custom Hostname %s 已更新 (ID: %s)", domain, hostnameID)
	}

	// CNAME 接入目标：有回退源时使用回退源二级域名（已开启 CF 代理），无回退源时兜底用 cdn.cloudflare.net
	zoneCname := fallbackHost
	if zoneCname == "" {
		zoneCname = fmt.Sprintf("%s.cdn.cloudflare.net", domain)
	}

	// 同步 Origin Rule（端口和 Host 重写）
	if err := c.ensureOriginRule(ctx, cfg, zoneID, domain, fallbackHost, originCfg); err != nil {
		log.Printf("[Cloudflare] 建立 Origin Rule 失败 [%s]: %v", domain, err)
	}

	return &model.AddDomainResult{
		ProviderSiteID: hostnameID,
		CNAME:          zoneCname,
		Status:         hostnameStatus,
	}, nil
}

// findCustomHostname 按 hostname 查找已有 custom hostname 记录
func (c *CloudflareProvider) findCustomHostname(ctx context.Context, cfg map[string]string, zoneID, hostname string) (*struct{ ID, Status string }, error) {
	url := fmt.Sprintf("%s/zones/%s/custom_hostnames?hostname=%s", cfAPIBase, zoneID, hostname)
	body, err := c.doRequest(ctx, cfg, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Success bool `json:"success"`
		Result  []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"result"`
		Errors []cfError `json:"errors"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("解析响应失败: %w", err)
	}
	if !resp.Success {
		return nil, cfErrorMsg(resp.Errors)
	}
	for _, r := range resp.Result {
		return &struct{ ID, Status string }{r.ID, r.Status}, nil
	}
	return nil, fmt.Errorf("未找到 Custom Hostname: %s", hostname)
}

// =============================================================================
// Origin Rules Helpers
// =============================================================================

// getOriginRules 获取 Zone 的 Origin 规则列表（无规则集时返回空切片）
func (c *CloudflareProvider) getOriginRules(ctx context.Context, cfg map[string]string, zoneID string) ([]map[string]interface{}, error) {
	url := fmt.Sprintf("%s/zones/%s/rulesets/phases/http_request_origin/entrypoint", cfAPIBase, zoneID)
	body, err := c.doRequest(ctx, cfg, "GET", url, nil)
	if err != nil {
		return []map[string]interface{}{}, nil
	}
	var resp struct {
		Success bool `json:"success"`
		Result  struct {
			Rules []map[string]interface{} `json:"rules"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &resp); err != nil || !resp.Success {
		return []map[string]interface{}{}, nil
	}
	if resp.Result.Rules == nil {
		return []map[string]interface{}{}, nil
	}
	return resp.Result.Rules, nil
}

// putOriginRules 替换 Zone 的整个 Origin 规则集
func (c *CloudflareProvider) putOriginRules(ctx context.Context, cfg map[string]string, zoneID string, rules []map[string]interface{}) error {
	url := fmt.Sprintf("%s/zones/%s/rulesets/phases/http_request_origin/entrypoint", cfAPIBase, zoneID)
	body, err := c.doRequest(ctx, cfg, "PUT", url, map[string]interface{}{"rules": rules})
	if err != nil {
		return err
	}
	var resp struct {
		Success bool      `json:"success"`
		Errors  []cfError `json:"errors"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return fmt.Errorf("解析响应失败: %w", err)
	}
	if !resp.Success {
		return cfErrorMsg(resp.Errors)
	}
	return nil
}

// ensureOriginRule 在 SaaS Zone 上创建/更新回源规则
// 匹配自定义主机名和回退源子域，重写非标准端口
func (c *CloudflareProvider) ensureOriginRule(ctx context.Context, cfg map[string]string, zoneID, domain, fallbackHost string, originCfg model.OriginConfig) error {
	// 计算目标端口
	httpsPort := originCfg.HTTPSPort
	if httpsPort == 0 {
		httpsPort = 443
	}
	httpPort := originCfg.HTTPPort
	if httpPort == 0 {
		httpPort = 80
	}
	port := 0
	if httpsPort != 443 {
		port = httpsPort
	} else if httpPort != 80 {
		port = httpPort
	}
	if port == 0 {
		return nil // 标准端口，无需规则
	}

	// 构建匹配表达式：同时匹配自定义主机名和回退源子域
	expr := fmt.Sprintf(`(http.host eq "%s")`, domain)
	if fallbackHost != "" {
		expr = fmt.Sprintf(`(http.host eq "%s") or (http.host eq "%s")`, domain, fallbackHost)
	}

	newRule := map[string]interface{}{
		"action": "route",
		"action_parameters": map[string]interface{}{
			"origin": map[string]interface{}{
				"port": port,
			},
		},
		"expression":  expr,
		"description": domain,
		"enabled":     true,
	}

	rules, err := c.getOriginRules(ctx, cfg, zoneID)
	if err != nil {
		return err
	}

	// 找到同名规则则替换，否则追加
	found := false
	for i, r := range rules {
		if desc, ok := r["description"].(string); ok && desc == domain {
			rules[i] = newRule
			found = true
			break
		}
	}
	if !found {
		rules = append(rules, newRule)
	}

	if err := c.putOriginRules(ctx, cfg, zoneID, rules); err != nil {
		return fmt.Errorf("保存 Origin Rule 失败: %w", err)
	}
	log.Printf("[Cloudflare] Origin Rule 已同步: %s → 端口 %d", domain, port)
	return nil
}

// removeOriginRule 从 SaaS Zone 中删除指定域名的回源规则
func (c *CloudflareProvider) removeOriginRule(ctx context.Context, cfg map[string]string, zoneID, domain string) error {
	rules, err := c.getOriginRules(ctx, cfg, zoneID)
	if err != nil {
		return err
	}
	filtered := make([]map[string]interface{}, 0, len(rules))
	for _, r := range rules {
		if desc, ok := r["description"].(string); ok && desc == domain {
			continue
		}
		filtered = append(filtered, r)
	}
	if len(filtered) == len(rules) {
		return nil // 规则不存在，无需操作
	}
	return c.putOriginRules(ctx, cfg, zoneID, filtered)
}

// getZoneIDByName 根据域名查找对应的 Zone ID
func (c *CloudflareProvider) getZoneIDByName(ctx context.Context, cfg map[string]string, zoneName string) (string, error) {
	url := fmt.Sprintf("%s/zones?name=%s", cfAPIBase, zoneName)
	body, err := c.doRequest(ctx, cfg, "GET", url, nil)
	if err != nil {
		return "", fmt.Errorf("查询Zone失败: %w", err)
	}

	var resp struct {
		Success bool `json:"success"`
		Result  []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"result"`
		Errors []cfError `json:"errors"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", fmt.Errorf("解析响应失败: %w", err)
	}
	if !resp.Success {
		return "", cfErrorMsg(resp.Errors)
	}
	if len(resp.Result) == 0 {
		return "", fmt.Errorf("未找到Zone: %s（请确认该域名已托管到当前CF账号）", zoneName)
	}
	return resp.Result[0].ID, nil
}

// ensureFallbackRecord 在指定 Zone 中创建/更新回退源 DNS 记录
// recordFQDN 为完整域名（如 ssl-fallback.081806.xyz），使用非代理模式（DNS only），以便 Cloudflare 直接回源
func (c *CloudflareProvider) ensureFallbackRecord(ctx context.Context, cfg map[string]string, zoneID, recordFQDN, origin string) error {
	recordType := "CNAME"
	content := provider.CleanOrigin(origin)
	if provider.IsIPAddress(content) {
		recordType = "A"
	}

	// 先查询是否已有同名记录（用全限定域名精确查询）
	listURL := fmt.Sprintf("%s/zones/%s/dns_records?name=%s", cfAPIBase, zoneID, recordFQDN)
	body, _ := c.doRequest(ctx, cfg, "GET", listURL, nil)
	var listResp struct {
		Success bool `json:"success"`
		Result  []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"result"`
	}
	existingID := ""
	if body != nil {
		if err := json.Unmarshal(body, &listResp); err == nil && len(listResp.Result) > 0 {
			existingID = listResp.Result[0].ID
		}
	}

	payload := map[string]interface{}{
		"type":    recordType,
		"name":    recordFQDN,
		"content": content,
		"proxied": true,
		"ttl":     1,
	}

	var resp2 []byte
	var err error
	if existingID != "" {
		// 已存在则更新
		updateURL := fmt.Sprintf("%s/zones/%s/dns_records/%s", cfAPIBase, zoneID, existingID)
		resp2, err = c.doRequest(ctx, cfg, "PUT", updateURL, payload)
	} else {
		// 不存在则新建
		createURL := fmt.Sprintf("%s/zones/%s/dns_records", cfAPIBase, zoneID)
		resp2, err = c.doRequest(ctx, cfg, "POST", createURL, payload)
	}
	if err != nil {
		return err
	}

	var createResp struct {
		Success bool      `json:"success"`
		Errors  []cfError `json:"errors"`
	}
	if err := json.Unmarshal(resp2, &createResp); err != nil {
		return fmt.Errorf("解析响应失败: %w", err)
	}
	if !createResp.Success {
		return cfErrorMsg(createResp.Errors)
	}
	log.Printf("[Cloudflare] 回退源记录已同步: %s %s → %s", recordType, recordFQDN, content)
	return nil
}

// getZoneFallbackCname 获取 Zone 的域名作为 CNAME 目标
func (c *CloudflareProvider) getZoneFallbackCname(ctx context.Context, cfg map[string]string, zoneID string) (string, error) {
	url := fmt.Sprintf("%s/zones/%s", cfAPIBase, zoneID)
	body, err := c.doRequest(ctx, cfg, "GET", url, nil)
	if err != nil {
		return "", err
	}

	var resp struct {
		Success bool `json:"success"`
		Result  struct {
			Name string `json:"name"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", err
	}
	if resp.Success {
		return resp.Result.Name, nil
	}
	return "", fmt.Errorf("获取Zone信息失败")
}

// getDomainStatusSaaS 查询 Custom Hostname 状态
func (c *CloudflareProvider) getDomainStatusSaaS(ctx context.Context, cfg map[string]string, hostnameID string) (*model.DomainStatusResult, error) {
	zoneID := cfg["zone_id"]

	url := fmt.Sprintf("%s/zones/%s/custom_hostnames/%s", cfAPIBase, zoneID, hostnameID)
	body, err := c.doRequest(ctx, cfg, "GET", url, nil)
	if err != nil {
		return nil, err
	}

	var resp struct {
		Success bool `json:"success"`
		Result  struct {
			Status string `json:"status"`
			SSL    struct {
				Status string `json:"status"`
			} `json:"ssl"`
		} `json:"result"`
		Errors []cfError `json:"errors"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("解析响应失败: %w", err)
	}
	if !resp.Success {
		return nil, cfErrorMsg(resp.Errors)
	}

	status := "pending"
	switch resp.Result.Status {
	case "active":
		status = "active"
	case "pending", "moved", "deleted":
		status = resp.Result.Status
	}

	return &model.DomainStatusResult{
		Status:  status,
		Message: fmt.Sprintf("Custom Hostname 状态: %s, SSL: %s", resp.Result.Status, resp.Result.SSL.Status),
	}, nil
}

// deleteDomainSaaS 删除 Custom Hostname 及对应的 Origin Rule
func (c *CloudflareProvider) deleteDomainSaaS(ctx context.Context, cfg map[string]string, domain, hostnameID string) error {
	zoneID := cfg["zone_id"]

	// 移除 Origin Rule（非致命）
	if zoneID != "" && domain != "" {
		if err := c.removeOriginRule(ctx, cfg, zoneID, domain); err != nil {
			log.Printf("[Cloudflare] 移除 Origin Rule 失败 [%s]: %v", domain, err)
		}
	}

	url := fmt.Sprintf("%s/zones/%s/custom_hostnames/%s", cfAPIBase, zoneID, hostnameID)
	body, err := c.doRequest(ctx, cfg, "DELETE", url, nil)
	if err != nil {
		return err
	}

	var resp struct {
		Success bool      `json:"success"`
		Errors  []cfError `json:"errors"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return fmt.Errorf("解析响应失败: %w", err)
	}
	if !resp.Success {
		return cfErrorMsg(resp.Errors)
	}
	return nil
}

// setupCertSaaS SaaS模式下证书由 Custom Hostname 创建时自动管理，此处查询并返回状态
func (c *CloudflareProvider) setupCertSaaS(ctx context.Context, cfg map[string]string, hostnameID string) (*model.CertRequestResult, error) {
	zoneID := cfg["zone_id"]

	url := fmt.Sprintf("%s/zones/%s/custom_hostnames/%s", cfAPIBase, zoneID, hostnameID)
	body, err := c.doRequest(ctx, cfg, "GET", url, nil)
	if err != nil {
		return nil, err
	}

	var resp struct {
		Success bool `json:"success"`
		Result  struct {
			ID  string `json:"id"`
			SSL struct {
				Status            string `json:"status"`
				ValidationRecords []struct {
					TxtName     string `json:"txt_name"`
					TxtValue    string `json:"txt_value"`
					CnameName   string `json:"cname"`
					CnameTarget string `json:"cname_target"`
				} `json:"validation_records"`
			} `json:"ssl"`
		} `json:"result"`
		Errors []cfError `json:"errors"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("解析响应失败: %w", err)
	}
	if !resp.Success {
		return nil, cfErrorMsg(resp.Errors)
	}

	certStatus := "deploying"
	switch resp.Result.SSL.Status {
	case "active":
		certStatus = "active"
	case "expired":
		certStatus = "expired"
	case "":
		certStatus = "none"
	}

	// 收集 DCV 验证记录
	var dnsRecords []model.CertDNSRecord
	for _, vr := range resp.Result.SSL.ValidationRecords {
		if vr.TxtName != "" && vr.TxtValue != "" {
			dnsRecords = append(dnsRecords, model.CertDNSRecord{
				Type:  "TXT",
				Name:  vr.TxtName,
				Value: vr.TxtValue,
			})
		}
		if vr.CnameName != "" && vr.CnameTarget != "" {
			dnsRecords = append(dnsRecords, model.CertDNSRecord{
				Type:  "CNAME",
				Name:  vr.CnameName,
				Value: vr.CnameTarget,
			})
		}
	}

	return &model.CertRequestResult{
		CertID:     hostnameID,
		Status:     certStatus,
		DNSRecords: dnsRecords,
	}, nil
}

// getCertStatusSaaS 查询 Custom Hostname 的 SSL 证书状态
func (c *CloudflareProvider) getCertStatusSaaS(ctx context.Context, cfg map[string]string, hostnameID string) (*model.CertStatusResult, error) {
	zoneID := cfg["zone_id"]

	url := fmt.Sprintf("%s/zones/%s/custom_hostnames/%s", cfAPIBase, zoneID, hostnameID)
	body, err := c.doRequest(ctx, cfg, "GET", url, nil)
	if err != nil {
		return nil, err
	}

	var resp struct {
		Success bool `json:"success"`
		Result  struct {
			SSL struct {
				Status string `json:"status"`
			} `json:"ssl"`
		} `json:"result"`
		Errors []cfError `json:"errors"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("解析响应失败: %w", err)
	}
	if !resp.Success {
		return nil, cfErrorMsg(resp.Errors)
	}

	sslStatus := resp.Result.SSL.Status
	status := "none"
	msg := "未配置"
	switch sslStatus {
	case "active":
		status = "active"
		msg = "证书已部署"
	case "pending_validation":
		status = "deploying"
		msg = "等待DCV域名验证"
	case "pending_issuance":
		status = "deploying"
		msg = "证书签发中"
	case "pending_deployment":
		status = "deploying"
		msg = "证书部署中"
	case "initializing":
		status = "deploying"
		msg = "证书初始化中"
	case "expired":
		status = "expired"
		msg = "证书已过期"
	default:
		msg = fmt.Sprintf("SSL 状态: %s", sslStatus)
	}

	return &model.CertStatusResult{
		Status:  status,
		Message: msg,
	}, nil
}

// =============================================================================
// NS 模式 — 传统 NS 接入
// =============================================================================

// addDomainNS 传统方式：创建 Zone + 添加源站 DNS 记录
func (c *CloudflareProvider) addDomainNS(ctx context.Context, cfg map[string]string, domain string, originCfg model.OriginConfig) (*model.AddDomainResult, error) {
	// 1. 查询是否已存在同名 Zone
	zoneID, cname, ns, err := c.findZone(ctx, cfg, domain)
	if err != nil {
		return nil, fmt.Errorf("查询Zone失败: %w", err)
	}

	// 2. 不存在则创建 Zone
	if zoneID == "" {
		zoneID, cname, ns, err = c.createZone(ctx, cfg, domain)
		if err != nil {
			return nil, fmt.Errorf("创建Zone失败: %w", err)
		}
	}

	// 3. 添加源站 DNS 记录
	if err := c.ensureOriginRecord(ctx, cfg, zoneID, domain, originCfg.Origin); err != nil {
		return nil, fmt.Errorf("添加源站DNS记录失败: %w", err)
	}

	// 4. 设置 SSL 模式为 Full
	_ = c.setSSLMode(ctx, cfg, zoneID, "full")

	return &model.AddDomainResult{
		ProviderSiteID: zoneID,
		CNAME:          cname,
		NameServers:    ns,
		Status:         "pending",
	}, nil
}

// getDomainStatusNS 查询 Zone 激活状态
func (c *CloudflareProvider) getDomainStatusNS(ctx context.Context, cfg map[string]string, zoneID string) (*model.DomainStatusResult, error) {
	if zoneID == "" {
		return &model.DomainStatusResult{Status: "pending", Message: "等待部署完成"}, nil
	}
	url := fmt.Sprintf("%s/zones/%s", cfAPIBase, zoneID)

	body, err := c.doRequest(ctx, cfg, "GET", url, nil)
	if err != nil {
		return nil, err
	}

	var resp struct {
		Success bool `json:"success"`
		Result  struct {
			Status string `json:"status"`
		} `json:"result"`
		Errors []cfError `json:"errors"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("解析响应失败: %w", err)
	}
	if !resp.Success {
		return nil, cfErrorMsg(resp.Errors)
	}

	status := "pending"
	if resp.Result.Status == "active" {
		status = "active"
	}

	return &model.DomainStatusResult{
		Status:  status,
		Message: fmt.Sprintf("Cloudflare Zone 状态: %s", resp.Result.Status),
	}, nil
}

// deleteDomainNS 删除 Cloudflare Zone
func (c *CloudflareProvider) deleteDomainNS(ctx context.Context, cfg map[string]string, zoneID string) error {
	url := fmt.Sprintf("%s/zones/%s", cfAPIBase, zoneID)

	body, err := c.doRequest(ctx, cfg, "DELETE", url, nil)
	if err != nil {
		return err
	}

	var resp struct {
		Success bool      `json:"success"`
		Errors  []cfError `json:"errors"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return fmt.Errorf("解析响应失败: %w", err)
	}
	if !resp.Success {
		return cfErrorMsg(resp.Errors)
	}
	return nil
}

// setupCertNS NS模式下开启 Universal SSL
func (c *CloudflareProvider) setupCertNS(ctx context.Context, cfg map[string]string, zoneID string) (*model.CertRequestResult, error) {
	url := fmt.Sprintf("%s/zones/%s/ssl/universal/settings", cfAPIBase, zoneID)
	payload := map[string]interface{}{"enabled": true}

	body, err := c.doRequest(ctx, cfg, "PATCH", url, payload)
	if err != nil {
		return nil, fmt.Errorf("开启Universal SSL失败: %w", err)
	}

	var resp struct {
		Success bool      `json:"success"`
		Errors  []cfError `json:"errors"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("解析响应失败: %w", err)
	}
	if !resp.Success {
		return nil, cfErrorMsg(resp.Errors)
	}

	return &model.CertRequestResult{
		CertID: zoneID,
		Status: "deploying",
	}, nil
}

// getCertStatusNS 查询 Universal SSL 证书状态
func (c *CloudflareProvider) getCertStatusNS(ctx context.Context, cfg map[string]string, zoneID string) (*model.CertStatusResult, error) {
	url := fmt.Sprintf("%s/zones/%s/ssl/verification", cfAPIBase, zoneID)

	body, err := c.doRequest(ctx, cfg, "GET", url, nil)
	if err != nil {
		return nil, err
	}

	var resp struct {
		Success bool `json:"success"`
		Result  []struct {
			CertificateStatus string `json:"certificate_status"`
		} `json:"result"`
		Errors []cfError `json:"errors"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("解析响应失败: %w", err)
	}

	status := "none"
	msg := "未配置"
	if resp.Success && len(resp.Result) > 0 {
		certStatus := resp.Result[0].CertificateStatus
		switch certStatus {
		case "active":
			status = "active"
			msg = "证书已部署"
		case "pending_validation":
			status = "deploying"
			msg = "等待域名验证"
		case "pending_issuance":
			status = "deploying"
			msg = "证书签发中"
		case "expired":
			status = "expired"
			msg = "证书已过期"
		default:
			msg = fmt.Sprintf("证书状态: %s", certStatus)
		}
	}

	return &model.CertStatusResult{
		Status:  status,
		Message: msg,
	}, nil
}

// =============================================================================
// NS 模式内部方法
// =============================================================================

func (c *CloudflareProvider) findZone(ctx context.Context, cfg map[string]string, domain string) (zoneID, cname string, ns []string, err error) {
	rootDomain := provider.ExtractRootDomain(domain)
	url := fmt.Sprintf("%s/zones?name=%s&status=active,pending", cfAPIBase, rootDomain)

	body, err := c.doRequest(ctx, cfg, "GET", url, nil)
	if err != nil {
		return "", "", nil, err
	}

	var resp struct {
		Success bool `json:"success"`
		Result  []struct {
			ID          string   `json:"id"`
			Name        string   `json:"name"`
			NameServers []string `json:"name_servers"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", "", nil, err
	}
	if !resp.Success || len(resp.Result) == 0 {
		return "", "", nil, nil
	}

	zone := resp.Result[0]
	zoneCname := fmt.Sprintf("%s.cdn.cloudflare.net", domain)
	return zone.ID, zoneCname, zone.NameServers, nil
}

func (c *CloudflareProvider) createZone(ctx context.Context, cfg map[string]string, domain string) (zoneID, cname string, ns []string, err error) {
	rootDomain := provider.ExtractRootDomain(domain)
	payload := map[string]interface{}{
		"name": rootDomain,
		"type": "full",
	}

	body, err := c.doRequest(ctx, cfg, "POST", cfAPIBase+"/zones", payload)
	if err != nil {
		return "", "", nil, err
	}

	var resp struct {
		Success bool `json:"success"`
		Result  struct {
			ID          string   `json:"id"`
			NameServers []string `json:"name_servers"`
		} `json:"result"`
		Errors []cfError `json:"errors"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", "", nil, err
	}
	if !resp.Success {
		return "", "", nil, cfErrorMsg(resp.Errors)
	}

	zoneCname := fmt.Sprintf("%s.cdn.cloudflare.net", domain)
	return resp.Result.ID, zoneCname, resp.Result.NameServers, nil
}

func (c *CloudflareProvider) ensureOriginRecord(ctx context.Context, cfg map[string]string, zoneID, domain, origin string) error {
	recordType := "CNAME"
	content := provider.CleanOrigin(origin)
	if provider.IsIPAddress(content) {
		recordType = "A"
	}

	rootDomain := provider.ExtractRootDomain(domain)
	recordName := domain
	if domain == rootDomain {
		recordName = "@"
	}

	payload := map[string]interface{}{
		"type":    recordType,
		"name":    recordName,
		"content": content,
		"proxied": true,
		"ttl":     1,
	}

	url := fmt.Sprintf("%s/zones/%s/dns_records", cfAPIBase, zoneID)
	body, err := c.doRequest(ctx, cfg, "POST", url, payload)
	if err != nil {
		return err
	}

	var resp struct {
		Success bool      `json:"success"`
		Errors  []cfError `json:"errors"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return err
	}
	if !resp.Success {
		for _, e := range resp.Errors {
			// 81057: 同主机已有 A/AAAA/CNAME 记录；81058: 完全相同的记录已存在 — 均视为无需处理
			if e.Code == 81057 || e.Code == 81058 {
				return nil
			}
		}
		return cfErrorMsg(resp.Errors)
	}
	return nil
}

func (c *CloudflareProvider) setSSLMode(ctx context.Context, cfg map[string]string, zoneID, mode string) error {
	url := fmt.Sprintf("%s/zones/%s/settings/ssl", cfAPIBase, zoneID)
	payload := map[string]interface{}{"value": mode}

	body, err := c.doRequest(ctx, cfg, "PATCH", url, payload)
	if err != nil {
		return err
	}

	var resp struct {
		Success bool `json:"success"`
	}
	json.Unmarshal(body, &resp)
	return nil
}

// =============================================================================
// 通用方法
// =============================================================================

func (c *CloudflareProvider) doRequest(ctx context.Context, cfg map[string]string, method, url string, payload interface{}) ([]byte, error) {
	var bodyReader io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("序列化请求失败: %w", err)
		}
		bodyReader = bytes.NewReader(data)
	}

	req, err := http.NewRequestWithContext(ctx, method, url, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("构建请求失败: %w", err)
	}

	// 根据认证方式设置 header
	if cfg["auth_type"] == "global_key" {
		req.Header.Set("X-Auth-Key", cfg["global_api_key"])
		req.Header.Set("X-Auth-Email", cfg["email"])
	} else {
		req.Header.Set("Authorization", "Bearer "+cfg["api_token"])
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求Cloudflare API失败: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("读取响应失败: %w", err)
	}
	log.Printf("[Cloudflare] %s %s → %d", method, url, resp.StatusCode)
	return body, nil
}

// =============================================================================
// 工具函数和类型
// =============================================================================

type cfError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func cfErrorMsg(errors []cfError) error {
	if len(errors) == 0 {
		return fmt.Errorf("Cloudflare API 返回未知错误")
	}
	msgs := make([]string, len(errors))
	for i, e := range errors {
		msgs[i] = fmt.Sprintf("[%d] %s", e.Code, e.Message)
	}
	return fmt.Errorf("Cloudflare API 错误: %s", strings.Join(msgs, "; "))
}
