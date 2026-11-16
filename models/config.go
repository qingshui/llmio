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
	// KeyChatLogsCountWatermark chat_logs 总数水位线（全量口径的 max_id + count），
	// 供日志页增量计数，见 service/chat_log_count.go
	KeyChatLogsCountWatermark = "chat_logs_count_watermark"
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
