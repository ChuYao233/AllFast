package service

import (
	"allfast/internal/database"
	"allfast/internal/model"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"strings"
	"time"
)

// 加密算法族
var SelfSignAlgorithmFamilies = []map[string]string{
	{"value": "rsa", "label": "RSA（大整数分解算法）"},
	{"value": "ecdsa", "label": "ECDSA（椭圆曲线数字签名算法）"},
	{"value": "ed25519", "label": "EdDSA（爱德华曲线数字签名算法）"},
	{"value": "sm2", "label": "SM2（国密椭圆曲线算法）"},
}

// 各算法对应的密钥长度选项
var SelfSignKeySizes = map[string][]map[string]string{
	"rsa": {
		{"value": "2048", "label": "2048 位", "recommended": "true"},
		{"value": "3072", "label": "3072 位"},
		{"value": "4096", "label": "4096 位"},
	},
	"ecdsa": {
		{"value": "p256", "label": "P-256（256 位）", "recommended": "true"},
		{"value": "p384", "label": "P-384（384 位）"},
		{"value": "p521", "label": "P-521（521 位）"},
	},
	"ed25519": {
		{"value": "256", "label": "Ed25519（256 位，固定）", "recommended": "true"},
	},
	"sm2": {
		{"value": "256", "label": "256 位（固定）", "recommended": "true"},
	},
}

// 证书类型（三种）
var SelfSignCertTypes = []map[string]string{
	{"value": "root_ca", "label": "根 CA 证书"},
	{"value": "intermediate_ca", "label": "中间 CA 证书"},
	{"value": "cert", "label": "普通证书"},
}

// 普通证书用途
var SelfSignPurposes = []map[string]string{
	{"value": "server", "label": "服务器证书（TLS）"},
	{"value": "client", "label": "客户端证书"},
	{"value": "email", "label": "邮件证书（S/MIME）"},
}

// generateKeyPair 根据算法族和密钥长度生成密钥对
func generateKeyPair(algo, keySize string) (crypto.Signer, error) {
	switch algo {
	case "rsa":
		bits := 2048
		switch keySize {
		case "3072":
			bits = 3072
		case "4096":
			bits = 4096
		}
		return rsa.GenerateKey(rand.Reader, bits)
	case "ecdsa":
		switch keySize {
		case "p384":
			return ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
		case "p521":
			return ecdsa.GenerateKey(elliptic.P521(), rand.Reader)
		default:
			return ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		}
	case "ed25519":
		_, priv, err := ed25519.GenerateKey(rand.Reader)
		return priv, err
	case "sm2":
		// SM2 需要 github.com/emmansun/gmsm 库，暂用 ECDSA P-256 替代
		return ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	default:
		return nil, fmt.Errorf("不支持的算法: %s", algo)
	}
}

// encodePrivateKeyPEM 将私钥编码为 PEM
func encodePrivateKeyPEM(key crypto.Signer) (string, error) {
	var keyBytes []byte
	var err error
	var blockType string

	switch k := key.(type) {
	case *rsa.PrivateKey:
		keyBytes, err = x509.MarshalPKCS8PrivateKey(k)
		blockType = "PRIVATE KEY"
	case *ecdsa.PrivateKey:
		keyBytes, err = x509.MarshalPKCS8PrivateKey(k)
		blockType = "PRIVATE KEY"
	case ed25519.PrivateKey:
		keyBytes, err = x509.MarshalPKCS8PrivateKey(k)
		blockType = "PRIVATE KEY"
	default:
		return "", fmt.Errorf("不支持的密钥类型")
	}
	if err != nil {
		return "", err
	}

	return string(pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: keyBytes})), nil
}

// certFingerprint 计算证书 SHA1 指纹
func certFingerprint(certDER []byte) string {
	h := sha1.Sum(certDER)
	return hex.EncodeToString(h[:])
}

// CreateSelfSignedCert 创建自签证书
func CreateSelfSignedCert(req model.CreateSelfSignedRequest) (*model.SelfSignedCert, error) {
	// 校验证书类型
	validType := false
	for _, t := range SelfSignCertTypes {
		if t["value"] == req.CertType {
			validType = true
			break
		}
	}
	if !validType {
		return nil, fmt.Errorf("不支持的证书类型: %s", req.CertType)
	}

	// 默认有效期
	if req.ValidityDays <= 0 {
		switch req.CertType {
		case "root_ca":
			req.ValidityDays = 3650 // 10 年
		case "intermediate_ca":
			req.ValidityDays = 1825 // 5 年
		default:
			req.ValidityDays = 365 // 1 年
		}
	}

	// 生成密钥
	// 校验普通证书必须指定用途
	if req.CertType == "cert" && req.Purpose == "" {
		return nil, fmt.Errorf("普通证书必须指定用途（server/client/email）")
	}

	key, err := generateKeyPair(req.Algorithm, req.KeySize)
	if err != nil {
		return nil, fmt.Errorf("生成密钥失败: %v", err)
	}

	// 存储用的算法标识
	algoLabel := req.Algorithm + "_" + req.KeySize

	// 构建 Subject
	subject := pkix.Name{
		CommonName: req.SubjectCN,
	}
	if req.SubjectO != "" {
		subject.Organization = []string{req.SubjectO}
	}
	if req.SubjectOU != "" {
		subject.OrganizationalUnit = []string{req.SubjectOU}
	}
	if req.SubjectC != "" {
		subject.Country = []string{req.SubjectC}
	}
	if req.SubjectST != "" {
		subject.Province = []string{req.SubjectST}
	}
	if req.SubjectL != "" {
		subject.Locality = []string{req.SubjectL}
	}

	// 序列号
	serialNumber, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))

	now := time.Now()
	notAfter := now.Add(time.Duration(req.ValidityDays) * 24 * time.Hour)

	// 构建证书模板
	template := &x509.Certificate{
		SerialNumber:          serialNumber,
		Subject:               subject,
		NotBefore:             now,
		NotAfter:              notAfter,
		BasicConstraintsValid: true,
	}

	// 根据类型设置证书属性
	switch req.CertType {
	case "root_ca":
		template.IsCA = true
		template.MaxPathLen = 1
		template.MaxPathLenZero = false
		template.KeyUsage = x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature

	case "intermediate_ca":
		template.IsCA = true
		template.MaxPathLen = 0
		template.MaxPathLenZero = true
		template.KeyUsage = x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature

	case "cert":
		// 根据用途设置
		switch req.Purpose {
		case "server":
			template.KeyUsage = x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment
			template.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
			if req.Domains != "" {
				for _, d := range strings.Split(req.Domains, ",") {
					d = strings.TrimSpace(d)
					if d != "" {
						template.DNSNames = append(template.DNSNames, d)
					}
				}
			}
			if req.IPs != "" {
				for _, ip := range strings.Split(req.IPs, ",") {
					ip = strings.TrimSpace(ip)
					if ip != "" {
						if parsed := net.ParseIP(ip); parsed != nil {
							template.IPAddresses = append(template.IPAddresses, parsed)
						}
					}
				}
			}
		case "client":
			template.KeyUsage = x509.KeyUsageDigitalSignature
			template.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
		case "email":
			template.KeyUsage = x509.KeyUsageDigitalSignature | x509.KeyUsageContentCommitment
			template.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageEmailProtection}
			if req.Emails != "" {
				for _, e := range strings.Split(req.Emails, ",") {
					e = strings.TrimSpace(e)
					if e != "" {
						template.EmailAddresses = append(template.EmailAddresses, e)
					}
				}
			}
		}
	}

	// 确定签发者
	var parentCert *x509.Certificate
	var parentKey crypto.Signer

	if req.CertType == "root_ca" {
		// 根 CA 自签
		parentCert = template
		parentKey = key
	} else {
		// 需要签发者
		if req.IssuerID == nil || *req.IssuerID == 0 {
			return nil, fmt.Errorf("非根 CA 证书必须指定签发者")
		}
		issuer, loadErr := loadIssuerCert(*req.IssuerID)
		if loadErr != nil {
			return nil, fmt.Errorf("加载签发者证书失败: %v", loadErr)
		}

		// 签发链校验：中间 CA 只能由根 CA 签发，普通证书只能由中间 CA 签发
		if req.CertType == "intermediate_ca" && issuer.CertType != "root_ca" {
			return nil, fmt.Errorf("中间 CA 只能由根 CA 签发")
		}
		if req.CertType == "cert" && issuer.CertType != "intermediate_ca" {
			return nil, fmt.Errorf("普通证书只能由中间 CA 签发")
		}

		// 算法一致性校验：子证书算法必须和签发 CA 一致
		issuerAlgoFamily := extractAlgoFamily(issuer.Algorithm)
		if req.Algorithm != issuerAlgoFamily {
			return nil, fmt.Errorf("子证书算法必须和签发 CA 一致（CA 使用 %s，当前选择 %s）", issuerAlgoFamily, req.Algorithm)
		}

		parentCert = issuer.Cert
		parentKey = issuer.Key
	}

	// 签发证书
	certDER, err := x509.CreateCertificate(rand.Reader, template, parentCert, key.Public(), parentKey)
	if err != nil {
		return nil, fmt.Errorf("签发证书失败: %v", err)
	}

	// 编码 PEM
	certPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER}))
	keyPEM, err := encodePrivateKeyPEM(key)
	if err != nil {
		return nil, fmt.Errorf("编码私钥失败: %v", err)
	}

	fp := certFingerprint(certDER)

	// KeyUsage 描述
	kuDesc := describeKeyUsage(template.KeyUsage)
	ekuDesc := describeExtKeyUsage(template.ExtKeyUsage)

	// 保存到数据库
	isCA := 0
	if template.IsCA {
		isCA = 1
	}

	var issuerID interface{}
	if req.IssuerID != nil && *req.IssuerID > 0 {
		issuerID = *req.IssuerID
	}

	purpose := req.Purpose
	if req.CertType != "cert" {
		purpose = ""
	}

	var id int64
	err = database.DB.QueryRow(`
		INSERT INTO self_signed_certs (name, cert_type, algorithm, subject_cn, subject_o, subject_ou,
			subject_c, subject_st, subject_l, domains, ips, emails, purpose, issuer_id,
			certificate, private_key, serial_number, fingerprint, not_before, not_after,
			validity_days, is_ca, key_usage, ext_key_usage)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24) RETURNING id`,
		req.Name, req.CertType, algoLabel, req.SubjectCN, req.SubjectO, req.SubjectOU,
		req.SubjectC, req.SubjectST, req.SubjectL, req.Domains, req.IPs, req.Emails, purpose, issuerID,
		certPEM, keyPEM, serialNumber.Text(16), fp, now, notAfter,
		req.ValidityDays, isCA, kuDesc, ekuDesc,
	).Scan(&id)
	if err != nil {
		return nil, fmt.Errorf("保存证书失败: %v", err)
	}

	return &model.SelfSignedCert{
		ID:           id,
		Name:         req.Name,
		CertType:     req.CertType,
		Algorithm:    req.Algorithm,
		SubjectCN:    req.SubjectCN,
		SerialNumber: serialNumber.Text(16),
		Fingerprint:  fp,
		ValidityDays: req.ValidityDays,
		IsCA:         template.IsCA,
	}, nil
}

// issuerInfo 签发者信息
type issuerInfo struct {
	Cert      *x509.Certificate
	Key       crypto.Signer
	CertType  string
	Algorithm string // 存储的完整算法标识，如 ecdsa_p256
}

// extractAlgoFamily 从存储的算法标识中提取算法族，如 ecdsa_p256 -> ecdsa, rsa_2048 -> rsa
func extractAlgoFamily(algoLabel string) string {
	parts := strings.SplitN(algoLabel, "_", 2)
	return parts[0]
}

// loadIssuerCert 从数据库加载签发者证书和私钥
func loadIssuerCert(issuerID int64) (*issuerInfo, error) {
	var certPEM, keyPEM, certType, algorithm string
	var isCA int
	err := database.DB.QueryRow(
		"SELECT certificate, private_key, is_ca, cert_type, algorithm FROM self_signed_certs WHERE id = $1", issuerID,
	).Scan(&certPEM, &keyPEM, &isCA, &certType, &algorithm)
	if err != nil {
		return nil, fmt.Errorf("未找到签发者 (ID=%d)", issuerID)
	}
	if isCA == 0 {
		return nil, fmt.Errorf("签发者 (ID=%d) 不是 CA 证书", issuerID)
	}

	// 解析证书
	block, _ := pem.Decode([]byte(certPEM))
	if block == nil {
		return nil, fmt.Errorf("解析签发者证书 PEM 失败")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("解析签发者证书失败: %v", err)
	}

	// 解析私钥
	keyBlock, _ := pem.Decode([]byte(keyPEM))
	if keyBlock == nil {
		return nil, fmt.Errorf("解析签发者私钥 PEM 失败")
	}
	rawKey, err := x509.ParsePKCS8PrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, fmt.Errorf("解析签发者私钥失败: %v", err)
	}
	signer, ok := rawKey.(crypto.Signer)
	if !ok {
		return nil, fmt.Errorf("签发者私钥不支持签名")
	}

	return &issuerInfo{
		Cert:      cert,
		Key:       signer,
		CertType:  certType,
		Algorithm: algorithm,
	}, nil
}

// ListSelfSignedCerts 列表查询
func ListSelfSignedCerts() ([]model.SelfSignedCert, error) {
	rows, err := database.DB.Query(`
		SELECT s.id, s.name, s.cert_type, s.algorithm, s.subject_cn,
		       s.subject_o, s.subject_ou, s.subject_c, s.purpose,
		       s.issuer_id, COALESCE(p.name, '') AS issuer_name,
		       s.serial_number, s.fingerprint, s.not_before, s.not_after,
		       s.validity_days, s.is_ca, s.key_usage, s.ext_key_usage, s.created_at
		FROM self_signed_certs s
		LEFT JOIN self_signed_certs p ON s.issuer_id = p.id
		ORDER BY s.id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var certs []model.SelfSignedCert
	for rows.Next() {
		var c model.SelfSignedCert
		var isCA int
		if err := rows.Scan(
			&c.ID, &c.Name, &c.CertType, &c.Algorithm, &c.SubjectCN,
			&c.SubjectO, &c.SubjectOU, &c.SubjectC, &c.Purpose,
			&c.IssuerID, &c.IssuerName,
			&c.SerialNumber, &c.Fingerprint, &c.NotBefore, &c.NotAfter,
			&c.ValidityDays, &isCA, &c.KeyUsage, &c.ExtKeyUsage, &c.CreatedAt,
		); err != nil {
			continue
		}
		c.IsCA = isCA == 1
		certs = append(certs, c)
	}
	return certs, nil
}

// GetSelfSignedCert 获取单个证书详情
func GetSelfSignedCert(id int64) (*model.SelfSignedCert, error) {
	var c model.SelfSignedCert
	var isCA int
	err := database.DB.QueryRow(`
		SELECT s.id, s.name, s.cert_type, s.algorithm, s.subject_cn,
		       s.subject_o, s.subject_ou, s.subject_c, s.subject_st, s.subject_l,
		       s.domains, s.ips, s.emails, s.purpose,
		       s.issuer_id, COALESCE(p.name, '') AS issuer_name,
		       s.certificate, s.private_key, s.serial_number, s.fingerprint,
		       s.not_before, s.not_after, s.validity_days, s.is_ca,
		       s.key_usage, s.ext_key_usage, s.created_at
		FROM self_signed_certs s
		LEFT JOIN self_signed_certs p ON s.issuer_id = p.id
		WHERE s.id = $1`, id,
	).Scan(
		&c.ID, &c.Name, &c.CertType, &c.Algorithm, &c.SubjectCN,
		&c.SubjectO, &c.SubjectOU, &c.SubjectC, &c.SubjectST, &c.SubjectL,
		&c.Domains, &c.IPs, &c.Emails, &c.Purpose,
		&c.IssuerID, &c.IssuerName,
		&c.Certificate, &c.PrivateKey, &c.SerialNumber, &c.Fingerprint,
		&c.NotBefore, &c.NotAfter, &c.ValidityDays, &isCA,
		&c.KeyUsage, &c.ExtKeyUsage, &c.CreatedAt,
	)
	if err != nil {
		return nil, err
	}
	c.IsCA = isCA == 1
	return &c, nil
}

// DeleteSelfSignedCert 删除证书
func DeleteSelfSignedCert(id int64) error {
	// 检查是否有子证书引用
	var childCount int
	database.DB.QueryRow("SELECT COUNT(*) FROM self_signed_certs WHERE issuer_id = $1", id).Scan(&childCount)
	if childCount > 0 {
		return fmt.Errorf("该 CA 证书还有 %d 个子证书，无法删除", childCount)
	}

	_, err := database.DB.Exec("DELETE FROM self_signed_certs WHERE id = $1", id)
	return err
}

// ListCACerts 列出可作为签发者的 CA 证书
func ListCACerts() ([]model.SelfSignedCert, error) {
	rows, err := database.DB.Query(`
		SELECT id, name, cert_type, algorithm, subject_cn, not_after
		FROM self_signed_certs WHERE is_ca = 1 ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var cas []model.SelfSignedCert
	for rows.Next() {
		var c model.SelfSignedCert
		if err := rows.Scan(&c.ID, &c.Name, &c.CertType, &c.Algorithm, &c.SubjectCN, &c.NotAfter); err != nil {
			continue
		}
		c.IsCA = true
		cas = append(cas, c)
	}
	return cas, nil
}

// describeKeyUsage 描述 KeyUsage
func describeKeyUsage(ku x509.KeyUsage) string {
	var parts []string
	if ku&x509.KeyUsageDigitalSignature != 0 {
		parts = append(parts, "数字签名")
	}
	if ku&x509.KeyUsageContentCommitment != 0 {
		parts = append(parts, "内容承诺")
	}
	if ku&x509.KeyUsageKeyEncipherment != 0 {
		parts = append(parts, "密钥加密")
	}
	if ku&x509.KeyUsageCertSign != 0 {
		parts = append(parts, "证书签名")
	}
	if ku&x509.KeyUsageCRLSign != 0 {
		parts = append(parts, "CRL签名")
	}
	return strings.Join(parts, ", ")
}

// describeExtKeyUsage 描述 ExtKeyUsage
func describeExtKeyUsage(ekus []x509.ExtKeyUsage) string {
	var parts []string
	for _, eku := range ekus {
		switch eku {
		case x509.ExtKeyUsageServerAuth:
			parts = append(parts, "服务器认证")
		case x509.ExtKeyUsageClientAuth:
			parts = append(parts, "客户端认证")
		case x509.ExtKeyUsageEmailProtection:
			parts = append(parts, "邮件保护")
		}
	}
	return strings.Join(parts, ", ")
}
