package consts

type ContextKey string

const (
	ContextKeyAllowAllModel ContextKey = "allow_all_model"
	ContextKeyAllowModels   ContextKey = "allow_models"
	ContextKeyAuthKeyID     ContextKey = "auth_key_id"
	ContextKeyAuthKeyIOLog  ContextKey = "auth_key_io_log"
	ContextKeyAuthKeyDebug ContextKey = "auth_key_debug"
	ContextKeyAuthKeyPreferredProvider ContextKey = "auth_key_preferred_provider"
)

const (
	ContextKeyGeminiStream ContextKey = "gemini_stream"
)
