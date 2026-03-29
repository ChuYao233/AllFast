package aliyun

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
	"net/url"
	"sort"
	"strings"
	"time"
)

func init() {
	provider.Register(&AliyunESAProvider{})
}

const (
	aliyunESAEndpoint = "https://esa.cn-hangzhou.aliyuncs.com"
	aliyunESAVersion  = "2024-09-10"
)

// AliyunESAProvider 阿里云ESA边缘安全加速 真实API集成
type AliyunESAProvider struct{}

func (a *AliyunESAProvider) Name() string { return "aliyun" }

func (a *AliyunESAProvider) Info() model.ProviderInfo {
	return model.ProviderInfo{
		Name:        "aliyun",
		DisplayName: "阿里云",
		Description: "阿里云边缘安全加速平台，提供全球CDN加速与安全防护",
		ConfigFields: []model.ConfigField{
			{Key: "access_key_id", Label: "AccessKey ID", Type: "text", Required: true, Secret: false, Placeholder: "阿里云 AccessKey ID"},
			{Key: "access_key_secret", Label: "AccessKey Secret", Type: "secret", Required: true, Secret: true, Placeholder: "阿里云 AccessKey Secret"},
		},
		DeployFields: []model.ConfigField{
			{Key: "instance_id", Label: "套餐实例", Type: "select", Required: true, Secret: false, Fetchable: true, Placeholder: "选择套餐实例"},
		},
	}
}

// FetchFieldOptions 动态加载字段选项
func (a *AliyunESAProvider) FetchFieldOptions(ctx context.Context, cfg map[string]string, fieldKey string) ([]model.SelectOption, error) {
	if fieldKey != "instance_id" {
		return nil, nil
	}
	if cfg["access_key_id"] == "" || cfg["access_key_secret"] == "" {
		return nil, fmt.Errorf("请先填写 AccessKey ID 和 AccessKey Secret")
	}

	params := map[string]string{
		"Action": "ListUserRatePlanInstances",
	}

	body, err := a.doRequest(ctx, cfg, "GET", params, nil)
	if err != nil {
		return nil, fmt.Errorf("获取套餐实例列表失败: %w", err)
	}

	log.Printf("[AliyunESA] ListUserRatePlanInstances 原始响应: %s", string(body))

	var resp struct {
		InstanceInfo []struct {
			InstanceId string `json:"InstanceId"`
			PlanName   string `json:"PlanName"`
			Status     string `json:"Status"`
			SiteQuota  string `json:"SiteQuota"`
			Sites      []struct {
				SiteName string `json:"SiteName"`
			} `json:"Sites"`
		} `json:"InstanceInfo"`
		Code    string `json:"Code"`
		Message string `json:"Message"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("解析响应失败: %w", err)
	}
	if resp.Code != "" {
		return nil, fmt.Errorf("阿里云API错误: [%s] %s", resp.Code, resp.Message)
	}

	options := make([]model.SelectOption, 0, len(resp.InstanceInfo))
	for _, inst := range resp.InstanceInfo {
		var label string
		if len(inst.Sites) == 0 {
			label = fmt.Sprintf("未使用（%s）", inst.InstanceId)
		} else {
			names := make([]string, 0, len(inst.Sites))
			for _, s := range inst.Sites {
				names = append(names, s.SiteName)
			}
			label = fmt.Sprintf("%s（%s）", strings.Join(names, "，"), inst.InstanceId)
		}
		desc := fmt.Sprintf("%s  ·  已用 %d / %s 个站点", inst.PlanName, len(inst.Sites), inst.SiteQuota)
		options = append(options, model.SelectOption{Value: inst.InstanceId, Label: label, Desc: desc})
	}

	if len(options) == 0 {
		return nil, fmt.Errorf("当前账号下没有可用的ESA套餐实例")
	}

	return options, nil
}

func (a *AliyunESAProvider) ValidateConfig(cfg map[string]string) error {
	if cfg["access_key_id"] == "" {
		return fmt.Errorf("缺少 access_key_id 配置")
	}
	if cfg["access_key_secret"] == "" {
		return fmt.Errorf("缺少 access_key_secret 配置")
	}
	return nil
}

// getAccessType 阿里云ESA只支持CNAME接入
func (a *AliyunESAProvider) getAccessType(cfg map[string]string) string {
	return "CNAME"
}

// AddDomain 通过阿里云ESA API部署域名
// 流程: 检查套餐绑定→创建/复用站点→创建记录→ListRecords获取真实CNAME→回源规则
func (a *AliyunESAProvider) AddDomain(ctx context.Context, cfg map[string]string, domain string, originCfg model.OriginConfig) (*model.AddDomainResult, error) {
	if err := a.ValidateConfig(cfg); err != nil {
		return nil, err
	}

	rootDomain := provider.ExtractRootDomain(domain)
	instanceId := cfg["instance_id"]

	// 1. 检查套餐实例是否已绑定根域名
	siteID, err := a.getInstanceSiteID(ctx, cfg, instanceId, rootDomain)
	if err != nil {
		// 未绑定，创建站点（绑定根域名到套餐）
		log.Printf("[AliyunESA] 套餐未绑定 %s，正在创建站点...", rootDomain)
		siteID, _, _, err = a.createSite(ctx, cfg, rootDomain)
		if err != nil {
			return nil, fmt.Errorf("创建ESA站点失败: %w", err)
		}
	} else {
		log.Printf("[AliyunESA] 套餐已绑定 %s (SiteID=%d)，复用已有站点", rootDomain, siteID)
	}

	// 2. 添加或更新加速记录
	originHost := strings.Split(originCfg.Origin, ":")[0]
	if err := a.batchCreateRecord(ctx, cfg, siteID, domain, originCfg.Origin); err != nil {
		if strings.Contains(err.Error(), "Conflict") || strings.Contains(err.Error(), "already exist") || strings.Contains(err.Error(), "AlreadyExist") {
			// 记录已存在，检查配置是否正确，不正确则更新
			log.Printf("[AliyunESA] 记录 %s 已存在，检查配置...", domain)
			if err := a.ensureRecordCorrect(ctx, cfg, siteID, domain, originHost); err != nil {
				log.Printf("[AliyunESA] 更新记录失败(非致命): %v", err)
			}
		} else {
			return nil, fmt.Errorf("添加加速记录失败: %w", err)
		}
	}

	// 3. 通过 ListRecords 获取真实 CNAME
	cname, err := a.getRecordCname(ctx, cfg, siteID, domain)
	if err != nil {
		log.Printf("[AliyunESA] 获取记录CNAME失败: %v", err)
	}

	// 4. 创建回源规则（配置回源协议、端口、HOST）
	if err := a.createOriginRule(ctx, cfg, siteID, domain, originCfg); err != nil {
		log.Printf("[AliyunESA] 创建回源规则失败(非致命): %v", err)
	}

	return &model.AddDomainResult{
		ProviderSiteID: fmt.Sprintf("%d", siteID),
		CNAME:          cname,
		Status:         "pending",
	}, nil
}

// getInstanceSiteID 检查套餐实例是否已绑定指定根域名，返回 siteID
func (a *AliyunESAProvider) getInstanceSiteID(ctx context.Context, cfg map[string]string, instanceId, rootDomain string) (int64, error) {
	params := map[string]string{
		"Action": "ListUserRatePlanInstances",
	}
	body, err := a.doRequest(ctx, cfg, "GET", params, nil)
	if err != nil {
		return 0, err
	}

	var resp struct {
		InstanceInfo []struct {
			InstanceId string `json:"InstanceId"`
			SiteQuota  string `json:"SiteQuota"`
			Sites      []struct {
				SiteId   int64  `json:"SiteId"`
				SiteName string `json:"SiteName"`
			} `json:"Sites"`
		} `json:"InstanceInfo"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return 0, fmt.Errorf("解析响应失败: %w", err)
	}

	for _, inst := range resp.InstanceInfo {
		if inst.InstanceId == instanceId {
			for _, site := range inst.Sites {
				if site.SiteName == rootDomain {
					return site.SiteId, nil
				}
			}
			return 0, fmt.Errorf("套餐 %s 未绑定域名 %s", instanceId, rootDomain)
		}
	}
	return 0, fmt.Errorf("未找到套餐实例 %s", instanceId)
}

// getRecordCname 通过 ListRecords API 获取记录的真实 CNAME（RecordCname 字段）
func (a *AliyunESAProvider) getRecordCname(ctx context.Context, cfg map[string]string, siteID int64, recordName string) (string, error) {
	params := map[string]string{
		"Action":     "ListRecords",
		"SiteId":     fmt.Sprintf("%d", siteID),
		"RecordName": recordName,
		"PageSize":   "10",
		"PageNumber": "1",
	}

	body, err := a.doRequest(ctx, cfg, "GET", params, nil)
	if err != nil {
		return "", err
	}

	var resp struct {
		Records []struct {
			RecordName  string `json:"RecordName"`
			RecordCname string `json:"RecordCname"`
		} `json:"Records"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", fmt.Errorf("解析ListRecords响应失败: %w", err)
	}

	for _, r := range resp.Records {
		if r.RecordName == recordName && r.RecordCname != "" {
			return r.RecordCname, nil
		}
	}

	return "", fmt.Errorf("未找到记录 %s 的CNAME", recordName)
}

// GetDomainStatus 查询站点状态
func (a *AliyunESAProvider) GetDomainStatus(ctx context.Context, cfg map[string]string, domain string, providerSiteID string) (*model.DomainStatusResult, error) {
	params := map[string]string{
		"Action": "GetSite",
		"SiteId": providerSiteID,
	}

	body, err := a.doRequest(ctx, cfg, "GET", params, nil)
	if err != nil {
		return nil, err
	}

	var resp struct {
		SiteModel struct {
			Status string `json:"Status"`
		} `json:"SiteModel"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("解析响应失败: %w", err)
	}

	status := "pending"
	if resp.SiteModel.Status == "active" || resp.SiteModel.Status == "online" {
		status = "active"
	}

	return &model.DomainStatusResult{
		Status:  status,
		Message: fmt.Sprintf("阿里云ESA站点状态: %s", resp.SiteModel.Status),
	}, nil
}

// DeleteDomain 从ESA站点删除对应域名的记录和回源规则（不删站点本身）
func (a *AliyunESAProvider) DeleteDomain(ctx context.Context, cfg map[string]string, domain string, providerSiteID string) error {
	siteID := providerSiteID

	// 1. 查找并删除对应的DNS记录
	listParams := map[string]string{
		"Action":     "ListRecords",
		"SiteId":     siteID,
		"RecordName": domain,
		"PageSize":   "50",
		"PageNumber": "1",
	}
	body, err := a.doRequest(ctx, cfg, "GET", listParams, nil)
	if err != nil {
		return fmt.Errorf("查询记录失败: %w", err)
	}

	var listResp struct {
		Records []struct {
			RecordId   int64  `json:"RecordId"`
			RecordName string `json:"RecordName"`
		} `json:"Records"`
	}
	if err := json.Unmarshal(body, &listResp); err != nil {
		return fmt.Errorf("解析ListRecords响应失败: %w", err)
	}

	for _, r := range listResp.Records {
		if r.RecordName == domain {
			delParams := map[string]string{
				"Action":   "DeleteRecord",
				"RecordId": fmt.Sprintf("%d", r.RecordId),
			}
			delBody, err := a.doRequest(ctx, cfg, "POST", delParams, nil)
			if err != nil {
				log.Printf("[AliyunESA] 删除记录 %d 失败: %v", r.RecordId, err)
				continue
			}
			if err := a.checkResponse(delBody); err != nil {
				log.Printf("[AliyunESA] 删除记录 %d 失败: %v", r.RecordId, err)
			} else {
				log.Printf("[AliyunESA] 已删除记录 %s (RecordId=%d)", domain, r.RecordId)
			}
		}
	}

	// 2. 查找并删除对应的回源规则
	ruleName := fmt.Sprintf("origin-%s", domain)
	ruleParams := map[string]string{
		"Action": "ListOriginRules",
		"SiteId": siteID,
	}
	ruleBody, err := a.doRequest(ctx, cfg, "GET", ruleParams, nil)
	if err != nil {
		return fmt.Errorf("查询回源规则失败: %w", err)
	}

	var ruleResp struct {
		Configs []struct {
			ConfigId int64  `json:"ConfigId"`
			RuleName string `json:"RuleName"`
		} `json:"Configs"`
	}
	if err := json.Unmarshal(ruleBody, &ruleResp); err != nil {
		return fmt.Errorf("解析ListOriginRules响应失败: %w", err)
	}

	for _, c := range ruleResp.Configs {
		if c.RuleName == ruleName {
			delRuleBody := map[string]interface{}{
				"SiteId":   siteID,
				"ConfigId": c.ConfigId,
			}
			delRuleParams := map[string]string{
				"Action": "DeleteOriginRule",
			}
			resp, err := a.doRequest(ctx, cfg, "POST", delRuleParams, delRuleBody)
			if err != nil {
				log.Printf("[AliyunESA] 删除回源规则 %s 失败: %v", ruleName, err)
			} else if err := a.checkResponse(resp); err != nil {
				log.Printf("[AliyunESA] 删除回源规则 %s 失败: %v", ruleName, err)
			} else {
				log.Printf("[AliyunESA] 已删除回源规则 %s (ConfigId=%d)", ruleName, c.ConfigId)
			}
		}
	}

	return nil
}

// UpdateOriginConfig 更新阿里云 ESA 回源配置
func (a *AliyunESAProvider) UpdateOriginConfig(ctx context.Context, cfg map[string]string, domain string, providerSiteID string, originCfg model.OriginConfig) error {
	siteID := providerSiteID
	if siteID == "" {
		id, err := a.findSiteIDByDomain(ctx, cfg, domain)
		if err != nil {
			return fmt.Errorf("未找到站点: %w", err)
		}
		siteID = fmt.Sprintf("%d", id)
	}
	var sid int64
	fmt.Sscanf(siteID, "%d", &sid)
	return a.ensureOriginRuleCorrect(ctx, cfg, sid, domain, originCfg)
}

// EnableEdgeCert 启用阿里云 ESA 免费证书
func (a *AliyunESAProvider) EnableEdgeCert(ctx context.Context, cfg map[string]string, domain string, providerSiteID string) error {
	siteID := providerSiteID
	if siteID == "" {
		id, err := a.findSiteIDByDomain(ctx, cfg, domain)
		if err != nil {
			return fmt.Errorf("未找到站点: %w", err)
		}
		siteID = fmt.Sprintf("%d", id)
	}

	reqBody := map[string]interface{}{
		"SiteId": siteID,
		"Type":   "free",
	}
	params := map[string]string{
		"Action": "SetCertificate",
	}

	body, err := a.doRequest(ctx, cfg, "POST", params, reqBody)
	if err != nil {
		return fmt.Errorf("启用免费证书失败: %w", err)
	}
	if err := a.checkResponse(body); err != nil {
		return fmt.Errorf("启用免费证书失败: %w", err)
	}

	log.Printf("[AliyunESA] 已启用站点 %s 的免费证书 (域名: %s)", siteID, domain)
	return nil
}

// DeployCertificate 将自定义证书部署到阿里云 ESA（使用 SetCertificate API）
func (a *AliyunESAProvider) DeployCertificate(ctx context.Context, cfg map[string]string, domain string, providerSiteID string, certPEM string, keyPEM string) error {
	siteID := providerSiteID
	if siteID == "" {
		id, err := a.findSiteIDByDomain(ctx, cfg, domain)
		if err != nil {
			return fmt.Errorf("未找到站点: %w", err)
		}
		siteID = fmt.Sprintf("%d", id)
	}

	reqBody := map[string]interface{}{
		"SiteId":      siteID,
		"Type":        "upload",
		"Certificate": certPEM,
		"PrivateKey":  keyPEM,
	}
	params := map[string]string{
		"Action": "SetCertificate",
	}

	body, err := a.doRequest(ctx, cfg, "POST", params, reqBody)
	if err != nil {
		return fmt.Errorf("部署证书请求失败: %w", err)
	}
	if err := a.checkResponse(body); err != nil {
		return fmt.Errorf("部署证书失败: %w", err)
	}

	log.Printf("[AliyunESA] 证书已部署到站点 %s (域名: %s)", siteID, domain)
	return nil
}

// SetupCertificate 不自动申请证书，只查询当前状态
func (a *AliyunESAProvider) SetupCertificate(ctx context.Context, cfg map[string]string, domain string, providerSiteID string) (*model.CertRequestResult, error) {
	status := a.queryCertByListCertificates(ctx, cfg, providerSiteID, domain)
	return &model.CertRequestResult{
		Status: status,
	}, nil
}

// GetCertificateStatus 通过 ListCertificates API 查询站点证书列表，匹配域名
func (a *AliyunESAProvider) GetCertificateStatus(ctx context.Context, cfg map[string]string, domain string, certID string) (*model.CertStatusResult, error) {
	siteID, err := a.findSiteIDByDomain(ctx, cfg, domain)
	if err != nil {
		return &model.CertStatusResult{Status: "pending", Message: err.Error()}, nil
	}

	status := a.queryCertByListCertificates(ctx, cfg, fmt.Sprintf("%d", siteID), domain)
	return &model.CertStatusResult{
		Status:  status,
		Message: fmt.Sprintf("证书状态: %s", status),
	}, nil
}

// queryCertByListCertificates 通过 ListCertificates API 查询站点证书列表
// 匹配 CommonName 或 SAN 包含目标域名
func (a *AliyunESAProvider) queryCertByListCertificates(ctx context.Context, cfg map[string]string, providerSiteID string, domain string) string {
	params := map[string]string{
		"Action":    "ListCertificates",
		"SiteId":    providerSiteID,
		"ValidOnly": "true",
		"PageSize":  "50",
	}

	body, err := a.doRequest(ctx, cfg, "GET", params, nil)
	if err != nil {
		log.Printf("[AliyunESA] ListCertificates 请求失败: %v", err)
		return "none"
	}

	var resp struct {
		Result []struct {
			CommonName string `json:"CommonName"`
			SAN        string `json:"SAN"`
			Status     string `json:"Status"`
			Type       string `json:"Type"`
			NotAfter   string `json:"NotAfter"`
		} `json:"Result"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		log.Printf("[AliyunESA] 解析ListCertificates响应失败: %v", err)
		return "none"
	}

	rootDomain := provider.ExtractRootDomain(domain)
	for _, cert := range resp.Result {
		// 匹配 CommonName 或 SAN 中包含该域名
		matched := false
		if cert.CommonName == domain {
			matched = true
		}
		// 泛域名匹配: *.example.com 匹配 sub.example.com
		if cert.CommonName == "*."+rootDomain {
			matched = true
		}
		// SAN 字段是逗号分隔的域名列表
		if cert.SAN != "" {
			for _, san := range strings.Split(cert.SAN, ",") {
				san = strings.TrimSpace(san)
				if san == domain || san == "*."+rootDomain {
					matched = true
					break
				}
			}
		}

		if matched {
			switch cert.Status {
			case "OK", "Issued", "Expiring":
				return "active"
			case "Applying":
				return "deploying"
			case "Expired":
				return "expired"
			case "ApplyFailed", "Canceled":
				return "none"
			default:
				return "deploying"
			}
		}
	}

	return "none"
}

// findSiteIDByDomain 通过域名查找站点ID
func (a *AliyunESAProvider) findSiteIDByDomain(ctx context.Context, cfg map[string]string, domain string) (int64, error) {
	rootDomain := provider.ExtractRootDomain(domain)
	return a.findSiteID(ctx, cfg, rootDomain)
}

// ===== 阿里云ESA 内部方法 =====

func (a *AliyunESAProvider) createSite(ctx context.Context, cfg map[string]string, siteName string) (siteID int64, verifyCode string, nsList string, err error) {
	accessType := a.getAccessType(cfg)
	instanceId := cfg["instance_id"]

	params := map[string]string{
		"Action": "CreateSite",
	}

	reqBody := map[string]interface{}{
		"SiteName":   siteName,
		"Coverage":   "global",
		"AccessType": accessType,
		"InstanceId": instanceId,
	}

	body, err := a.doRequest(ctx, cfg, "POST", params, reqBody)
	if err != nil {
		return 0, "", "", err
	}

	var resp struct {
		SiteId         int64  `json:"SiteId"`
		NameServerList string `json:"NameServerList"`
		VerifyCode     string `json:"VerifyCode"`
		RequestId      string `json:"RequestId"`
		Code           string `json:"Code"`
		Message        string `json:"Message"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return 0, "", "", fmt.Errorf("解析响应失败: %w", err)
	}

	// 站点已存在时，查询已有站点ID
	if resp.Code == "SiteAlreadyExist" || resp.Code == "Site.AlreadyExist" {
		existID, findErr := a.findSiteID(ctx, cfg, siteName)
		return existID, "", "", findErr
	}

	if resp.Code != "" && resp.SiteId == 0 {
		return 0, "", "", fmt.Errorf("阿里云ESA API错误: [%s] %s", resp.Code, resp.Message)
	}

	return resp.SiteId, resp.VerifyCode, resp.NameServerList, nil
}

func (a *AliyunESAProvider) findSiteID(ctx context.Context, cfg map[string]string, siteName string) (int64, error) {
	params := map[string]string{
		"Action":   "ListSites",
		"SiteName": siteName,
	}

	body, err := a.doRequest(ctx, cfg, "GET", params, nil)
	if err != nil {
		return 0, err
	}

	var resp struct {
		Sites []struct {
			SiteId   int64  `json:"SiteId"`
			SiteName string `json:"SiteName"`
		} `json:"Sites"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return 0, fmt.Errorf("解析响应失败: %w", err)
	}

	for _, s := range resp.Sites {
		if s.SiteName == siteName {
			return s.SiteId, nil
		}
	}

	return 0, fmt.Errorf("未找到站点: %s", siteName)
}

// ensureRecordCorrect 检查已有记录配置是否正确，不正确则更新
func (a *AliyunESAProvider) ensureRecordCorrect(ctx context.Context, cfg map[string]string, siteID int64, recordName, expectedOrigin string) error {
	// 查询已有记录
	params := map[string]string{
		"Action":     "ListRecords",
		"SiteId":     fmt.Sprintf("%d", siteID),
		"RecordName": recordName,
		"PageSize":   "10",
		"PageNumber": "1",
	}
	body, err := a.doRequest(ctx, cfg, "GET", params, nil)
	if err != nil {
		return err
	}

	var resp struct {
		Records []struct {
			RecordId   int64  `json:"RecordId"`
			RecordName string `json:"RecordName"`
			RecordType string `json:"RecordType"`
			Data       struct {
				Value string `json:"Value"`
			} `json:"Data"`
			Proxied bool `json:"Proxied"`
		} `json:"Records"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return fmt.Errorf("解析ListRecords响应失败: %w", err)
	}

	for _, r := range resp.Records {
		if r.RecordName == recordName {
			// 检查记录值是否正确
			if r.Data.Value == expectedOrigin && r.Proxied {
				log.Printf("[AliyunESA] 记录 %s 配置正确 (Value=%s)，跳过", recordName, r.Data.Value)
				return nil
			}
			// 配置不正确，更新记录
			log.Printf("[AliyunESA] 记录 %s 配置不正确 (当前=%s, 期望=%s)，更新中...", recordName, r.Data.Value, expectedOrigin)
			return a.updateRecord(ctx, cfg, r.RecordId, expectedOrigin)
		}
	}
	return fmt.Errorf("未找到记录 %s", recordName)
}

// updateRecord 通过 UpdateRecord API 更新记录
func (a *AliyunESAProvider) updateRecord(ctx context.Context, cfg map[string]string, recordID int64, origin string) error {
	reqBody := map[string]interface{}{
		"RecordId": recordID,
		"Data":     map[string]interface{}{"Value": origin},
		"Proxied":  true,
		"BizName":  "web",
		"Ttl":      1,
	}
	params := map[string]string{
		"Action": "UpdateRecord",
	}
	body, err := a.doRequest(ctx, cfg, "POST", params, reqBody)
	if err != nil {
		return err
	}
	return a.checkResponse(body)
}

// batchCreateRecord 通过 BatchCreateRecords API 添加加速记录
func (a *AliyunESAProvider) batchCreateRecord(ctx context.Context, cfg map[string]string, siteID int64, domain, origin string) error {
	// 根据源站是IP还是域名自动决定记录类型
	originHost := strings.Split(origin, ":")[0]
	recordType := "A/AAAA"
	if !provider.IsIPAddress(originHost) {
		recordType = "CNAME"
	}

	record := map[string]interface{}{
		"RecordName": domain,
		"Type":       recordType,
		"Proxied":    true,
		"BizName":    "web",
		"Ttl":        1,
		"Data":       map[string]interface{}{"Value": originHost},
	}

	reqBody := map[string]interface{}{
		"SiteId":     siteID,
		"RecordList": []interface{}{record},
	}

	params := map[string]string{
		"Action": "BatchCreateRecords",
	}

	body, err := a.doRequest(ctx, cfg, "POST", params, reqBody)
	if err != nil {
		return err
	}

	// 解析响应检查失败记录
	var resp struct {
		RecordResultList struct {
			Success []struct {
				RecordName string `json:"RecordName"`
				RecordId   int64  `json:"RecordId"`
			} `json:"Success"`
			Failed []struct {
				RecordName  string `json:"RecordName"`
				Description string `json:"Description"`
			} `json:"Failed"`
		} `json:"RecordResultList"`
		Code    string `json:"Code"`
		Message string `json:"Message"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return fmt.Errorf("解析BatchCreateRecords响应失败: %w", err)
	}
	if resp.Code != "" {
		return fmt.Errorf("阿里云ESA API错误: [%s] %s", resp.Code, resp.Message)
	}

	// 检查是否有失败记录
	if len(resp.RecordResultList.Failed) > 0 {
		f := resp.RecordResultList.Failed[0]
		return fmt.Errorf("记录 %s 创建失败: %s", f.RecordName, f.Description)
	}

	if len(resp.RecordResultList.Success) > 0 {
		log.Printf("[AliyunESA] 记录 %s 创建成功 (RecordId=%d)", domain, resp.RecordResultList.Success[0].RecordId)
	}

	return nil
}

// createOriginRule 通过 CreateOriginRule API 配置回源规则（协议、端口、HOST）
func (a *AliyunESAProvider) createOriginRule(ctx context.Context, cfg map[string]string, siteID int64, domain string, originCfg model.OriginConfig) error {
	params := map[string]string{
		"Action": "CreateOriginRule",
	}

	// 回源协议
	originScheme := "follow"
	switch originCfg.OriginProtocol {
	case "http":
		originScheme = "http"
	case "https":
		originScheme = "https"
	}

	// 回源HOST，默认为域名
	originHost := originCfg.OriginHost
	if originHost == "" {
		originHost = domain
	}

	reqBody := map[string]interface{}{
		"SiteId":       siteID,
		"RuleName":     fmt.Sprintf("origin-%s", domain),
		"RuleEnable":   "on",
		"Rule":         fmt.Sprintf("(http.host eq \"%s\")", domain),
		"OriginScheme": originScheme,
		"OriginHost":   originHost,
	}

	// 设置回源端口
	if originCfg.HTTPPort > 0 {
		reqBody["HttpPort"] = fmt.Sprintf("%d", originCfg.HTTPPort)
	}
	if originCfg.HTTPSPort > 0 {
		reqBody["HttpsPort"] = fmt.Sprintf("%d", originCfg.HTTPSPort)
	}

	body, err := a.doRequest(ctx, cfg, "POST", params, reqBody)
	if err != nil {
		return err
	}

	// 规则已存在时检查配置是否正确，不正确则更新
	if err := a.checkResponse(body); err != nil {
		if strings.Contains(err.Error(), "ConfigConflicts") {
			log.Printf("[AliyunESA] 回源规则 origin-%s 已存在，检查配置...", domain)
			return a.ensureOriginRuleCorrect(ctx, cfg, siteID, domain, originCfg)
		}
		return err
	}
	return nil
}

// ensureOriginRuleCorrect 检查已有回源规则配置是否正确，不正确则更新
func (a *AliyunESAProvider) ensureOriginRuleCorrect(ctx context.Context, cfg map[string]string, siteID int64, domain string, originCfg model.OriginConfig) error {
	// 查询已有规则
	listParams := map[string]string{
		"Action": "ListOriginRules",
		"SiteId": fmt.Sprintf("%d", siteID),
	}
	body, err := a.doRequest(ctx, cfg, "GET", listParams, nil)
	if err != nil {
		return err
	}

	var resp struct {
		Configs []struct {
			ConfigId        int64  `json:"ConfigId"`
			RuleName        string `json:"RuleName"`
			OriginScheme    string `json:"OriginScheme"`
			OriginHost      string `json:"OriginHost"`
			OriginHttpPort  string `json:"OriginHttpPort"`
			OriginHttpsPort string `json:"OriginHttpsPort"`
		} `json:"Configs"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return fmt.Errorf("解析ListOriginRules响应失败: %w", err)
	}

	ruleName := fmt.Sprintf("origin-%s", domain)

	// 期望的配置
	expectScheme := "follow"
	switch originCfg.OriginProtocol {
	case "http":
		expectScheme = "http"
	case "https":
		expectScheme = "https"
	}
	expectHost := originCfg.OriginHost
	if expectHost == "" {
		expectHost = domain
	}
	expectHttpPort := ""
	if originCfg.HTTPPort > 0 {
		expectHttpPort = fmt.Sprintf("%d", originCfg.HTTPPort)
	}
	expectHttpsPort := ""
	if originCfg.HTTPSPort > 0 {
		expectHttpsPort = fmt.Sprintf("%d", originCfg.HTTPSPort)
	}

	for _, c := range resp.Configs {
		if c.RuleName == ruleName {
			// 逐字段检查
			if c.OriginScheme == expectScheme && c.OriginHost == expectHost && c.OriginHttpPort == expectHttpPort && c.OriginHttpsPort == expectHttpsPort {
				log.Printf("[AliyunESA] 回源规则 %s 配置正确，跳过", ruleName)
				return nil
			}
			// 配置不正确，更新
			log.Printf("[AliyunESA] 回源规则 %s 配置不正确，更新中... (协议: %s→%s, HOST: %s→%s, HTTP端口: %s→%s, HTTPS端口: %s→%s)",
				ruleName, c.OriginScheme, expectScheme, c.OriginHost, expectHost, c.OriginHttpPort, expectHttpPort, c.OriginHttpsPort, expectHttpsPort)
			return a.updateOriginRule(ctx, cfg, siteID, c.ConfigId, domain, originCfg)
		}
	}
	log.Printf("[AliyunESA] 未找到回源规则 %s", ruleName)
	return nil
}

// updateOriginRule 通过 UpdateOriginRule API 更新回源规则
func (a *AliyunESAProvider) updateOriginRule(ctx context.Context, cfg map[string]string, siteID int64, configID int64, domain string, originCfg model.OriginConfig) error {
	originScheme := "follow"
	switch originCfg.OriginProtocol {
	case "http":
		originScheme = "http"
	case "https":
		originScheme = "https"
	}
	originHost := originCfg.OriginHost
	if originHost == "" {
		originHost = domain
	}

	reqBody := map[string]interface{}{
		"SiteId":       siteID,
		"ConfigId":     configID,
		"RuleName":     fmt.Sprintf("origin-%s", domain),
		"RuleEnable":   "on",
		"Rule":         fmt.Sprintf("(http.host eq \"%s\")", domain),
		"OriginScheme": originScheme,
		"OriginHost":   originHost,
	}
	if originCfg.HTTPPort > 0 {
		reqBody["OriginHttpPort"] = fmt.Sprintf("%d", originCfg.HTTPPort)
	}
	if originCfg.HTTPSPort > 0 {
		reqBody["OriginHttpsPort"] = fmt.Sprintf("%d", originCfg.HTTPSPort)
	}

	params := map[string]string{
		"Action": "UpdateOriginRule",
	}
	updateBody, err := a.doRequest(ctx, cfg, "POST", params, reqBody)
	if err != nil {
		return err
	}
	return a.checkResponse(updateBody)
}

func (a *AliyunESAProvider) checkResponse(body []byte) error {
	var resp struct {
		Code    string `json:"Code"`
		Message string `json:"Message"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil
	}
	if resp.Code != "" && resp.Code != "OK" && resp.Code != "Success" {
		return fmt.Errorf("阿里云ESA API错误: [%s] %s", resp.Code, resp.Message)
	}
	return nil
}

// doRequest 执行阿里云 ESA API 请求（使用 ACS3-HMAC-SHA256 签名）
func (a *AliyunESAProvider) doRequest(ctx context.Context, cfg map[string]string, method string, queryParams map[string]string, reqBody interface{}) ([]byte, error) {
	accessKeyID := cfg["access_key_id"]
	accessKeySecret := cfg["access_key_secret"]

	// 构建请求URL
	action := queryParams["Action"]

	var bodyBytes []byte
	var bodyReader io.Reader
	if reqBody != nil {
		var err error
		bodyBytes, err = json.Marshal(reqBody)
		if err != nil {
			return nil, fmt.Errorf("序列化请求体失败: %w", err)
		}
		bodyReader = bytes.NewReader(bodyBytes)
	}

	// 使用 url.Values 构建查询字符串
	qValues := url.Values{}
	for k, v := range queryParams {
		if k == "Action" {
			continue
		}
		qValues.Set(k, v)
	}
	// 签名用：完整 percent-encoding（%5B %5D %22 全编码）
	canonicalQueryString := strings.ReplaceAll(qValues.Encode(), "+", "%20")
	// URL 用：[ ] 不编码（字面量），" 保持 %22——避免 HTTP 请求行裸引号问题
	urlQueryString := strings.ReplaceAll(canonicalQueryString, "%5B", "[")
	urlQueryString = strings.ReplaceAll(urlQueryString, "%5D", "]")

	requestURL := aliyunESAEndpoint
	if urlQueryString != "" {
		requestURL = requestURL + "?" + urlQueryString
	}

	req, err := http.NewRequestWithContext(ctx, method, requestURL, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("构建请求失败: %w", err)
	}

	// 设置必要的头部
	now := time.Now().UTC()
	dateISO := now.Format("2006-01-02T15:04:05Z")
	nonce := fmt.Sprintf("%d", now.UnixNano())

	req.Header.Set("x-acs-action", action)
	req.Header.Set("x-acs-version", aliyunESAVersion)
	req.Header.Set("x-acs-date", dateISO)
	req.Header.Set("x-acs-signature-nonce", nonce)
	req.Header.Set("host", "esa.cn-hangzhou.aliyuncs.com")

	if reqBody != nil {
		req.Header.Set("content-type", "application/json")
	}

	log.Printf("[AliyunESA DEBUG] doRequest %s %s", method, req.URL.String())

	// ACS3-HMAC-SHA256 签名
	signedHeaders, signature := a.sign(method, canonicalQueryString, req.Header, bodyBytes, accessKeySecret)

	authHeader := fmt.Sprintf("ACS3-HMAC-SHA256 Credential=%s,SignedHeaders=%s,Signature=%s",
		accessKeyID, signedHeaders, signature)
	req.Header.Set("Authorization", authHeader)

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求阿里云ESA API失败: %w", err)
	}
	defer resp.Body.Close()

	return io.ReadAll(resp.Body)
}

// sign 实现 ACS3-HMAC-SHA256 签名算法
func (a *AliyunESAProvider) sign(method, queryString string, headers http.Header, body []byte, secret string) (signedHeaders, signature string) {
	// 1. 收集需要签名的头部（以 x-acs- 开头和 host、content-type）
	signHeaderKeys := []string{}
	headerMap := map[string]string{}
	for k := range headers {
		lk := strings.ToLower(k)
		if strings.HasPrefix(lk, "x-acs-") || lk == "host" || lk == "content-type" {
			signHeaderKeys = append(signHeaderKeys, lk)
			headerMap[lk] = strings.TrimSpace(headers.Get(k))
		}
	}
	sort.Strings(signHeaderKeys)
	signedHeaders = strings.Join(signHeaderKeys, ";")

	// 2. 构建 Canonical Headers
	canonicalHeaders := ""
	for _, k := range signHeaderKeys {
		canonicalHeaders += k + ":" + headerMap[k] + "\n"
	}

	// 3. Body Hash
	bodyHash := provider.Sha256Hex(body)

	// 4. Canonical Request
	canonicalRequest := strings.Join([]string{
		method,
		"/",
		queryString,
		canonicalHeaders,
		signedHeaders,
		bodyHash,
	}, "\n")

	// 5. String To Sign
	stringToSign := "ACS3-HMAC-SHA256\n" + provider.Sha256Hex([]byte(canonicalRequest))

	// 6. 计算签名
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(stringToSign))
	signature = hex.EncodeToString(mac.Sum(nil))

	return signedHeaders, signature
}
