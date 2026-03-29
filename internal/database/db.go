package database

import (
	"allfast/internal/config"
	"database/sql"
	"log"
	"time"

	_ "github.com/lib/pq"
	"golang.org/x/crypto/bcrypt"
)

var DB *sql.DB         // 主业务库（PostgreSQL）
var DnsCacheDB *sql.DB // DNS 缓存使用同一个 PG 连接

// Init 初始化 PostgreSQL 连接和表结构
func Init() error {
	var err error

	dsn := config.C.Database.DSN()
	DB, err = sql.Open("postgres", dsn)
	if err != nil {
		return err
	}
	DB.SetMaxOpenConns(config.C.Database.MaxOpenConns)
	DB.SetMaxIdleConns(config.C.Database.MaxIdleConns)
	DB.SetConnMaxLifetime(30 * time.Minute) // 防止连接被 NAT/防火墙静默关闭
	DB.SetConnMaxIdleTime(10 * time.Minute)
	if err = DB.Ping(); err != nil {
		return err
	}

	// DNS 缓存复用同一个 PG 连接
	DnsCacheDB = DB

	if err = createTables(); err != nil {
		return err
	}

	if err = createDnsCacheTables(); err != nil {
		return err
	}

	if err = createStatsTables(); err != nil {
		return err
	}

	if err = seedAdmin(); err != nil {
		return err
	}

	log.Println("数据库初始化完成 (PostgreSQL)")
	return nil
}

// Close 关闭数据库连接
func Close() {
	if DB != nil {
		DB.Close()
	}
}

// createTables 创建业务表（PostgreSQL 语法）
func createTables() error {
	tables := []string{
		`CREATE TABLE IF NOT EXISTS users (
			id SERIAL PRIMARY KEY,
			username TEXT NOT NULL UNIQUE,
			password TEXT NOT NULL,
			created_at TIMESTAMP DEFAULT NOW()
		)`,
		`CREATE TABLE IF NOT EXISTS sites (
			id SERIAL PRIMARY KEY,
			domain TEXT NOT NULL UNIQUE,
			origin TEXT NOT NULL,
			origin_protocol TEXT NOT NULL DEFAULT 'follow',
			http_port INTEGER NOT NULL DEFAULT 80,
			https_port INTEGER NOT NULL DEFAULT 443,
			origin_host TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL DEFAULT 'pending',
			config_auto_sync INTEGER DEFAULT 0,
			created_at TIMESTAMP DEFAULT NOW(),
			updated_at TIMESTAMP DEFAULT NOW()
		)`,
		`CREATE TABLE IF NOT EXISTS deployments (
			id SERIAL PRIMARY KEY,
			site_id INTEGER NOT NULL REFERENCES sites(id) ON DELETE CASCADE,
			provider TEXT NOT NULL,
			config_id INTEGER NOT NULL DEFAULT 0,
			config_name TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL DEFAULT 'pending',
			provider_site_id TEXT DEFAULT '',
			cdn_cname TEXT DEFAULT '',
			deploy_params TEXT DEFAULT '{}',
			error_message TEXT DEFAULT '',
			created_at TIMESTAMP DEFAULT NOW(),
			updated_at TIMESTAMP DEFAULT NOW()
		)`,
		`CREATE TABLE IF NOT EXISTS dns_records (
			id SERIAL PRIMARY KEY,
			site_id INTEGER NOT NULL REFERENCES sites(id) ON DELETE CASCADE,
			deployment_id INTEGER REFERENCES deployments(id) ON DELETE CASCADE,
			record_type TEXT NOT NULL,
			name TEXT NOT NULL,
			value TEXT NOT NULL,
			purpose TEXT NOT NULL DEFAULT 'traffic',
			status TEXT NOT NULL DEFAULT 'pending',
			created_at TIMESTAMP DEFAULT NOW()
		)`,
		`CREATE TABLE IF NOT EXISTS certificates (
			id SERIAL PRIMARY KEY,
			site_id INTEGER NOT NULL REFERENCES sites(id) ON DELETE CASCADE,
			deployment_id INTEGER REFERENCES deployments(id) ON DELETE CASCADE,
			provider TEXT NOT NULL,
			status TEXT NOT NULL DEFAULT 'pending',
			domain TEXT NOT NULL,
			cert_id TEXT DEFAULT '',
			expires_at TIMESTAMP,
			error_message TEXT DEFAULT '',
			created_at TIMESTAMP DEFAULT NOW(),
			updated_at TIMESTAMP DEFAULT NOW()
		)`,
		`CREATE TABLE IF NOT EXISTS provider_configs (
			id SERIAL PRIMARY KEY,
			name TEXT NOT NULL DEFAULT '',
			provider TEXT NOT NULL,
			config TEXT NOT NULL DEFAULT '{}',
			enabled INTEGER NOT NULL DEFAULT 1,
			created_at TIMESTAMP DEFAULT NOW(),
			updated_at TIMESTAMP DEFAULT NOW()
		)`,
		`CREATE TABLE IF NOT EXISTS ssl_certificates (
			id SERIAL PRIMARY KEY,
			domains TEXT NOT NULL DEFAULT '',
			brand TEXT NOT NULL DEFAULT '',
			source TEXT NOT NULL DEFAULT 'upload',
			certificate TEXT DEFAULT '',
			private_key TEXT DEFAULT '',
			issuer_cert TEXT DEFAULT '',
			fingerprint TEXT DEFAULT '',
			not_before TIMESTAMP,
			not_after TIMESTAMP,
			status TEXT NOT NULL DEFAULT 'valid',
			error_message TEXT DEFAULT '',
			created_at TIMESTAMP DEFAULT NOW(),
			updated_at TIMESTAMP DEFAULT NOW()
		)`,
		`CREATE TABLE IF NOT EXISTS self_signed_certs (
			id SERIAL PRIMARY KEY,
			name TEXT NOT NULL DEFAULT '',
			cert_type TEXT NOT NULL DEFAULT '',
			algorithm TEXT NOT NULL DEFAULT '',
			subject_cn TEXT DEFAULT '',
			subject_o TEXT DEFAULT '',
			subject_ou TEXT DEFAULT '',
			subject_c TEXT DEFAULT '',
			subject_st TEXT DEFAULT '',
			subject_l TEXT DEFAULT '',
			domains TEXT DEFAULT '',
			ips TEXT DEFAULT '',
			emails TEXT DEFAULT '',
			purpose TEXT DEFAULT '',
			issuer_id INTEGER DEFAULT NULL REFERENCES self_signed_certs(id) ON DELETE SET NULL,
			certificate TEXT DEFAULT '',
			private_key TEXT DEFAULT '',
			serial_number TEXT DEFAULT '',
			fingerprint TEXT DEFAULT '',
			not_before TIMESTAMP,
			not_after TIMESTAMP,
			validity_days INTEGER DEFAULT 365,
			is_ca INTEGER DEFAULT 0,
			key_usage TEXT DEFAULT '',
			ext_key_usage TEXT DEFAULT '',
			created_at TIMESTAMP DEFAULT NOW()
		)`,
		`CREATE TABLE IF NOT EXISTS acme_accounts (
			id SERIAL PRIMARY KEY,
			email TEXT NOT NULL DEFAULT '',
			ca TEXT NOT NULL DEFAULT '',
			private_key TEXT DEFAULT '',
			registration TEXT DEFAULT '',
			kid TEXT DEFAULT '',
			hmac_encoded TEXT DEFAULT '',
			ca_dir_url TEXT DEFAULT '',
			created_at TIMESTAMP DEFAULT NOW(),
			updated_at TIMESTAMP DEFAULT NOW(),
			UNIQUE(email, ca)
		)`,
	}

	for _, t := range tables {
		if _, err := DB.Exec(t); err != nil {
			return err
		}
	}

	// 增量迁移：新列 + 关键索引（幂等，已存在时静默跳过）
	migrations := []string{
		`ALTER TABLE deployments ADD COLUMN IF NOT EXISTS deploy_log TEXT DEFAULT ''`,
		// 关键查询索引
		`CREATE INDEX IF NOT EXISTS idx_deployments_site_id ON deployments(site_id)`,
		`CREATE INDEX IF NOT EXISTS idx_deployments_config_id ON deployments(config_id)`,
		`CREATE INDEX IF NOT EXISTS idx_deployments_status ON deployments(status)`,
		`CREATE INDEX IF NOT EXISTS idx_dns_records_site_id ON dns_records(site_id)`,
		`CREATE INDEX IF NOT EXISTS idx_dns_records_deployment_id ON dns_records(deployment_id)`,
		`CREATE INDEX IF NOT EXISTS idx_certificates_site_id ON certificates(site_id)`,
		`CREATE INDEX IF NOT EXISTS idx_certificates_deployment_id ON certificates(deployment_id)`,
		`CREATE INDEX IF NOT EXISTS idx_cdn_stats_raw_lookup ON cdn_stats_raw(config_id, period_start)`,
		`CREATE INDEX IF NOT EXISTS idx_cdn_stats_daily_lookup ON cdn_stats_daily(config_id, stat_date)`,
	}
	for _, m := range migrations {
		DB.Exec(m)
	}

	return nil
}

// createDnsCacheTables DNS 缓存表（首次创建，保留已有数据跨重启持久化）
// DnsSync 每次同步时已 DELETE per config_id + re-insert，无需在启动时清空
func createDnsCacheTables() error {

	tables := []string{
		`CREATE TABLE IF NOT EXISTS dns_cache_zones (
			config_id INTEGER NOT NULL,
			zone_id TEXT NOT NULL,
			zone_name TEXT NOT NULL,
			zone_status TEXT DEFAULT 'active',
			record_count INTEGER DEFAULT 0,
			plan_name TEXT DEFAULT '',
			synced_at TIMESTAMP DEFAULT NOW(),
			PRIMARY KEY (config_id, zone_id)
		)`,
		`CREATE TABLE IF NOT EXISTS dns_cache_records (
			config_id INTEGER NOT NULL,
			zone_id TEXT NOT NULL,
			record_id TEXT NOT NULL,
			record_type TEXT NOT NULL,
			host_record TEXT DEFAULT '',
			name TEXT NOT NULL,
			value TEXT NOT NULL,
			ttl INTEGER DEFAULT 0,
			priority INTEGER DEFAULT 0,
			weight INTEGER DEFAULT 0,
			proxied INTEGER DEFAULT -1,
			line TEXT DEFAULT '',
			line_label TEXT DEFAULT '',
			status TEXT DEFAULT 'enable',
			remark TEXT DEFAULT '',
			synced_at TIMESTAMP DEFAULT NOW(),
			PRIMARY KEY (config_id, zone_id, record_id)
		)`,
		`CREATE TABLE IF NOT EXISTS dns_sync_status (
			config_id INTEGER PRIMARY KEY,
			last_zone_sync TIMESTAMP,
			status TEXT DEFAULT 'idle'
		)`,
	}

	for _, t := range tables {
		if _, err := DnsCacheDB.Exec(t); err != nil {
			return err
		}
	}
	return nil
}

// createStatsTables 创建 CDN 流量统计相关表（幂等）
func createStatsTables() error {
	tables := []string{
		// 小时粒度原始数据，15分钟采集写入，0点后合并清理
		`CREATE TABLE IF NOT EXISTS cdn_stats_raw (
			id BIGSERIAL PRIMARY KEY,
			config_id INTEGER NOT NULL,
			provider TEXT NOT NULL,
			zone_id TEXT NOT NULL DEFAULT '',
			period_start TIMESTAMP NOT NULL,
			requests BIGINT DEFAULT 0,
			bytes BIGINT DEFAULT 0,
			cached_requests BIGINT DEFAULT 0,
			cached_bytes BIGINT DEFAULT 0,
			collected_at TIMESTAMP DEFAULT NOW(),
			UNIQUE(config_id, zone_id, period_start)
		)`,
		// 每日聚合（永久保留）
		`CREATE TABLE IF NOT EXISTS cdn_stats_daily (
			id BIGSERIAL PRIMARY KEY,
			config_id INTEGER NOT NULL,
			provider TEXT NOT NULL,
			zone_id TEXT NOT NULL DEFAULT '',
			stat_date DATE NOT NULL,
			requests BIGINT DEFAULT 0,
			bytes BIGINT DEFAULT 0,
			cached_requests BIGINT DEFAULT 0,
			cached_bytes BIGINT DEFAULT 0,
			UNIQUE(config_id, zone_id, stat_date)
		)`,
		// 地区分布（每日，永久保留）
		`CREATE TABLE IF NOT EXISTS cdn_stats_geo (
			id BIGSERIAL PRIMARY KEY,
			config_id INTEGER NOT NULL,
			provider TEXT NOT NULL,
			zone_id TEXT NOT NULL DEFAULT '',
			stat_date DATE NOT NULL,
			country_code TEXT NOT NULL,
			country_name TEXT NOT NULL,
			requests BIGINT DEFAULT 0,
			bytes BIGINT DEFAULT 0,
			UNIQUE(config_id, zone_id, stat_date, country_code)
		)`,
		// 采集状态记录
		`CREATE TABLE IF NOT EXISTS cdn_stats_collect_status (
			config_id INTEGER NOT NULL,
			zone_id TEXT NOT NULL DEFAULT '',
			last_collected_at TIMESTAMP,
			last_geo_collected_at TIMESTAMP,
			PRIMARY KEY(config_id, zone_id)
		)`,
	}
	for _, t := range tables {
		if _, err := DB.Exec(t); err != nil {
			return err
		}
	}
	// 列迁移：为已存在的表添加新列（忽略"列已存在"错误）
	migrations := []string{
		`ALTER TABLE users ADD COLUMN IF NOT EXISTS totp_secret TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE users ADD COLUMN IF NOT EXISTS totp_enabled INTEGER NOT NULL DEFAULT 0`,
	}
	for _, m := range migrations {
		DB.Exec(m) // 忽略错误（列已存在时会报错但不影响功能）
	}
	return nil
}

// seedAdmin 初始化管理员账户 admin/admin
func seedAdmin() error {
	var count int
	err := DB.QueryRow("SELECT COUNT(*) FROM users WHERE username = $1", "admin").Scan(&count)
	if err != nil {
		return err
	}
	if count > 0 {
		return nil
	}

	hashed, err := bcrypt.GenerateFromPassword([]byte("admin"), bcrypt.DefaultCost)
	if err != nil {
		return err
	}

	_, err = DB.Exec("INSERT INTO users (username, password) VALUES ($1, $2)", "admin", string(hashed))
	if err != nil {
		return err
	}

	log.Println("默认管理员账户已创建: admin/admin")
	return nil
}
