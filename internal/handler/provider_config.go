package handler

import (
	"allfast/internal/database"
	"allfast/internal/model"
	"allfast/internal/provider"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// ListProviderConfigs 获取所有提供商账户配置
func ListProviderConfigs(c *gin.Context) {
	rows, err := database.DB.Query(
		"SELECT id, name, provider, config, enabled, created_at, updated_at FROM provider_configs ORDER BY id",
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "查询提供商配置失败"})
		return
	}
	defer rows.Close()

	configs := []model.ProviderConfig{}
	for rows.Next() {
		var pc model.ProviderConfig
		var enabled int
		if err := rows.Scan(&pc.ID, &pc.Name, &pc.Provider, &pc.Config, &enabled, &pc.CreatedAt, &pc.UpdatedAt); err != nil {
			continue
		}
		pc.Enabled = enabled == 1
		configs = append(configs, pc)
	}

	c.JSON(http.StatusOK, gin.H{"configs": configs})
}

// SaveProviderConfig 创建或更新提供商账户配置
func SaveProviderConfig(c *gin.Context) {
	var req model.SaveProviderConfigRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请提供完整的配置信息"})
		return
	}

	// 验证提供商是否存在
	p, err := provider.Get(req.Provider)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// 验证配置JSON格式
	cfg := make(map[string]string)
	if err := json.Unmarshal([]byte(req.Config), &cfg); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "配置格式错误，需要JSON格式"})
		return
	}

	// 编辑时：脱敏字段用旧值替换
	if req.ID > 0 {
		cfg = mergeWithExistingByID(req.ID, cfg)
	}

	// 验证配置完整性
	if err := p.ValidateConfig(cfg); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	configBytes, _ := json.Marshal(cfg)
	configJSON := string(configBytes)

	now := time.Now()
	enabledInt := 0
	if req.Enabled {
		enabledInt = 1
	}

	if req.ID > 0 {
		// 更新已有账户
		_, err = database.DB.Exec(
			`UPDATE provider_configs SET name = $1, config = $2, enabled = $3, updated_at = $4 WHERE id = $5`,
			req.Name, configJSON, enabledInt, now, req.ID,
		)
	} else {
		// 新增账户
		_, err = database.DB.Exec(
			`INSERT INTO provider_configs (name, provider, config, enabled, created_at, updated_at) VALUES ($1, $2, $3, $4, $5, $6)`,
			req.Name, req.Provider, configJSON, enabledInt, now, now,
		)
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "保存配置失败"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "配置已保存"})
}

// DeleteProviderConfig 按ID删除提供商账户配置
func DeleteProviderConfig(c *gin.Context) {
	configID := c.Param("id")

	result, err := database.DB.Exec("DELETE FROM provider_configs WHERE id = $1", configID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "删除配置失败"})
		return
	}

	affected, _ := result.RowsAffected()
	if affected == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "配置不存在"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "配置已删除"})
}

// TestProviderConfig 按ID测试提供商API连接
func TestProviderConfig(c *gin.Context) {
	configID := c.Param("id")

	var providerName, configJSON string
	err := database.DB.QueryRow(
		"SELECT provider, config FROM provider_configs WHERE id = $1", configID,
	).Scan(&providerName, &configJSON)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "配置不存在"})
		return
	}

	p, err := provider.Get(providerName)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	cfg := make(map[string]string)
	if err := json.Unmarshal([]byte(configJSON), &cfg); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "配置格式错误"})
		return
	}

	if err := p.ValidateConfig(cfg); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	ctx := c.Request.Context()
	_, err = p.GetDomainStatus(ctx, cfg, "test.example.com", "test")
	if err != nil {
		errStr := err.Error()
		if strings.Contains(errStr, "请求") && strings.Contains(errStr, "失败") {
			c.JSON(http.StatusBadGateway, gin.H{
				"success": false,
				"error":   "API连接失败: " + errStr,
			})
			return
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "API连接正常",
	})
}

// GetProviderConfigByID 按ID获取单个配置（内部用）
func GetProviderConfigByID(id int64) (providerName string, cfg map[string]string, configName string, err error) {
	var configJSON string
	var enabled int
	err = database.DB.QueryRow(
		"SELECT name, provider, config, enabled FROM provider_configs WHERE id = $1", id,
	).Scan(&configName, &providerName, &configJSON, &enabled)
	if err != nil {
		return "", nil, "", fmt.Errorf("提供商配置不存在 (ID=%d)", id)
	}
	if enabled == 0 {
		return "", nil, "", fmt.Errorf("提供商配置已禁用: %s", configName)
	}

	cfg = make(map[string]string)
	if err := json.Unmarshal([]byte(configJSON), &cfg); err != nil {
		return "", nil, "", fmt.Errorf("配置格式错误")
	}

	return providerName, cfg, configName, nil
}

// FetchFieldOptions 动态加载配置字段的选项（如套餐实例列表）
// 直接用前端传的表单值调API
func FetchFieldOptions(c *gin.Context) {
	var req struct {
		Provider string            `json:"provider" binding:"required"`
		Field    string            `json:"field" binding:"required"`
		Config   map[string]string `json:"config" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "参数错误"})
		return
	}

	p, err := provider.Get(req.Provider)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	options, err := p.FetchFieldOptions(c.Request.Context(), req.Config, req.Field)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if options == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "该字段不支持动态加载"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"options": options})
}

// FetchDeployFieldOptions 部署时动态加载部署字段选项
// 通过 config_id 从数据库读取认证信息，合并前端传来的部署参数后调API
func FetchDeployFieldOptions(c *gin.Context) {
	var req struct {
		ConfigID     int64             `json:"config_id" binding:"required"`
		Field        string            `json:"field" binding:"required"`
		DeployParams map[string]string `json:"deploy_params"` // 已填的部署参数
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "参数错误"})
		return
	}

	// 从数据库读取提供商配置（含认证凭证）
	var providerName, configJSON string
	err := database.DB.QueryRow(
		"SELECT provider, config FROM provider_configs WHERE id = $1", req.ConfigID,
	).Scan(&providerName, &configJSON)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "配置不存在"})
		return
	}

	cfg := make(map[string]string)
	if err := json.Unmarshal([]byte(configJSON), &cfg); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "配置格式错误"})
		return
	}

	// 合并部署参数
	for k, v := range req.DeployParams {
		cfg[k] = v
	}

	p, err := provider.Get(providerName)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	options, err := p.FetchFieldOptions(c.Request.Context(), cfg, req.Field)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if options == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "该字段不支持动态加载"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"options": options})
}

// maskSecrets 对配置中的敏感字段进行脱敏
func maskSecrets(providerName, configJSON string) string {
	p, err := provider.Get(providerName)
	if err != nil {
		return configJSON
	}

	cfg := make(map[string]string)
	if err := json.Unmarshal([]byte(configJSON), &cfg); err != nil {
		return configJSON
	}

	info := p.Info()
	for _, field := range info.ConfigFields {
		if field.Secret {
			if v, ok := cfg[field.Key]; ok && v != "" {
				if len(v) > 4 {
					cfg[field.Key] = v[:4] + "******"
				} else {
					cfg[field.Key] = "******"
				}
			}
		}
	}

	masked, _ := json.Marshal(cfg)
	return string(masked)
}

// mergeWithExistingByID 按ID获取旧配置，将脱敏字段还原
func mergeWithExistingByID(id int64, newCfg map[string]string) map[string]string {
	var oldConfigJSON string
	err := database.DB.QueryRow(
		"SELECT config FROM provider_configs WHERE id = $1", id,
	).Scan(&oldConfigJSON)
	if err != nil {
		return newCfg
	}

	oldCfg := make(map[string]string)
	if err := json.Unmarshal([]byte(oldConfigJSON), &oldCfg); err != nil {
		return newCfg
	}

	for k, v := range newCfg {
		if strings.Contains(v, "******") {
			if oldVal, ok := oldCfg[k]; ok {
				newCfg[k] = oldVal
			}
		}
	}

	return newCfg
}
