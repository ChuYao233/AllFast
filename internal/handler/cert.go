package handler

import (
	"allfast/internal/model"
	"allfast/internal/service"
	"archive/zip"
	"bytes"
	"fmt"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
)

// CertList GET /api/certs — 证书列表
func CertList(c *gin.Context) {
	certs, err := service.ListSSLCerts()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if certs == nil {
		certs = []model.SSLCertificate{}
	}
	c.JSON(http.StatusOK, gin.H{"certs": certs})
}

// CertGet GET /api/certs/:id — 查看证书详情（含证书内容）
func CertGet(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的证书 ID"})
		return
	}
	cert, err := service.GetSSLCert(id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"cert": cert})
}

// CertUpload POST /api/certs/upload — 上传证书
func CertUpload(c *gin.Context) {
	var req model.UploadCertRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请提供证书和私钥"})
		return
	}
	cert, err := service.SaveUploadedCert(req.Certificate, req.PrivateKey)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"cert": cert})
}

// CertApply POST /api/certs/apply — 申请证书（异步）
func CertApply(c *gin.Context) {
	var req model.ApplyCertRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "参数不完整"})
		return
	}
	if req.Algorithm == "" {
		req.Algorithm = "EC256"
	}
	certID, err := service.ApplyCert(req)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"cert_id": certID, "message": "证书申请已提交，请稍后刷新查看状态"})
}

// CertDelete DELETE /api/certs/:id — 删除证书
func CertDelete(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的证书 ID"})
		return
	}
	if err := service.DeleteSSLCert(id); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "删除成功"})
}

// CertDownload GET /api/certs/:id/download — 下载证书（zip 包含 cert.pem + key.pem）
func CertDownload(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的证书 ID"})
		return
	}
	cert, err := service.GetSSLCert(id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	if cert.Certificate == "" || cert.PrivateKey == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "证书数据不完整，可能正在申请中"})
		return
	}

	// 生成 zip
	buf := new(bytes.Buffer)
	w := zip.NewWriter(buf)

	// 证书文件
	f, _ := w.Create("cert.pem")
	f.Write([]byte(cert.Certificate))

	// 私钥文件
	f, _ = w.Create("key.pem")
	f.Write([]byte(cert.PrivateKey))

	w.Close()

	filename := fmt.Sprintf("cert_%d.zip", id)
	c.Header("Content-Type", "application/zip")
	c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=%s", filename))
	c.Data(http.StatusOK, "application/zip", buf.Bytes())
}

// CertCAList GET /api/certs/ca-providers — 获取可用 CA 列表
func CertCAList(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"providers": service.GetCAProviders()})
}

// CertDNSConfigs GET /api/certs/dns-configs — 获取支持 DNS 管理的提供商配置
func CertDNSConfigs(c *gin.Context) {
	configs, err := service.GetDNSConfigsForACME()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if configs == nil {
		configs = []map[string]interface{}{}
	}
	c.JSON(http.StatusOK, gin.H{"configs": configs})
}
