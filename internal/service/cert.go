package service

import (
	"allfast/internal/database"
	"allfast/internal/model"
	"crypto/ecdsa"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"database/sql"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"strings"
	"time"
)

// ===== 证书解析工具 =====

// ParseCertPEM 解析 PEM 证书，返回 x509.Certificate
func ParseCertPEM(certPEM string) (*x509.Certificate, error) {
	block, _ := pem.Decode([]byte(certPEM))
	if block == nil {
		return nil, fmt.Errorf("无法解析证书 PEM")
	}
	return x509.ParseCertificate(block.Bytes)
}

// ParsePrivateKeyPEM 解析 PEM 私钥
func ParsePrivateKeyPEM(keyPEM string) (interface{}, error) {
	block, _ := pem.Decode([]byte(keyPEM))
	if block == nil {
		return nil, fmt.Errorf("无法解析私钥 PEM")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	if key, err := x509.ParseECPrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	if key, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	return nil, fmt.Errorf("无法识别的私钥格式")
}

// GetCertFingerprint 计算证书 SHA256 指纹
func GetCertFingerprint(certPEM string) (string, error) {
	block, _ := pem.Decode([]byte(certPEM))
	if block == nil {
		return "", fmt.Errorf("无法解析证书 PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "", err
	}
	h := sha256.Sum256(cert.Raw)
	return hex.EncodeToString(h[:]), nil
}

// ===== 证书 CRUD =====

// SaveUploadedCert 保存上传的证书
func SaveUploadedCert(certPEM, keyPEM string) (*model.SSLCertificate, error) {
	// 解析证书
	cert, err := ParseCertPEM(certPEM)
	if err != nil {
		return nil, fmt.Errorf("解析证书失败: %v", err)
	}
	// 解析私钥
	if _, err := ParsePrivateKeyPEM(keyPEM); err != nil {
		return nil, fmt.Errorf("解析私钥失败: %v", err)
	}
	// 指纹
	fp, err := GetCertFingerprint(certPEM)
	if err != nil {
		return nil, fmt.Errorf("计算指纹失败: %v", err)
	}
	// 检查重复
	var existID int64
	err = database.DB.QueryRow("SELECT id FROM ssl_certificates WHERE fingerprint = $1", fp).Scan(&existID)
	if err == nil {
		return nil, fmt.Errorf("证书已存在 (ID=%d)", existID)
	}

	// 提取域名
	domainSet := make(map[string]bool)
	if cert.Subject.CommonName != "" {
		domainSet[cert.Subject.CommonName] = true
	}
	for _, dns := range cert.DNSNames {
		domainSet[dns] = true
	}
	var domains []string
	for d := range domainSet {
		domains = append(domains, d)
	}
	domainList := strings.Join(domains, ",")

	// 颁发机构
	brand := "Unknown"
	if len(cert.Issuer.Organization) > 0 {
		brand = cert.Issuer.Organization[0]
	} else if cert.Issuer.CommonName != "" {
		brand = cert.Issuer.CommonName
	}

	now := time.Now()
	notBefore := cert.NotBefore
	notAfter := cert.NotAfter

	status := "valid"
	if now.After(notAfter) {
		status = "expired"
	}

	var id int64
	err = database.DB.QueryRow(`
		INSERT INTO ssl_certificates (domains, brand, source, certificate, private_key, issuer_cert, fingerprint, not_before, not_after, status, created_at, updated_at)
		VALUES ($1, $2, 'upload', $3, $4, '', $5, $6, $7, $8, $9, $10) RETURNING id`,
		domainList, brand, certPEM, keyPEM, fp, notBefore, notAfter, status, now, now,
	).Scan(&id)
	if err != nil {
		return nil, fmt.Errorf("保存证书失败: %v", err)
	}

	return &model.SSLCertificate{
		ID:          id,
		Domains:     domainList,
		Brand:       brand,
		Source:      "upload",
		Fingerprint: fp,
		NotBefore:   &notBefore,
		NotAfter:    &notAfter,
		Status:      status,
		CreatedAt:   now,
		UpdatedAt:   now,
	}, nil
}

// SaveACMECert 保存 ACME 申请的证书
func SaveACMECert(certPEM, keyPEM, issuerCertPEM, caName string) (*model.SSLCertificate, error) {
	cert, err := ParseCertPEM(certPEM)
	if err != nil {
		return nil, fmt.Errorf("解析证书失败: %v", err)
	}
	fp, err := GetCertFingerprint(certPEM)
	if err != nil {
		return nil, fmt.Errorf("计算指纹失败: %v", err)
	}

	// 提取域名
	domainSet := make(map[string]bool)
	if cert.Subject.CommonName != "" {
		domainSet[cert.Subject.CommonName] = true
	}
	for _, dns := range cert.DNSNames {
		domainSet[dns] = true
	}
	var domains []string
	for d := range domainSet {
		domains = append(domains, d)
	}
	domainList := strings.Join(domains, ",")

	// 颁发机构
	brand := caName
	if brand == "" {
		if len(cert.Issuer.Organization) > 0 {
			brand = cert.Issuer.Organization[0]
		} else if cert.Issuer.CommonName != "" {
			brand = cert.Issuer.CommonName
		}
	}

	now := time.Now()
	notBefore := cert.NotBefore
	notAfter := cert.NotAfter

	var id int64
	err = database.DB.QueryRow(`
		INSERT INTO ssl_certificates (domains, brand, source, certificate, private_key, issuer_cert, fingerprint, not_before, not_after, status, created_at, updated_at)
		VALUES ($1, $2, 'acme', $3, $4, $5, $6, $7, $8, 'valid', $9, $10) RETURNING id`,
		domainList, brand, certPEM, keyPEM, issuerCertPEM, fp, notBefore, notAfter, now, now,
	).Scan(&id)
	if err != nil {
		return nil, fmt.Errorf("保存证书失败: %v", err)
	}

	return &model.SSLCertificate{
		ID:          id,
		Domains:     domainList,
		Brand:       brand,
		Source:      "acme",
		Fingerprint: fp,
		NotBefore:   &notBefore,
		NotAfter:    &notAfter,
		Status:      "valid",
		CreatedAt:   now,
		UpdatedAt:   now,
	}, nil
}

// ListSSLCerts 列出证书
func ListSSLCerts() ([]model.SSLCertificate, error) {
	rows, err := database.DB.Query(`
		SELECT id, domains, brand, source, fingerprint, not_before, not_after, status, error_message, created_at, updated_at
		FROM ssl_certificates ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var certs []model.SSLCertificate
	for rows.Next() {
		var c model.SSLCertificate
		var notBefore, notAfter sql.NullTime
		err := rows.Scan(&c.ID, &c.Domains, &c.Brand, &c.Source, &c.Fingerprint,
			&notBefore, &notAfter, &c.Status, &c.ErrorMsg, &c.CreatedAt, &c.UpdatedAt)
		if err != nil {
			continue
		}
		if notBefore.Valid {
			c.NotBefore = &notBefore.Time
		}
		if notAfter.Valid {
			c.NotAfter = &notAfter.Time
		}
		certs = append(certs, c)
	}
	return certs, nil
}

// GetSSLCert 获取证书详情（含证书内容）
func GetSSLCert(id int64) (*model.SSLCertificate, error) {
	var c model.SSLCertificate
	var notBefore, notAfter sql.NullTime
	err := database.DB.QueryRow(`
		SELECT id, domains, brand, source, certificate, private_key, issuer_cert, fingerprint, not_before, not_after, status, error_message, created_at, updated_at
		FROM ssl_certificates WHERE id = $1`, id,
	).Scan(&c.ID, &c.Domains, &c.Brand, &c.Source, &c.Certificate, &c.PrivateKey, &c.IssuerCert,
		&c.Fingerprint, &notBefore, &notAfter, &c.Status, &c.ErrorMsg, &c.CreatedAt, &c.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("证书不存在")
	}
	if notBefore.Valid {
		c.NotBefore = &notBefore.Time
	}
	if notAfter.Valid {
		c.NotAfter = &notAfter.Time
	}

	// 从 PEM 解析额外信息
	if c.Certificate != "" {
		if cert, err := ParseCertPEM(c.Certificate); err == nil {
			// SAN
			for _, dns := range cert.DNSNames {
				c.SANDomains = append(c.SANDomains, dns)
			}
			for _, ip := range cert.IPAddresses {
				c.SANIPs = append(c.SANIPs, ip.String())
			}
			for _, email := range cert.EmailAddresses {
				c.SANEmails = append(c.SANEmails, email)
			}
			// 算法和密钥长度
			switch cert.PublicKeyAlgorithm {
			case x509.RSA:
				c.Algorithm = "RSA"
				if pub, ok := cert.PublicKey.(*rsa.PublicKey); ok {
					c.KeySize = fmt.Sprintf("%d", pub.N.BitLen())
				}
			case x509.ECDSA:
				c.Algorithm = "ECDSA"
				if pub, ok := cert.PublicKey.(*ecdsa.PublicKey); ok {
					c.KeySize = pub.Curve.Params().Name
				}
			case x509.Ed25519:
				c.Algorithm = "Ed25519"
				c.KeySize = "256"
			default:
				c.Algorithm = cert.PublicKeyAlgorithm.String()
			}
		}
	}

	return &c, nil
}

// DeleteSSLCert 删除证书
func DeleteSSLCert(id int64) error {
	res, err := database.DB.Exec("DELETE FROM ssl_certificates WHERE id = $1", id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("证书不存在")
	}
	return nil
}

// CreatePendingCert 创建等待中的证书记录（ACME 申请前）
func CreatePendingCert(domains, ca string) (int64, error) {
	now := time.Now()
	var id int64
	err := database.DB.QueryRow(`
		INSERT INTO ssl_certificates (domains, brand, source, status, created_at, updated_at)
		VALUES ($1, $2, 'acme', 'issuing', $3, $4) RETURNING id`,
		domains, ca, now, now,
	).Scan(&id)
	if err != nil {
		return 0, err
	}
	return id, nil
}

// UpdateCertStatus 更新证书状态
func UpdateCertStatus(id int64, status, errMsg string) error {
	_, err := database.DB.Exec(
		"UPDATE ssl_certificates SET status = $1, error_message = $2, updated_at = $3 WHERE id = $4",
		status, errMsg, time.Now(), id,
	)
	return err
}

// UpdateCertData 填充证书数据（ACME 申请成功后）
func UpdateCertData(id int64, certPEM, keyPEM, issuerCert, brand, domains, fingerprint string, notBefore, notAfter time.Time) error {
	_, err := database.DB.Exec(`
		UPDATE ssl_certificates SET certificate=$1, private_key=$2, issuer_cert=$3, brand=$4, domains=$5,
		fingerprint=$6, not_before=$7, not_after=$8, status='valid', error_message='', updated_at=$9
		WHERE id = $10`,
		certPEM, keyPEM, issuerCert, brand, domains, fingerprint, notBefore, notAfter, time.Now(), id,
	)
	return err
}
