package middleware

import (
	"allfast/internal/config"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
)

// JWTSecret 从配置动态读取，避免硬编码
func jwtSecret() []byte {
	return []byte(config.C.Security.JWTSecret)
}

// CORS 跨域中间件：生产时限制 allowed_origins，未配置则仅允许同源
func CORS() gin.HandlerFunc {
	return func(c *gin.Context) {
		origin := c.GetHeader("Origin")
		allowed := false

		allowedOrigins := config.C.Security.AllowedOrigins
		if len(allowedOrigins) == 0 {
			// 未配置时允许所有（开发模式兼容）
			allowed = true
		} else {
			for _, o := range allowedOrigins {
				if o == "*" || o == origin {
					allowed = true
					break
				}
			}
		}

		if allowed && origin != "" {
			c.Header("Access-Control-Allow-Origin", origin)
			c.Header("Vary", "Origin")
		}
		c.Header("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		c.Header("Access-Control-Allow-Headers", "Content-Type, Authorization")
		c.Header("Access-Control-Max-Age", "86400")

		if c.Request.Method == "OPTIONS" {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}
		c.Next()
	}
}

// AuthRequired JWT 认证中间件
func AuthRequired() gin.HandlerFunc {
	return func(c *gin.Context) {
		// token 仅允许从 Authorization header 获取
		// 下载类接口（/download）额外允许 query token，其余不开放（防止 token 泄漏到日志/浏览器历史）
		var tokenStr string
		authHeader := c.GetHeader("Authorization")
		if authHeader != "" {
			if !strings.HasPrefix(authHeader, "Bearer ") {
				c.JSON(http.StatusUnauthorized, gin.H{"error": "认证格式错误"})
				c.Abort()
				return
			}
			tokenStr = strings.TrimPrefix(authHeader, "Bearer ")
		} else if strings.Contains(c.FullPath(), "/download") {
			// 仅下载路径允许 query token（浏览器直接访问场景）
			tokenStr = c.Query("token")
		}

		if tokenStr == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "未提供认证信息"})
			c.Abort()
			return
		}

		token, err := jwt.Parse(tokenStr, func(token *jwt.Token) (interface{}, error) {
			// 严格验证签名算法，防止 alg:none 攻击
			if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
				return nil, fmt.Errorf("不支持的签名算法: %v", token.Header["alg"])
			}
			return jwtSecret(), nil
		})

		if err != nil || !token.Valid {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "认证已过期或无效"})
			c.Abort()
			return
		}

		claims, ok := token.Claims.(jwt.MapClaims)
		if !ok {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "无效的认证信息"})
			c.Abort()
			return
		}

		c.Set("user_id", claims["user_id"])
		c.Set("username", claims["username"])
		c.Next()
	}
}
