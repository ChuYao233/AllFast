package config

import (
	"fmt"
	"log"
	"os"

	"gopkg.in/yaml.v3"
)

// Config 全局配置结构
type Config struct {
	Server   ServerConfig   `yaml:"server"`
	Database DatabaseConfig `yaml:"database"`
}

// ServerConfig 服务配置
type ServerConfig struct {
	Port int `yaml:"port"`
}

// DatabaseConfig PostgreSQL 配置
type DatabaseConfig struct {
	Host         string `yaml:"host"`
	Port         int    `yaml:"port"`
	User         string `yaml:"user"`
	Password     string `yaml:"password"`
	DBName       string `yaml:"dbname"`
	SSLMode      string `yaml:"sslmode"`
	MaxOpenConns int    `yaml:"max_open_conns"`
	MaxIdleConns int    `yaml:"max_idle_conns"`
}

// DSN 返回 PostgreSQL 连接字符串
func (d *DatabaseConfig) DSN() string {
	return fmt.Sprintf("host=%s port=%d user=%s password=%s dbname=%s sslmode=%s",
		d.Host, d.Port, d.User, d.Password, d.DBName, d.SSLMode)
}

// 全局配置实例
var C Config

// Load 加载配置文件
func Load(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("读取配置文件失败: %w", err)
	}
	if err := yaml.Unmarshal(data, &C); err != nil {
		return fmt.Errorf("解析配置文件失败: %w", err)
	}

	// 默认值
	if C.Server.Port <= 0 {
		C.Server.Port = 8080
	}
	if C.Database.Host == "" {
		C.Database.Host = "localhost"
	}
	if C.Database.Port <= 0 {
		C.Database.Port = 5432
	}
	if C.Database.SSLMode == "" {
		C.Database.SSLMode = "disable"
	}
	if C.Database.MaxOpenConns <= 0 {
		C.Database.MaxOpenConns = 20
	}
	if C.Database.MaxIdleConns <= 0 {
		C.Database.MaxIdleConns = 5
	}

	log.Printf("配置加载完成: PG=%s:%d/%s, 服务端口=%d", C.Database.Host, C.Database.Port, C.Database.DBName, C.Server.Port)
	return nil
}
