package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/viper"
)

// Config 完整配置结构
type Config struct {
	Interaction  InteractionConfig  `mapstructure:"interaction"`
	Email        EmailConfig        `mapstructure:"email"`
	LLM          LLMConfig          `mapstructure:"llm"`
	Executor     ExecutorConfig     `mapstructure:"executor"`
	Cache        CacheConfig        `mapstructure:"cache"`
	StatusReport StatusReportConfig `mapstructure:"status_report"`
	Logging      LoggingConfig      `mapstructure:"logging"`
}

// InteractionConfig 交互通道配置
type InteractionConfig struct {
	Provider string   `mapstructure:"provider"`
	IM       IMConfig `mapstructure:"im"`
}

// IMConfig IM 通道配置（骨架）
type IMConfig struct {
	Platform     string        `mapstructure:"platform"`
	Endpoint     string        `mapstructure:"endpoint"`
	BotID        string        `mapstructure:"bot_id"`
	BotToken     string        `mapstructure:"bot_token"`
	PollInterval time.Duration `mapstructure:"poll_interval"`
}

// EmailConfig 邮箱配置
type EmailConfig struct {
	IMAP                   IMAPConfig    `mapstructure:"imap"`
	SMTP                   SMTPConfig    `mapstructure:"smtp"`
	Inbox                  string        `mapstructure:"inbox"`
	PollInterval           time.Duration `mapstructure:"poll_interval"`
	MaxConcurrentTasks     int           `mapstructure:"max_concurrent_tasks"`
	UseSubAgent            bool          `mapstructure:"use_subagent"`
	SubAgentQueueSize      int           `mapstructure:"subagent_queue_size"`
	ProcessLatestOnStartup bool          `mapstructure:"process_latest_on_startup"`
	MarkAsRead             bool          `mapstructure:"mark_as_read"`
	AllowedSenders         []string      `mapstructure:"allowed_senders"`
}

// IMAPConfig IMAP 配置
type IMAPConfig struct {
	Host     string `mapstructure:"host"`
	Port     int    `mapstructure:"port"`
	Username string `mapstructure:"username"`
	Password string `mapstructure:"password"`
	UseSSL   bool   `mapstructure:"use_ssl"`
}

// SMTPConfig SMTP 配置
type SMTPConfig struct {
	Host     string `mapstructure:"host"`
	Port     int    `mapstructure:"port"`
	Username string `mapstructure:"username"`
	Password string `mapstructure:"password"`
	UseSSL   bool   `mapstructure:"use_ssl"`
}

// LLMConfig LLM 配置
type LLMConfig struct {
	Provider  string        `mapstructure:"provider"`
	Model     string        `mapstructure:"model"`
	Command   string        `mapstructure:"command"`
	MaxTokens int           `mapstructure:"max_tokens"`
	Timeout   time.Duration `mapstructure:"timeout"`
}

// ExecutorConfig 执行器配置
type ExecutorConfig struct {
	Sandbox             bool          `mapstructure:"sandbox"`
	SandboxAllowNetwork bool          `mapstructure:"sandbox_allow_network"`
	SandboxCPUs         string        `mapstructure:"sandbox_cpus"`
	SandboxMemory       string        `mapstructure:"sandbox_memory"`
	SandboxPidsLimit    int           `mapstructure:"sandbox_pids_limit"`
	SandboxTmpfsSize    string        `mapstructure:"sandbox_tmpfs_size"`
	SandboxReadOnly     bool          `mapstructure:"sandbox_read_only"`
	Timeout             time.Duration `mapstructure:"timeout"`
	AllowedLangs        []string      `mapstructure:"allowed_languages"`
	WorkDir             string        `mapstructure:"work_dir"`
	MaxOutputSize       int64         `mapstructure:"max_output_size"`
}

// LoggingConfig 日志配置
type LoggingConfig struct {
	Level      string `mapstructure:"level"`
	File       string `mapstructure:"file"`
	MaxSize    int64  `mapstructure:"max_size"`
	MaxBackups int    `mapstructure:"max_backups"`
}

// CacheConfig 工具缓存配置
type CacheConfig struct {
	Enabled           bool          `mapstructure:"enabled"`
	TTL               time.Duration `mapstructure:"ttl"`
	MaxEntries        int           `mapstructure:"max_entries"`
	LLMValidateOnMiss bool          `mapstructure:"llm_validate_on_miss"`
}

// StatusReportConfig 定时状态汇报配置
type StatusReportConfig struct {
	Enabled    bool          `mapstructure:"enabled"`
	Interval   time.Duration `mapstructure:"interval"`
	Recipients []string      `mapstructure:"recipients"`
}

// Load 加载配置
func Load(configPath string) (*Config, error) {
	viper.SetConfigFile(configPath)
	viper.SetConfigType("yaml")

	// 读取配置文件
	if err := viper.ReadInConfig(); err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	// 展开环境变量
	expandEnvVars()

	var config Config
	if err := viper.Unmarshal(&config); err != nil {
		return nil, fmt.Errorf("failed to unmarshal config: %w", err)
	}

	// 验证配置
	if err := config.Validate(); err != nil {
		return nil, fmt.Errorf("config validation failed: %w", err)
	}

	return &config, nil
}

// expandEnvVars 展开环境变量
func expandEnvVars() {
	for _, key := range viper.AllKeys() {
		value := viper.GetString(key)
		if expanded := os.ExpandEnv(value); expanded != value {
			viper.Set(key, expanded)
		}
	}
}

// Validate 验证配置
func (c *Config) Validate() error {
	if strings.TrimSpace(c.Interaction.Provider) == "" {
		c.Interaction.Provider = "email"
	}

	// 验证邮箱配置
	switch strings.ToLower(strings.TrimSpace(c.Interaction.Provider)) {
	case "email":
		if c.Email.IMAP.Host == "" {
			return fmt.Errorf("imap host is required")
		}
		if c.Email.IMAP.Username == "" {
			return fmt.Errorf("imap username is required")
		}
		if c.Email.IMAP.Password == "" {
			return fmt.Errorf("imap password is required")
		}

		if c.Email.SMTP.Host == "" {
			return fmt.Errorf("smtp host is required")
		}
		if c.Email.SMTP.Username == "" {
			return fmt.Errorf("smtp username is required")
		}
		if c.Email.SMTP.Password == "" {
			return fmt.Errorf("smtp password is required")
		}
	case "im":
		// IM 通道骨架模式：不强制字段，保留给具体平台实现校验。
	default:
		return fmt.Errorf("unsupported interaction provider: %s", c.Interaction.Provider)
	}

	// 验证 LLM 配置
	provider := strings.ToLower(strings.TrimSpace(c.LLM.Provider))
	switch provider {
	case "", "claude_code", "claude-code", "local_claude_code", "codex", "codex_cli", "codex-cli":
		// 仅支持本地 CLI 模式
	default:
		return fmt.Errorf("unsupported llm provider: %s", c.LLM.Provider)
	}

	// 验证执行器配置
	if c.Executor.WorkDir == "" {
		return fmt.Errorf("executor work_dir is required")
	}
	if strings.TrimSpace(c.Executor.SandboxCPUs) == "" {
		c.Executor.SandboxCPUs = "1.0"
	}
	if strings.TrimSpace(c.Executor.SandboxMemory) == "" {
		c.Executor.SandboxMemory = "512m"
	}
	if c.Executor.SandboxPidsLimit <= 0 {
		c.Executor.SandboxPidsLimit = 128
	}
	if strings.TrimSpace(c.Executor.SandboxTmpfsSize) == "" {
		c.Executor.SandboxTmpfsSize = "64m"
	}

	if c.Email.PollInterval <= 0 {
		c.Email.PollInterval = 30 * time.Second
	}
	if c.Email.MaxConcurrentTasks <= 0 {
		c.Email.MaxConcurrentTasks = 1
	}
	if c.Email.SubAgentQueueSize <= 0 {
		c.Email.SubAgentQueueSize = 100
	}
	if c.Interaction.IM.PollInterval <= 0 {
		c.Interaction.IM.PollInterval = 3 * time.Second
	}

	// 缓存配置默认值
	if c.Cache.TTL <= 0 {
		c.Cache.TTL = 24 * time.Hour
	}
	if c.Cache.MaxEntries <= 0 {
		c.Cache.MaxEntries = 200
	}
	if c.StatusReport.Interval <= 0 {
		c.StatusReport.Interval = time.Hour
	}

	return nil
}
