package provider

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// ExtractRootDomain 提取主域名（最后两段，如 sub.example.com -> example.com）
func ExtractRootDomain(domain string) string {
	parts := strings.Split(domain, ".")
	if len(parts) <= 2 {
		return domain
	}
	return strings.Join(parts[len(parts)-2:], ".")
}

// CleanOrigin 清理源站地址：去掉协议前缀和端口
func CleanOrigin(origin string) string {
	s := strings.TrimPrefix(origin, "https://")
	s = strings.TrimPrefix(s, "http://")
	s = strings.Split(s, ":")[0]
	s = strings.TrimSuffix(s, "/")
	return s
}

// IsIPAddress 简单判断是否为 IP 地址
func IsIPAddress(s string) bool {
	s = strings.Split(s, ":")[0]
	parts := strings.Split(s, ".")
	if len(parts) != 4 {
		return false
	}
	for _, p := range parts {
		if len(p) == 0 || len(p) > 3 {
			return false
		}
		for _, ch := range p {
			if ch < '0' || ch > '9' {
				return false
			}
		}
	}
	return true
}

// Sha256Hex 计算 SHA-256 哈希的十六进制字符串
func Sha256Hex(data []byte) string {
	h := sha256.New()
	if data != nil {
		h.Write(data)
	}
	return hex.EncodeToString(h.Sum(nil))
}
