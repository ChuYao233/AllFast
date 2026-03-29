package aliyun

import (
	"allfast/internal/model"
	"allfast/internal/provider"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"
)

func init() {
	provider.RegisterStats("aliyun", &AliyunESAProvider{})
}

// GetTimeSeries 阿里云 ESA DescribeSiteTimeSeriesData（时序流量数据）
func (a *AliyunESAProvider) GetTimeSeries(ctx context.Context, cfg map[string]string, zoneID string, from, to time.Time) ([]model.StatPoint, error) {
	// zoneID 格式：siteID:domain（阿里云 ESA 不支持按域名过滤，只用 siteID）
	parts := strings.SplitN(zoneID, ":", 2)
	siteID := parts[0]

	// Interval 单位为秒：3600=小时，86400=天
	intervalInt := 3600
	if to.Sub(from) > 10*24*time.Hour {
		intervalInt = 86400
	}

	// 分两次查询：流量和请求数（阿里云 ESA 不支持按域名过滤，只能按 SiteId 汇总整站数据）
	fetch := func(fieldName string) (map[string]int64, error) {
		type fieldItem struct {
			FieldName string   `json:"FieldName"`
			Dimension []string `json:"Dimension"`
		}
		fieldsJSON, _ := json.Marshal([]fieldItem{{FieldName: fieldName, Dimension: []string{"ALL"}}})
		params := map[string]string{
			"Action":    "DescribeSiteTimeSeriesData",
			"SiteId":    siteID,
			"StartTime": from.UTC().Format("2006-01-02T15:04:05Z"),
			"EndTime":   to.UTC().Format("2006-01-02T15:04:05Z"),
			"Interval":  fmt.Sprintf("%d", intervalInt),
			"Fields":    string(fieldsJSON),
		}
		log.Printf("[AliyunESA DEBUG] GetTimeSeries params: %v", params)
		body, err := a.doRequest(ctx, cfg, "POST", params, nil)
		if err != nil {
			return nil, err
		}
		log.Printf("[AliyunESA DEBUG] GetTimeSeries raw response: %s", string(body))
		var resp struct {
			Code    string `json:"Code"`
			Message string `json:"Message"`
			Data    []struct {
				FieldName      string `json:"FieldName"`
				DimensionValue string `json:"DimensionValue"` // 域名或 ALL
				DetailData     []struct {
					TimeStamp string  `json:"TimeStamp"`
					Value     float64 `json:"Value"`
				} `json:"DetailData"`
			} `json:"Data"`
		}
		if err := json.Unmarshal(body, &resp); err != nil {
			return nil, fmt.Errorf("解析阿里云ESA时间序列失败: %w", err)
		}
		if resp.Code != "" && resp.Code != "OK" && resp.Code != "Success" {
			return nil, fmt.Errorf("阿里云ESA API错误: [%s] %s", resp.Code, resp.Message)
		}
		result := make(map[string]int64)
		for _, d := range resp.Data {
			for _, pt := range d.DetailData {
				result[pt.TimeStamp] += int64(pt.Value)
			}
		}
		return result, nil
	}

	trafficMap, err := fetch("Traffic")
	if err != nil {
		return nil, err
	}
	requestMap, err := fetch("Requests")
	if err != nil {
		return nil, err
	}

	// 合并时间戳
	tsSet := make(map[string]bool)
	for k := range trafficMap {
		tsSet[k] = true
	}
	for k := range requestMap {
		tsSet[k] = true
	}

	var pts []model.StatPoint
	for ts := range tsSet {
		t, _ := time.Parse("2006-01-02T15:04:05Z", ts)
		if t.IsZero() {
			continue
		}
		pts = append(pts, model.StatPoint{
			Time:     t.UTC(),
			Requests: requestMap[ts],
			Bytes:    trafficMap[ts],
		})
	}
	return pts, nil
}

// GetGeoDistribution 阿里云 ESA DescribeSiteTopData（按国家/地区）
func (a *AliyunESAProvider) GetGeoDistribution(ctx context.Context, cfg map[string]string, zoneID string, from, to time.Time) ([]model.GeoPoint, error) {
	// zoneID 格式：siteID:domain（地区分布不支持按域名过滤，只用 siteID）
	parts := strings.SplitN(zoneID, ":", 2)
	siteID := parts[0]

	// Fields 格式：[{"FieldName":"Requests","Dimension":["ClientCountryCode"]}]
	type geoFieldItem struct {
		FieldName string   `json:"FieldName"`
		Dimension []string `json:"Dimension,omitempty"`
	}
	geoFieldsJSON, _ := json.Marshal([]geoFieldItem{
		{FieldName: "Requests", Dimension: []string{"ClientCountryCode"}},
	})
	params := map[string]string{
		"Action":    "DescribeSiteTopData",
		"SiteId":    siteID,
		"StartTime": from.UTC().Format("2006-01-02T15:04:05Z"),
		"EndTime":   to.UTC().Format("2006-01-02T15:04:05Z"),
		"Fields":    string(geoFieldsJSON),
		"Limit":     "150",
	}
	// 注：阿里云 ESA DescribeSiteTopData 不支持按域名过滤，返回整个 Site 的地区分布
	log.Printf("[AliyunESA DEBUG] GetGeoDistribution params: %v", params)

	body, err := a.doRequest(ctx, cfg, "POST", params, nil)
	if err != nil {
		return nil, err
	}

	var resp struct {
		Code    string `json:"Code"`
		Message string `json:"Message"`
		Data    []struct {
			FieldName     string `json:"FieldName"`
			DimensionName string `json:"DimensionName"`
			DetailData    []struct {
				DimensionValue string  `json:"DimensionValue"`
				Value          float64 `json:"Value"`
			} `json:"DetailData"`
		} `json:"Data"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("解析阿里云ESA地区分布失败: %w", err)
	}
	if resp.Code != "" && resp.Code != "OK" && resp.Code != "Success" {
		return nil, fmt.Errorf("阿里云ESA API错误: [%s] %s", resp.Code, resp.Message)
	}

	var pts []model.GeoPoint
	for _, d := range resp.Data {
		for _, item := range d.DetailData {
			if item.DimensionValue == "" || item.DimensionValue == "Unknown" {
				continue
			}
			pts = append(pts, model.GeoPoint{
				CountryCode: item.DimensionValue,
				CountryName: item.DimensionValue,
				Requests:    int64(item.Value),
			})
		}
	}
	return pts, nil
}
