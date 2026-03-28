package aliyun

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
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	alidnsEndpoint = "https://alidns.cn-hangzhou.aliyuncs.com"
	alidnsHost     = "alidns.cn-hangzhou.aliyuncs.com"
	alidnsVersion  = "2015-01-09"
)

// DNS 能力注册
func init() {
	provider.RegisterDNS("aliyun", &AliyunESAProvider{})
}

// zone ID 前缀，区分来源
const (
	prefixESA    = "esa:"
	prefixAlidns = "alidns:"
)

// Features 阿里云根据 zone 类型返回不同特性：ESA 显示代理，Alidns 不显示
func (a *AliyunESAProvider) Features(zoneID string) model.DnsProviderFeatures {
	if strings.HasPrefix(zoneID, prefixESA) {
		// ESA 使用代理，但不支持按线路做大陆/海外分流
		return model.DnsProviderFeatures{
			HasProxy:  true,
			ProxyName: "ESA 代理",
			HasLine:   false,
			HasWeight: false,
			HasRemark: true,
		}
	}
	// Alidns 支持线路，中国大陆线路 = "internal"
	return model.DnsProviderFeatures{
		HasLine:      true,
		HasWeight:    true,
		HasRemark:    true,
		MainlandLine: "internal",
	}
}

// SupportedRecordTypes 阿里云支持的 DNS 记录类型（Alidns 全量）
func (a *AliyunESAProvider) SupportedRecordTypes() []string {
	return []string{
		"A", "AAAA", "CNAME", "ALIAS", "NS", "MX",
		"SRV", "TXT", "CAA",
		"REDIRECT_URL", "FORWARD_URL",
		"SVCB", "HTTPS",
	}
}

// SupportedLines 阿里云云解析支持的解析线路（按分组）
func (a *AliyunESAProvider) SupportedLines() []model.DnsLine {
	return []model.DnsLine{
		// 默认
		{Value: "default", Label: "默认", Group: "default"},

		// 运营商
		{Value: "telecom", Label: "中国电信", Group: "isp"},
		{Value: "cn_telecom_dongbei", Label: "电信_东北", Group: "isp"},
		{Value: "cn_telecom_huabei", Label: "电信_华北", Group: "isp"},
		{Value: "cn_telecom_huadong", Label: "电信_华东", Group: "isp"},
		{Value: "cn_telecom_huanan", Label: "电信_华南", Group: "isp"},
		{Value: "cn_telecom_huazhong", Label: "电信_华中", Group: "isp"},
		{Value: "cn_telecom_xibei", Label: "电信_西北", Group: "isp"},
		{Value: "cn_telecom_xinan", Label: "电信_西南", Group: "isp"},
		{Value: "unicom", Label: "中国联通", Group: "isp"},
		{Value: "cn_unicom_dongbei", Label: "联通_东北", Group: "isp"},
		{Value: "cn_unicom_huabei", Label: "联通_华北", Group: "isp"},
		{Value: "cn_unicom_huadong", Label: "联通_华东", Group: "isp"},
		{Value: "cn_unicom_huanan", Label: "联通_华南", Group: "isp"},
		{Value: "cn_unicom_huazhong", Label: "联通_华中", Group: "isp"},
		{Value: "cn_unicom_xibei", Label: "联通_西北", Group: "isp"},
		{Value: "cn_unicom_xinan", Label: "联通_西南", Group: "isp"},
		{Value: "mobile", Label: "中国移动", Group: "isp"},
		{Value: "cn_mobile_dongbei", Label: "移动_东北", Group: "isp"},
		{Value: "cn_mobile_huabei", Label: "移动_华北", Group: "isp"},
		{Value: "cn_mobile_huadong", Label: "移动_华东", Group: "isp"},
		{Value: "cn_mobile_huanan", Label: "移动_华南", Group: "isp"},
		{Value: "cn_mobile_huazhong", Label: "移动_华中", Group: "isp"},
		{Value: "cn_mobile_xibei", Label: "移动_西北", Group: "isp"},
		{Value: "cn_mobile_xinan", Label: "移动_西南", Group: "isp"},
		{Value: "edu", Label: "中国教育网", Group: "isp"},
		{Value: "drpeng", Label: "中国鹏博士", Group: "isp"},
		{Value: "btvn", Label: "中国广电网", Group: "isp"},
		{Value: "cstnet", Label: "科技网", Group: "isp"},
		{Value: "wexchange", Label: "驰联网络", Group: "isp"},
		{Value: "founder", Label: "方正宽带", Group: "isp"},
		{Value: "topway_video", Label: "天威视讯", Group: "isp"},
		{Value: "wasu", Label: "华数宽带", Group: "isp"},
		{Value: "ocn", Label: "东方有线", Group: "isp"},
		{Value: "cnix", Label: "皓宽网络", Group: "isp"},
		{Value: "bgctv", Label: "歌华有线", Group: "isp"},

		// 地域
		{Value: "internal", Label: "中国地区", Group: "geo"},
		{Value: "cn_region_dongbei", Label: "中国_东北", Group: "geo"},
		{Value: "cn_region_huabei", Label: "中国_华北", Group: "geo"},
		{Value: "cn_region_huadong", Label: "中国_华东", Group: "geo"},
		{Value: "cn_region_huanan", Label: "中国_华南", Group: "geo"},
		{Value: "cn_region_huazhong", Label: "中国_华中", Group: "geo"},
		{Value: "cn_region_xibei", Label: "中国_西北", Group: "geo"},
		{Value: "cn_region_xinan", Label: "中国_西南", Group: "geo"},
		{Value: "oversea", Label: "境外", Group: "geo"},
		{Value: "os_asia", Label: "亚洲", Group: "geo"},
		{Value: "os_europe", Label: "欧洲", Group: "geo"},
		{Value: "os_namerica", Label: "北美洲", Group: "geo"},
		{Value: "os_samerica", Label: "南美洲", Group: "geo"},
		{Value: "os_africa", Label: "非洲", Group: "geo"},
		{Value: "os_oceania", Label: "大洋洲", Group: "geo"},

		// 云厂商
		{Value: "aliyun", Label: "阿里云", Group: "cloud"},
		{Value: "cn_aliyun", Label: "阿里云_中国内地", Group: "cloud"},
		{Value: "os_aliyun", Label: "阿里云_境外", Group: "cloud"},

		// 搜索引擎
		{Value: "search", Label: "搜索引擎", Group: "search"},
		{Value: "google", Label: "谷歌", Group: "search"},
		{Value: "baidu", Label: "百度", Group: "search"},
		{Value: "biying", Label: "必应", Group: "search"},
		{Value: "sougou", Label: "搜狗", Group: "search"},
		{Value: "qihu", Label: "奇虎(360)", Group: "search"},
		{Value: "youdao", Label: "有道", Group: "search"},
		{Value: "yahoo", Label: "雅虎", Group: "search"},
	}
}

// =====================================================================
// ListZones: 聚合 ESA(NS接入) + 云解析(Alidns) 的域名
// =====================================================================

func (a *AliyunESAProvider) ListZones(ctx context.Context, cfg map[string]string) ([]model.DnsZone, error) {
	if err := a.ValidateConfig(cfg); err != nil {
		return nil, err
	}

	var zones []model.DnsZone

	// 1. ESA NS 接入站点
	esaZones, err := a.listESANSZones(ctx, cfg)
	if err != nil {
		log.Printf("[Aliyun] 获取ESA站点失败（跳过）: %v", err)
	} else {
		zones = append(zones, esaZones...)
	}

	// 2. 云解析 Alidns 域名
	alidnsZones, err := a.listAlidnsZones(ctx, cfg)
	if err != nil {
		log.Printf("[Aliyun] 获取云解析域名失败（跳过）: %v", err)
	} else {
		zones = append(zones, alidnsZones...)
	}

	return zones, nil
}

// listESANSZones 只获取 ESA 中 NS 接入的站点
func (a *AliyunESAProvider) listESANSZones(ctx context.Context, cfg map[string]string) ([]model.DnsZone, error) {
	var zones []model.DnsZone
	pageNumber := 1
	for {
		params := map[string]string{
			"Action":     "ListSites",
			"PageNumber": strconv.Itoa(pageNumber),
			"PageSize":   "50",
		}
		body, err := a.doRequest(ctx, cfg, "GET", params, nil)
		if err != nil {
			return nil, err
		}

		var resp struct {
			Sites []struct {
				SiteId     int64  `json:"SiteId"`
				SiteName   string `json:"SiteName"`
				Status     string `json:"Status"`
				AccessType string `json:"AccessType"` // NS / CNAME
			} `json:"Sites"`
			TotalCount int    `json:"TotalCount"`
			Code       string `json:"Code"`
			Message    string `json:"Message"`
		}
		if err := json.Unmarshal(body, &resp); err != nil {
			return nil, fmt.Errorf("解析ESA站点响应失败: %w", err)
		}
		if resp.Code != "" {
			return nil, fmt.Errorf("[ESA] %s: %s", resp.Code, resp.Message)
		}

		for _, s := range resp.Sites {
			// 只保留 NS 接入
			if strings.ToUpper(s.AccessType) != "NS" {
				continue
			}
			status := "pending"
			if s.Status == "active" || s.Status == "online" {
				status = "active"
			}
			zones = append(zones, model.DnsZone{
				ID:       prefixESA + strconv.FormatInt(s.SiteId, 10),
				Name:     s.SiteName,
				Status:   status,
				PlanName: "ESA",
			})
		}

		if pageNumber*50 >= resp.TotalCount || len(resp.Sites) == 0 {
			break
		}
		pageNumber++
	}
	return zones, nil
}

// listAlidnsZones 获取阿里云云解析的域名列表
func (a *AliyunESAProvider) listAlidnsZones(ctx context.Context, cfg map[string]string) ([]model.DnsZone, error) {
	var zones []model.DnsZone
	pageNumber := 1
	for {
		params := map[string]string{
			"Action":     "DescribeDomains",
			"PageNumber": strconv.Itoa(pageNumber),
			"PageSize":   "50",
		}
		body, err := a.doAlidnsRequest(ctx, cfg, "GET", params, nil)
		if err != nil {
			return nil, err
		}

		var resp struct {
			Domains struct {
				Domain []struct {
					DomainName  string `json:"DomainName"`
					DomainId    string `json:"DomainId"`
					RecordCount int64  `json:"RecordCount"`
					VersionCode string `json:"VersionCode"` // 套餐代码：mianfei / version_personal 等
				} `json:"Domain"`
			} `json:"Domains"`
			TotalCount int    `json:"TotalCount"`
			Code       string `json:"Code"`
			Message    string `json:"Message"`
		}
		if err := json.Unmarshal(body, &resp); err != nil {
			return nil, fmt.Errorf("解析Alidns响应失败: %w", err)
		}
		if resp.Code != "" {
			return nil, fmt.Errorf("[Alidns] %s: %s", resp.Code, resp.Message)
		}

		for _, d := range resp.Domains.Domain {
			planName := alidnsPlanName(d.VersionCode)
			zones = append(zones, model.DnsZone{
				ID:          prefixAlidns + d.DomainName,
				Name:        d.DomainName,
				Status:      "active",
				RecordCount: int(d.RecordCount),
				PlanName:    planName,
			})
		}

		if pageNumber*50 >= resp.TotalCount || len(resp.Domains.Domain) == 0 {
			break
		}
		pageNumber++
	}
	return zones, nil
}

// =====================================================================
// ListRecords: 根据 zone ID 前缀路由到 ESA 或 Alidns
// =====================================================================

func (a *AliyunESAProvider) ListRecords(ctx context.Context, cfg map[string]string, zoneID string) ([]model.DnsRecord, error) {
	if err := a.ValidateConfig(cfg); err != nil {
		return nil, err
	}
	if strings.HasPrefix(zoneID, prefixESA) {
		return a.listESARecords(ctx, cfg, strings.TrimPrefix(zoneID, prefixESA))
	}
	if strings.HasPrefix(zoneID, prefixAlidns) {
		return a.listAlidnsRecords(ctx, cfg, strings.TrimPrefix(zoneID, prefixAlidns))
	}
	return nil, fmt.Errorf("未知的zone ID格式: %s", zoneID)
}

// listESARecords ESA 站点 DNS 记录
func (a *AliyunESAProvider) listESARecords(ctx context.Context, cfg map[string]string, siteID string) ([]model.DnsRecord, error) {
	var records []model.DnsRecord
	pageNumber := 1
	for {
		params := map[string]string{
			"Action":     "ListRecords",
			"SiteId":     siteID,
			"PageNumber": strconv.Itoa(pageNumber),
			"PageSize":   "100",
		}
		body, err := a.doRequest(ctx, cfg, "GET", params, nil)
		if err != nil {
			return nil, fmt.Errorf("获取ESA DNS记录失败: %w", err)
		}

		var resp struct {
			Records []struct {
				RecordId   int64  `json:"RecordId"`
				RecordName string `json:"RecordName"`
				RecordType string `json:"RecordType"`
				Ttl        int    `json:"Ttl"`
				Data       struct {
					Value    string `json:"Value"`
					Priority int    `json:"Priority"`
					Flag     int    `json:"Flag"`
					Tag      string `json:"Tag"`
				} `json:"Data"`
			} `json:"Records"`
			TotalCount int    `json:"TotalCount"`
			Code       string `json:"Code"`
			Message    string `json:"Message"`
		}
		if err := json.Unmarshal(body, &resp); err != nil {
			return nil, fmt.Errorf("解析响应失败: %w", err)
		}
		if resp.Code != "" {
			return nil, fmt.Errorf("[ESA] %s: %s", resp.Code, resp.Message)
		}

		for _, r := range resp.Records {
			rec := model.DnsRecord{
				ID:         strconv.FormatInt(r.RecordId, 10),
				ZoneID:     prefixESA + siteID,
				Type:       normalizeRecordType(r.RecordType),
				HostRecord: r.RecordName,
				Name:       r.RecordName,
				Value:      r.Data.Value,
				TTL:        r.Ttl,
				Line:       "default",
				LineLabel:  "默认",
				Status:     "enable",
			}
			if r.Data.Priority > 0 {
				rec.Priority = r.Data.Priority
			}
			if r.RecordType == "CAA" {
				rec.Value = fmt.Sprintf("%d %s %s", r.Data.Flag, r.Data.Tag, r.Data.Value)
			}
			records = append(records, rec)
		}

		if len(records) >= resp.TotalCount || len(resp.Records) == 0 {
			break
		}
		pageNumber++
	}
	return records, nil
}

// listAlidnsRecords 云解析 DNS 记录
func (a *AliyunESAProvider) listAlidnsRecords(ctx context.Context, cfg map[string]string, domainName string) ([]model.DnsRecord, error) {
	var records []model.DnsRecord
	pageNumber := 1
	for {
		params := map[string]string{
			"Action":     "DescribeDomainRecords",
			"DomainName": domainName,
			"PageNumber": strconv.Itoa(pageNumber),
			"PageSize":   "100",
		}
		body, err := a.doAlidnsRequest(ctx, cfg, "GET", params, nil)
		if err != nil {
			return nil, fmt.Errorf("获取Alidns记录失败: %w", err)
		}

		var resp struct {
			DomainRecords struct {
				Record []struct {
					RecordId string `json:"RecordId"`
					RR       string `json:"RR"`
					Type     string `json:"Type"`
					Value    string `json:"Value"`
					TTL      int    `json:"TTL"`
					Line     string `json:"Line"`
					Status   string `json:"Status"`
					Remark   string `json:"Remark"`
					Priority int    `json:"Priority"`
					Weight   int    `json:"Weight"`
				} `json:"Record"`
			} `json:"DomainRecords"`
			TotalCount int    `json:"TotalCount"`
			Code       string `json:"Code"`
			Message    string `json:"Message"`
		}
		if err := json.Unmarshal(body, &resp); err != nil {
			return nil, fmt.Errorf("解析响应失败: %w", err)
		}
		if resp.Code != "" {
			return nil, fmt.Errorf("[Alidns] %s: %s", resp.Code, resp.Message)
		}

		for _, r := range resp.DomainRecords.Record {
			status := "enable"
			if r.Status == "DISABLE" {
				status = "disable"
			}
			// 完整域名 = RR + "." + domainName（RR 为 @ 时直接用 domainName）
			fullName := r.RR + "." + domainName
			if r.RR == "@" {
				fullName = domainName
			}
			rec := model.DnsRecord{
				ID:         r.RecordId,
				ZoneID:     prefixAlidns + domainName,
				Type:       r.Type,
				HostRecord: r.RR,
				Name:       fullName,
				Value:      r.Value,
				TTL:        r.TTL,
				Priority:   r.Priority,
				Weight:     r.Weight,
				Line:       r.Line,
				LineLabel:  alidnsLineLabel(r.Line),
				Status:     status,
				Remark:     r.Remark,
			}
			records = append(records, rec)
		}

		if len(records) >= resp.TotalCount || len(resp.DomainRecords.Record) == 0 {
			break
		}
		pageNumber++
	}
	return records, nil
}

// =====================================================================
// AddRecord
// =====================================================================

func (a *AliyunESAProvider) AddRecord(ctx context.Context, cfg map[string]string, zoneID string, req model.DnsRecordRequest) (*model.DnsRecord, error) {
	if err := a.ValidateConfig(cfg); err != nil {
		return nil, err
	}
	if strings.HasPrefix(zoneID, prefixESA) {
		return a.addESARecord(ctx, cfg, strings.TrimPrefix(zoneID, prefixESA), req)
	}
	if strings.HasPrefix(zoneID, prefixAlidns) {
		return a.addAlidnsRecord(ctx, cfg, strings.TrimPrefix(zoneID, prefixAlidns), req)
	}
	return nil, fmt.Errorf("未知的zone ID格式: %s", zoneID)
}

func (a *AliyunESAProvider) addESARecord(ctx context.Context, cfg map[string]string, siteID string, req model.DnsRecordRequest) (*model.DnsRecord, error) {
	sid, err := strconv.ParseInt(siteID, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("无效的ESA站点ID: %s", siteID)
	}
	ttl := req.TTL
	if ttl <= 0 {
		ttl = 1
	}
	reqBody := map[string]interface{}{
		"SiteId":     sid,
		"RecordName": req.Name,
		"Type":       req.Type,
		"Data":       buildESARecordData(req),
		"Ttl":        ttl,
		"BizName":    "web",
	}
	params := map[string]string{"Action": "CreateRecord"}
	body, err := a.doRequest(ctx, cfg, "POST", params, reqBody)
	if err != nil {
		return nil, fmt.Errorf("添加ESA DNS记录失败: %w", err)
	}
	var resp struct {
		RecordId int64  `json:"RecordId"`
		Code     string `json:"Code"`
		Message  string `json:"Message"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("解析响应失败: %w", err)
	}
	if resp.Code != "" {
		return nil, fmt.Errorf("[ESA] %s: %s", resp.Code, resp.Message)
	}
	log.Printf("[AliyunESA] DNS记录创建成功: %s %s → %s", req.Type, req.Name, req.Value)
	return &model.DnsRecord{
		ID: strconv.FormatInt(resp.RecordId, 10), ZoneID: prefixESA + siteID,
		Type: req.Type, Name: req.Name, Value: req.Value, TTL: ttl, Status: "enable",
	}, nil
}

func (a *AliyunESAProvider) addAlidnsRecord(ctx context.Context, cfg map[string]string, domainName string, req model.DnsRecordRequest) (*model.DnsRecord, error) {
	// 从完整域名提取 RR（主机记录）
	rr := extractRR(req.Name, domainName)
	ttl := req.TTL
	if ttl <= 0 {
		ttl = 600
	}
	params := map[string]string{
		"Action":     "AddDomainRecord",
		"DomainName": domainName,
		"RR":         rr,
		"Type":       req.Type,
		"Value":      req.Value,
		"TTL":        strconv.Itoa(ttl),
	}
	if req.Line != "" {
		params["Line"] = req.Line
	} else {
		params["Line"] = "default"
	}
	if req.Priority > 0 {
		params["Priority"] = strconv.Itoa(req.Priority)
	}
	if req.Weight > 0 {
		params["Weight"] = strconv.Itoa(req.Weight)
	}
	if req.Remark != "" {
		params["Remark"] = req.Remark
	}
	body, err := a.doAlidnsRequest(ctx, cfg, "GET", params, nil)
	if err != nil {
		return nil, fmt.Errorf("添加Alidns记录失败: %w", err)
	}
	var resp struct {
		RecordId string `json:"RecordId"`
		Code     string `json:"Code"`
		Message  string `json:"Message"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("解析响应失败: %w", err)
	}
	if resp.Code != "" {
		return nil, fmt.Errorf("[Alidns] %s: %s", resp.Code, resp.Message)
	}
	log.Printf("[Alidns] DNS记录创建成功: %s %s → %s (RecordId=%s)", req.Type, req.Name, req.Value, resp.RecordId)
	return &model.DnsRecord{
		ID: resp.RecordId, ZoneID: prefixAlidns + domainName,
		Type: req.Type, Name: req.Name, Value: req.Value, TTL: ttl, Line: req.Line, Status: "enable",
	}, nil
}

// =====================================================================
// UpdateRecord
// =====================================================================

func (a *AliyunESAProvider) UpdateRecord(ctx context.Context, cfg map[string]string, zoneID string, recordID string, req model.DnsRecordRequest) error {
	if err := a.ValidateConfig(cfg); err != nil {
		return err
	}
	if strings.HasPrefix(zoneID, prefixESA) {
		return a.updateESARecord(ctx, cfg, recordID, req)
	}
	if strings.HasPrefix(zoneID, prefixAlidns) {
		domainName := strings.TrimPrefix(zoneID, prefixAlidns)
		return a.updateAlidnsRecord(ctx, cfg, domainName, recordID, req)
	}
	return fmt.Errorf("未知的zone ID格式: %s", zoneID)
}

func (a *AliyunESAProvider) updateESARecord(ctx context.Context, cfg map[string]string, recordID string, req model.DnsRecordRequest) error {
	recID, _ := strconv.ParseInt(recordID, 10, 64)
	ttl := req.TTL
	if ttl <= 0 {
		ttl = 1
	}
	reqBody := map[string]interface{}{
		"RecordId": recID,
		"Data":     buildESARecordData(req),
		"Ttl":      ttl,
		"BizName":  "web",
	}
	params := map[string]string{"Action": "UpdateRecord"}
	body, err := a.doRequest(ctx, cfg, "POST", params, reqBody)
	if err != nil {
		return fmt.Errorf("更新ESA记录失败: %w", err)
	}
	return a.checkResponse(body)
}

func (a *AliyunESAProvider) updateAlidnsRecord(ctx context.Context, cfg map[string]string, domainName, recordID string, req model.DnsRecordRequest) error {
	rr := extractRR(req.Name, domainName)
	ttl := req.TTL
	if ttl <= 0 {
		ttl = 600
	}
	params := map[string]string{
		"Action":   "UpdateDomainRecord",
		"RecordId": recordID,
		"RR":       rr,
		"Type":     req.Type,
		"Value":    req.Value,
		"TTL":      strconv.Itoa(ttl),
	}
	if req.Line != "" {
		params["Line"] = req.Line
	} else {
		params["Line"] = "default"
	}
	if req.Priority > 0 {
		params["Priority"] = strconv.Itoa(req.Priority)
	}
	if req.Weight > 0 {
		params["Weight"] = strconv.Itoa(req.Weight)
	}
	if req.Remark != "" {
		params["Remark"] = req.Remark
	}
	body, err := a.doAlidnsRequest(ctx, cfg, "GET", params, nil)
	if err != nil {
		return fmt.Errorf("更新Alidns记录失败: %w", err)
	}
	// DomainRecordDuplicate 表示记录核心字段(RR+Type+Value)未变，视为更新成功
	if err := a.checkAlidnsResponse(body); err != nil {
		if !strings.Contains(err.Error(), "DomainRecordDuplicate") {
			return err
		}
		log.Printf("[Alidns] 记录核心字段未变更，跳过: RecordId=%s", recordID)
	}

	// 备注通过独立 API 更新
	if req.Remark != "" {
		remarkParams := map[string]string{
			"Action":   "UpdateDomainRecordRemark",
			"RecordId": recordID,
			"Remark":   req.Remark,
		}
		remarkBody, err := a.doAlidnsRequest(ctx, cfg, "GET", remarkParams, nil)
		if err != nil {
			log.Printf("[Alidns] 更新备注失败: %v", err)
		} else {
			_ = a.checkAlidnsResponse(remarkBody)
		}
	}
	return nil
}

// =====================================================================
// DeleteRecord
// =====================================================================

func (a *AliyunESAProvider) DeleteRecord(ctx context.Context, cfg map[string]string, zoneID string, recordID string) error {
	if err := a.ValidateConfig(cfg); err != nil {
		return err
	}
	if strings.HasPrefix(zoneID, prefixESA) {
		return a.deleteESARecord(ctx, cfg, recordID)
	}
	if strings.HasPrefix(zoneID, prefixAlidns) {
		return a.deleteAlidnsRecord(ctx, cfg, recordID)
	}
	return fmt.Errorf("未知的zone ID格式: %s", zoneID)
}

func (a *AliyunESAProvider) deleteESARecord(ctx context.Context, cfg map[string]string, recordID string) error {
	params := map[string]string{"Action": "DeleteRecord", "RecordId": recordID}
	body, err := a.doRequest(ctx, cfg, "POST", params, nil)
	if err != nil {
		return fmt.Errorf("删除ESA记录失败: %w", err)
	}
	return a.checkResponse(body)
}

func (a *AliyunESAProvider) deleteAlidnsRecord(ctx context.Context, cfg map[string]string, recordID string) error {
	params := map[string]string{"Action": "DeleteDomainRecord", "RecordId": recordID}
	body, err := a.doAlidnsRequest(ctx, cfg, "GET", params, nil)
	if err != nil {
		return fmt.Errorf("删除Alidns记录失败: %w", err)
	}
	return a.checkAlidnsResponse(body)
}

// =====================================================================
// Alidns API 请求（ACS3-HMAC-SHA256 签名，不同 endpoint/version/host）
// =====================================================================

func (a *AliyunESAProvider) doAlidnsRequest(ctx context.Context, cfg map[string]string, method string, queryParams map[string]string, reqBody interface{}) ([]byte, error) {
	accessKeyID := cfg["access_key_id"]
	accessKeySecret := cfg["access_key_secret"]

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

	// 构建查询字符串（跳过 Action）
	queryParts := []string{}
	sortedKeys := make([]string, 0, len(queryParams))
	for k := range queryParams {
		if k == "Action" {
			continue
		}
		sortedKeys = append(sortedKeys, k)
	}
	sort.Strings(sortedKeys)
	for _, k := range sortedKeys {
		queryParts = append(queryParts, fmt.Sprintf("%s=%s", k, queryParams[k]))
	}
	queryString := strings.Join(queryParts, "&")

	requestURL := alidnsEndpoint
	if queryString != "" {
		requestURL = requestURL + "?" + queryString
	}

	req, err := http.NewRequestWithContext(ctx, method, requestURL, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("构建请求失败: %w", err)
	}

	now := time.Now().UTC()
	dateISO := now.Format("2006-01-02T15:04:05Z")
	nonce := fmt.Sprintf("%d", now.UnixNano())

	req.Header.Set("x-acs-action", action)
	req.Header.Set("x-acs-version", alidnsVersion)
	req.Header.Set("x-acs-date", dateISO)
	req.Header.Set("x-acs-signature-nonce", nonce)
	req.Header.Set("host", alidnsHost)

	if reqBody != nil {
		req.Header.Set("content-type", "application/json")
	}

	// 复用 ESA 的 ACS3-HMAC-SHA256 签名方法
	signedHeaders, signature := a.sign(method, queryString, req.Header, bodyBytes, accessKeySecret)

	authHeader := fmt.Sprintf("ACS3-HMAC-SHA256 Credential=%s,SignedHeaders=%s,Signature=%s",
		accessKeyID, signedHeaders, signature)
	req.Header.Set("Authorization", authHeader)

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求Alidns API失败: %w", err)
	}
	defer resp.Body.Close()

	return io.ReadAll(resp.Body)
}

func (a *AliyunESAProvider) checkAlidnsResponse(body []byte) error {
	var resp struct {
		Code    string `json:"Code"`
		Message string `json:"Message"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil
	}
	if resp.Code != "" && resp.Code != "OK" && resp.Code != "Success" {
		return fmt.Errorf("[Alidns] %s: %s", resp.Code, resp.Message)
	}
	return nil
}

// =====================================================================
// 工具函数
// =====================================================================

// extractRR 从完整域名提取主机记录 (RR)
// "www.example.com" + "example.com" → "www"
// "example.com" + "example.com" → "@"
func extractRR(fullName, domainName string) string {
	if fullName == domainName || fullName == "@" {
		return "@"
	}
	suffix := "." + domainName
	if strings.HasSuffix(fullName, suffix) {
		return strings.TrimSuffix(fullName, suffix)
	}
	// 如果没有后缀，则认为 fullName 本身就是 RR
	return fullName
}

// buildESARecordData 构建 ESA 记录的 Data 字段
func buildESARecordData(req model.DnsRecordRequest) map[string]interface{} {
	data := map[string]interface{}{"Value": req.Value}
	switch req.Type {
	case "MX":
		data["Priority"] = req.Priority
	case "SRV":
		parts := strings.Fields(req.Value)
		if len(parts) >= 4 {
			priority, _ := strconv.Atoi(parts[0])
			weight, _ := strconv.Atoi(parts[1])
			port, _ := strconv.Atoi(parts[2])
			data["Value"] = parts[3]
			data["Priority"] = priority
			data["Weight"] = weight
			data["Port"] = port
		}
	case "CAA":
		parts := strings.SplitN(req.Value, " ", 3)
		if len(parts) >= 3 {
			flag, _ := strconv.Atoi(parts[0])
			data["Flag"] = flag
			data["Tag"] = parts[1]
			data["Value"] = parts[2]
		}
	}
	return data
}

// normalizeRecordType 将 ESA 返回的记录类型标准化
func normalizeRecordType(t string) string {
	if t == "A/AAAA" {
		return "A"
	}
	return t
}

// alidnsPlanName 将 Alidns VersionCode 映射为中文套餐名
func alidnsPlanName(code string) string {
	m := map[string]string{
		"mianfei":                     "免费版",
		"bumianfei":                   "付费版",
		"version_personal":            "个人版",
		"version_enterprise_basic":    "企业标准版",
		"version_enterprise_advanced": "企业旗舰版",
		"version_enterprise_ultimate": "企业至尊版",
	}
	if name, ok := m[code]; ok {
		return name
	}
	if code == "" {
		return "免费版"
	}
	return code
}

// alidnsLineLabel 将 Alidns 解析线路 CODE 转为中文显示名
// 覆盖：默认 / 运营商 / 地域 / 云厂商 / 搜索引擎 及其细分线路
func alidnsLineLabel(line string) string {
	if line == "" {
		return "默认"
	}

	// 一级线路 + 主要二级线路（直接映射）
	m := map[string]string{
		// 默认
		"default": "默认",
		// 运营商一级
		"telecom": "中国电信", "unicom": "中国联通", "mobile": "中国移动",
		"edu": "中国教育网", "drpeng": "中国鹏博士", "btvn": "中国广电网",
		"cstnet": "科技网", "wexchange": "驰联网络", "founder": "方正宽带",
		"topway_video": "天威视讯", "wasu": "华数宽带", "ocn": "东方有线",
		"cnix": "皓宽网络", "bgctv": "歌华有线",
		// 地域一级
		"internal": "中国地区", "oversea": "境外",
		// 云厂商
		"aliyun": "阿里云", "cn_aliyun": "阿里云_中国内地", "os_aliyun": "阿里云_境外",
		// 搜索引擎
		"search": "搜索引擎", "google": "谷歌", "baidu": "百度",
		"biying": "必应", "sougou": "搜狗", "qihu": "奇虎(360)",
		"youdao": "有道", "yahoo": "雅虎",
		// 境外大洲
		"os_asia": "亚洲", "os_europe": "欧洲", "os_namerica": "北美洲",
		"os_samerica": "南美洲", "os_africa": "非洲", "os_oceania": "大洋洲",
	}
	if label, ok := m[line]; ok {
		return label
	}

	// 省份 CODE → 中文名映射（用于三级线路模式匹配）
	provinces := map[string]string{
		"beijing": "北京", "tianjin": "天津", "hebei": "河北", "shanxi": "山西",
		"neimenggu": "内蒙古", "liaoning": "辽宁", "jilin": "吉林", "heilongjiang": "黑龙江",
		"shanghai": "上海", "jiangsu": "江苏", "zhejiang": "浙江", "anhui": "安徽",
		"fujian": "福建", "jiangxi": "江西", "shandong": "山东", "henan": "河南",
		"hubei": "湖北", "hunan": "湖南", "guangdong": "广东", "guangxi": "广西",
		"hainan": "海南", "chongqing": "重庆", "sichuan": "四川", "guizhou": "贵州",
		"yunnan": "云南", "xizang": "西藏", "shaanxi": "陕西", "shannxi": "陕西",
		"gansu": "甘肃", "qinghai": "青海", "ningxia": "宁夏", "xinjiang": "新疆",
	}

	// 区域 CODE → 中文名
	regions := map[string]string{
		"dongbei": "东北", "huabei": "华北", "huadong": "华东",
		"huanan": "华南", "huazhong": "华中", "xibei": "西北", "xinan": "西南",
	}

	// 运营商线路：cn_{isp}_{region/province}
	ispNames := map[string]string{
		"telecom": "电信", "unicom": "联通", "mobile": "移动", "edu": "教育网",
	}
	for code, isp := range ispNames {
		prefix := "cn_" + code + "_"
		if strings.HasPrefix(line, prefix) {
			suffix := line[len(prefix):]
			if name, ok := provinces[suffix]; ok {
				return isp + "_" + name
			}
			if name, ok := regions[suffix]; ok {
				return isp + "_" + name
			}
		}
	}

	// 地域线路：cn_region_{region/province}
	if strings.HasPrefix(line, "cn_region_") {
		suffix := line[len("cn_region_"):]
		if name, ok := provinces[suffix]; ok {
			return "中国_" + name
		}
		if name, ok := regions[suffix]; ok {
			return "中国_" + name
		}
	}

	// 搜索引擎细分：cn_search_{engine} / os_search_{engine}
	searchNames := map[string]string{
		"google": "谷歌", "baidu": "百度", "biying": "必应",
		"sougou": "搜狗", "qihu": "奇虎", "youdao": "有道", "yahoo": "雅虎",
	}
	if strings.HasPrefix(line, "cn_search_") {
		suffix := line[len("cn_search_"):]
		if name, ok := searchNames[suffix]; ok {
			return name + "_中国内地"
		}
	}
	if strings.HasPrefix(line, "os_search_") {
		suffix := line[len("os_search_"):]
		if name, ok := searchNames[suffix]; ok {
			return name + "_境外"
		}
	}

	// 阿里云区域：aliyun_r_{region-id}
	aliyunRegions := map[string]string{
		"cn-beijing": "华北2(北京)", "cn-chengdu": "西南1(成都)",
		"cn-guangzhou": "华南3(广州)", "cn-hangzhou": "华东1(杭州)",
		"cn-heyuan": "华南2(河源)", "cn-hongkong": "中国(香港)",
		"cn-huhehaote": "华北5(呼和浩特)", "cn-nantong": "华东3(南通)",
		"cn-qingdao": "华北1(青岛)", "cn-shanghai": "华东2(上海)",
		"cn-shenzhen": "华南1(深圳)", "cn-wulanchabu": "华北6(乌兰察布)",
		"cn-zhangjiakou": "华北3(张家口)",
		"ap-northeast-1": "日本(东京)", "ap-south-1": "印度(孟买)",
		"ap-southeast-1": "新加坡", "ap-southeast-2": "澳大利亚(悉尼)",
		"ap-southeast-3": "马来西亚(吉隆坡)", "ap-southeast-5": "印尼(雅加达)",
		"eu-central-1": "德国(法兰克福)", "eu-west-1": "英国(伦敦)",
		"me-east-1": "中东(迪拜)", "us-east-1": "美国(弗吉尼亚)",
		"us-west-1": "美国(硅谷)",
	}
	if strings.HasPrefix(line, "aliyun_r_") {
		regionID := line[len("aliyun_r_"):]
		if name, ok := aliyunRegions[regionID]; ok {
			return "阿里云_" + name
		}
	}

	// 境外国家/地区：os_{continent}_{country_code}
	// 数量太多，直接返回原始 CODE
	return line
}

// =====================================================================
// SetRecordStatus 启用/禁用 DNS 记录
// =====================================================================

func (a *AliyunESAProvider) SetRecordStatus(ctx context.Context, cfg map[string]string, zoneID string, recordID string, enable bool) error {
	if err := a.ValidateConfig(cfg); err != nil {
		return err
	}
	if strings.HasPrefix(zoneID, prefixESA) {
		// ESA 不支持启用/禁用记录，需要通过删除/重建实现
		return fmt.Errorf("ESA 域名暂不支持启用/禁用记录功能")
	}
	if strings.HasPrefix(zoneID, prefixAlidns) {
		return a.setAlidnsRecordStatus(ctx, cfg, recordID, enable)
	}
	return fmt.Errorf("未知的zone ID格式: %s", zoneID)
}

func (a *AliyunESAProvider) setAlidnsRecordStatus(ctx context.Context, cfg map[string]string, recordID string, enable bool) error {
	status := "Disable"
	if enable {
		status = "Enable"
	}
	params := map[string]string{
		"Action":   "SetDomainRecordStatus",
		"RecordId": recordID,
		"Status":   status,
	}
	body, err := a.doAlidnsRequest(ctx, cfg, "GET", params, nil)
	if err != nil {
		return fmt.Errorf("设置Alidns记录状态失败: %w", err)
	}
	return a.checkAlidnsResponse(body)
}
