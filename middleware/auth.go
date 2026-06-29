package middleware

import (
	"context"
	"net/http"
	"strings"
	"time"

	"llmio/common"
	"llmio/consts"
	"llmio/service"
	"github.com/gin-gonic/gin"
	"github.com/samber/lo"
)

// 用于系统数据操作相关鉴权
func Auth(token string) gin.HandlerFunc {
	return func(c *gin.Context) {
		// 不设置token，则不进行验证
		if token == "" {
			return
		}
		authHeader := c.GetHeader("Authorization")
		if authHeader == "" {
			common.ErrorWithHttpStatus(c, http.StatusUnauthorized, http.StatusUnauthorized, "Authorization header is missing")
			c.Abort()
			return
		}

		parts := strings.SplitN(authHeader, " ", 2)
		if !(len(parts) == 2 && parts[0] == "Bearer") {
			common.ErrorWithHttpStatus(c, http.StatusUnauthorized, http.StatusUnauthorized, "Invalid authorization header")
			c.Abort()
			return
		}

		tokenString := parts[1]
		if tokenString != token {
			common.ErrorWithHttpStatus(c, http.StatusUnauthorized, http.StatusUnauthorized, "Invalid token")
			c.Abort()
			return
		}
	}
}

// extractAuthKey 按优先级从多个 header 中提取鉴权 key。
// 依次尝试：协议标准 header → Authorization: Bearer → x-api-key → x-goog-api-key。
// 这样任一协议都同时支持 Bearer（ANTHROPIC_AUTH_TOKEN）与 x-api-key（ANTHROPIC_API_KEY）等携带方式。
func extractAuthKey(c *gin.Context, primaryHeader string) string {
	if key := c.GetHeader(primaryHeader); key != "" {
		return key
	}
	parts := strings.SplitN(c.GetHeader("Authorization"), " ", 2)
	if len(parts) == 2 && parts[0] == "Bearer" && parts[1] != "" {
		return parts[1]
	}
	if key := c.GetHeader("x-api-key"); key != "" {
		return key
	}
	if key := c.GetHeader("x-goog-api-key"); key != "" {
		return key
	}
	return ""
}

// 用于OpenAI接口鉴权
func AuthOpenAI(adminToken string) gin.HandlerFunc {
	return func(c *gin.Context) {
		checkAuthKey(c, extractAuthKey(c, ""), adminToken)
	}
}

// 用于Anthropic接口鉴权
func AuthAnthropic(adminToken string) gin.HandlerFunc {
	return func(c *gin.Context) {
		checkAuthKey(c, extractAuthKey(c, "x-api-key"), adminToken)
	}
}

// 用于Gemini原生接口鉴权
func AuthGemini(adminToken string) gin.HandlerFunc {
	return func(c *gin.Context) {
		checkAuthKey(c, extractAuthKey(c, "x-goog-api-key"), adminToken)
	}
}

func checkAuthKey(c *gin.Context, key string, adminToken string) {
	ctx := c.Request.Context()
	// 如果系统中未配置Token 或者使用的是最高权限的token 则允许访问所有模型
	if adminToken == "" || key == adminToken {
		ctx = context.WithValue(ctx, consts.ContextKeyAllowAllModel, true)
		c.Request = c.Request.WithContext(ctx)
		return
	}
	// 如果key为空 则拒绝访问
	if key == "" {
		common.ErrorWithHttpStatus(c, http.StatusUnauthorized, http.StatusUnauthorized, "Authorization key is missing")
		c.Abort()
		return
	}
	authKey, err := service.GetAuthKey(ctx, key)
	if err != nil {
		common.ErrorWithHttpStatus(c, http.StatusUnauthorized, http.StatusUnauthorized, "Invalid token")
		c.Abort()
		return
	}
	// 检查是否过期
	if authKey.ExpiresAt != nil && authKey.ExpiresAt.Before(time.Now()) {
		common.ErrorWithHttpStatus(c, http.StatusUnauthorized, http.StatusUnauthorized, "Token has expired")
		c.Abort()
		return
	}
	// 异步更新使用次数
	go service.KeyUpdate(authKey.ID, time.Now())

	ctx = context.WithValue(ctx, consts.ContextKeyAuthKeyID, authKey.ID)
	ctx = context.WithValue(ctx, consts.ContextKeyAuthKeyIOLog, lo.FromPtrOr(authKey.IOLog, false))
	ctx = context.WithValue(ctx, consts.ContextKeyAuthKeyDebug, lo.FromPtrOr(authKey.Debug, false))

	allowAll := lo.FromPtrOr(authKey.AllowAll, false)
	ctx = context.WithValue(ctx, consts.ContextKeyAllowAllModel, allowAll)
	// 如果不允许所有模型 则设置允许的模型列表
	if !allowAll {
		ctx = context.WithValue(ctx, consts.ContextKeyAllowModels, authKey.Models)
	}

	c.Request = c.Request.WithContext(ctx)
}
