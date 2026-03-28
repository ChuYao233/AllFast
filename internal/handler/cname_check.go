package handler

import (
	"context"
	"net"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// DNS 服务器列表，多个查询只要有1次匹配就算生效
var dnsServers = []string{
	"8.8.8.8:53",
	"1.1.1.1:53",
	"223.5.5.5:53",
	"119.29.29.29:53",
	"114.114.114.114:53",
}

// CheckCNAME 检查域名的CNAME是否已生效
func CheckCNAME(c *gin.Context) {
	domain := c.Query("domain")
	expected := c.Query("expected")

	if domain == "" || expected == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "缺少参数"})
		return
	}

	// 在多个DNS服务器上查询，只要有1次匹配就算生效
	matched := false
	for _, server := range dnsServers {
		resolver := &net.Resolver{
			PreferGo: true,
			Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
				d := net.Dialer{}
				return d.DialContext(ctx, "udp", server)
			},
		}
		cname, err := resolver.LookupCNAME(c.Request.Context(), domain)
		if err != nil {
			continue
		}
		// CNAME 查询结果末尾可能带点号
		cname = strings.TrimSuffix(cname, ".")
		expected = strings.TrimSuffix(expected, ".")
		if strings.EqualFold(cname, expected) {
			matched = true
			break
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"domain":   domain,
		"expected": expected,
		"resolved": matched,
	})
}
