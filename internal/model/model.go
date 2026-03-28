package model

import "time"

// ===== 用户 =====

type User struct {
	ID        int64     `json:"id"`
	Username  string    `json:"username"`
	Password  string    `json:"-"`
	CreatedAt time.Time `json:"created_at"`
}

// ===== 站点 =====

type Site struct {
	ID             int64     `json:"id"`
	Domain         string    `json:"domain"`
	Origin         string    `json:"origin"`
	OriginProtocol string    `json:"origin_protocol"` // follow / http / https
	HTTPPort       int       `json:"http_port"`
	HTTPSPort      int       `json:"https_port"`
	OriginHost     string    `json:"origin_host"`
	Status         string    `json:"status"`           // pending / deploying / active / partial / failed
	ConfigAutoSync int       `json:"config_auto_sync"` // 0=关闭 1=开启，自动同步配置到提供商
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

type SiteDetail struct {
	Site
	Deployments  []Deployment  `json:"deployments"`
	DNSRecords   []DNSRecord   `json:"dns_records"`
	Certificates []Certificate `json:"certificates"`
}

// ===== 部署记录 =====

type Deployment struct {
	ID             int64     `json:"id"`
	SiteID         int64     `json:"site_id"`
	Provider       string    `json:"provider"`
	ConfigID       int64     `json:"config_id"`   // 关联的提供商配置ID
	ConfigName     string    `json:"config_name"` // 配置名称（展示用）
	Status         string    `json:"status"`      // pending / deploying / active / failed
	ProviderSiteID string    `json:"provider_site_id"`
	CDNCname       string    `json:"cdn_cname"`
	DeployParams   string    `json:"deploy_params"` // JSON: 部署时的额外参数（mode/zone_id/instance_id等）
	ErrorMessage   string    `json:"error_message"`
	DeployLog      string    `json:"deploy_log"` // 部署过程日志（逐行累积）
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// ===== DNS记录 =====

type DNSRecord struct {
	ID           int64     `json:"id"`
	SiteID       int64     `json:"site_id"`
	DeploymentID int64     `json:"deployment_id"`
	RecordType   string    `json:"record_type"` // CNAME / TXT / A / NS
	Name         string    `json:"name"`
	Value        string    `json:"value"`
	Purpose      string    `json:"purpose"` // traffic / validation
	Status       string    `json:"status"`  // pending / active
	CreatedAt    time.Time `json:"created_at"`
}

// ===== 证书 =====

type Certificate struct {
	ID           int64      `json:"id"`
	SiteID       int64      `json:"site_id"`
	DeploymentID int64      `json:"deployment_id"`
	Provider     string     `json:"provider"`
	Status       string     `json:"status"` // pending / issuing / active / failed
	Domain       string     `json:"domain"`
	CertID       string     `json:"cert_id"`
	ExpiresAt    *time.Time `json:"expires_at"`
	ErrorMessage string     `json:"error_message"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
}

// ===== CDN 提供商配置（支持同一提供商多账户） =====

type ProviderConfig struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`     // 用户自定义名称，如"我的CF账户"
	Provider  string    `json:"provider"` // cloudflare / aliyun / edgeone
	Config    string    `json:"config"`   // JSON: 各提供商的 API 凭证
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ===== 请求 / 响应 =====

type LoginRequest struct {
	Username string `json:"username" binding:"required"`
	Password string `json:"password" binding:"required"`
}

type LoginResponse struct {
	Token string `json:"token"`
	User  User   `json:"user"`
}

type CreateSiteRequest struct {
	Domain         string                       `json:"domain" binding:"required"`
	Origin         string                       `json:"origin" binding:"required"`
	OriginProtocol string                       `json:"origin_protocol"` // follow / http / https，默认 follow
	HTTPPort       int                          `json:"http_port"`       // HTTP回源端口，默认80
	HTTPSPort      int                          `json:"https_port"`      // HTTPS回源端口，默认443
	OriginHost     string                       `json:"origin_host"`     // 回源HOST头，默认为域名
	ConfigIDs      []int64                      `json:"config_ids"`      // 创建时选择的CDN账户，创建后自动部署
	DeployParams   map[string]map[string]string `json:"deploy_params"`   // config_id(string) -> 部署参数
}

type DeployRequest struct {
	ConfigIDs    []int64                      `json:"config_ids" binding:"required"` // 提供商配置ID列表
	DeployParams map[string]map[string]string `json:"deploy_params"`                 // config_id(string) -> 部署参数
}

type SaveProviderConfigRequest struct {
	ID       int64  `json:"id"`                          // 编辑时传，新增时不传
	Name     string `json:"name" binding:"required"`     // 账户名称
	Provider string `json:"provider" binding:"required"` // 提供商类型
	Config   string `json:"config" binding:"required"`   // JSON配置
	Enabled  bool   `json:"enabled"`
}

// ===== 回源配置 =====

// OriginConfig 回源配置，传递给各CDN提供商
type OriginConfig struct {
	Origin         string // 回源地址（IP或域名，不含协议和端口）
	OriginProtocol string // 回源协议: follow / http / https
	HTTPPort       int    // HTTP回源端口，默认80
	HTTPSPort      int    // HTTPS回源端口，默认443
	OriginHost     string // 回源HOST头
}

// ===== CDN Provider 接口结果类型 =====

type ProviderInfo struct {
	Name              string        `json:"name"`
	DisplayName       string        `json:"display_name"`
	Description       string        `json:"description"`
	ConfigFields      []ConfigField `json:"config_fields"`           // 账户认证字段（保存在提供商配置中）
	DeployFields      []ConfigField `json:"deploy_fields,omitempty"` // 部署时配置字段（保存在部署记录中）
	SupportsOptDomain bool          `json:"supports_opt_domain"`     // 是否支持优选域名（境内外分流接入点）
	SupportsDNS       bool          `json:"supports_dns"`            // 是否同时支持 DNS 管理
}

type ConfigField struct {
	Key         string         `json:"key"`
	Label       string         `json:"label"`
	Type        string         `json:"type"` // text / secret / select，默认 text
	Required    bool           `json:"required"`
	Secret      bool           `json:"secret"`
	Placeholder string         `json:"placeholder"`
	Options     []SelectOption `json:"options,omitempty"`   // Type=select 时的静态选项
	Fetchable   bool           `json:"fetchable,omitempty"` // true=选项通过API动态加载
}

type SelectOption struct {
	Value string `json:"value"`
	Label string `json:"label"`
	Desc  string `json:"desc,omitempty"` // 副标题（供 CustomSelect 展示）
}

type AddDomainResult struct {
	ProviderSiteID string   `json:"provider_site_id"`
	CNAME          string   `json:"cname"`
	NameServers    []string `json:"name_servers,omitempty"`
	Status         string   `json:"status"`
}

type DomainStatusResult struct {
	Status  string `json:"status"`
	Message string `json:"message"`
}

type CertRequestResult struct {
	CertID     string          `json:"cert_id"`
	Status     string          `json:"status"`
	DNSRecords []CertDNSRecord `json:"dns_records,omitempty"`
}

type CertDNSRecord struct {
	Type  string `json:"type"`
	Name  string `json:"name"`
	Value string `json:"value"`
}

type CertStatusResult struct {
	Status    string     `json:"status"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	Message   string     `json:"message"`
}

// ===== DNS 解析管理 =====

// DnsZone 域名区域（从提供商同步）
type DnsZone struct {
	ID          string `json:"id"`                  // 提供商侧 Zone ID
	Name        string `json:"name"`                // 域名
	Status      string `json:"status"`              // active / pending / paused
	RecordCount int    `json:"record_count"`        // 记录数量
	PlanName    string `json:"plan_name,omitempty"` // 套餐名称（免费版/个人版/ESA 等）
}

// DnsRecord DNS 解析记录
type DnsRecord struct {
	ID         string `json:"id"`                   // 提供商侧记录 ID
	ZoneID     string `json:"zone_id"`              // 所属 Zone ID
	Type       string `json:"type"`                 // A / AAAA / CNAME / MX / TXT / NS / SRV / CAA
	HostRecord string `json:"host_record"`          // 主机记录（如 www / @ / mail）
	Name       string `json:"name"`                 // 完整域名（如 www.example.com）
	Value      string `json:"value"`                // 记录值
	TTL        int    `json:"ttl"`                  // TTL 秒数，0 或 1 表示自动
	Priority   int    `json:"priority,omitempty"`   // MX / SRV 优先级
	Weight     int    `json:"weight"`               // 负载均衡权重（0=未启用，1-100）
	Proxied    *bool  `json:"proxied,omitempty"`    // 是否开启 CDN 代理（Cloudflare 特有，nil=不显示）
	Line       string `json:"line,omitempty"`       // 解析线路
	LineLabel  string `json:"line_label,omitempty"` // 解析线路显示名称
	Status     string `json:"status,omitempty"`     // enable / disable
	Remark     string `json:"remark,omitempty"`     // 备注
}

// DnsRecordRequest 添加/更新 DNS 记录请求
type DnsRecordRequest struct {
	Type     string `json:"type" binding:"required"`
	Name     string `json:"name" binding:"required"`
	Value    string `json:"value" binding:"required"`
	TTL      int    `json:"ttl"`
	Priority int    `json:"priority,omitempty"`
	Weight   int    `json:"weight,omitempty"`
	Proxied  bool   `json:"proxied,omitempty"`
	Line     string `json:"line,omitempty"`
	Remark   string `json:"remark,omitempty"`
}

// DnsLine 解析线路定义
type DnsLine struct {
	Value string `json:"value"`
	Label string `json:"label"`
	Group string `json:"group"` // default / geo / isp / cloud / search / custom
}

// DnsProviderFeatures 每个 DNS 提供商声明自己支持的 UI 特性
type DnsProviderFeatures struct {
	HasProxy     bool   `json:"has_proxy"`               // 是否支持代理开关（CF 小黄云 / ESA 代理）
	ProxyName    string `json:"proxy_name,omitempty"`    // 代理功能名称（如 "Proxied" / "ESA 代理"）
	HasLine      bool   `json:"has_line"`                // 是否支持解析线路
	HasWeight    bool   `json:"has_weight"`              // 是否支持负载均衡权重
	HasRemark    bool   `json:"has_remark"`              // 是否支持备注
	MainlandLine string `json:"mainland_line,omitempty"` // 中国大陆线路值（空=不支持大陆/海外分流）
}

// ===== SSL 证书管理 =====

// SSLCertificate 证书记录
type SSLCertificate struct {
	ID          int64      `json:"id"`
	Domains     string     `json:"domains"`               // 逗号分隔的域名列表
	Brand       string     `json:"brand"`                 // 颁发机构
	Source      string     `json:"source"`                // upload / acme
	Certificate string     `json:"certificate,omitempty"` // PEM 证书（查看时返回）
	PrivateKey  string     `json:"private_key,omitempty"` // PEM 私钥（查看时返回）
	IssuerCert  string     `json:"issuer_cert,omitempty"` // CA 中间证书
	Fingerprint string     `json:"fingerprint"`           // SHA256 指纹
	NotBefore   *time.Time `json:"not_before"`            // 生效时间
	NotAfter    *time.Time `json:"not_after"`             // 到期时间
	Status      string     `json:"status"`                // valid / expired / pending / issuing / failed
	ErrorMsg    string     `json:"error_message"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	// 以下字段从 PEM 解析，不存数据库
	SANDomains []string `json:"san_domains,omitempty"`
	SANIPs     []string `json:"san_ips,omitempty"`
	SANEmails  []string `json:"san_emails,omitempty"`
	Algorithm  string   `json:"algorithm,omitempty"`
	KeySize    string   `json:"key_size,omitempty"`
}

// AcmeAccount ACME 账户
type AcmeAccount struct {
	ID            int64     `json:"id"`
	Email         string    `json:"email"`
	CA            string    `json:"ca"`
	PrivateKeyPEM string    `json:"-"`
	Registration  string    `json:"-"`
	Kid           string    `json:"kid"`
	HmacEncoded   string    `json:"-"`
	CADirURL      string    `json:"ca_dir_url"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// UploadCertRequest 上传证书请求
type UploadCertRequest struct {
	Certificate string `json:"certificate" binding:"required"`
	PrivateKey  string `json:"private_key" binding:"required"`
}

// ApplyCertRequest 申请证书请求
type ApplyCertRequest struct {
	Domains   string `json:"domains" binding:"required"`   // 逗号分隔的域名
	CA        string `json:"ca" binding:"required"`        // letsencrypt / zerossl / litessl
	ConfigID  int64  `json:"config_id" binding:"required"` // DNS 提供商配置 ID
	Algorithm string `json:"algorithm"`                    // EC256 / RSA2048，默认 EC256
}

// ===== 自签证书 =====

// SelfSignedCert 自签证书记录
type SelfSignedCert struct {
	ID           int64   `json:"id"`
	Name         string  `json:"name"`
	CertType     string  `json:"cert_type"` // root_ca / intermediate_ca / cert
	Algorithm    string  `json:"algorithm"` // 存储完整标识：rsa_2048 / ecdsa_p256 / ed25519 / sm2 等
	SubjectCN    string  `json:"subject_cn"`
	SubjectO     string  `json:"subject_o"`
	SubjectOU    string  `json:"subject_ou"`
	SubjectC     string  `json:"subject_c"`
	SubjectST    string  `json:"subject_st"`
	SubjectL     string  `json:"subject_l"`
	Domains      string  `json:"domains"`
	IPs          string  `json:"ips"`
	Emails       string  `json:"emails"`
	Purpose      string  `json:"purpose"` // server / client / email（仅 cert 类型）
	IssuerID     *int64  `json:"issuer_id"`
	IssuerName   string  `json:"issuer_name"` // 签发者名称（查询时填充）
	Certificate  string  `json:"certificate"`
	PrivateKey   string  `json:"private_key"`
	SerialNumber string  `json:"serial_number"`
	Fingerprint  string  `json:"fingerprint"`
	NotBefore    *string `json:"not_before"`
	NotAfter     *string `json:"not_after"`
	ValidityDays int     `json:"validity_days"`
	IsCA         bool    `json:"is_ca"`
	KeyUsage     string  `json:"key_usage"`
	ExtKeyUsage  string  `json:"ext_key_usage"`
	CreatedAt    string  `json:"created_at"`
}

// CreateSelfSignedRequest 创建自签证书请求
type CreateSelfSignedRequest struct {
	Name         string `json:"name" binding:"required"`
	CertType     string `json:"cert_type" binding:"required"` // root_ca / intermediate_ca / cert
	Algorithm    string `json:"algorithm" binding:"required"` // rsa / ecdsa / ed25519 / sm2
	KeySize      string `json:"key_size" binding:"required"`  // 2048/3072/4096 / p256/p384/p521 / 256
	Purpose      string `json:"purpose"`                      // server / client / email（仅 cert 类型）
	SubjectCN    string `json:"subject_cn" binding:"required"`
	SubjectO     string `json:"subject_o"`
	SubjectOU    string `json:"subject_ou"`
	SubjectC     string `json:"subject_c"`
	SubjectST    string `json:"subject_st"`
	SubjectL     string `json:"subject_l"`
	Domains      string `json:"domains"`       // 逗号分隔的域名（server 用途）
	IPs          string `json:"ips"`           // 逗号分隔的 IP（server 用途）
	Emails       string `json:"emails"`        // 逗号分隔的邮箱（email 用途）
	IssuerID     *int64 `json:"issuer_id"`     // 签发者 ID（root_ca 为空）
	ValidityDays int    `json:"validity_days"` // 有效期（天）
}

// CAProvider ACME CA 提供商信息
type CAProvider struct {
	Name         string `json:"name"`
	DisplayName  string `json:"display_name"`
	DirURL       string `json:"dir_url"`
	Wildcard     bool   `json:"wildcard"`      // 是否支持泛域名
	NeedEAB      bool   `json:"need_eab"`      // 是否需要 EAB
	ValidityDays int    `json:"validity_days"` // 证书有效期（天）
}
