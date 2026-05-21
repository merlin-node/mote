package agent

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
)

const DefaultConfigPath = "/etc/bk/config.json"

type Config struct {
	Server       string `json:"server"`        // wss://panel.example.com
	Token        string `json:"token"`         // 节点 Token,留空则用 AutoDiscovery
	AutoDiscovery string `json:"auto_discovery,omitempty"`
	Interval     int    `json:"interval"`      // 采集间隔秒,默认 2
	Heartbeat    int    `json:"heartbeat"`     // 心跳秒,默认 30
	NICInclude          string `json:"nic_include,omitempty"`
	NICExclude          string `json:"nic_exclude,omitempty"`
	DisableCompression  bool   `json:"disable_compression,omitempty"` // 禁用 permessage-deflate(低内存/高 CPU 场景)

	path string
	mu   sync.Mutex
}

func LoadConfig(path string) (*Config, error) {
	if path == "" {
		path = DefaultConfigPath
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	c := &Config{}
	if err := json.Unmarshal(data, c); err != nil {
		return nil, err
	}
	c.path = path
	c.applyDefaults()
	return c, nil
}

func NewConfig(path, server, token, adKey string) *Config {
	c := &Config{
		Server:        server,
		Token:         token,
		AutoDiscovery: adKey,
		path:          path,
	}
	c.applyDefaults()
	return c
}

func (c *Config) applyDefaults() {
	if c.Interval <= 0 {
		c.Interval = 2
	}
	if c.Heartbeat <= 0 {
		c.Heartbeat = 30
	}
	if c.NICExclude == "" {
		// 排除常见虚拟接口。用户可在 config.json 自行覆盖。
		c.NICExclude = `^(lo|docker|veth|br-|tun|tap|wg|cni|flannel|cali|kube|podman|nerdctl|zt|vmnet|vnet|virbr|dummy|gre|sit|ip6tnl|teql)`
	}
}

func (c *Config) Save() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.path == "" {
		return errors.New("config path empty")
	}
	dir := filepath.Dir(c.path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	// 原子写:先写 .tmp 再 rename
	tmp := c.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, c.path)
}

func (c *Config) Validate() error {
	if c.Server == "" {
		return errors.New("server URL is empty")
	}
	if c.Token == "" && c.AutoDiscovery == "" {
		return errors.New("either token or auto_discovery key is required")
	}
	return nil
}

// SetRuntime 在收到主控 reconfig 时调用,更新内存配置并尝试持久化。
// 持久化失败不致命(下次重启会丢这次的覆盖值,但运行中行为已生效)。
func (c *Config) SetRuntime(interval, heartbeat int) {
	c.mu.Lock()
	changed := false
	if interval > 0 && interval != c.Interval {
		c.Interval = interval
		changed = true
	}
	if heartbeat > 0 && heartbeat != c.Heartbeat {
		c.Heartbeat = heartbeat
		changed = true
	}
	c.mu.Unlock()
	if changed {
		// Save 内部会再次取锁,这里释放后再调用
		_ = c.Save()
	}
}
