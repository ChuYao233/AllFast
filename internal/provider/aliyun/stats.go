package aliyun

import (
	"allfast/internal/model"
	"allfast/internal/provider"
	"context"
	"encoding/json"
	"fmt"
	"time"
)

func init() {
	provider.RegisterStats("aliyun", &AliyunESAProvider{})
}

// GetTimeSeries 阿里云 ESA DescribeSiteTrafficStatistics（小时/天粒度）
func (a *AliyunESAProvider) GetTimeSeries(ctx context.Context, cfg map[string]string, zoneID string, from, to time.Time) ([]model.StatPoint, error) {
	interval := "hour"
	if to.Sub(from) > 72*time.Hour {
		interval = "day"
	}

	params := map[string]string{
		"Action":    "DescribeSiteTrafficStatistics",
		"SiteId":    zoneID,
		"StartTime": from.UTC().Format("2006-01-02T15:04:05Z"),
		"EndTime":   to.UTC().Format("2006-01-02T15:04:05Z"),
		"Interval":  interval,
	}

	body, err := a.doRequest(ctx, cfg, "GET", params, nil)
	if err != nil {
		return nil, err
	}

	var resp struct {
		Code    string `json:"Code"`
		Message string `json:"Message"`
		Data    struct {
			TimeSeries []struct {
				Time     string `json:"Time"`
				Flow     int64  `json:"Flow"`
				Requests int64  `json:"Requests"`
			} `json:"TimeSeries"`
		} `json:"Data"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("解析阿里云ESA时间序列失败: %w", err)
	}
	if resp.Code != "" && resp.Code != "OK" && resp.Code != "Success" {
		return nil, fmt.Errorf("阿里云ESA API错误: [%s] %s", resp.Code, resp.Message)
	}

	var pts []model.StatPoint
	for _, ts := range resp.Data.TimeSeries {
		t, _ := time.Parse("2006-01-02T15:04:05Z", ts.Time)
		if t.IsZero() {
			t, _ = time.Parse("2006-01-02", ts.Time)
		}
		pts = append(pts, model.StatPoint{
			Time:     t.UTC(),
			Requests: ts.Requests,
			Bytes:    ts.Flow,
		})
	}
	return pts, nil
}

// GetGeoDistribution 阿里云 ESA DescribeSiteTopStatisticsInfo（按国家/地区）
func (a *AliyunESAProvider) GetGeoDistribution(ctx context.Context, cfg map[string]string, zoneID string, from, to time.Time) ([]model.GeoPoint, error) {
	params := map[string]string{
		"Action":    "DescribeSiteTopStatisticsInfo",
		"SiteId":    zoneID,
		"StartTime": from.UTC().Format("2006-01-02T15:04:05Z"),
		"EndTime":   to.UTC().Format("2006-01-02T15:04:05Z"),
		"Field":     "Country",
		"Metric":    "request",
		"Limit":     "200",
	}

	body, err := a.doRequest(ctx, cfg, "GET", params, nil)
	if err != nil {
		return nil, err
	}

	var resp struct {
		Code    string `json:"Code"`
		Message string `json:"Message"`
		Data    struct {
			TopList []struct {
				Key      string  `json:"Key"`
				Value    float64 `json:"Value"`
				Flow     float64 `json:"Flow"`
			} `json:"TopList"`
		} `json:"Data"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("解析阿里云ESA地区分布失败: %w", err)
	}
	if resp.Code != "" && resp.Code != "OK" && resp.Code != "Success" {
		return nil, fmt.Errorf("阿里云ESA API错误: [%s] %s", resp.Code, resp.Message)
	}

	var pts []model.GeoPoint
	for _, item := range resp.Data.TopList {
		if item.Key == "" || item.Key == "Unknown" {
			continue
		}
		pts = append(pts, model.GeoPoint{
			CountryCode: item.Key,
			CountryName: item.Key,
			Requests:    int64(item.Value),
			Bytes:       int64(item.Flow),
		})
	}
	return pts, nil
}
