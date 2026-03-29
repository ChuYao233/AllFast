package handler

import (
	"allfast/internal/database"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

// BackupData 备份文件结构
type BackupData struct {
	Version         int              `json:"version"`
	ExportedAt      time.Time        `json:"exported_at"`
	Sites           []map[string]any `json:"sites"`
	Deployments     []map[string]any `json:"deployments"`
	ProviderConfigs []map[string]any `json:"provider_configs"`
	DNSRecords      []map[string]any `json:"dns_records"`
	SSLCerts        []map[string]any `json:"ssl_certificates"`
	SelfSignedCerts []map[string]any `json:"self_signed_certs"`
	AcmeAccounts    []map[string]any `json:"acme_accounts"`
}

// ExportBackup GET /api/backup/export — 导出全量数据为 JSON 文件下载
func ExportBackup(c *gin.Context) {
	data := BackupData{
		Version:    1,
		ExportedAt: time.Now(),
	}

	var err error
	if data.Sites, err = queryAllRows("SELECT * FROM sites ORDER BY id"); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "导出 sites 失败: " + err.Error()})
		return
	}
	if data.Deployments, err = queryAllRows("SELECT * FROM deployments ORDER BY id"); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "导出 deployments 失败: " + err.Error()})
		return
	}
	if data.ProviderConfigs, err = queryAllRows("SELECT * FROM provider_configs ORDER BY id"); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "导出 provider_configs 失败: " + err.Error()})
		return
	}
	if data.DNSRecords, err = queryAllRows("SELECT * FROM dns_records ORDER BY id"); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "导出 dns_records 失败: " + err.Error()})
		return
	}
	if data.SSLCerts, err = queryAllRows("SELECT * FROM ssl_certificates ORDER BY id"); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "导出 ssl_certificates 失败: " + err.Error()})
		return
	}
	if data.SelfSignedCerts, err = queryAllRows("SELECT * FROM self_signed_certs ORDER BY id"); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "导出 self_signed_certs 失败: " + err.Error()})
		return
	}
	if data.AcmeAccounts, err = queryAllRows("SELECT * FROM acme_accounts ORDER BY id"); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "导出 acme_accounts 失败: " + err.Error()})
		return
	}

	jsonBytes, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "序列化失败: " + err.Error()})
		return
	}

	filename := fmt.Sprintf("allfast-backup-%s.json", time.Now().Format("20060102-150405"))
	c.Header("Content-Disposition", "attachment; filename="+filename)
	c.Header("Content-Type", "application/json")
	c.Data(http.StatusOK, "application/json", jsonBytes)
	log.Printf("[Backup] 导出备份成功，共 %d 站点 / %d 接入 / %d 提供商配置",
		len(data.Sites), len(data.Deployments), len(data.ProviderConfigs))
}

// ImportBackup POST /api/backup/import — 导入 JSON 备份文件恢复数据
func ImportBackup(c *gin.Context) {
	file, _, err := c.Request.FormFile("file")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请上传备份文件"})
		return
	}
	defer file.Close()

	const maxSize = 100 * 1024 * 1024 // 100 MB 限制
	limitedReader := io.LimitReader(file, maxSize)
	raw, err := io.ReadAll(limitedReader)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "读取文件失败: " + err.Error()})
		return
	}

	var data BackupData
	if err := json.Unmarshal(raw, &data); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "文件格式无效: " + err.Error()})
		return
	}
	if data.Version != 1 {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("不支持的备份版本: %d", data.Version)})
		return
	}

	// 在事务中执行导入
	tx, err := database.DB.Begin()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "开启事务失败"})
		return
	}
	defer tx.Rollback()

	// 按依赖顺序清空表（保留 users 表，不覆盖账户信息）
	cleanupTables := []string{
		"certificates", "dns_records", "deployments",
		"ssl_certificates", "self_signed_certs", "acme_accounts",
		"provider_configs", "sites",
	}
	for _, t := range cleanupTables {
		if _, err := tx.Exec("DELETE FROM " + t); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "清空表 " + t + " 失败: " + err.Error()})
			return
		}
	}

	// 按顺序插入各表
	if err := insertRows(tx, "sites", data.Sites); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "导入 sites 失败: " + err.Error()})
		return
	}
	if err := insertRows(tx, "deployments", data.Deployments); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "导入 deployments 失败: " + err.Error()})
		return
	}
	if err := insertRows(tx, "provider_configs", data.ProviderConfigs); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "导入 provider_configs 失败: " + err.Error()})
		return
	}
	if err := insertRows(tx, "dns_records", data.DNSRecords); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "导入 dns_records 失败: " + err.Error()})
		return
	}
	if err := insertRows(tx, "ssl_certificates", data.SSLCerts); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "导入 ssl_certificates 失败: " + err.Error()})
		return
	}
	if err := insertRows(tx, "self_signed_certs", data.SelfSignedCerts); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "导入 self_signed_certs 失败: " + err.Error()})
		return
	}
	if err := insertRows(tx, "acme_accounts", data.AcmeAccounts); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "导入 acme_accounts 失败: " + err.Error()})
		return
	}

	// 重置 PostgreSQL 自增序列，避免 id 冲突
	seqTables := []string{"sites", "deployments", "provider_configs", "dns_records",
		"ssl_certificates", "self_signed_certs", "acme_accounts"}
	for _, t := range seqTables {
		tx.Exec(fmt.Sprintf("SELECT setval('%s_id_seq', COALESCE((SELECT MAX(id) FROM %s), 1))", t, t))
	}

	if err := tx.Commit(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "提交事务失败: " + err.Error()})
		return
	}

	log.Printf("[Backup] 导入备份成功（来自 %s），共 %d 站点 / %d 接入",
		data.ExportedAt.Format("2006-01-02 15:04:05"), len(data.Sites), len(data.Deployments))
	c.JSON(http.StatusOK, gin.H{
		"message": "导入成功",
		"stats": gin.H{
			"sites":            len(data.Sites),
			"deployments":      len(data.Deployments),
			"provider_configs": len(data.ProviderConfigs),
			"dns_records":      len(data.DNSRecords),
			"ssl_certs":        len(data.SSLCerts),
		},
	})
}

// queryAllRows 通用：将 SQL 查询结果转为 []map[string]any
func queryAllRows(query string, args ...any) ([]map[string]any, error) {
	rows, err := database.DB.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}

	var result []map[string]any
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		row := make(map[string]any, len(cols))
		for i, col := range cols {
			// []byte 转 string，方便 JSON 序列化
			if b, ok := vals[i].([]byte); ok {
				row[col] = string(b)
			} else {
				row[col] = vals[i]
			}
		}
		result = append(result, row)
	}
	return result, rows.Err()
}

// insertRows 通用：将 []map[string]any 批量插入指定表（在事务中执行）
func insertRows(tx dbExecer, table string, rows []map[string]any) error {
	if len(rows) == 0 {
		return nil
	}
	for _, row := range rows {
		if len(row) == 0 {
			continue
		}
		// 收集列名和值（顺序固定，避免 map 随机）
		cols := make([]string, 0, len(row))
		for k := range row {
			cols = append(cols, k)
		}
		// 排序保证稳定
		for i := 0; i < len(cols)-1; i++ {
			for j := i + 1; j < len(cols); j++ {
				if cols[i] > cols[j] {
					cols[i], cols[j] = cols[j], cols[i]
				}
			}
		}
		vals := make([]any, len(cols))
		for i, c := range cols {
			vals[i] = row[c]
		}

		placeholders := make([]string, len(cols))
		quotedCols := make([]string, len(cols))
		for i, c := range cols {
			placeholders[i] = fmt.Sprintf("$%d", i+1)
			quotedCols[i] = `"` + c + `"`
		}
		query := fmt.Sprintf(`INSERT INTO %s (%s) VALUES (%s) ON CONFLICT (id) DO NOTHING`,
			table,
			joinStrings(quotedCols),
			joinStrings(placeholders),
		)
		if _, err := tx.Exec(query, vals...); err != nil {
			return fmt.Errorf("插入 %s 行失败: %w", table, err)
		}
	}
	return nil
}

// dbExecer 支持事务执行的接口
type dbExecer interface {
	Exec(query string, args ...any) (interface{ RowsAffected() (int64, error) }, error)
}

// sqlTxExecer 包装 *sql.Tx 以满足 dbExecer 接口（实际 sql.Result 已实现 RowsAffected）
type sqlTxWrapper struct {
	tx interface {
		Exec(string, ...any) (sqlResult, error)
	}
}

type sqlResult interface {
	RowsAffected() (int64, error)
}

func joinStrings(ss []string) string {
	result := ""
	for i, s := range ss {
		if i > 0 {
			result += ", "
		}
		result += s
	}
	return result
}
