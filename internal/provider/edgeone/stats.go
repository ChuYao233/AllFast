package edgeone

import (
	"allfast/internal/model"
	"allfast/internal/provider"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

func init() {
	provider.RegisterStats("edgeone", &EdgeOneProvider{})
}

// GetTimeSeries 腾讯云 EdgeOne DescribeTimingL7AnalysisData
// zoneID 格式可以是纯 zone ID 或 "zoneID:domain"（按域名过滤）
func (e *EdgeOneProvider) GetTimeSeries(ctx context.Context, cfg map[string]string, zoneID string, from, to time.Time) ([]model.StatPoint, error) {
	interval := "hour"
	if to.Sub(from) > 72*time.Hour {
		interval = "day"
	}

	// 解析 zoneID:domain 格式
	var domain string
	if idx := strings.Index(zoneID, ":"); idx > 0 {
		domain = zoneID[idx+1:]
		zoneID = zoneID[:idx]
	}

	params := map[string]interface{}{
		"StartTime":   from.UTC().Format("2006-01-02T15:04:05Z"),
		"EndTime":     to.UTC().Format("2006-01-02T15:04:05Z"),
		"ZoneIds":     []string{zoneID},
		"MetricNames": []string{"l7Flow_outFlux", "l7Flow_request"},
		"Interval":    interval,
	}
	// 按域名过滤（EdgeOne 使用 Key/Operator/Value 格式）
	if domain != "" {
		params["Filters"] = []map[string]interface{}{
			{"Key": "domain", "Operator": "equals", "Value": []string{domain}},
		}
	}

	body, err := e.doRequest(ctx, cfg, "DescribeTimingL7AnalysisData", params)
	if err != nil {
		return nil, err
	}

	// 实际响应结构：Data[].TypeValue[].{MetricName, Detail[].{Timestamp, Value}}
	var resp struct {
		Response struct {
			Data []struct {
				TypeValue []struct {
					MetricName string `json:"MetricName"`
					Detail     []struct {
						Timestamp int64   `json:"Timestamp"`
						Value     float64 `json:"Value"`
					} `json:"Detail"`
				} `json:"TypeValue"`
			} `json:"Data"`
			Error *struct {
				Code    string `json:"Code"`
				Message string `json:"Message"`
			} `json:"Error"`
		} `json:"Response"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("解析 EdgeOne 时间序列失败: %w", err)
	}
	if resp.Response.Error != nil {
		return nil, fmt.Errorf("EdgeOne API 错误: [%s] %s", resp.Response.Error.Code, resp.Response.Error.Message)
	}

	// 按时间戳聚合两个指标
	type pt struct{ requests, bytes int64 }
	timeMap := map[int64]*pt{}
	for _, d := range resp.Response.Data {
		for _, tv := range d.TypeValue {
			for _, detail := range tv.Detail {
				if timeMap[detail.Timestamp] == nil {
					timeMap[detail.Timestamp] = &pt{}
				}
				switch tv.MetricName {
				case "l7Flow_request":
					timeMap[detail.Timestamp].requests += int64(detail.Value)
				case "l7Flow_outFlux":
					timeMap[detail.Timestamp].bytes += int64(detail.Value)
				}
			}
		}
	}

	pts := make([]model.StatPoint, 0, len(timeMap))
	for ts, v := range timeMap {
		pts = append(pts, model.StatPoint{
			Time:     time.Unix(ts, 0).UTC(),
			Requests: v.requests,
			Bytes:    v.bytes,
		})
	}
	// 按时间排序
	for i := 1; i < len(pts); i++ {
		for j := i; j > 0 && pts[j].Time.Before(pts[j-1].Time); j-- {
			pts[j], pts[j-1] = pts[j-1], pts[j]
		}
	}
	return pts, nil
}

// GetGeoDistribution 腾讯云 EdgeOne DescribeTopL7AnalysisData（按国家/地区）
// zoneID 格式可以是纯 zone ID 或 "zoneID:domain"（按域名过滤）
func (e *EdgeOneProvider) GetGeoDistribution(ctx context.Context, cfg map[string]string, zoneID string, from, to time.Time) ([]model.GeoPoint, error) {
	// 解析 zoneID:domain 格式
	var domain string
	if idx := strings.Index(zoneID, ":"); idx > 0 {
		domain = zoneID[idx+1:]
		zoneID = zoneID[:idx]
	}

	// MetricName 格式为 {指标}_{维度}，l7Flow_request_country = 按国家的请求数
	params := map[string]interface{}{
		"StartTime":  from.UTC().Format("2006-01-02T15:04:05Z"),
		"EndTime":    to.UTC().Format("2006-01-02T15:04:05Z"),
		"ZoneIds":    []string{zoneID},
		"MetricName": "l7Flow_request_country",
		"Interval":   "day",
		"Limit":      100,
	}
	// 按域名过滤（EdgeOne 使用 Key/Operator/Value 格式）
	if domain != "" {
		params["Filters"] = []map[string]interface{}{
			{"Key": "domain", "Operator": "equals", "Value": []string{domain}},
		}
	}

	body, err := e.doRequest(ctx, cfg, "DescribeTopL7AnalysisData", params)
	if err != nil {
		return nil, err
	}

	// 实际响应结构：Data[].DetailData[].{Key(国家名), Value(请求数)}
	var resp struct {
		Response struct {
			Data []struct {
				TypeKey    string `json:"TypeKey"`
				DetailData []struct {
					Key   string  `json:"Key"`
					Value float64 `json:"Value"`
				} `json:"DetailData"`
			} `json:"Data"`
			Error *struct {
				Code    string `json:"Code"`
				Message string `json:"Message"`
			} `json:"Error"`
		} `json:"Response"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("解析 EdgeOne 地区分布失败: %w", err)
	}
	if resp.Response.Error != nil {
		return nil, fmt.Errorf("EdgeOne API 错误: [%s] %s", resp.Response.Error.Code, resp.Response.Error.Message)
	}

	var pts []model.GeoPoint
	for _, d := range resp.Response.Data {
		for _, item := range d.DetailData {
			if item.Key == "" || item.Key == "Unknown" || item.Key == "-" {
				continue
			}
			pts = append(pts, model.GeoPoint{
				CountryCode: item.Key,
				CountryName: item.Key,
				Requests:    int64(item.Value),
			})
		}
	}
	return pts, nil
}
