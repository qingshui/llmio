package service

import (
	"bytes"
	"encoding/base64"
	"errors"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
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
	imagePixels      int
	SessionID        string
	raw              []byte
}

// bytesPerToken 是按请求体字节数粗估输入 token 的系数，中英文混合的 JSON 取 3。
const bytesPerToken = 3

// defaultImagePixels 是无法解码尺寸的图片按多少像素兜底估算（约等于一张 1280×1600 截图）。
const defaultImagePixels = 2_048_000

// estimateImageTokens 按请求体字节数粗估输入 token 数，base64 图片数据不计入。
func estimateInputTokens(data []byte) int {
	n := len(data) - base64Bytes(gjson.ParseBytes(data))
	if n < 0 {
		return 0
	}
	return n / bytesPerToken
}

// estimateImagePixels 统计请求体内嵌 base64 图片的总像素数（含 tool_result 嵌套图片）。
// 解不出尺寸的图片按 defaultImagePixels 兜底。
func estimateImagePixels(data []byte) int {
	return sumImagePixels(gjson.ParseBytes(data))
}

// sumImagePixels 递归遍历 JSON，累加所有内嵌 base64 图片的像素数。
// 覆盖 Anthropic source、OpenAI data URI、Gemini inlineData 三种形态。
func sumImagePixels(v gjson.Result) int {
	var total int
	switch {
	case v.IsObject():
		if v.Get("type").String() == "base64" {
			total += imagePixelsFromB64(v.Get("data").String())
		}
		v.ForEach(func(k, val gjson.Result) bool {
			switch k.String() {
			case "inlineData", "inline_data":
				total += imagePixelsFromB64(val.Get("data").String())
			case "url":
				if s := val.String(); strings.HasPrefix(s, "data:") {
					total += imagePixelsFromDataURL(s)
				}
			default:
				total += sumImagePixels(val)
			}
			return true
		})
	case v.IsArray():
		v.ForEach(func(_, val gjson.Result) bool {
			total += sumImagePixels(val)
			return true
		})
	}
	return total
}

// imagePixelsFromB64 解码 base64 图片并返回其像素数；解码或识别失败按 defaultImagePixels 兜底。
func imagePixelsFromB64(b64 string) int {
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		// 容忍 base64 里的换行/空白后重试
		raw, err = base64.StdEncoding.DecodeString(strings.Map(func(r rune) rune {
			switch r {
			case ' ', '\n', '\r', '\t':
				return -1
			}
			return r
		}, b64))
		if err != nil {
			return defaultImagePixels
		}
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil || cfg.Width <= 0 || cfg.Height <= 0 {
		return defaultImagePixels
	}
	return cfg.Width * cfg.Height
}

// imagePixelsFromDataURL 解析 data:image/...;base64,xxx 形式的内嵌图片像素数。
func imagePixelsFromDataURL(s string) int {
	if i := strings.Index(s, ","); i >= 0 {
		return imagePixelsFromB64(s[i+1:])
	}
	return defaultImagePixels
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
			imagePixels:      estimateImagePixels(data),
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
		imagePixels:      estimateImagePixels(data),
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
		imagePixels:      estimateImagePixels(data),
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
		imagePixels:      estimateImagePixels(data),
		SessionID:        gjson.GetBytes(data, "session_id").String(),
		raw:              data,
	}, nil
}
