package handler

import (
	"allfast/internal/database"
	"allfast/internal/model"
	"allfast/internal/util"
	cryptoRand "crypto/rand"
	"crypto/sha256"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// tracker.js 内容（内联，避免额外文件依赖）
const trackerJS = `(function(){
'use strict';
var el=document.currentScript;
var id=el&&el.getAttribute('data-site-id');
if(!id)return;
var base=el.src.replace(/\/tracker\.js(\?.*)?$/,'');
var ep=base+'/api/track/'+id;
var sk='_af_'+id;
var sid=sessionStorage.getItem(sk);
if(!sid){sid=Math.random().toString(36).slice(2)+Date.now().toString(36);sessionStorage.setItem(sk,sid);}
var t0=Date.now();
function br(ua){
  if(/edg\//i.test(ua))return'Edge';
  if(/opr\/|opera/i.test(ua))return'Opera';
  if(/chrome/i.test(ua))return'Chrome';
  if(/firefox/i.test(ua))return'Firefox';
  if(/safari/i.test(ua))return'Safari';
  if(/ios/i.test(ua)||/iphone|ipad/i.test(ua))return'Safari';
  return'Other';
}
function os(ua){
  if(/iphone|ipad|ipod/i.test(ua))return'iOS';
  if(/android/i.test(ua))return'Android';
  if(/windows/i.test(ua))return'Windows';
  if(/mac os x/i.test(ua))return'macOS';
  if(/linux/i.test(ua))return'Linux';
  return'Other';
}
function send(dur){
  var d=JSON.stringify({path:location.pathname,referrer:document.referrer||'',browser:br(navigator.userAgent),os:os(navigator.userAgent),duration:dur,session_id:sid});
  try{navigator.sendBeacon(ep,d);}catch(e){fetch(ep,{method:'POST',headers:{'Content-Type':'application/json'},body:d,keepalive:true}).catch(function(){});}
}
send(0);
window.addEventListener('pagehide',function(){send(Math.round((Date.now()-t0)/1000));});
var ph=history.pushState;
history.pushState=function(){ph.apply(history,arguments);t0=Date.now();setTimeout(function(){send(0);},0);};
window.addEventListener('popstate',function(){t0=Date.now();send(0);});
})();`

// ServeTrackerScript GET /tracker.js — 公开，无需认证
func ServeTrackerScript(c *gin.Context) {
	c.Header("Content-Type", "application/javascript; charset=utf-8")
	c.Header("Cache-Control", "public, max-age=3600")
	c.Header("Access-Control-Allow-Origin", "*")
	c.String(http.StatusOK, trackerJS)
}

// TrackPageView POST /api/track/:trackingId — 公开，允许跨域
func TrackPageView(c *gin.Context) {
	c.Header("Access-Control-Allow-Origin", "*")
	c.Header("Access-Control-Allow-Methods", "POST, OPTIONS")
	c.Header("Access-Control-Allow-Headers", "Content-Type")
	if c.Request.Method == http.MethodOptions {
		c.Status(http.StatusNoContent)
		return
	}

	trackingID := c.Param("siteId") // 路由参数名保持兼容

	var req struct {
		Path      string `json:"path"`
		Referrer  string `json:"referrer"`
		Browser   string `json:"browser"`
		OS        string `json:"os"`
		Duration  int    `json:"duration"`
		SessionID string `json:"session_id"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.Status(http.StatusNoContent)
		return
	}

	// 通过 tracking_id 查找站点（支持旧的数字 ID 兼容）
	var siteID int64
	err := database.DB.QueryRow("SELECT id FROM sites WHERE tracking_id = $1", trackingID).Scan(&siteID)
	if err != nil {
		// 兼容旧的数字 ID
		if err2 := database.DB.QueryRow("SELECT id FROM sites WHERE id = $1", trackingID).Scan(&siteID); err2 != nil {
			c.Status(http.StatusNoContent)
			return
		}
	}

	// 生成匿名访客 ID
	ip := trackGetClientIP(c)
	ua := c.GetHeader("User-Agent")
	hash := sha256.Sum256([]byte(ip + "|" + ua + "|" + time.Now().UTC().Format("2006-01-02")))
	visitorID := fmt.Sprintf("%x", hash[:8])

	// 截断字段
	path := req.Path
	if len(path) > 500 {
		path = path[:500]
	}
	referrer := trackCleanReferrer(req.Referrer)
	browser := req.Browser
	if len(browser) > 50 {
		browser = browser[:50]
	}
	osName := req.OS
	if len(osName) > 50 {
		osName = osName[:50]
	}
	sessionID := req.SessionID
	if len(sessionID) > 64 {
		sessionID = sessionID[:64]
	}

	// duration > 0 说明是页面离开事件，更新现有记录
	if req.Duration > 0 && sessionID != "" {
		database.DB.Exec(
			`UPDATE page_views SET duration = $1
			 WHERE session_id = $2 AND site_id = $3 AND path = $4 AND duration = 0`,
			req.Duration, sessionID, siteID, path,
		)
		c.Status(http.StatusNoContent)
		return
	}

	// 先插入记录（country_code 留空），取回新行 ID 以便异步回填
	var pvID int64
	database.DB.QueryRow(
		`INSERT INTO page_views
		 (site_id, visitor_id, session_id, path, referrer, browser, os, country_code, duration, created_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,'',0,$8)
		 RETURNING id`,
		siteID, visitorID, sessionID, path, referrer, browser, osName, time.Now().UTC(),
	).Scan(&pvID)

	// 异步查询 GeoIP，不阻塞响应
	go func(id int64, rawIP string) {
		code := util.LookupCountry(rawIP)
		if code != "" {
			database.DB.Exec(
				"UPDATE page_views SET country_code = $1 WHERE id = $2",
				code, id,
			)
		}
	}(pvID, ip)

	c.Status(http.StatusNoContent)
}

// GetTrackingStats GET /api/sites/:id/tracking/stats?range=30d
func GetTrackingStats(c *gin.Context) {
	siteID := c.Param("id")
	from, to := parseStatsRangeParams(c)

	var stats model.TrackingStats

	// ---- 当前周期汇总 ----
	queryPeriodTrackingStats(siteID, from, to, &stats.Visitors, &stats.Sessions, &stats.Pageviews, &stats.BounceRate, &stats.AvgDuration)

	// ---- 上一周期（环比）----
	duration := to.Sub(from)
	prevTo := from
	prevFrom := from.Add(-duration)
	queryPeriodTrackingStats(siteID, prevFrom, prevTo, &stats.PrevVisitors, &stats.PrevSessions, &stats.PrevPageviews, &stats.PrevBounceRate, &stats.PrevAvgDuration)

	// ---- 分页 Top ----
	stats.Pages = trackingBreakdown(siteID, "path", from, to, 10)
	stats.Referrers = trackingBreakdown(siteID, "referrer", from, to, 10)
	stats.Browsers = trackingBreakdown(siteID, "browser", from, to, 8)
	stats.OSes = trackingBreakdown(siteID, "os", from, to, 8)
	stats.Countries = trackingBreakdown(siteID, "country_code", from, to, 10)

	// ---- 流量趋势 + 热力图 ----
	stats.Chart = trackingChart(siteID, from, to)
	stats.Heatmap = trackingHeatmap(siteID, from, to)

	c.JSON(http.StatusOK, stats)
}

// GetTrackingCode GET /api/sites/:id/tracking/code — 返回嵌入代码片段
func GetTrackingCode(c *gin.Context) {
	siteID := c.Param("id")
	var domain, trackingID string
	if err := database.DB.QueryRow("SELECT domain, COALESCE(tracking_id,'') FROM sites WHERE id = $1", siteID).Scan(&domain, &trackingID); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "站点不存在"})
		return
	}

	// 如果没有 tracking_id，生成一个随机的
	if trackingID == "" {
		raw := make([]byte, 12)
		if _, err := cryptoRand.Read(raw); err != nil {
			h := sha256.Sum256([]byte(fmt.Sprintf("%d-%s-%d", time.Now().UnixNano(), siteID, time.Now().Unix())))
			raw = h[:12]
		}
		trackingID = fmt.Sprintf("%x", raw)
		database.DB.Exec("UPDATE sites SET tracking_id = $1 WHERE id = $2", trackingID, siteID)
	}

	scheme := "https"
	host := c.Request.Host
	if host == "" {
		host = "your-server.com"
	}
	code := fmt.Sprintf(`<script defer src="%s://%s/tracker.js" data-site-id="%s"></script>`,
		scheme, host, trackingID)
	c.JSON(http.StatusOK, gin.H{"code": code, "site_id": trackingID, "domain": domain})
}

// GetTrackingShare GET /api/sites/:id/tracking/share — 获取当前共享 token
func GetTrackingShare(c *gin.Context) {
	siteID := c.Param("id")
	var token string
	if err := database.DB.QueryRow("SELECT COALESCE(tracking_share_token,'') FROM sites WHERE id = $1", siteID).Scan(&token); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "站点不存在"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"token": token, "enabled": token != ""})
}

// CreateTrackingShare POST /api/sites/:id/tracking/share — 生成或重置共享 token
func CreateTrackingShare(c *gin.Context) {
	siteID := c.Param("id")
	// 生成随机 token（16 字节 hex，crypto/rand）
	raw := make([]byte, 16)
	if _, err := cryptoRand.Read(raw); err != nil {
		// fallback: sha256(timestamp + siteID)
		h := sha256.Sum256([]byte(fmt.Sprintf("%d-%s", time.Now().UnixNano(), siteID)))
		raw = h[:16]
	}
	token := fmt.Sprintf("%x", raw)
	if _, err := database.DB.Exec("UPDATE sites SET tracking_share_token=$1 WHERE id=$2", token, siteID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "生成失败"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"token": token, "enabled": true})
}

// DeleteTrackingShare DELETE /api/sites/:id/tracking/share — 禁用共享
func DeleteTrackingShare(c *gin.Context) {
	siteID := c.Param("id")
	database.DB.Exec("UPDATE sites SET tracking_share_token='' WHERE id=$1", siteID)
	c.JSON(http.StatusOK, gin.H{"token": "", "enabled": false})
}

// GetPublicTrackingStats GET /share/analytics/:token — 公开访问（无需认证）
func GetPublicTrackingStats(c *gin.Context) {
	token := c.Param("token")
	if token == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效链接"})
		return
	}
	var siteID string
	if err := database.DB.QueryRow(
		"SELECT id FROM sites WHERE tracking_share_token=$1 AND tracking_share_token != ''", token,
	).Scan(&siteID); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "共享链接不存在或已禁用"})
		return
	}
	from, to := parseStatsRangeParams(c)
	var stats model.TrackingStats
	queryPeriodTrackingStats(siteID, from, to, &stats.Visitors, &stats.Sessions, &stats.Pageviews, &stats.BounceRate, &stats.AvgDuration)
	duration := to.Sub(from)
	prevTo, prevFrom := from, from.Add(-duration)
	queryPeriodTrackingStats(siteID, prevFrom, prevTo, &stats.PrevVisitors, &stats.PrevSessions, &stats.PrevPageviews, &stats.PrevBounceRate, &stats.PrevAvgDuration)
	stats.Pages = trackingBreakdown(siteID, "path", from, to, 10)
	stats.Referrers = trackingBreakdown(siteID, "referrer", from, to, 10)
	stats.Browsers = trackingBreakdown(siteID, "browser", from, to, 8)
	stats.OSes = trackingBreakdown(siteID, "os", from, to, 8)
	stats.Countries = trackingBreakdown(siteID, "country_code", from, to, 10)
	stats.Chart = trackingChart(siteID, from, to)
	stats.Heatmap = trackingHeatmap(siteID, from, to)
	c.JSON(http.StatusOK, stats)
}

// ---- helpers ----

func queryPeriodTrackingStats(siteID string, from, to time.Time, visitors, sessions, pageviews *int64, bounceRate, avgDur *float64) {
	database.DB.QueryRow(`
		SELECT COUNT(*), COUNT(DISTINCT visitor_id), COUNT(DISTINCT session_id)
		FROM page_views
		WHERE site_id = $1 AND created_at >= $2 AND created_at < $3`,
		siteID, from, to,
	).Scan(pageviews, visitors, sessions)

	if *sessions == 0 {
		return
	}

	// 跳出率：只有 1 次页面浏览的会话 / 总会话数
	var bounceSessions int64
	database.DB.QueryRow(`
		SELECT COUNT(*) FROM (
			SELECT session_id FROM page_views
			WHERE site_id = $1 AND created_at >= $2 AND created_at < $3
			GROUP BY session_id
			HAVING COUNT(*) = 1
		) t`,
		siteID, from, to,
	).Scan(&bounceSessions)
	if *sessions > 0 {
		*bounceRate = float64(bounceSessions) / float64(*sessions)
	}

	// 平均停留时长：取每个会话最大 duration 求均值（排除 0）
	database.DB.QueryRow(`
		SELECT COALESCE(AVG(max_dur), 0) FROM (
			SELECT MAX(duration) AS max_dur FROM page_views
			WHERE site_id = $1 AND created_at >= $2 AND created_at < $3
			GROUP BY session_id
			HAVING MAX(duration) > 0
		) t`,
		siteID, from, to,
	).Scan(avgDur)
}

func trackingBreakdown(siteID, field string, from, to time.Time, limit int) []model.TrackingMetric {
	// 对 referrer 过滤空值
	whereExtra := ""
	if field == "referrer" {
		whereExtra = " AND referrer != ''"
	}
	if field == "country_code" {
		whereExtra = " AND country_code != ''"
	}

	rows, err := database.DB.Query(fmt.Sprintf(`
		SELECT %s, COUNT(DISTINCT visitor_id) AS v
		FROM page_views
		WHERE site_id = $1 AND created_at >= $2 AND created_at < $3%s
		GROUP BY %s
		ORDER BY v DESC
		LIMIT %d`, field, whereExtra, field, limit),
		siteID, from, to,
	)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var total int64
	var items []model.TrackingMetric
	for rows.Next() {
		var m model.TrackingMetric
		rows.Scan(&m.Label, &m.Visitors)
		total += m.Visitors
		items = append(items, m)
	}
	if total > 0 {
		for i := range items {
			items[i].Pct = float64(items[i].Visitors) / float64(total)
		}
	}
	return items
}

func trackingChart(siteID string, from, to time.Time) []model.TrackingChartPoint {
	longRange := to.Sub(from) > 48*time.Hour
	var rows interface {
		Next() bool
		Scan(...interface{}) error
		Close() error
	}
	if longRange {
		r, err := database.DB.Query(`
			SELECT DATE(created_at AT TIME ZONE 'UTC')::text,
			       COUNT(DISTINCT visitor_id), COUNT(*)
			FROM page_views
			WHERE site_id = $1 AND created_at >= $2 AND created_at < $3
			GROUP BY DATE(created_at AT TIME ZONE 'UTC')
			ORDER BY 1 ASC`,
			siteID, from, to,
		)
		if err != nil {
			return nil
		}
		rows = r
	} else {
		r, err := database.DB.Query(`
			SELECT to_char(date_trunc('hour', created_at AT TIME ZONE 'UTC'), 'YYYY-MM-DD"T"HH24:00:00Z'),
			       COUNT(DISTINCT visitor_id), COUNT(*)
			FROM page_views
			WHERE site_id = $1 AND created_at >= $2 AND created_at < $3
			GROUP BY date_trunc('hour', created_at AT TIME ZONE 'UTC')
			ORDER BY 1 ASC`,
			siteID, from, to,
		)
		if err != nil {
			return nil
		}
		rows = r
	}
	defer rows.Close()

	var pts []model.TrackingChartPoint
	for rows.Next() {
		var p model.TrackingChartPoint
		rows.Scan(&p.T, &p.Visitors, &p.Pageviews)
		pts = append(pts, p)
	}
	return pts
}

func trackingHeatmap(siteID string, from, to time.Time) []model.TrackingHeatmapPoint {
	rows, err := database.DB.Query(`
		SELECT EXTRACT(DOW FROM created_at AT TIME ZONE 'UTC')::int AS wd,
		       EXTRACT(HOUR FROM created_at AT TIME ZONE 'UTC')::int AS hr,
		       COUNT(*) AS cnt
		FROM page_views
		WHERE site_id = $1 AND created_at >= $2 AND created_at < $3
		GROUP BY wd, hr
		ORDER BY wd, hr`,
		siteID, from, to,
	)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var pts []model.TrackingHeatmapPoint
	for rows.Next() {
		var p model.TrackingHeatmapPoint
		rows.Scan(&p.Weekday, &p.Hour, &p.Count)
		pts = append(pts, p)
	}
	return pts
}

func trackCleanReferrer(ref string) string {
	if ref == "" {
		return ""
	}
	u, err := url.Parse(ref)
	if err != nil || u.Hostname() == "" {
		return ""
	}
	return u.Hostname()
}

func trackGetClientIP(c *gin.Context) string {
	if ip := c.GetHeader("CF-Connecting-IP"); ip != "" {
		return ip
	}
	if ip := c.GetHeader("X-Real-IP"); ip != "" {
		return ip
	}
	if ip := c.GetHeader("X-Forwarded-For"); ip != "" {
		parts := strings.SplitN(ip, ",", 2)
		return strings.TrimSpace(parts[0])
	}
	host, _, _ := net.SplitHostPort(c.Request.RemoteAddr)
	return host
}
