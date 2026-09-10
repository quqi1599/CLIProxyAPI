package helps

import (
	"encoding/base64"
	"fmt"
	"net/url"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	"github.com/tidwall/gjson"
)

// IsOfficialDeepSeekURL restricts new official capabilities to the provider host.
func IsOfficialDeepSeekURL(baseURL string) bool {
	u, err := url.Parse(baseURL)
	return err == nil && strings.EqualFold(u.Hostname(), "api.deepseek.com")
}

// OfficialDeepSeekModel translates our versioned public alias only on official routes.
func OfficialDeepSeekModel(model, baseURL string) string {
	if IsOfficialDeepSeekURL(baseURL) && strings.EqualFold(model, "deepseek-v4.1-flash") {
		return "deepseek-flash"
	}
	return model
}

// DeepSeekFlashVisionEnabled excludes third-party routes and older Pro models.
func DeepSeekFlashVisionEnabled(model, baseURL string) bool {
	return IsOfficialDeepSeekURL(baseURL) && thinking.IsDeepSeekFlashModel(model)
}

type deepSeekImageValidator struct {
	count       int
	inlineBytes int
}

// ValidateDeepSeekFlashImages validates locally knowable constraints without
// downloading URLs or changing image contents. Upstream validates decoded images
// and file ownership. See the official vision guide dated 2026-09-10.
func ValidateDeepSeekFlashImages(body []byte, format string) error {
	v := deepSeekImageValidator{}
	if format == "responses" {
		for _, item := range gjson.GetBytes(body, "input").Array() {
			kind := item.Get("type").String()
			if kind == "function_call_output" || kind == "custom_tool_call_output" {
				if err := v.parts(item.Get("output"), true, format); err != nil {
					return err
				}
			} else if kind == "message" || kind == "" {
				role := item.Get("role").String()
				if err := v.parts(item.Get("content"), role == "user" || role == "developer", format); err != nil {
					return err
				}
			}
		}
	} else {
		for _, message := range gjson.GetBytes(body, "messages").Array() {
			if err := v.parts(message.Get("content"), message.Get("role").String() == "user", format); err != nil {
				return err
			}
		}
	}
	if v.count > 600 {
		return fmt.Errorf("一次最多发送 600 张图片，请减少图片数量后重试")
	}
	if v.count > 0 && len(body) > 48*1024*1024 {
		return fmt.Errorf("图片和对话内容合计超过 48 MiB，请压缩图片或减少本次图片数量后重试")
	}
	if v.inlineBytes > 64*1024*1024 {
		return fmt.Errorf("本次内嵌图片合计超过 64 MiB，请压缩图片后重试")
	}
	return nil
}

func (v *deepSeekImageValidator) parts(content gjson.Result, allowed bool, format string) error {
	for _, part := range content.Array() {
		kind := part.Get("type").String()
		if kind == "tool_result" && format == "claude" {
			// Tool results may contain text and images, but not nested tool results.
			if err := v.parts(part.Get("content"), allowed, "claude-image"); err != nil {
				return err
			}
			continue
		}
		if kind == "document" || kind == "input_file" {
			return fmt.Errorf("当前 DeepSeek 支持图片，但不支持直接读取文档附件；请将文档转换成文字或图片后重新发送")
		}
		if kind != "image" && kind != "image_url" && kind != "input_image" && kind != "file" {
			continue
		}
		if (strings.HasPrefix(format, "claude") && kind != "image") ||
			(format == "responses" && kind != "input_image") ||
			(format == "openai" && kind != "image_url" && kind != "file") {
			return fmt.Errorf("图片格式与当前接口不匹配，请更新客户端或重新上传图片后重试")
		}
		v.count++
		if !allowed {
			return fmt.Errorf("图片放在了当前接口不支持的消息位置；请新建对话，将图片和问题一起重新发送")
		}
		if err := v.image(part, kind); err != nil {
			return err
		}
	}
	return nil
}

func (v *deepSeekImageValidator) image(part gjson.Result, kind string) error {
	var imageURL, fileID string
	switch kind {
	case "image":
		source := part.Get("source")
		switch source.Get("type").String() {
		case "base64":
			return v.inline(source.Get("media_type").String(), source.Get("data").String())
		case "url":
			imageURL = source.Get("url").String()
		case "file":
			fileID = source.Get("file_id").String()
		default:
			return fmt.Errorf("图片格式无法识别，请重新上传 JPEG、PNG、GIF 或 WebP 图片")
		}
	case "input_image":
		imageURL, fileID = part.Get("image_url").String(), part.Get("file_id").String()
	case "image_url":
		imageURL = part.Get("image_url.url").String()
	case "file":
		imageURL, fileID = part.Get("file_data").String(), part.Get("file_id").String()
	}
	if (imageURL == "") == (fileID == "") {
		return fmt.Errorf("图片来源缺失或重复，请重新上传图片，或仅提供一个图片链接")
	}
	if fileID != "" {
		if !strings.HasPrefix(fileID, "file-api-") {
			return fmt.Errorf("图片引用不属于当前 DeepSeek 通道，请重新上传图片")
		}
		return nil
	}
	if strings.HasPrefix(imageURL, "data:") {
		header, data, ok := strings.Cut(strings.TrimPrefix(imageURL, "data:"), ",")
		if !ok || !strings.HasSuffix(header, ";base64") {
			return fmt.Errorf("图片编码无效，请重新上传图片")
		}
		return v.inline(strings.TrimSuffix(header, ";base64"), data)
	}
	u, err := url.Parse(imageURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || len(imageURL) > 8192 {
		return fmt.Errorf("图片链接无效或过长，请改用可访问的 HTTP/HTTPS 图片链接，或直接上传图片")
	}
	return nil
}

func (v *deepSeekImageValidator) inline(mediaType, data string) error {
	switch mediaType {
	case "image/jpeg", "image/png", "image/gif", "image/webp":
	default:
		return fmt.Errorf("图片格式不支持，请转换成 JPEG、PNG、GIF 或 WebP 后重试")
	}
	if data == "" {
		return fmt.Errorf("图片内容为空，请重新上传图片")
	}
	padding := len(data) - len(strings.TrimRight(data, "="))
	size := base64.StdEncoding.DecodedLen(len(data)) - padding
	if size > 32*1024*1024 {
		return fmt.Errorf("单张内嵌图片超过 32 MiB，请压缩图片后重试")
	}
	v.inlineBytes += size
	return nil
}
