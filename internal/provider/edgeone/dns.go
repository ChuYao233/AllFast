package edgeone

import (
	"allfast/internal/model"
	"allfast/internal/provider"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"
)

// DNS 能力注册
func init() {
	provider.RegisterDNS("edgeone", &EdgeOneProvider{})
}

// SupportedRecordTypes 腾讯云 EdgeOne 支持的 DNS 记录类型
func (e *EdgeOneProvider) SupportedRecordTypes() []string {
	return []string{
		"A", "AAAA", "CNAME", "MX", "TXT", "NS",
		"SRV", "CAA",
	}
}

// Features 根据 zone 类型返回不同特性
func (e *EdgeOneProvider) Features(zoneID string) model.DnsProviderFeatures {
	if strings.HasPrefix(zoneID, prefixDNSPod) {
		// DNSPod 支持线路，中国大陆线路 = "境内"
		return model.DnsProviderFeatures{
			HasProxy:     false,
			HasLine:      true,
			HasWeight:    true,
			HasRemark:    true,
			MainlandLine: "境内",
		}
	}
	// EdgeOne 本身不支持线路分流
	return model.DnsProviderFeatures{
		HasProxy:  true,
		ProxyName: "EdgeOne 代理",
		HasLine:   false,
		HasWeight: false,
		HasRemark: true,
	}
}

// SupportedLines DNSPod 支持解析线路
func (e *EdgeOneProvider) SupportedLines() []model.DnsLine {
	return []model.DnsLine{
		{Value: "默认", Label: "默认", Group: "default"},
		{Value: "电信", Label: "电信", Group: "isp"},
		{Value: "联通", Label: "联通", Group: "isp"},
		{Value: "移动", Label: "移动", Group: "isp"},
		{Value: "铁通", Label: "铁通", Group: "isp"},
		{Value: "广电网", Label: "广电网", Group: "isp"},
		{Value: "教育网", Label: "教育网", Group: "isp"},
		{Value: "境内", Label: "境内", Group: "geo"},
		{Value: "境外", Label: "境外", Group: "geo"},
		{Value: "百度", Label: "百度", Group: "search"},
		{Value: "谷歌", Label: "谷歌", Group: "search"},
		{Value: "必应", Label: "必应", Group: "search"},
		{Value: "搜狗", Label: "搜狗", Group: "search"},
		{Value: "有道", Label: "有道", Group: "search"},
	}
}

// ListZones 只返回 DNSPod 域名（EO 站点是 CDN 接入，不纳入 DNS 解析管理）
func (e *EdgeOneProvider) ListZones(ctx context.Context, cfg map[string]string) ([]model.DnsZone, error) {
	if err := e.ValidateConfig(cfg); err != nil {
		return nil, err
	}
	return e.listDNSPodDomains(ctx, cfg)
}

// listEOZones 获取 EdgeOne 站点列表
func (e *EdgeOneProvider) listEOZones(ctx context.Context, cfg map[string]string) ([]model.DnsZone, error) {
	var zones []model.DnsZone
	offset := 0
	for {
		params := map[string]interface{}{"Offset": offset, "Limit": 50}
		body, err := e.doRequest(ctx, cfg, "DescribeZones", params)
		if err != nil {
			return nil, fmt.Errorf("获取站点列表失败: %w", err)
		}
		var resp struct {
			Response struct {
				TotalCount int `json:"TotalCount"`
				Zones      []struct {
					ZoneId   string `json:"ZoneId"`
					ZoneName string `json:"ZoneName"`
					Status   string `json:"Status"`
					Type     string `json:"Type"`
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
		for _, z := range resp.Response.Zones {
			status := "pending"
			if z.Status == "active" {
				status = "active"
			}
			planName := "EdgeOne"
			if z.Type == "full" {
				planName = "EdgeOne NS"
			} else if z.Type == "partial" {
				planName = "EdgeOne CNAME"
			}
			zones = append(zones, model.DnsZone{
				ID:       prefixEO + z.ZoneId,
				Name:     z.ZoneName,
				Status:   status,
				PlanName: planName,
			})
		}
		offset += len(resp.Response.Zones)
		if offset >= resp.Response.TotalCount || len(resp.Response.Zones) == 0 {
			break
		}
	}
	return zones, nil
}

// listDNSPodDomains 获取 DNSPod 域名列表
func (e *EdgeOneProvider) listDNSPodDomains(ctx context.Context, cfg map[string]string) ([]model.DnsZone, error) {
	var zones []model.DnsZone
	offset := 0
	for {
		params := map[string]interface{}{"Offset": offset, "Limit": 100, "Type": "ALL"}
		body, err := e.doDnspodRequest(ctx, cfg, "DescribeDomainList", params)
		if err != nil {
			return nil, err
		}
		var resp struct {
			Response struct {
				DomainCountInfo struct {
					AllTotal int `json:"AllTotal"`
				} `json:"DomainCountInfo"`
				DomainList []struct {
					DomainId    int    `json:"DomainId"`
					Name        string `json:"Name"`
					Status      string `json:"Status"`
					RecordCount int    `json:"RecordCount"`
					GradeTitle  string `json:"GradeTitle"`
				} `json:"DomainList"`
				Error *eoAPIError `json:"Error"`
			} `json:"Response"`
		}
		if err := json.Unmarshal(body, &resp); err != nil {
			return nil, fmt.Errorf("解析DNSPod响应失败: %w", err)
		}
		if resp.Response.Error != nil {
			return nil, resp.Response.Error
		}
		for _, d := range resp.Response.DomainList {
			status := "active"
			if d.Status != "ENABLE" {
				status = "paused"
			}
			zones = append(zones, model.DnsZone{
				ID:          prefixDNSPod + d.Name,
				Name:        d.Name,
				Status:      status,
				RecordCount: d.RecordCount,
				PlanName:    d.GradeTitle,
			})
		}
		offset += len(resp.Response.DomainList)
		if offset >= resp.Response.DomainCountInfo.AllTotal || len(resp.Response.DomainList) == 0 {
			break
		}
	}
	return zones, nil
}

// ListRecords 根据 zone 前缀路由到 EdgeOne 或 DNSPod
func (e *EdgeOneProvider) ListRecords(ctx context.Context, cfg map[string]string, zoneID string) ([]model.DnsRecord, error) {
	if err := e.ValidateConfig(cfg); err != nil {
		return nil, err
	}
	if strings.HasPrefix(zoneID, prefixDNSPod) {
		return e.listDnspodRecords(ctx, cfg, strings.TrimPrefix(zoneID, prefixDNSPod), zoneID)
	}
	return e.listEORecords(ctx, cfg, strings.TrimPrefix(zoneID, prefixEO), zoneID)
}

// listEORecords EdgeOne DNS 记录列表
func (e *EdgeOneProvider) listEORecords(ctx context.Context, cfg map[string]string, realZoneID, zoneID string) ([]model.DnsRecord, error) {
	var records []model.DnsRecord
	offset := 0
	for {
		params := map[string]interface{}{"ZoneId": realZoneID, "Offset": offset, "Limit": 100}
		body, err := e.doRequest(ctx, cfg, "DescribeDnsRecords", params)
		if err != nil {
			return nil, fmt.Errorf("获取DNS记录失败: %w", err)
		}
		var resp struct {
			Response struct {
				TotalCount int `json:"TotalCount"`
				DnsRecords []struct {
					DnsRecordId   string `json:"DnsRecordId"`
					DnsRecordType string `json:"DnsRecordType"`
					DnsRecordName string `json:"DnsRecordName"`
					Content       string `json:"Content"`
					TTL           int    `json:"TTL"`
					Priority      int    `json:"Priority"`
					Mode          string `json:"Mode"`
					Status        string `json:"Status"`
					Comment       string `json:"Comment"`
				} `json:"DnsRecords"`
				Error *eoAPIError `json:"Error"`
			} `json:"Response"`
		}
		if err := json.Unmarshal(body, &resp); err != nil {
			return nil, fmt.Errorf("解析响应失败: %w", err)
		}
		if resp.Response.Error != nil {
			return nil, resp.Response.Error
		}
		for _, r := range resp.Response.DnsRecords {
			status := "enable"
			if r.Status == "disabled" {
				status = "disable"
			}
			records = append(records, model.DnsRecord{
				ID: r.DnsRecordId, ZoneID: zoneID, Type: r.DnsRecordType,
				Name: r.DnsRecordName, Value: r.Content, TTL: r.TTL,
				Priority: r.Priority, Status: status, Remark: r.Comment,
			})
		}
		offset += len(resp.Response.DnsRecords)
		if offset >= resp.Response.TotalCount || len(resp.Response.DnsRecords) == 0 {
			break
		}
	}
	return records, nil
}

// listDnspodRecords DNSPod 记录列表
func (e *EdgeOneProvider) listDnspodRecords(ctx context.Context, cfg map[string]string, domain, zoneID string) ([]model.DnsRecord, error) {
	var records []model.DnsRecord
	offset := 0
	for {
		params := map[string]interface{}{"Domain": domain, "Offset": offset, "Limit": 100}
		body, err := e.doDnspodRequest(ctx, cfg, "DescribeRecordList", params)
		if err != nil {
			return nil, err
		}
		var resp struct {
			Response struct {
				RecordCountInfo struct {
					TotalCount int `json:"TotalCount"`
				} `json:"RecordCountInfo"`
				RecordList []struct {
					RecordId int    `json:"RecordId"`
					Name     string `json:"Name"`
					Type     string `json:"Type"`
					Value    string `json:"Value"`
					TTL      int    `json:"TTL"`
					MX       int    `json:"MX"`
					Line     string `json:"Line"`
					Weight   *int   `json:"Weight"`
					Status   string `json:"Status"`
					Remark   string `json:"Remark"`
				} `json:"RecordList"`
				Error *eoAPIError `json:"Error"`
			} `json:"Response"`
		}
		if err := json.Unmarshal(body, &resp); err != nil {
			return nil, fmt.Errorf("解析DNSPod响应失败: %w", err)
		}
		if resp.Response.Error != nil {
			// 无记录时 DNSPod 返回错误而不是空列表
			if resp.Response.Error.Code == "ResourceNotFound.NoDataOfRecord" {
				return records, nil
			}
			return nil, resp.Response.Error
		}
		for _, r := range resp.Response.RecordList {
			status := "enable"
			if r.Status != "ENABLE" {
				status = "disable"
			}
			fullName := r.Name + "." + domain
			if r.Name == "@" {
				fullName = domain
			}
			weight := 0
			if r.Weight != nil {
				weight = *r.Weight
			}
			records = append(records, model.DnsRecord{
				ID: strconv.Itoa(r.RecordId), ZoneID: zoneID, Type: r.Type,
				HostRecord: r.Name, Name: fullName, Value: r.Value, TTL: r.TTL,
				Priority: r.MX, Weight: weight, Line: r.Line, LineLabel: r.Line,
				Status: status, Remark: r.Remark,
			})
		}
		offset += len(resp.Response.RecordList)
		if offset >= resp.Response.RecordCountInfo.TotalCount || len(resp.Response.RecordList) == 0 {
			break
		}
	}
	return records, nil
}

// AddRecord 根据 zone 前缀路由
func (e *EdgeOneProvider) AddRecord(ctx context.Context, cfg map[string]string, zoneID string, req model.DnsRecordRequest) (*model.DnsRecord, error) {
	if err := e.ValidateConfig(cfg); err != nil {
		return nil, err
	}
	if strings.HasPrefix(zoneID, prefixDNSPod) {
		return e.addDnspodRecord(ctx, cfg, strings.TrimPrefix(zoneID, prefixDNSPod), zoneID, req)
	}
	return e.addEORecord(ctx, cfg, strings.TrimPrefix(zoneID, prefixEO), zoneID, req)
}

func (e *EdgeOneProvider) addEORecord(ctx context.Context, cfg map[string]string, realZoneID, zoneID string, req model.DnsRecordRequest) (*model.DnsRecord, error) {
	ttl := req.TTL
	if ttl <= 0 {
		ttl = 300
	}
	params := map[string]interface{}{
		"ZoneId": realZoneID, "DnsRecordType": req.Type,
		"DnsRecordName": req.Name, "Content": buildEdgeOneContent(req),
		"TTL": ttl, "Mode": "proxied",
	}
	if req.Priority > 0 {
		params["Priority"] = req.Priority
	}
	body, err := e.doRequest(ctx, cfg, "CreateDnsRecord", params)
	if err != nil {
		return nil, fmt.Errorf("添加DNS记录失败: %w", err)
	}
	var resp struct {
		Response struct {
			DnsRecordId string      `json:"DnsRecordId"`
			Error       *eoAPIError `json:"Error"`
		} `json:"Response"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("解析响应失败: %w", err)
	}
	if resp.Response.Error != nil {
		return nil, resp.Response.Error
	}
	log.Printf("[EdgeOne] DNS记录创建成功: %s %s → %s", req.Type, req.Name, req.Value)
	return &model.DnsRecord{
		ID: resp.Response.DnsRecordId, ZoneID: zoneID, Type: req.Type,
		Name: req.Name, Value: req.Value, TTL: ttl, Priority: req.Priority, Status: "enable",
	}, nil
}

func (e *EdgeOneProvider) addDnspodRecord(ctx context.Context, cfg map[string]string, domain, zoneID string, req model.DnsRecordRequest) (*model.DnsRecord, error) {
	rr := extractRR(req.Name, domain)
	ttl := req.TTL
	if ttl <= 0 {
		ttl = 600
	}
	params := map[string]interface{}{
		"Domain":     domain,
		"SubDomain":  rr,
		"RecordType": req.Type,
		"Value":      req.Value,
		"RecordLine": req.Line,
		"TTL":        ttl,
	}
	if req.Line == "" {
		params["RecordLine"] = "默认"
	}
	if req.Priority > 0 {
		params["MX"] = req.Priority
	}
	if req.Weight > 0 {
		params["Weight"] = req.Weight
	}
	body, err := e.doDnspodRequest(ctx, cfg, "CreateRecord", params)
	if err != nil {
		return nil, fmt.Errorf("添加DNSPod记录失败: %w", err)
	}
	var resp struct {
		Response struct {
			RecordId int         `json:"RecordId"`
			Error    *eoAPIError `json:"Error"`
		} `json:"Response"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("解析响应失败: %w", err)
	}
	if resp.Response.Error != nil {
		return nil, resp.Response.Error
	}
	log.Printf("[DNSPod] DNS记录创建成功: %s %s → %s", req.Type, req.Name, req.Value)
	return &model.DnsRecord{
		ID: strconv.Itoa(resp.Response.RecordId), ZoneID: zoneID, Type: req.Type,
		Name: req.Name, Value: req.Value, TTL: ttl, Line: req.Line, Status: "enable",
	}, nil
}

// UpdateRecord 根据 zone 前缀路由
func (e *EdgeOneProvider) UpdateRecord(ctx context.Context, cfg map[string]string, zoneID string, recordID string, req model.DnsRecordRequest) error {
	if err := e.ValidateConfig(cfg); err != nil {
		return err
	}
	if strings.HasPrefix(zoneID, prefixDNSPod) {
		return e.updateDnspodRecord(ctx, cfg, strings.TrimPrefix(zoneID, prefixDNSPod), recordID, req)
	}
	return e.updateEORecord(ctx, cfg, strings.TrimPrefix(zoneID, prefixEO), recordID, req)
}

func (e *EdgeOneProvider) updateEORecord(ctx context.Context, cfg map[string]string, realZoneID, recordID string, req model.DnsRecordRequest) error {
	ttl := req.TTL
	if ttl <= 0 {
		ttl = 300
	}
	dnsRecord := map[string]interface{}{
		"DnsRecordId": recordID, "DnsRecordType": req.Type,
		"DnsRecordName": req.Name, "Content": buildEdgeOneContent(req),
		"TTL": ttl, "Mode": "proxied",
	}
	if req.Priority > 0 {
		dnsRecord["Priority"] = req.Priority
	}
	params := map[string]interface{}{"ZoneId": realZoneID, "DnsRecord": dnsRecord}
	body, err := e.doRequest(ctx, cfg, "ModifyDnsRecords", params)
	if err != nil {
		return fmt.Errorf("更新DNS记录失败: %w", err)
	}
	if apiErr := e.parseError(body); apiErr != nil {
		return apiErr
	}
	log.Printf("[EdgeOne] DNS记录更新成功: RecordId=%s", recordID)
	return nil
}

func (e *EdgeOneProvider) updateDnspodRecord(ctx context.Context, cfg map[string]string, domain, recordID string, req model.DnsRecordRequest) error {
	rr := extractRR(req.Name, domain)
	ttl := req.TTL
	if ttl <= 0 {
		ttl = 600
	}
	recID, _ := strconv.Atoi(recordID)
	params := map[string]interface{}{
		"Domain":     domain,
		"RecordId":   recID,
		"SubDomain":  rr,
		"RecordType": req.Type,
		"Value":      req.Value,
		"RecordLine": req.Line,
		"TTL":        ttl,
	}
	if req.Line == "" {
		params["RecordLine"] = "默认"
	}
	if req.Priority > 0 {
		params["MX"] = req.Priority
	}
	if req.Weight > 0 {
		params["Weight"] = req.Weight
	}
	body, err := e.doDnspodRequest(ctx, cfg, "ModifyRecord", params)
	if err != nil {
		return fmt.Errorf("更新DNSPod记录失败: %w", err)
	}
	if apiErr := e.parseError(body); apiErr != nil {
		return apiErr
	}
	// 更新备注
	if req.Remark != "" {
		remarkParams := map[string]interface{}{
			"Domain": domain, "RecordId": recID, "Remark": req.Remark,
		}
		e.doDnspodRequest(ctx, cfg, "ModifyRecordRemark", remarkParams)
	}
	log.Printf("[DNSPod] DNS记录更新成功: RecordId=%s", recordID)
	return nil
}

// DeleteRecord 根据 zone 前缀路由
func (e *EdgeOneProvider) DeleteRecord(ctx context.Context, cfg map[string]string, zoneID string, recordID string) error {
	if err := e.ValidateConfig(cfg); err != nil {
		return err
	}
	if strings.HasPrefix(zoneID, prefixDNSPod) {
		return e.deleteDnspodRecord(ctx, cfg, strings.TrimPrefix(zoneID, prefixDNSPod), recordID)
	}
	return e.deleteEORecord(ctx, cfg, strings.TrimPrefix(zoneID, prefixEO), recordID)
}

func (e *EdgeOneProvider) deleteEORecord(ctx context.Context, cfg map[string]string, realZoneID, recordID string) error {
	params := map[string]interface{}{"ZoneId": realZoneID, "DnsRecordIds": []string{recordID}}
	body, err := e.doRequest(ctx, cfg, "DeleteDnsRecords", params)
	if err != nil {
		return fmt.Errorf("删除DNS记录失败: %w", err)
	}
	if apiErr := e.parseError(body); apiErr != nil {
		return apiErr
	}
	log.Printf("[EdgeOne] DNS记录删除成功: RecordId=%s", recordID)
	return nil
}

func (e *EdgeOneProvider) deleteDnspodRecord(ctx context.Context, cfg map[string]string, domain, recordID string) error {
	recID, _ := strconv.Atoi(recordID)
	params := map[string]interface{}{"Domain": domain, "RecordId": recID}
	body, err := e.doDnspodRequest(ctx, cfg, "DeleteRecord", params)
	if err != nil {
		return fmt.Errorf("删除DNSPod记录失败: %w", err)
	}
	if apiErr := e.parseError(body); apiErr != nil {
		return apiErr
	}
	log.Printf("[DNSPod] DNS记录删除成功: RecordId=%s", recordID)
	return nil
}

// extractRR 从完整域名提取主机记录
func extractRR(fullName, domain string) string {
	if fullName == domain || fullName == "@" {
		return "@"
	}
	suffix := "." + domain
	if strings.HasSuffix(fullName, suffix) {
		return strings.TrimSuffix(fullName, suffix)
	}
	return fullName
}

// ===== 内部工具 =====

// buildEdgeOneContent 构建 EdgeOne 记录内容
// SRV 格式: "priority weight port target"
// CAA 格式: "flag tag value"
func buildEdgeOneContent(req model.DnsRecordRequest) string {
	switch req.Type {
	case "SRV":
		// 确保 SRV 内容完整
		parts := strings.Fields(req.Value)
		if len(parts) >= 4 {
			return req.Value
		}
		// 不完整则加上 priority
		return fmt.Sprintf("%d %s", req.Priority, req.Value)
	case "CAA":
		// CAA 内容格式: flag tag "value"
		parts := strings.SplitN(req.Value, " ", 3)
		if len(parts) >= 3 {
			// 确保 value 部分有引号
			val := parts[2]
			if !strings.HasPrefix(val, "\"") {
				val = "\"" + val + "\""
			}
			flag, _ := strconv.Atoi(parts[0])
			return fmt.Sprintf("%d %s %s", flag, parts[1], val)
		}
		return req.Value
	default:
		return req.Value
	}
}

// =====================================================================
// SetRecordStatus 启用/禁用 DNS 记录
// =====================================================================

func (e *EdgeOneProvider) SetRecordStatus(ctx context.Context, cfg map[string]string, zoneID string, recordID string, enable bool) error {
	if strings.HasPrefix(zoneID, prefixDNSPod) {
		return e.setDnspodRecordStatus(ctx, cfg, zoneID, recordID, enable)
	}
	// EdgeOne 原生 DNS 不支持启用/禁用
	return fmt.Errorf("EdgeOne DNS 暂不支持启用/禁用记录功能")
}

func (e *EdgeOneProvider) setDnspodRecordStatus(ctx context.Context, cfg map[string]string, zoneID string, recordID string, enable bool) error {
	domainName := strings.TrimPrefix(zoneID, prefixDNSPod)
	status := "DISABLE"
	if enable {
		status = "ENABLE"
	}

	params := map[string]interface{}{
		"Domain":   domainName,
		"RecordId": recordID,
		"Status":   status,
	}
	body, err := e.doDnspodRequest(ctx, cfg, "ModifyRecordStatus", params)
	if err != nil {
		return fmt.Errorf("设置DNSPod记录状态失败: %w", err)
	}

	var resp struct {
		Response struct {
			Error *struct {
				Code    string `json:"Code"`
				Message string `json:"Message"`
			} `json:"Error"`
		} `json:"Response"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return fmt.Errorf("解析响应失败: %w", err)
	}
	if resp.Response.Error != nil {
		return fmt.Errorf("DNSPod API错误: [%s] %s", resp.Response.Error.Code, resp.Response.Error.Message)
	}

	log.Printf("[DNSPod] 记录 %s 状态已设置为 %s", recordID, status)
	return nil
}
