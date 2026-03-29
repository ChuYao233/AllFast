package handler

import (
	"allfast/internal/config"
	"allfast/internal/database"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/pquerna/otp/totp"
	"golang.org/x/crypto/bcrypt"
)

func getUserID(c *gin.Context) int64 {
	if v, ok := c.Get("user_id"); ok {
		switch id := v.(type) {
		case float64:
			return int64(id)
		case int64:
			return id
		}
	}
	return 0
}

// SystemGetProfile GET /api/system/profile — 获取当前用户信息
func SystemGetProfile(c *gin.Context) {
	userID := getUserID(c)
	var username string
	var totpEnabled int
	err := database.DB.QueryRow(
		"SELECT username, totp_enabled FROM users WHERE id = $1", userID,
	).Scan(&username, &totpEnabled)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "获取用户信息失败"})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"username":     username,
		"totp_enabled": totpEnabled == 1,
	})
}

// SystemUpdateProfile PUT /api/system/profile — 修改用户名和/或密码
func SystemUpdateProfile(c *gin.Context) {
	userID := getUserID(c)
	var req struct {
		Username    string `json:"username"`
		OldPassword string `json:"old_password"`
		NewPassword string `json:"new_password"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "参数错误"})
		return
	}

	var curUsername, curHash string
	database.DB.QueryRow("SELECT username, password FROM users WHERE id = $1", userID).
		Scan(&curUsername, &curHash)

	// 验证旧密码（修改任何字段时都需要）
	if bcrypt.CompareHashAndPassword([]byte(curHash), []byte(req.OldPassword)) != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "当前密码错误"})
		return
	}

	newUsername := strings.TrimSpace(req.Username)
	if newUsername == "" {
		newUsername = curUsername
	}

	if req.NewPassword != "" {
		// 修改密码
		hashed, err := bcrypt.GenerateFromPassword([]byte(req.NewPassword), bcrypt.DefaultCost)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "密码加密失败"})
			return
		}
		_, err = database.DB.Exec(
			"UPDATE users SET username=$1, password=$2 WHERE id=$3",
			newUsername, string(hashed), userID,
		)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "保存失败: " + err.Error()})
			return
		}
	} else {
		// 只修改用户名
		_, err := database.DB.Exec("UPDATE users SET username=$1 WHERE id=$2", newUsername, userID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "保存失败: " + err.Error()})
			return
		}
	}

	// 重新签发 Token（用户名可能已变）
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"user_id":  userID,
		"username": newUsername,
		"exp":      time.Now().Add(24 * time.Hour).Unix(),
	})
	tokenStr, _ := token.SignedString([]byte(config.C.Security.JWTSecret))
	c.JSON(http.StatusOK, gin.H{"message": "保存成功", "token": tokenStr, "username": newUsername})
}

// SystemTOTPSetup POST /api/system/totp/setup — 生成 TOTP 密钥和二维码 URL
func SystemTOTPSetup(c *gin.Context) {
	userID := getUserID(c)
	var username string
	database.DB.QueryRow("SELECT username FROM users WHERE id=$1", userID).Scan(&username)

	key, err := totp.Generate(totp.GenerateOpts{
		Issuer:      "AllFast",
		AccountName: username,
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "生成密钥失败"})
		return
	}

	// 临时存储 secret（未验证前不设为 enabled）
	database.DB.Exec("UPDATE users SET totp_secret=$1 WHERE id=$2", key.Secret(), userID)

	c.JSON(http.StatusOK, gin.H{
		"secret":  key.Secret(),
		"otpauth": key.URL(),
	})
}

// SystemTOTPEnable POST /api/system/totp/enable — 验证 OTP code 后开启 2FA
func SystemTOTPEnable(c *gin.Context) {
	userID := getUserID(c)
	var req struct {
		Code string `json:"code"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Code == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请提供验证码"})
		return
	}

	var secret string
	database.DB.QueryRow("SELECT totp_secret FROM users WHERE id=$1", userID).Scan(&secret)
	if secret == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请先生成密钥"})
		return
	}

	if !totp.Validate(req.Code, secret) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "验证码错误"})
		return
	}

	database.DB.Exec("UPDATE users SET totp_enabled=1 WHERE id=$1", userID)
	c.JSON(http.StatusOK, gin.H{"message": "两步验证已开启"})
}

// SystemTOTPDisable DELETE /api/system/totp — 关闭 2FA
func SystemTOTPDisable(c *gin.Context) {
	userID := getUserID(c)
	var req struct {
		Code string `json:"code"`
	}
	c.ShouldBindJSON(&req)

	var secret string
	var totpEnabled int
	database.DB.QueryRow("SELECT totp_secret, totp_enabled FROM users WHERE id=$1", userID).
		Scan(&secret, &totpEnabled)

	if totpEnabled == 1 {
		if req.Code == "" || !totp.Validate(req.Code, secret) {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "需要验证码确认"})
			return
		}
	}

	database.DB.Exec("UPDATE users SET totp_enabled=0, totp_secret='' WHERE id=$1", userID)
	c.JSON(http.StatusOK, gin.H{"message": "两步验证已关闭"})
}

// SystemSaveConsoleTLS PUT /api/system/console-tls — 保存控制台 HTTPS 证书
func SystemSaveConsoleTLS(c *gin.Context) {
	var req struct {
		Cert string `json:"cert"` // PEM 格式证书
		Key  string `json:"key"`  // PEM 格式私钥
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Cert == "" || req.Key == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请提供证书和私钥"})
		return
	}

	if err := os.WriteFile("console.crt", []byte(req.Cert), 0600); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "保存证书失败: " + err.Error()})
		return
	}
	if err := os.WriteFile("console.key", []byte(req.Key), 0600); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "保存私钥失败: " + err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "证书已保存，重启服务后生效"})
}

// SystemGetConsoleTLSStatus GET /api/system/console-tls — 查询证书是否已配置
func SystemGetConsoleTLSStatus(c *gin.Context) {
	_, errCrt := os.Stat("console.crt")
	_, errKey := os.Stat("console.key")
	c.JSON(http.StatusOK, gin.H{"configured": errCrt == nil && errKey == nil})
}

// SystemDeleteConsoleTLS DELETE /api/system/console-tls — 删除控制台证书
func SystemDeleteConsoleTLS(c *gin.Context) {
	os.Remove("console.crt")
	os.Remove("console.key")
	c.JSON(http.StatusOK, gin.H{"message": "证书已删除，重启服务后恢复 HTTP"})
}
