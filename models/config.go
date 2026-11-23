package models

import "gorm.io/gorm"

type Config struct {
	gorm.Model
	Key   string // 配置类型
	Value string // 配置内容
}

const (
	KeyAnthropicCountTokens = "anthropic_count_tokens"
	KeyLogCleanupPolicy     = "log_cleanup_policy"
	// KeyChatLogsCountWatermark chat_logs 总数水位线。v2 起按月分表：
	// 值为当前月分表内 (max_id, count)（id ≤ max_id 的行数），封存月行数
	// 直接来自注册表 chat_log_shards，见 service/chat_log_count.go。
	// 换 key 使旧「全表口径」的存量值天然失效，不会被误加载。
	KeyChatLogsCountWatermark = "chat_logs_count_watermark_v2"
)

type AnthropicCountTokens struct {
	BaseURL string `json:"base_url"`
	APIKey  string `json:"api_key"`
	Version string `json:"version"`
}

type LogCleanupPolicy struct {
	Enabled       bool `json:"enabled"`
	RetentionDays int  `json:"retention_days"`
}
