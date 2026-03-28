package provider

import (
	"allfast/internal/model"
	"context"
	"fmt"
	"time"
)

// CDNProvider CDN提供商统一接口
type CDNProvider interface {
	// Name 提供商标识
	Name() string
	// Info 提供商元信息（含配置字段定义）
	Info() model.ProviderInfo
	// ValidateConfig 校验API凭证配置是否完整
	ValidateConfig(cfg map[string]string) error
	// FetchFieldOptions 动态加载配置字段的选项（如套餐实例列表），不支持的字段返回nil
	FetchFieldOptions(ctx context.Context, cfg map[string]string, fieldKey string) ([]model.SelectOption, error)
	// AddDomain 将域名添加到CDN加速，返回CNAME等信息
	AddDomain(ctx context.Context, cfg map[string]string, domain string, originCfg model.OriginConfig) (*model.AddDomainResult, error)
	// GetDomainStatus 查询域名在CDN侧的状态
	GetDomainStatus(ctx context.Context, cfg map[string]string, domain string, providerSiteID string) (*model.DomainStatusResult, error)
	// DeleteDomain 从CDN移除域名
	DeleteDomain(ctx context.Context, cfg map[string]string, domain string, providerSiteID string) error
	// SetupCertificate 为域名申请/配置SSL证书
	SetupCertificate(ctx context.Context, cfg map[string]string, domain string, providerSiteID string) (*model.CertRequestResult, error)
	// GetCertificateStatus 查询证书签发状态
	GetCertificateStatus(ctx context.Context, cfg map[string]string, domain string, certID string) (*model.CertStatusResult, error)
	// UpdateOriginConfig 更新提供商侧的回源配置（源站、协议、端口、Host）
	UpdateOriginConfig(ctx context.Context, cfg map[string]string, domain string, providerSiteID string, originCfg model.OriginConfig) error
	// EnableEdgeCert 启用服务商边缘免费证书
	EnableEdgeCert(ctx context.Context, cfg map[string]string, domain string, providerSiteID string) error
	// DeployCertificate 将自定义证书部署到 CDN 平台（certPEM 和 keyPEM 为 PEM 格式）
	DeployCertificate(ctx context.Context, cfg map[string]string, domain string, providerSiteID string, certPEM string, keyPEM string) error
}

// DNSProvider DNS 解析管理接口
type DNSProvider interface {
	// ListZones 列出所有托管的域名区域
	ListZones(ctx context.Context, cfg map[string]string) ([]model.DnsZone, error)
	// ListRecords 列出指定 Zone 的所有 DNS 记录
	ListRecords(ctx context.Context, cfg map[string]string, zoneID string) ([]model.DnsRecord, error)
	// AddRecord 添加 DNS 记录
	AddRecord(ctx context.Context, cfg map[string]string, zoneID string, req model.DnsRecordRequest) (*model.DnsRecord, error)
	// UpdateRecord 更新 DNS 记录
	UpdateRecord(ctx context.Context, cfg map[string]string, zoneID string, recordID string, req model.DnsRecordRequest) error
	// DeleteRecord 删除 DNS 记录
	DeleteRecord(ctx context.Context, cfg map[string]string, zoneID string, recordID string) error
	// SetRecordStatus 启用/禁用 DNS 记录 (enable=true 启用, enable=false 禁用)
	SetRecordStatus(ctx context.Context, cfg map[string]string, zoneID string, recordID string, enable bool) error
	// SupportedRecordTypes 返回支持的记录类型列表
	SupportedRecordTypes() []string
	// SupportedLines 返回支持的解析线路列表（不支持线路的返回空）
	SupportedLines() []model.DnsLine
	// Features 返回该提供商支持的 UI 特性（可根据 zoneID 区分）
	Features(zoneID string) model.DnsProviderFeatures
}

// ===== CDN 提供商注册表 =====

var providers = map[string]CDNProvider{}

// Register 注册 CDN 提供商
func Register(p CDNProvider) {
	providers[p.Name()] = p
}

// Get 按名称获取 CDN 提供商
func Get(name string) (CDNProvider, error) {
	p, ok := providers[name]
	if !ok {
		return nil, fmt.Errorf("未知的CDN提供商: %s", name)
	}
	return p, nil
}

// ListAll 获取所有已注册的 CDN 提供商
func ListAll() []CDNProvider {
	result := make([]CDNProvider, 0, len(providers))
	for _, p := range providers {
		result = append(result, p)
	}
	return result
}

// ===== DNS 提供商注册表 =====

var dnsProviders = map[string]DNSProvider{}

// RegisterDNS 注册 DNS 提供商
func RegisterDNS(name string, p DNSProvider) {
	dnsProviders[name] = p
}

// GetDNS 按名称获取 DNS 提供商
func GetDNS(name string) (DNSProvider, error) {
	p, ok := dnsProviders[name]
	if !ok {
		return nil, fmt.Errorf("提供商 %s 不支持DNS管理", name)
	}
	return p, nil
}

// ListAllDNS 获取所有支持 DNS 管理的提供商名称
func ListAllDNS() []string {
	names := make([]string, 0, len(dnsProviders))
	for name := range dnsProviders {
		names = append(names, name)
	}
	return names
}

// ===== 流量统计提供商注册表 =====

// StatsProvider 流量统计接口（各 CDN 提供商实现）
type StatsProvider interface {
	// GetTimeSeries 获取指定 zone 的小时粒度时间序列数据
	GetTimeSeries(ctx context.Context, cfg map[string]string, zoneID string, from, to time.Time) ([]model.StatPoint, error)
	// GetGeoDistribution 获取地区分布统计
	GetGeoDistribution(ctx context.Context, cfg map[string]string, zoneID string, from, to time.Time) ([]model.GeoPoint, error)
}

var statsProviders = map[string]StatsProvider{}

// RegisterStats 注册流量统计提供商
func RegisterStats(name string, p StatsProvider) {
	statsProviders[name] = p
}

// GetStats 按名称获取流量统计提供商，不存在返回 nil
func GetStats(name string) StatsProvider {
	return statsProviders[name]
}

// Init 由各子包 init() 自动注册，此处保留为空以兼容调用方
func Init() {}
