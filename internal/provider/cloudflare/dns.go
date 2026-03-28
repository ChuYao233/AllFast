package cloudflare

import (
	"allfast/internal/model"
	"allfast/internal/provider"
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
)

var cfDisabledRecordStore = struct {
	mu   sync.RWMutex
	data map[string]model.DnsRecordRequest
}{
	data: map[string]model.DnsRecordRequest{},
}

// DNS 能力注册
func init() {
	provider.RegisterDNS("cloudflare", &CloudflareProvider{})
}

// Features Cloudflare 支持代理（小黄云），不支持线路/权重
func (c *CloudflareProvider) Features(zoneID string) model.DnsProviderFeatures {
	return model.DnsProviderFeatures{
		HasProxy:  true,
		ProxyName: "Proxied",
		HasLine:   false,
		HasWeight: false,
		HasRemark: true,
	}
}

// SupportedRecordTypes Cloudflare 支持的 DNS 记录类型
func (c *CloudflareProvider) SupportedRecordTypes() []string {
	return []string{
		"A", "AAAA", "CNAME", "MX", "TXT", "NS",
		"SRV", "CAA", "HTTPS", "SVCB",
	}
}

// SupportedLines Cloudflare 不支持解析线路
func (c *CloudflareProvider) SupportedLines() []model.DnsLine {
	return nil
}

// ListZones 列出账号下所有 Zone（分页获取）
func (c *CloudflareProvider) ListZones(ctx context.Context, cfg map[string]string) ([]model.DnsZone, error) {
	if err := c.validateAuth(cfg); err != nil {
		return nil, err
	}

	var zones []model.DnsZone
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
			zones = append(zones, model.DnsZone{
				ID:       z.ID,
				Name:     z.Name,
				Status:   z.Status,
				PlanName: cfPlanShortName(z.Plan.Name),
			})
		}

		if page >= resp.ResultInfo.TotalPages {
			break
		}
		page++
	}
	return zones, nil
}

// ListRecords 列出指定 Zone 的所有 DNS 记录（分页获取）
func (c *CloudflareProvider) ListRecords(ctx context.Context, cfg map[string]string, zoneID string) ([]model.DnsRecord, error) {
	if err := c.validateAuth(cfg); err != nil {
		return nil, err
	}

	var records []model.DnsRecord
	page := 1
	for {
		url := fmt.Sprintf("%s/zones/%s/dns_records?page=%d&per_page=100", cfAPIBase, zoneID, page)
		body, err := c.doRequest(ctx, cfg, "GET", url, nil)
		if err != nil {
			return nil, fmt.Errorf("获取DNS记录失败: %w", err)
		}

		var resp struct {
			Success    bool `json:"success"`
			ResultInfo struct {
				TotalPages int `json:"total_pages"`
			} `json:"result_info"`
			Result []struct {
				ID       string `json:"id"`
				Type     string `json:"type"`
				Name     string `json:"name"`
				Content  string `json:"content"`
				TTL      int    `json:"ttl"`
				Priority *int   `json:"priority"`
				Proxied  bool   `json:"proxied"`
				Comment  string `json:"comment"`
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
			rec := model.DnsRecord{
				ID:      r.ID,
				ZoneID:  zoneID,
				Type:    r.Type,
				Name:    r.Name,
				Value:   r.Content,
				TTL:     r.TTL,
				Proxied: &r.Proxied,
				Remark:  r.Comment,
				Status:  "enable",
			}
			if r.Priority != nil {
				rec.Priority = *r.Priority
			}
			records = append(records, rec)
		}

		if page >= resp.ResultInfo.TotalPages {
			break
		}
		page++
	}
	return records, nil
}

// AddRecord 添加 DNS 记录
func (c *CloudflareProvider) AddRecord(ctx context.Context, cfg map[string]string, zoneID string, req model.DnsRecordRequest) (*model.DnsRecord, error) {
	if err := c.validateAuth(cfg); err != nil {
		return nil, err
	}

	payload := map[string]interface{}{
		"type":    req.Type,
		"name":    req.Name,
		"content": req.Value,
		"ttl":     cfTTL(req.TTL),
		"proxied": req.Proxied,
	}
	if req.Priority > 0 {
		payload["priority"] = req.Priority
	}
	if req.Remark != "" {
		payload["comment"] = req.Remark
	}
	// SRV 记录需要特殊处理 data 字段
	if req.Type == "SRV" {
		c.buildSRVPayload(payload, req)
	}

	url := fmt.Sprintf("%s/zones/%s/dns_records", cfAPIBase, zoneID)
	body, err := c.doRequest(ctx, cfg, "POST", url, payload)
	if err != nil {
		return nil, fmt.Errorf("添加DNS记录失败: %w", err)
	}

	var resp struct {
		Success bool `json:"success"`
		Result  struct {
			ID       string `json:"id"`
			Type     string `json:"type"`
			Name     string `json:"name"`
			Content  string `json:"content"`
			TTL      int    `json:"ttl"`
			Priority *int   `json:"priority"`
			Proxied  bool   `json:"proxied"`
		} `json:"result"`
		Errors []cfError `json:"errors"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("解析响应失败: %w", err)
	}
	if !resp.Success {
		return nil, cfErrorMsg(resp.Errors)
	}

	rec := &model.DnsRecord{
		ID:      resp.Result.ID,
		ZoneID:  zoneID,
		Type:    resp.Result.Type,
		Name:    resp.Result.Name,
		Value:   resp.Result.Content,
		TTL:     resp.Result.TTL,
		Proxied: &resp.Result.Proxied,
		Status:  "enable",
	}
	if resp.Result.Priority != nil {
		rec.Priority = *resp.Result.Priority
	}
	return rec, nil
}

// UpdateRecord 更新 DNS 记录
func (c *CloudflareProvider) UpdateRecord(ctx context.Context, cfg map[string]string, zoneID string, recordID string, req model.DnsRecordRequest) error {
	if err := c.validateAuth(cfg); err != nil {
		return err
	}

	payload := map[string]interface{}{
		"type":    req.Type,
		"name":    req.Name,
		"content": req.Value,
		"ttl":     cfTTL(req.TTL),
		"proxied": req.Proxied,
	}
	if req.Priority > 0 {
		payload["priority"] = req.Priority
	}
	if req.Remark != "" {
		payload["comment"] = req.Remark
	}
	if req.Type == "SRV" {
		c.buildSRVPayload(payload, req)
	}

	url := fmt.Sprintf("%s/zones/%s/dns_records/%s", cfAPIBase, zoneID, recordID)
	body, err := c.doRequest(ctx, cfg, "PATCH", url, payload)
	if err != nil {
		return fmt.Errorf("更新DNS记录失败: %w", err)
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

// DeleteRecord 删除 DNS 记录
func (c *CloudflareProvider) DeleteRecord(ctx context.Context, cfg map[string]string, zoneID string, recordID string) error {
	if err := c.validateAuth(cfg); err != nil {
		return err
	}

	url := fmt.Sprintf("%s/zones/%s/dns_records/%s", cfAPIBase, zoneID, recordID)
	body, err := c.doRequest(ctx, cfg, "DELETE", url, nil)
	if err != nil {
		return fmt.Errorf("删除DNS记录失败: %w", err)
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

// ===== 内部工具 =====

// cfPlanShortName 将 Cloudflare 套餐全名映射为简短显示名
func cfPlanShortName(name string) string {
	switch {
	case strings.Contains(name, "Enterprise"):
		return "Enterprise"
	case strings.Contains(name, "Business"):
		return "Business"
	case strings.Contains(name, "Pro"):
		return "Pro"
	case strings.Contains(name, "Free"):
		return "Free"
	default:
		return name
	}
}

// cfTTL 处理 TTL 值，0 表示自动
func cfTTL(ttl int) int {
	if ttl <= 0 {
		return 1 // Cloudflare: 1 = automatic
	}
	return ttl
}

// buildSRVPayload SRV 记录需要拆分 value 到 data 字段
// SRV value 格式: priority weight port target (如 "10 5 8080 srv.example.com")
func (c *CloudflareProvider) buildSRVPayload(payload map[string]interface{}, req model.DnsRecordRequest) {
	parts := strings.Fields(req.Value)
	if len(parts) >= 4 {
		priority, _ := strconv.Atoi(parts[0])
		weight, _ := strconv.Atoi(parts[1])
		port, _ := strconv.Atoi(parts[2])
		target := parts[3]
		// 从 name 提取 service 和 protocol: _sip._tcp.example.com
		nameParts := strings.SplitN(req.Name, ".", 3)
		service := ""
		proto := ""
		srvName := req.Name
		if len(nameParts) >= 3 {
			service = strings.TrimPrefix(nameParts[0], "_")
			proto = strings.TrimPrefix(nameParts[1], "_")
			srvName = nameParts[2]
		}
		payload["data"] = map[string]interface{}{
			"service":  service,
			"proto":    proto,
			"name":     srvName,
			"priority": priority,
			"weight":   weight,
			"port":     port,
			"target":   target,
		}
		// SRV 不用 content 字段
		delete(payload, "content")
	}
}

// =====================================================================
// SetRecordStatus 启用/禁用 DNS 记录
// =====================================================================

func (c *CloudflareProvider) SetRecordStatus(ctx context.Context, cfg map[string]string, zoneID string, recordID string, enable bool) error {
	if err := c.validateAuth(cfg); err != nil {
		return err
	}

	key := zoneID + ":" + recordID

	if !enable {
		// 禁用：先读取记录详情，备份后删除
		req, err := c.getRecordAsRequest(ctx, cfg, zoneID, recordID)
		if err != nil {
			return fmt.Errorf("读取记录详情失败: %w", err)
		}
		if err := c.DeleteRecord(ctx, cfg, zoneID, recordID); err != nil {
			return err
		}
		cfDisabledRecordStore.mu.Lock()
		cfDisabledRecordStore.data[key] = req
		cfDisabledRecordStore.mu.Unlock()
		return nil
	}

	// 启用：从备份恢复（重新添加记录）
	cfDisabledRecordStore.mu.RLock()
	req, ok := cfDisabledRecordStore.data[key]
	cfDisabledRecordStore.mu.RUnlock()
	if !ok {
		return fmt.Errorf("未找到已禁用记录的备份，请先同步后重试")
	}
	if _, err := c.AddRecord(ctx, cfg, zoneID, req); err != nil {
		return fmt.Errorf("恢复记录失败: %w", err)
	}
	cfDisabledRecordStore.mu.Lock()
	delete(cfDisabledRecordStore.data, key)
	cfDisabledRecordStore.mu.Unlock()
	return nil
}

func (c *CloudflareProvider) getRecordAsRequest(ctx context.Context, cfg map[string]string, zoneID string, recordID string) (model.DnsRecordRequest, error) {
	url := fmt.Sprintf("%s/zones/%s/dns_records/%s", cfAPIBase, zoneID, recordID)
	body, err := c.doRequest(ctx, cfg, "GET", url, nil)
	if err != nil {
		return model.DnsRecordRequest{}, err
	}

	var resp struct {
		Success bool `json:"success"`
		Result  struct {
			Type     string `json:"type"`
			Name     string `json:"name"`
			Content  string `json:"content"`
			TTL      int    `json:"ttl"`
			Priority *int   `json:"priority"`
			Proxied  bool   `json:"proxied"`
			Comment  string `json:"comment"`
			Data     struct {
				Priority int    `json:"priority"`
				Weight   int    `json:"weight"`
				Port     int    `json:"port"`
				Target   string `json:"target"`
			} `json:"data"`
		} `json:"result"`
		Errors []cfError `json:"errors"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return model.DnsRecordRequest{}, fmt.Errorf("解析响应失败: %w", err)
	}
	if !resp.Success {
		return model.DnsRecordRequest{}, cfErrorMsg(resp.Errors)
	}

	req := model.DnsRecordRequest{
		Type:    resp.Result.Type,
		Name:    resp.Result.Name,
		Value:   resp.Result.Content,
		TTL:     resp.Result.TTL,
		Proxied: resp.Result.Proxied,
		Remark:  resp.Result.Comment,
	}
	if resp.Result.Priority != nil {
		req.Priority = *resp.Result.Priority
	}

	// SRV 记录优先使用 data 结构重建 value
	if req.Type == "SRV" && resp.Result.Data.Target != "" {
		req.Value = fmt.Sprintf("%d %d %d %s", resp.Result.Data.Priority, resp.Result.Data.Weight, resp.Result.Data.Port, resp.Result.Data.Target)
		req.Priority = resp.Result.Data.Priority
		req.Weight = resp.Result.Data.Weight
	}

	return req, nil
}
