package service

import (
	"errors"
	"regexp"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ctxMarkerRe 匹配模型名末尾的 context window 标记，如 Claude Code 的 [1m]、[200k]。
var ctxMarkerRe = regexp.MustCompile(`(\[\d+[km]\])+$`)

// stripModelMarker 剥掉模型名末尾的 [1m]/[200k] 等标记，使 glm-5[1m] 自动映射到 glm-5。
func stripModelMarker(model string) string {
	return ctxMarkerRe.ReplaceAllString(model, "")
}

type Before struct {
	Model            string
	Stream           bool
	toolCall         bool
	structuredOutput bool
	image            bool
	inputTokens      int
	SessionID        string
	raw              []byte
}

// bytesPerToken 是按请求体字节数粗估输入 token 的系数，中英文混合的 JSON 取 3。
const bytesPerToken = 3

// estimateInputTokens 按请求体字节数粗估输入 token 数，base64 图片数据不计入。
func estimateInputTokens(data []byte) int {
	n := len(data) - base64Bytes(gjson.ParseBytes(data))
	if n < 0 {
		return 0
	}
	return n / bytesPerToken
}

// base64Bytes 统计请求体中内嵌 base64 数据（Anthropic source、OpenAI data URI、Gemini inlineData）的字节数。
func base64Bytes(v gjson.Result) int {
	var total int
	switch {
	case v.IsObject():
		if v.Get("type").String() == "base64" {
			total += len(v.Get("data").String())
		}
		v.ForEach(func(k, val gjson.Result) bool {
			switch k.String() {
			case "inlineData", "inline_data":
				total += len(val.Get("data").String())
			case "url":
				if s := val.String(); strings.HasPrefix(s, "data:") {
					total += len(s)
				}
			default:
				total += base64Bytes(val)
			}
			return true
		})
	case v.IsArray():
		v.ForEach(func(_, val gjson.Result) bool {
			total += base64Bytes(val)
			return true
		})
	}
	return total
}

// anthropicBlockHasImage 判断 content 块本身是 image，或是 tool_result 且其嵌套 content 含 image。
func anthropicBlockHasImage(block gjson.Result) bool {
	switch block.Get("type").String() {
	case "image":
		return true
	case "tool_result":
		var found bool
		block.Get("content").ForEach(func(_, inner gjson.Result) bool {
			if inner.Get("type").String() == "image" {
				found = true
				return false
			}
			return true
		})
		return found
	}
	return false
}

type Beforer func(data []byte) (*Before, error)

// NewBeforerGemini 返回一个与现有 pre-processor 风格一致的 Gemini 解析器。
// 注意：Gemini 模型来自 URL 路径，不在 body 中。
func NewBeforerGemini(model string, stream bool) Beforer {
	return func(data []byte) (*Before, error) {
		if model == "" {
			return nil, errors.New("model is empty")
		}

		var toolCall bool
		if tools := gjson.GetBytes(data, "tools"); tools.Exists() && len(tools.Array()) != 0 {
			toolCall = true
		}
		if gjson.GetBytes(data, "toolConfig").Exists() || gjson.GetBytes(data, "tool_config").Exists() {
			toolCall = true
		}
		if !toolCall {
			gjson.GetBytes(data, "contents").ForEach(func(_, content gjson.Result) bool {
				if toolCall {
					return false
				}
				content.Get("parts").ForEach(func(_, part gjson.Result) bool {
					if part.Get("functionCall").Exists() || part.Get("function_call").Exists() ||
						part.Get("functionResponse").Exists() || part.Get("function_response").Exists() {
						toolCall = true
						return false
					}
					return true
				})
				return true
			})
		}

		var structuredOutput bool
		if gjson.GetBytes(data, "generationConfig.responseJsonSchema").Exists() ||
			gjson.GetBytes(data, "generationConfig.response_json_schema").Exists() ||
			gjson.GetBytes(data, "generation_config.responseJsonSchema").Exists() ||
			gjson.GetBytes(data, "generation_config.response_json_schema").Exists() ||
			gjson.GetBytes(data, "config.responseJsonSchema").Exists() ||
			gjson.GetBytes(data, "config.response_json_schema").Exists() {
			structuredOutput = true
		}
		if strings.EqualFold(gjson.GetBytes(data, "generationConfig.responseMimeType").String(), "application/json") ||
			strings.EqualFold(gjson.GetBytes(data, "generationConfig.response_mime_type").String(), "application/json") ||
			strings.EqualFold(gjson.GetBytes(data, "generation_config.responseMimeType").String(), "application/json") ||
			strings.EqualFold(gjson.GetBytes(data, "generation_config.response_mime_type").String(), "application/json") ||
			strings.EqualFold(gjson.GetBytes(data, "config.responseMimeType").String(), "application/json") ||
			strings.EqualFold(gjson.GetBytes(data, "config.response_mime_type").String(), "application/json") {
			structuredOutput = true
		}

		var image bool
		gjson.GetBytes(data, "contents").ForEach(func(_, content gjson.Result) bool {
			if image {
				return false
			}
			content.Get("parts").ForEach(func(_, part gjson.Result) bool {
				if image {
					return false
				}

				// 支持 camelCase 与 snake_case 两种字段命名
				inlineData := part.Get("inlineData")
				if !inlineData.Exists() {
					inlineData = part.Get("inline_data")
				}
				if inlineData.Exists() {
					mimeType := inlineData.Get("mimeType").String()
					if mimeType == "" {
						mimeType = inlineData.Get("mime_type").String()
					}
					if strings.HasPrefix(mimeType, "image/") {
						image = true
						return false
					}
				}

				fileData := part.Get("fileData")
				if !fileData.Exists() {
					fileData = part.Get("file_data")
				}
				if fileData.Exists() {
					mimeType := fileData.Get("mimeType").String()
					if mimeType == "" {
						mimeType = fileData.Get("mime_type").String()
					}
					if strings.HasPrefix(mimeType, "image/") {
						image = true
						return false
					}
				}
				return true
			})
			return true
		})

		return &Before{
			Model:            model,
			Stream:           stream,
			toolCall:         toolCall,
			structuredOutput: structuredOutput,
			image:            image,
			inputTokens:      estimateInputTokens(data),
			SessionID:        gjson.GetBytes(data, "session_id").String(),
			raw:              data,
		}, nil
	}
}

func BeforerOpenAI(data []byte) (*Before, error) {
	model := stripModelMarker(gjson.GetBytes(data, "model").String())
	if model == "" {
		return nil, errors.New("model is empty")
	}
	stream := gjson.GetBytes(data, "stream").Bool()
	if stream {
		// 为processTee记录usage添加选项 PS:很多客户端只会开启stream 而不会开启include_usage
		newData, err := sjson.SetBytes(data, "stream_options", struct {
			IncludeUsage bool `json:"include_usage"`
		}{IncludeUsage: true})
		if err != nil {
			return nil, err
		}
		data = newData
	}
	var toolCall bool
	tools := gjson.GetBytes(data, "tools")
	if tools.Exists() && len(tools.Array()) != 0 {
		toolCall = true
	}
	var structuredOutput bool
	if gjson.GetBytes(data, "response_format").Exists() {
		structuredOutput = true
	}
	var image bool
	gjson.GetBytes(data, "messages").ForEach(func(_, value gjson.Result) bool {
		if image {
			return false
		}
		if value.Get("role").String() == "user" {
			value.Get("content").ForEach(func(_, value gjson.Result) bool {
				if value.Get("type").String() == "image_url" {
					image = true
					return false
				}
				return true
			})
		}
		return true
	})
	return &Before{
		Model:            model,
		Stream:           stream,
		toolCall:         toolCall,
		structuredOutput: structuredOutput,
		image:            image,
		inputTokens:      estimateInputTokens(data),
		SessionID:        gjson.GetBytes(data, "session_id").String(),
		raw:              data,
	}, nil
}

func BeforerOpenAIRes(data []byte) (*Before, error) {
	model := stripModelMarker(gjson.GetBytes(data, "model").String())
	if model == "" {
		return nil, errors.New("model is empty")
	}
	stream := gjson.GetBytes(data, "stream").Bool()
	var toolCall bool
	tools := gjson.GetBytes(data, "tools")
	if tools.Exists() && len(tools.Array()) != 0 {
		toolCall = true
	}
	var structuredOutput bool
	if gjson.GetBytes(data, "text.format.type").String() == "json_schema" {
		structuredOutput = true
	}
	var image bool
	gjson.GetBytes(data, "input").ForEach(func(_, value gjson.Result) bool {
		if image {
			return false
		}
		if value.Get("role").String() == "user" {
			value.Get("content").ForEach(func(_, value gjson.Result) bool {
				if value.Get("type").String() == "input_image" {
					image = true
					return false
				}
				return true
			})
		}
		return true
	})
	return &Before{
		Model:            model,
		Stream:           stream,
		toolCall:         toolCall,
		structuredOutput: structuredOutput,
		image:            image,
		inputTokens:      estimateInputTokens(data),
		SessionID:        gjson.GetBytes(data, "session_id").String(),
		raw:              data,
	}, nil
}

func BeforerAnthropic(data []byte) (*Before, error) {
	model := stripModelMarker(gjson.GetBytes(data, "model").String())
	if model == "" {
		return nil, errors.New("model is empty")
	}
	stream := gjson.GetBytes(data, "stream").Bool()
	var toolCall bool
	tools := gjson.GetBytes(data, "tools")
	if tools.Exists() && len(tools.Array()) != 0 {
		toolCall = true
	}
	var image bool
	gjson.GetBytes(data, "messages").ForEach(func(_, value gjson.Result) bool {
		if image {
			return false
		}
		if value.Get("role").String() == "user" {
			value.Get("content").ForEach(func(_, value gjson.Result) bool {
				if anthropicBlockHasImage(value) {
					image = true
					return false
				}
				return true
			})
		}
		return true
	})
	return &Before{
		Model:            model,
		Stream:           stream,
		toolCall:         toolCall,
		structuredOutput: toolCall,
		image:            image,
		inputTokens:      estimateInputTokens(data),
		SessionID:        gjson.GetBytes(data, "session_id").String(),
		raw:              data,
	}, nil
}
