package handler

import (
	"allfast/internal/database"
	"allfast/internal/middleware"
	"allfast/internal/model"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/pquerna/otp/totp"
	"golang.org/x/crypto/bcrypt"
)

// 登录接口
func Login(c *gin.Context) {
	var req model.LoginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请提供用户名和密码"})
		return
	}

	var user model.User
	var hashedPwd string
	var totpSecret string
	var totpEnabled int
	err := database.DB.QueryRow(
		"SELECT id, username, password, created_at, totp_secret, totp_enabled FROM users WHERE username = $1",
		req.Username,
	).Scan(&user.ID, &user.Username, &hashedPwd, &user.CreatedAt, &totpSecret, &totpEnabled)

	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "用户名或密码错误"})
		return
	}

	if err := bcrypt.CompareHashAndPassword([]byte(hashedPwd), []byte(req.Password)); err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "用户名或密码错误"})
		return
	}

	// 密码验证通过后检查 TOTP
	if totpEnabled == 1 {
		if req.TotpCode == "" {
			// 告知前端需要输入 TOTP 验证码
			c.JSON(http.StatusOK, gin.H{"requires_totp": true})
			return
		}
		if !totp.Validate(req.TotpCode, totpSecret) {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "两步验证码错误"})
			return
		}
	}

	// 生成 JWT Token
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"user_id":  user.ID,
		"username": user.Username,
		"exp":      time.Now().Add(24 * time.Hour).Unix(),
	})

	tokenStr, err := token.SignedString(middleware.JWTSecret)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "生成Token失败"})
		return
	}

	c.JSON(http.StatusOK, model.LoginResponse{
		Token: tokenStr,
		User:  user,
	})
}
