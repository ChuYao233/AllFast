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

// SelfSignList GET /api/self-sign — 列表
func SelfSignList(c *gin.Context) {
	certs, err := service.ListSelfSignedCerts()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if certs == nil {
		certs = []model.SelfSignedCert{}
	}
	c.JSON(http.StatusOK, gin.H{"certs": certs})
}

// SelfSignGet GET /api/self-sign/:id — 详情
func SelfSignGet(c *gin.Context) {
	id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
	cert, err := service.GetSelfSignedCert(id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "证书不存在"})
		return
	}
	c.JSON(http.StatusOK, cert)
}

// SelfSignCreate POST /api/self-sign — 创建
func SelfSignCreate(c *gin.Context) {
	var req model.CreateSelfSignedRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求参数错误: " + err.Error()})
		return
	}

	cert, err := service.CreateSelfSignedCert(req)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "证书创建成功", "cert": cert})
}

// SelfSignDelete DELETE /api/self-sign/:id — 删除
func SelfSignDelete(c *gin.Context) {
	id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
	if err := service.DeleteSelfSignedCert(id); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "已删除"})
}

// SelfSignDownload GET /api/self-sign/:id/download — 下载 zip
func SelfSignDownload(c *gin.Context) {
	id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
	cert, err := service.GetSelfSignedCert(id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "证书不存在"})
		return
	}

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)

	// cert.pem
	w, _ := zw.Create("cert.pem")
	w.Write([]byte(cert.Certificate))

	// key.pem
	w, _ = zw.Create("key.pem")
	w.Write([]byte(cert.PrivateKey))

	zw.Close()

	filename := fmt.Sprintf("%s.zip", cert.Name)
	c.Header("Content-Type", "application/zip")
	c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=%s", filename))
	c.Data(http.StatusOK, "application/zip", buf.Bytes())
}

// SelfSignCAList GET /api/self-sign/ca-list — 可用 CA 列表
func SelfSignCAList(c *gin.Context) {
	cas, err := service.ListCACerts()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if cas == nil {
		cas = []model.SelfSignedCert{}
	}
	c.JSON(http.StatusOK, gin.H{"cas": cas})
}

// SelfSignAlgorithms GET /api/self-sign/algorithms — 支持的算法和选项
func SelfSignAlgorithms(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"algorithm_families": service.SelfSignAlgorithmFamilies,
		"key_sizes":          service.SelfSignKeySizes,
		"cert_types":         service.SelfSignCertTypes,
		"purposes":           service.SelfSignPurposes,
	})
}
