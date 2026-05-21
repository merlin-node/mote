package server

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
)

const (
	DefaultConfigPath = "/etc/zk/config.json"
	DefaultDataDir    = "/var/lib/zk"
	DefaultPanelTitle = "mote 监控面板"
)

type Config struct {
	Listen           string `json:"listen"`             // :1888
	DataDir          string `json:"data_dir"`           // /var/lib/zk
	AdminUsername    string `json:"admin_username"`     // admin
	AdminPassword    string `json:"admin_password"`     // 初始密码,登录后可改
	AutoDiscoveryKey string `json:"auto_discovery_key,omitempty"`
	PublicEnabled    bool   `json:"public_enabled"`     // 公开页是否开放
	MainCurrency     string `json:"main_currency"`      // CNY/USD,展示汇总用
	PanelTitle       string `json:"panel_title"`        // 网页标题(可在面板内修改)
	AgentInterval    int    `json:"agent_interval"`     // 下发给 agent 的默认采集间隔秒
	AgentHeartbeat   int    `json:"agent_heartbeat"`    // 下发给 agent 的默认心跳间隔秒

	path    string     // 配置文件路径,Save 用
	saveMu  sync.Mutex // 保护并发写
}

func LoadOrCreateConfig(path string) (*Config, error) {
	if path == "" {
		path = DefaultConfigPath
	}
	if data, err := os.ReadFile(path); err == nil {
		c := &Config{}
		if err := json.Unmarshal(data, c); err != nil {
			return nil, err
		}
		c.path = path
		c.applyDefaults()
		return c, nil
	}
	// 首次启动:创建默认
	c := &Config{path: path}
	c.applyDefaults()
	c.AdminPassword = randomString(12)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return nil, err
	}
	if err := c.Save(); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *Config) applyDefaults() {
	if c.Listen == "" {
		c.Listen = ":1888"
	}
	if c.DataDir == "" {
		c.DataDir = DefaultDataDir
	}
	if c.AdminUsername == "" {
		c.AdminUsername = "admin"
	}
	if c.MainCurrency == "" {
		c.MainCurrency = "USD"
	}
	if c.PanelTitle == "" {
		c.PanelTitle = DefaultPanelTitle
	}
	if c.AgentInterval <= 0 {
		c.AgentInterval = 2
	}
	if c.AgentHeartbeat <= 0 {
		c.AgentHeartbeat = 30
	}
}

// Save 把当前配置写回磁盘(原子替换)
func (c *Config) Save() error {
	c.saveMu.Lock()
	defer c.saveMu.Unlock()
	if c.path == "" {
		return errors.New("config path empty")
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := c.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, c.path)
}

func (c *Config) DBPath() string {
	return filepath.Join(c.DataDir, "data.db")
}

func (c *Config) Validate() error {
	if c.AdminPassword == "" {
		return errors.New("admin_password is empty")
	}
	return nil
}

// randomString 生成可读的随机密码
func randomString(n int) string {
	const charset = "abcdefghijkmnpqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	b := make([]byte, n)
	for i := range b {
		b[i] = charset[secureRand(len(charset))]
	}
	return string(b)
}
