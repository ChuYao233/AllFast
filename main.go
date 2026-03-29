package main

import (
	"allfast/internal/config"
	"allfast/internal/database"
	"allfast/internal/handler"
	"allfast/internal/middleware"
	"allfast/internal/provider"
	"allfast/internal/service"
	"fmt"
	"log"
	"net/http"
	"time"

	// 注册各 CDN 提供商（init() 自动注册）
	_ "allfast/internal/provider/aliyun"
	_ "allfast/internal/provider/cloudflare"
	_ "allfast/internal/provider/edgeone"

	"github.com/gin-gonic/gin"
)

func main() {
	// 加载配置文件
	if err := config.Load("config.yaml"); err != nil {
		log.Fatalf("加载配置失败: %v", err)
	}

	// 初始化CDN提供商注册表
	provider.Init()

	// 初始化数据库
	if err := database.Init(); err != nil {
		log.Fatalf("数据库初始化失败: %v", err)
	}
	defer database.Close()

	r := gin.Default()

	// CORS 中间件
	r.Use(middleware.CORS())

	// 公开路由
	r.POST("/api/login", handler.Login)
	r.GET("/tracker.js", handler.ServeTrackerScript)
	r.POST("/api/track/:siteId", handler.TrackPageView)
	r.OPTIONS("/api/track/:siteId", handler.TrackPageView) // CORS preflight

	// 需要认证的路由
	auth := r.Group("/api")
	auth.Use(middleware.AuthRequired())
	{
		// 站点管理
		auth.GET("/sites", handler.ListSites)
		auth.POST("/sites", handler.CreateSite)
		auth.GET("/sites/:id", handler.GetSite)
		auth.PUT("/sites/:id", handler.UpdateSite)
		auth.DELETE("/sites/:id", handler.DeleteSite)

		// 自动同步配置开关
		auth.PUT("/sites/:id/auto-sync", handler.ToggleAutoSync)

		// 一键部署
		auth.POST("/sites/:id/deploy", handler.DeploySite)

		// 部署状态
		auth.GET("/sites/:id/deployments", handler.ListDeployments)
		auth.DELETE("/sites/:id/deployments/:dep_id", handler.RemoveSiteDeployment)
		auth.POST("/sites/:id/deployments/:dep_id/redeploy", handler.RedeployDeployment)
		auth.GET("/deployments/:id", handler.GetDeployment)

		// 更新单个 deployment 的回源配置
		auth.PUT("/deployments/:id/origin", handler.UpdateDeploymentOrigin)

		// HTTPS 证书配置
		auth.POST("/deployments/:id/https", handler.DeployHTTPS)

		// DNS 记录
		auth.GET("/sites/:id/dns-records", handler.ListDNSRecords)

		// CNAME 生效检查
		auth.GET("/check-cname", handler.CheckCNAME)

		// DNS 解析管理
		auth.GET("/dns/zones", handler.DnsListZones)
		auth.GET("/dns/all-cached-zones", handler.DnsAllCachedZones)
		auth.GET("/dns/records", handler.DnsListRecords)
		auth.POST("/dns/records", handler.DnsAddRecord)
		auth.PUT("/dns/records/:id", handler.DnsUpdateRecord)
		auth.DELETE("/dns/records/:id", handler.DnsDeleteRecord)
		auth.PUT("/dns/records/:id/status", handler.DnsSetRecordStatus)
		auth.POST("/dns/sync", handler.DnsSyncZones)
		auth.POST("/dns/sync-records", handler.DnsSyncRecords)

		// 证书状态
		auth.GET("/sites/:id/certificates", handler.ListCertificates)

		// SSL 证书管理
		auth.GET("/certs", handler.CertList)
		auth.GET("/certs/ca-providers", handler.CertCAList)
		auth.GET("/certs/dns-configs", handler.CertDNSConfigs)
		auth.GET("/certs/:id", handler.CertGet)
		auth.GET("/certs/:id/download", handler.CertDownload)
		auth.POST("/certs/upload", handler.CertUpload)
		auth.POST("/certs/apply", handler.CertApply)
		auth.DELETE("/certs/:id", handler.CertDelete)

		// 自签证书管理
		auth.GET("/self-sign", handler.SelfSignList)
		auth.GET("/self-sign/algorithms", handler.SelfSignAlgorithms)
		auth.GET("/self-sign/ca-list", handler.SelfSignCAList)
		auth.GET("/self-sign/:id", handler.SelfSignGet)
		auth.GET("/self-sign/:id/download", handler.SelfSignDownload)
		auth.POST("/self-sign", handler.SelfSignCreate)
		auth.DELETE("/self-sign/:id", handler.SelfSignDelete)

		// CDN 提供商
		auth.GET("/providers", handler.ListProviders)

		// 提供商配置管理（支持多账户）
		auth.GET("/provider-configs", handler.ListProviderConfigs)
		auth.POST("/provider-configs", handler.SaveProviderConfig)
		auth.POST("/provider-configs/fetch-options", handler.FetchFieldOptions)
		auth.POST("/provider-configs/fetch-deploy-options", handler.FetchDeployFieldOptions)
		auth.DELETE("/provider-configs/:id", handler.DeleteProviderConfig)
		auth.POST("/provider-configs/:id/test", handler.TestProviderConfig)

		// 系统设置
		auth.GET("/system/profile", handler.SystemGetProfile)
		auth.PUT("/system/profile", handler.SystemUpdateProfile)
		auth.POST("/system/totp/setup", handler.SystemTOTPSetup)
		auth.POST("/system/totp/enable", handler.SystemTOTPEnable)
		auth.DELETE("/system/totp", handler.SystemTOTPDisable)
		auth.GET("/system/console-tls", handler.SystemGetConsoleTLSStatus)
		auth.PUT("/system/console-tls", handler.SystemSaveConsoleTLS)
		auth.DELETE("/system/console-tls", handler.SystemDeleteConsoleTLS)

		// 数据备份
		auth.GET("/backup/export", handler.ExportBackup)
		auth.POST("/backup/import", handler.ImportBackup)

		// 流量统计
		auth.GET("/stats/summary", handler.StatsSummary)
		auth.GET("/stats/timeseries", handler.StatsTimeSeries)
		auth.GET("/stats/geo", handler.StatsGeo)
		auth.POST("/stats/collect", handler.StatsTriggerCollect)
		auth.POST("/stats/clear", handler.StatsClear)
		auth.GET("/sites/:id/stats", handler.SiteStat)
		auth.GET("/sites/:id/tracking/stats", handler.GetTrackingStats)
		auth.GET("/sites/:id/tracking/code", handler.GetTrackingCode)
	}

	// 注册前端静态文件（嵌入式 SPA，NoRoute fallback → index.html）
	registerStaticFiles(r)

	// 启动后台 DNS 缓存同步（每15分钟）
	service.DnsSync.StartBackgroundSync()

	// 启动后台流量统计采集（每15分钟）
	service.StatsCollect.StartBackgroundCollect()

	// 启动后台证书状态同步（每5分钟）
	service.CertSync.StartBackgroundSync()

	addr := fmt.Sprintf(":%d", config.C.Server.Port)
	log.Printf("AllFast CDN聚合部署平台启动在 %s", addr)
	srv := &http.Server{
		Addr:         addr,
		Handler:      r,
		ReadTimeout:  30 * time.Second,  // 请求读取超时
		WriteTimeout: 120 * time.Second, // 响应写入超时（异步接口等待时间较长）
		IdleTimeout:  60 * time.Second,  // 空闲连接超时
	}
	if err := srv.ListenAndServe(); err != nil {
		log.Fatalf("服务启动失败: %v", err)
	}
}
