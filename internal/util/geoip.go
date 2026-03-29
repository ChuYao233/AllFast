package util

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"
)

type ipCacheEntry struct {
	code      string
	expiresAt time.Time
}

var (
	ipCache sync.Map
	geoHTTP = &http.Client{Timeout: 3 * time.Second}

	// 私有/保留地址段
	privateNets []*net.IPNet
)

func init() {
	cidrs := []string{
		"10.0.0.0/8",
		"172.16.0.0/12",
		"192.168.0.0/16",
		"127.0.0.0/8",
		"169.254.0.0/16",
		"::1/128",
		"fc00::/7",
		"fe80::/10",
	}
	for _, cidr := range cidrs {
		_, n, err := net.ParseCIDR(cidr)
		if err == nil {
			privateNets = append(privateNets, n)
		}
	}
}

// LookupCountry 通过 IP 查询国家代码（ISO 3166-1 alpha-2）。
// 使用 ip-api.com 免费接口（无需 API Key）+ 内存缓存（TTL 24h）。
// 私有/保留 IP 直接返回空字符串。
func LookupCountry(ip string) string {
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return ""
	}
	for _, n := range privateNets {
		if n.Contains(parsed) {
			return ""
		}
	}

	// 查缓存
	if v, ok := ipCache.Load(ip); ok {
		entry := v.(ipCacheEntry)
		if time.Now().Before(entry.expiresAt) {
			return entry.code
		}
	}

	// 调用 ip-api.com
	code := geoLookupAPI(ip)
	ipCache.Store(ip, ipCacheEntry{
		code:      code,
		expiresAt: time.Now().Add(24 * time.Hour),
	})
	return code
}

func geoLookupAPI(ip string) string {
	url := fmt.Sprintf("http://ip-api.com/json/%s?fields=countryCode", ip)
	resp, err := geoHTTP.Get(url)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()

	var result struct {
		CountryCode string `json:"countryCode"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return ""
	}
	return result.CountryCode
}
