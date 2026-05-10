package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Server  ServerConfig  `yaml:"server"`
	Lark    LarkConfig    `yaml:"lark"`
	Tmux    TmuxConfig    `yaml:"tmux"`
	Timeout TimeoutConfig `yaml:"timeout"`
	Log     LogConfig     `yaml:"log"`
	Filter  FilterConfig  `yaml:"filter"`
}

type FilterConfig struct {
	// ExcludePaths is a list of path substrings. Any hook event whose CWD contains one of these
	// substrings is silently ignored. Use this to filter out noise from tools like CodexBar.
	ExcludePaths []string `yaml:"exclude_paths"`
}

type ServerConfig struct {
	Port int    `yaml:"port"`
	Host string `yaml:"host"`
}

type LarkConfig struct {
	AppID          string `yaml:"app_id"`
	AppSecret      string `yaml:"app_secret"`
	ReceiverType   string `yaml:"receiver_type"` // "open_id" 或 "chat_id"
	ReceiverID     string `yaml:"receiver_id"`
	BaseURL        string `yaml:"base_url"` // 飞书: https://open.feishu.cn, Lark国际: https://open.larksuite.com
	WorkingEmoji   string `yaml:"working_emoji"` // 收到消息后添加的 reaction emoji，默认 COMPUTER
}

type TmuxConfig struct {
	SessionPrefix string `yaml:"session_prefix"`
	KeepSessions  bool   `yaml:"keep_sessions"`
}

type TimeoutConfig struct {
	Confirmation int `yaml:"confirmation"` // seconds
}

type LogConfig struct {
	File      string `yaml:"file"`
	Level     string `yaml:"level"`
	MaxSizeMB int    `yaml:"max_size_mb"`
}

func Load() (*Config, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("get home dir: %w", err)
	}

	path := filepath.Join(home, ".larker", "config.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config file %s: %w", path, err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	// defaults
	if cfg.Server.Port == 0 {
		cfg.Server.Port = 8765
	}
	if cfg.Server.Host == "" {
		cfg.Server.Host = "127.0.0.1"
	}
	if cfg.Tmux.SessionPrefix == "" {
		cfg.Tmux.SessionPrefix = "larker"
	}
	if cfg.Timeout.Confirmation == 0 {
		cfg.Timeout.Confirmation = 3600
	}
	if cfg.Log.File == "" {
		cfg.Log.File = filepath.Join(home, ".larker", "larker.log")
	}
	cfg.Log.File = expandPath(cfg.Log.File, home)
	if cfg.Log.Level == "" {
		cfg.Log.Level = "info"
	}
	if cfg.Log.MaxSizeMB == 0 {
		cfg.Log.MaxSizeMB = 10
	}
	if cfg.Lark.BaseURL == "" {
		cfg.Lark.BaseURL = "https://open.feishu.cn"
	}
	if cfg.Lark.ReceiverType == "" {
		cfg.Lark.ReceiverType = "chat_id"
	}
	if cfg.Lark.WorkingEmoji == "" {
		cfg.Lark.WorkingEmoji = "COMPUTER"
	}

	return &cfg, nil
}

func expandPath(path, home string) string {
	if strings.HasPrefix(path, "~/") {
		return filepath.Join(home, path[2:])
	}
	return path
}
