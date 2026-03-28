package service

import (
	"allfast/internal/database"
	"allfast/internal/model"
	"allfast/internal/provider"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"golang.org/x/crypto/acme"
)

// CA 提供商目录 URL
var CADirURLMap = map[string]string{
	"letsencrypt": "https://acme-v02.api.letsencrypt.org/directory",
	"zerossl":     "https://acme.zerossl.com/v2/DV90",
	"litessl":     "https://acme.litessl.com/acme/v2/directory",
}

// CA 提供商信息
var CAProviders = []model.CAProvider{
	{Name: "letsencrypt", DisplayName: "Let's Encrypt", DirURL: CADirURLMap["letsencrypt"], Wildcard: true, NeedEAB: false, ValidityDays: 90},
	{Name: "zerossl", DisplayName: "ZeroSSL", DirURL: CADirURLMap["zerossl"], Wildcard: true, NeedEAB: true, ValidityDays: 90},
	{Name: "litessl", DisplayName: "LiteSSL", DirURL: CADirURLMap["litessl"], Wildcard: true, NeedEAB: true, ValidityDays: 90},
}

// ===== ACME 账户管理 =====

// createACMEAccount 每次申请随机生成新的 ACME 账户
func createACMEAccount(ctx context.Context, ca string) (*acme.Client, crypto.Signer, error) {
	dirURL := CADirURLMap[ca]
	if dirURL == "" {
		return nil, nil, fmt.Errorf("不支持的 CA: %s", ca)
	}

	// 随机生成邮箱
	randBytes := make([]byte, 8)
	rand.Read(randBytes)
	email := fmt.Sprintf("acme-%x@allfast.cn", randBytes)

	// 生成新密钥
	newKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("生成密钥失败: %v", err)
	}

	client := &acme.Client{Key: newKey, DirectoryURL: dirURL}

	// 获取 EAB（如需要）
	var eabKid, eabHmac string
	switch ca {
	case "zerossl":
		eabKid, eabHmac, err = fetchZeroSSLEAB(email)
		if err != nil {
			return nil, nil, fmt.Errorf("获取 ZeroSSL EAB 失败: %v", err)
		}
	case "litessl":
		eabKid, eabHmac, err = fetchLiteSSLEAB(email)
		if err != nil {
			return nil, nil, fmt.Errorf("获取 LiteSSL EAB 失败: %v", err)
		}
	}

	// 注册账户
	acct := &acme.Account{Contact: []string{"mailto:" + email}}
	if eabKid != "" && eabHmac != "" {
		// EAB HMAC key 是 base64url 编码的，需要解码
		decodedKey, decErr := base64.RawURLEncoding.DecodeString(eabHmac)
		if decErr != nil {
			decodedKey, decErr = base64.StdEncoding.DecodeString(eabHmac)
			if decErr != nil {
				return nil, nil, fmt.Errorf("EAB HMAC key 解码失败: %v", decErr)
			}
		}
		acct.ExternalAccountBinding = &acme.ExternalAccountBinding{
			KID: eabKid,
			Key: decodedKey,
		}
	}

	_, err = client.Register(ctx, acct, acme.AcceptTOS)
	if err != nil {
		return nil, nil, fmt.Errorf("ACME 账户注册失败: %v", err)
	}

	log.Printf("[ACME] 新账户注册成功: %s (CA=%s)", email, ca)
	return client, newKey, nil
}

// fetchZeroSSLEAB 从 ZeroSSL API 获取 EAB
func fetchZeroSSLEAB(email string) (string, string, error) {
	reqBody := fmt.Sprintf(`{"email":"%s"}`, email)
	resp, err := http.Post("https://api.zerossl.com/acme/eab-credentials-email", "application/json", strings.NewReader(reqBody))
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return "", "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}
	var result map[string]interface{}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", "", err
	}
	kid, _ := result["eab_kid"].(string)
	hmac, _ := result["eab_hmac_key"].(string)
	if kid == "" || hmac == "" {
		return "", "", fmt.Errorf("EAB 信息不完整")
	}
	return kid, hmac, nil
}

// fetchLiteSSLEAB 从宝塔 API 获取 LiteSSL EAB
func fetchLiteSSLEAB(email string) (string, string, error) {
	reqBody := fmt.Sprintf(`{"email":"%s"}`, email)
	resp, err := http.Post("https://www.bt.cn/api/v3/litessl/eab", "application/json", strings.NewReader(reqBody))
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return "", "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}
	var result map[string]interface{}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", "", err
	}
	res, ok := result["res"].(map[string]interface{})
	if !ok {
		return "", "", fmt.Errorf("响应格式错误")
	}
	data, ok := res["data"].(map[string]interface{})
	if !ok {
		return "", "", fmt.Errorf("响应格式错误")
	}
	kid, _ := data["eab_kid"].(string)
	hmac, _ := data["eab_mac_key"].(string)
	if kid == "" || hmac == "" {
		return "", "", fmt.Errorf("EAB 信息不完整")
	}
	return kid, hmac, nil
}

// ===== ACME 证书申请 =====

// ApplyCert 异步申请证书
func ApplyCert(req model.ApplyCertRequest) (int64, error) {
	// 检查域名
	domains := strings.Split(req.Domains, ",")
	for i := range domains {
		domains[i] = strings.TrimSpace(domains[i])
	}

	// 检查 CA 是否支持泛域名
	for _, ca := range CAProviders {
		if ca.Name == req.CA {
			if !ca.Wildcard {
				for _, d := range domains {
					if strings.HasPrefix(d, "*.") {
						return 0, fmt.Errorf("%s 不支持泛域名证书", ca.DisplayName)
					}
				}
			}
			break
		}
	}

	// 验证 DNS 配置是否存在
	var configCount int
	err := database.DB.QueryRow(
		"SELECT COUNT(*) FROM provider_configs WHERE id = $1 AND enabled = 1", req.ConfigID,
	).Scan(&configCount)
	if err != nil || configCount == 0 {
		return 0, fmt.Errorf("未找到 DNS 提供商配置 (ID=%d)", req.ConfigID)
	}

	// 获取 CA 显示名
	caBrand := req.CA
	for _, p := range CAProviders {
		if p.Name == req.CA {
			caBrand = p.DisplayName
			break
		}
	}

	// 创建待处理记录
	certID, err := CreatePendingCert(req.Domains, caBrand)
	if err != nil {
		return 0, fmt.Errorf("创建证书记录失败: %v", err)
	}

	// 异步执行申请
	go func() {
		err := doApplyCert(certID, domains, req.CA, req.ConfigID, req.Algorithm)
		if err != nil {
			log.Printf("证书申请失败 (ID=%d): %v", certID, err)
			UpdateCertStatus(certID, "failed", err.Error())
		}
	}()

	return certID, nil
}

// doApplyCert 执行 ACME 证书申请
func doApplyCert(certID int64, domains []string, ca string, configID int64, _ string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	// 1. 获取 ACME 客户端
	client, _, err := createACMEAccount(ctx, ca)
	if err != nil {
		return fmt.Errorf("ACME 账户初始化失败: %v", err)
	}

	// 2. 创建订单
	var ids []acme.AuthzID
	for _, d := range domains {
		ids = append(ids, acme.AuthzID{Type: "dns", Value: d})
	}
	order, err := client.AuthorizeOrder(ctx, ids)
	if err != nil {
		return fmt.Errorf("创建订单失败: %v", err)
	}

	// 3. 加载 DNS 提供商
	cfgIDStr := fmt.Sprintf("%d", configID)
	var providerName, configJSON string
	err = database.DB.QueryRow(
		"SELECT provider, config FROM provider_configs WHERE id = $1", cfgIDStr,
	).Scan(&providerName, &configJSON)
	if err != nil {
		return fmt.Errorf("未找到 DNS 配置")
	}
	cfg := map[string]string{}
	if err := json.Unmarshal([]byte(configJSON), &cfg); err != nil {
		return fmt.Errorf("解析 DNS 配置失败: %v", err)
	}
	dnsProvider, err := provider.GetDNS(providerName)
	if err != nil {
		return fmt.Errorf("DNS 提供商不支持: %v", err)
	}

	// 获取 Zone 列表，用于匹配域名
	zones, err := dnsProvider.ListZones(ctx, cfg)
	if err != nil {
		return fmt.Errorf("获取 DNS Zone 失败: %v", err)
	}

	// 4. 处理 DNS-01 挑战
	type cleanupInfo struct {
		zoneID   string
		recordID string
	}
	var cleanups []cleanupInfo

	defer func() {
		// 清理 ACME DNS 验证记录
		for _, c := range cleanups {
			log.Printf("[ACME] 清理 TXT 记录: zoneID=%s recordID=%s", c.zoneID, c.recordID)
			_ = dnsProvider.DeleteRecord(context.Background(), cfg, c.zoneID, c.recordID)
		}
		log.Printf("[ACME] DNS 验证记录清理完成")
	}()

	// 收集所有挑战信息
	type challengeInfo struct {
		authzURL  string
		challenge *acme.Challenge
		domain    string
		txtFQDN   string
	}
	var challenges []challengeInfo

	// 4a. 添加所有 TXT 记录
	for _, authzURL := range order.AuthzURLs {
		authz, err := client.GetAuthorization(ctx, authzURL)
		if err != nil {
			return fmt.Errorf("获取授权失败: %v", err)
		}

		// 找到 dns-01 挑战
		var ch *acme.Challenge
		for _, c := range authz.Challenges {
			if c.Type == "dns-01" {
				ch = c
				break
			}
		}
		if ch == nil {
			return fmt.Errorf("域名 %s 没有 dns-01 挑战可用", authz.Identifier.Value)
		}

		// 计算 DNS TXT 值
		txtValue, err := client.DNS01ChallengeRecord(ch.Token)
		if err != nil {
			return fmt.Errorf("计算 DNS 验证值失败: %v", err)
		}

		// 匹配域名对应的 Zone
		domain := authz.Identifier.Value
		cleanDomain := strings.TrimPrefix(domain, "*.")
		zoneID := findZoneForDomain(zones, cleanDomain)
		if zoneID == "" {
			return fmt.Errorf("未找到域名 %s 对应的 DNS Zone，请检查 DNS 提供商是否托管该域名", cleanDomain)
		}

		// 构建 _acme-challenge 主机记录
		challengeHost := "_acme-challenge"
		var zoneName string
		for _, z := range zones {
			if z.ID == zoneID {
				zoneName = z.Name
				break
			}
		}
		if zoneName != "" && cleanDomain != zoneName {
			sub := strings.TrimSuffix(cleanDomain, "."+zoneName)
			challengeHost = "_acme-challenge." + sub
		}

		txtFQDN := challengeHost + "." + zoneName

		// 查找已有 _acme-challenge TXT 记录
		var existingRecordID string
		records, listErr := dnsProvider.ListRecords(ctx, cfg, zoneID)
		if listErr == nil {
			for _, r := range records {
				if r.Type == "TXT" && (r.Name == challengeHost || r.Name == txtFQDN || r.Name == txtFQDN+".") {
					existingRecordID = r.ID
					break
				}
			}
		}

		txtReq := model.DnsRecordRequest{
			Type:  "TXT",
			Name:  challengeHost,
			Value: txtValue,
			TTL:   600,
		}

		if existingRecordID != "" {
			// 已有记录，修改
			log.Printf("[ACME] 更新已有 TXT 记录: %s (ID=%s) -> %s", txtFQDN, existingRecordID, txtValue)
			if err := dnsProvider.UpdateRecord(ctx, cfg, zoneID, existingRecordID, txtReq); err != nil {
				return fmt.Errorf("更新 DNS 验证记录失败 (%s): %v", domain, err)
			}
			cleanups = append(cleanups, cleanupInfo{zoneID: zoneID, recordID: existingRecordID})
		} else {
			// 新增记录
			log.Printf("[ACME] 添加 TXT 记录: %s -> %s", txtFQDN, txtValue)
			record, err := dnsProvider.AddRecord(ctx, cfg, zoneID, txtReq)
			if err != nil {
				return fmt.Errorf("添加 DNS 验证记录失败 (%s): %v", domain, err)
			}
			cleanups = append(cleanups, cleanupInfo{zoneID: zoneID, recordID: record.ID})
		}
		challenges = append(challenges, challengeInfo{
			authzURL:  authzURL,
			challenge: ch,
			domain:    domain,
			txtFQDN:   txtFQDN,
		})
	}

	// 4b. 等待 DNS 传播：每 10 秒检测一次，最多 18 次（共 3 分钟）
	log.Printf("[ACME] 所有 TXT 记录已添加，等待 DNS 传播...")
	propagated := false
	for attempt := 1; attempt <= 18; attempt++ {
		time.Sleep(10 * time.Second)

		// 用 net.LookupTXT 检测第一个域名的 TXT 记录是否已生效
		if len(challenges) > 0 {
			fqdn := challenges[0].txtFQDN
			txts, err := net.LookupTXT(fqdn)
			if err == nil && len(txts) > 0 {
				log.Printf("[ACME] DNS 传播检测第 %d/18 次: %s 已生效 (%d 条记录)", attempt, fqdn, len(txts))
				propagated = true
				break
			}
			log.Printf("[ACME] DNS 传播检测第 %d/18 次: %s 尚未生效", attempt, fqdn)
		}
	}
	if !propagated {
		log.Printf("[ACME] DNS 传播等待超时（3分钟），强制继续验证...")
	}

	// 4c. 统一接受所有挑战
	for _, ci := range challenges {
		log.Printf("[ACME] 接受挑战: %s", ci.domain)
		if _, err := client.Accept(ctx, ci.challenge); err != nil {
			return fmt.Errorf("接受挑战失败 (%s): %v", ci.domain, err)
		}
	}

	// 4d. 统一等待所有授权完成
	for _, ci := range challenges {
		log.Printf("[ACME] 等待域名验证: %s", ci.domain)
		if _, err := client.WaitAuthorization(ctx, ci.authzURL); err != nil {
			return fmt.Errorf("域名验证失败 (%s): %v", ci.domain, err)
		}
		log.Printf("[ACME] 域名验证通过: %s", ci.domain)
	}

	// 5. 等待订单就绪
	order, err = client.WaitOrder(ctx, order.URI)
	if err != nil {
		return fmt.Errorf("等待订单失败: %v", err)
	}

	// 6. 生成 CSR
	certKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("生成证书密钥失败: %v", err)
	}

	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject:  pkix.Name{CommonName: domains[0]},
		DNSNames: domains,
	}, certKey)
	if err != nil {
		return fmt.Errorf("生成 CSR 失败: %v", err)
	}

	// 7. 完成订单，获取证书
	derChain, _, err := client.CreateOrderCert(ctx, order.FinalizeURL, csr, true)
	if err != nil {
		return fmt.Errorf("签发证书失败: %v", err)
	}

	// 8. 编码为 PEM
	var certPEM, issuerPEM strings.Builder
	for i, der := range derChain {
		block := &pem.Block{Type: "CERTIFICATE", Bytes: der}
		if i == 0 {
			pem.Encode(&certPEM, block)
		} else {
			pem.Encode(&issuerPEM, block)
		}
	}

	keyBytes, _ := x509.MarshalPKCS8PrivateKey(certKey)
	keyPEM := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyBytes}))

	// 9. 解析证书信息
	parsedCert, err := ParseCertPEM(certPEM.String())
	if err != nil {
		return fmt.Errorf("解析签发的证书失败: %v", err)
	}
	fp, _ := GetCertFingerprint(certPEM.String())

	// 颁发机构名
	brand := ca
	for _, p := range CAProviders {
		if p.Name == ca {
			brand = p.DisplayName
			break
		}
	}

	// 提取域名
	domainSet := make(map[string]bool)
	if parsedCert.Subject.CommonName != "" {
		domainSet[parsedCert.Subject.CommonName] = true
	}
	for _, dns := range parsedCert.DNSNames {
		domainSet[dns] = true
	}
	var domainList []string
	for d := range domainSet {
		domainList = append(domainList, d)
	}

	// 10. 更新数据库
	err = UpdateCertData(certID, certPEM.String(), keyPEM, issuerPEM.String(), brand,
		strings.Join(domainList, ","), fp, parsedCert.NotBefore, parsedCert.NotAfter)
	if err != nil {
		return fmt.Errorf("保存证书数据失败: %v", err)
	}

	log.Printf("证书申请成功 (ID=%d): %s", certID, strings.Join(domainList, ", "))
	return nil
}

// findZoneForDomain 查找域名对应的 Zone ID（匹配最长后缀）
func findZoneForDomain(zones []model.DnsZone, domain string) string {
	var bestZone string
	var bestLen int
	for _, z := range zones {
		if domain == z.Name || strings.HasSuffix(domain, "."+z.Name) {
			if len(z.Name) > bestLen {
				bestLen = len(z.Name)
				bestZone = z.ID
			}
		}
	}
	return bestZone
}

// GetCAProviders 返回可用 CA 列表
func GetCAProviders() []model.CAProvider {
	return CAProviders
}

// GetDNSConfigsForACME 获取支持 DNS 管理的提供商配置列表
func GetDNSConfigsForACME() ([]map[string]interface{}, error) {
	rows, err := database.DB.Query(
		"SELECT id, name, provider FROM provider_configs WHERE enabled = 1 ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []map[string]interface{}
	dnsNames := provider.ListAllDNS()
	dnsSet := make(map[string]bool)
	for _, n := range dnsNames {
		dnsSet[n] = true
	}

	for rows.Next() {
		var id int64
		var name, prov string
		var nullName sql.NullString
		if err := rows.Scan(&id, &nullName, &prov); err != nil {
			continue
		}
		if nullName.Valid {
			name = nullName.String
		}
		// 只返回支持 DNS 管理的提供商
		if !dnsSet[prov] {
			continue
		}
		result = append(result, map[string]interface{}{
			"id":       id,
			"name":     name,
			"provider": prov,
		})
	}
	return result, nil
}
