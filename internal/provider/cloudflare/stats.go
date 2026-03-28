package cloudflare

import (
	"allfast/internal/model"
	"allfast/internal/provider"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

func init() {
	provider.RegisterStats("cloudflare", &CloudflareProvider{})
}

const cfGraphQLURL = "https://api.cloudflare.com/client/v4/graphql"

// GetTimeSeries Cloudflare GraphQL Analytics：按小时粒度返回流量时间序列
func (c *CloudflareProvider) GetTimeSeries(ctx context.Context, cfg map[string]string, zoneID string, from, to time.Time) ([]model.StatPoint, error) {
	if err := c.validateAuth(cfg); err != nil {
		return nil, err
	}

	// 时间跨度 > 3 天用日粒度，否则用小时粒度
	useDailyGranularity := to.Sub(from) > 72*time.Hour

	var gqlQuery string
	if useDailyGranularity {
		gqlQuery = fmt.Sprintf(`{
  viewer {
    zones(filter: {zoneTag: %q}) {
      httpRequests1dGroups(
        limit: 366,
        filter: {date_geq: %q, date_lt: %q}
        orderBy: [date_ASC]
      ) {
        sum { requests bytes cachedRequests cachedBytes }
        dimensions { date }
      }
    }
  }
}`, zoneID,
			from.UTC().Format("2006-01-02"),
			to.UTC().Format("2006-01-02"))
	} else {
		gqlQuery = fmt.Sprintf(`{
  viewer {
    zones(filter: {zoneTag: %q}) {
      httpRequests1hGroups(
        limit: 168,
        filter: {datetime_geq: %q, datetime_lt: %q}
        orderBy: [datetime_ASC]
      ) {
        sum { requests bytes cachedRequests cachedBytes }
        dimensions { datetime }
      }
    }
  }
}`, zoneID,
			from.UTC().Format("2006-01-02T15:04:05Z"),
			to.UTC().Format("2006-01-02T15:04:05Z"))
	}

	body, err := c.doGraphQL(ctx, cfg, gqlQuery)
	if err != nil {
		return nil, err
	}

	if useDailyGranularity {
		var resp struct {
			Data struct {
				Viewer struct {
					Zones []struct {
						Daily []struct {
							Sum struct {
								Requests       int64 `json:"requests"`
								Bytes          int64 `json:"bytes"`
								CachedRequests int64 `json:"cachedRequests"`
								CachedBytes    int64 `json:"cachedBytes"`
							} `json:"sum"`
							Dimensions struct {
								Date string `json:"date"`
							} `json:"dimensions"`
						} `json:"httpRequests1dGroups"`
					} `json:"zones"`
				} `json:"viewer"`
			} `json:"data"`
		}
		if err := json.Unmarshal(body, &resp); err != nil {
			return nil, fmt.Errorf("解析日粒度响应失败: %w", err)
		}
		var pts []model.StatPoint
		for _, z := range resp.Data.Viewer.Zones {
			for _, g := range z.Daily {
				t, _ := time.Parse("2006-01-02", g.Dimensions.Date)
				pts = append(pts, model.StatPoint{
					Time: t, Requests: g.Sum.Requests, Bytes: g.Sum.Bytes,
					CachedRequests: g.Sum.CachedRequests, CachedBytes: g.Sum.CachedBytes,
				})
			}
		}
		return pts, nil
	}

	var resp struct {
		Data struct {
			Viewer struct {
				Zones []struct {
					Hourly []struct {
						Sum struct {
							Requests       int64 `json:"requests"`
							Bytes          int64 `json:"bytes"`
							CachedRequests int64 `json:"cachedRequests"`
							CachedBytes    int64 `json:"cachedBytes"`
						} `json:"sum"`
						Dimensions struct {
							Datetime string `json:"datetime"`
						} `json:"dimensions"`
					} `json:"httpRequests1hGroups"`
				} `json:"zones"`
			} `json:"viewer"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("解析小时粒度响应失败: %w", err)
	}
	var pts []model.StatPoint
	for _, z := range resp.Data.Viewer.Zones {
		for _, g := range z.Hourly {
			t, _ := time.Parse("2006-01-02T15:04:05Z", g.Dimensions.Datetime)
			pts = append(pts, model.StatPoint{
				Time: t, Requests: g.Sum.Requests, Bytes: g.Sum.Bytes,
				CachedRequests: g.Sum.CachedRequests, CachedBytes: g.Sum.CachedBytes,
			})
		}
	}
	return pts, nil
}

// GetGeoDistribution Cloudflare GraphQL：地区分布
func (c *CloudflareProvider) GetGeoDistribution(ctx context.Context, cfg map[string]string, zoneID string, from, to time.Time) ([]model.GeoPoint, error) {
	if err := c.validateAuth(cfg); err != nil {
		return nil, err
	}

	gqlQuery := fmt.Sprintf(`{
  viewer {
    zones(filter: {zoneTag: %q}) {
      httpRequestsAdaptiveGroups(
        limit: 200,
        filter: {date_geq: %q, date_leq: %q}
      ) {
        sum { requests bytes }
        dimensions { clientCountryName }
      }
    }
  }
}`, zoneID,
		from.UTC().Format("2006-01-02"),
		to.UTC().Format("2006-01-02"))

	body, err := c.doGraphQL(ctx, cfg, gqlQuery)
	if err != nil {
		return nil, err
	}

	var resp struct {
		Data struct {
			Viewer struct {
				Zones []struct {
					Groups []struct {
						Sum struct {
							Requests int64 `json:"requests"`
							Bytes    int64 `json:"bytes"`
						} `json:"sum"`
						Dimensions struct {
							ClientCountryName string `json:"clientCountryName"`
						} `json:"dimensions"`
					} `json:"httpRequestsAdaptiveGroups"`
				} `json:"zones"`
			} `json:"viewer"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("解析地区分布响应失败: %w", err)
	}

	var pts []model.GeoPoint
	for _, z := range resp.Data.Viewer.Zones {
		for _, g := range z.Groups {
			if g.Dimensions.ClientCountryName == "" {
				continue
			}
			pts = append(pts, model.GeoPoint{
				CountryCode: g.Dimensions.ClientCountryName,
				CountryName: g.Dimensions.ClientCountryName,
				Requests:    g.Sum.Requests,
				Bytes:       g.Sum.Bytes,
			})
		}
	}
	return pts, nil
}

// doGraphQL 发送 GraphQL 请求到 Cloudflare Analytics API
func (c *CloudflareProvider) doGraphQL(ctx context.Context, cfg map[string]string, query string) ([]byte, error) {
	payload, _ := json.Marshal(map[string]string{"query": query})
	req, err := http.NewRequestWithContext(ctx, "POST", cfGraphQLURL, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	if cfg["auth_type"] == "global_key" {
		req.Header.Set("X-Auth-Key", cfg["global_api_key"])
		req.Header.Set("X-Auth-Email", cfg["email"])
	} else {
		req.Header.Set("Authorization", "Bearer "+cfg["api_token"])
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("Cloudflare GraphQL 请求失败: %w", err)
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}
