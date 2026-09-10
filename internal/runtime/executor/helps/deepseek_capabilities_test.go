package helps

import "testing"

func TestDeepSeekFlashImageValidation(t *testing.T) {
	for _, tc := range []struct {
		name, format, body string
		valid              bool
	}{
		{"chat url", "openai", `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.com/a.png"}}]}]}`, true},
		{"chat assistant image", "openai", `{"messages":[{"role":"assistant","content":[{"type":"image_url","image_url":{"url":"https://example.com/a.png"}}]}]}`, false},
		{"responses developer", "responses", `{"input":[{"role":"developer","content":[{"type":"input_image","image_url":"https://example.com/a.png"}]}]}`, true},
		{"responses tool screenshot", "responses", `{"input":[{"type":"function_call_output","call_id":"a","output":[{"type":"input_image","image_url":"https://example.com/a.png"}]}]}`, true},
		{"responses system image", "responses", `{"input":[{"role":"system","content":[{"type":"input_image","image_url":"https://example.com/a.png"}]}]}`, false},
		{"duplicate sources", "responses", `{"input":[{"role":"user","content":[{"type":"input_image","image_url":"https://example.com/a.png","file_id":"file-api-a"}]}]}`, false},
		{"missing source", "responses", `{"input":[{"role":"user","content":[{"type":"input_image"}]}]}`, false},
		{"upstream file reference", "responses", `{"input":[{"role":"user","content":[{"type":"input_image","file_id":"file-api-a"}]}]}`, true},
		{"foreign file reference", "responses", `{"input":[{"role":"user","content":[{"type":"input_image","file_id":"file-openai-a"}]}]}`, false},
		{"document remains unsupported", "responses", `{"input":[{"role":"user","content":[{"type":"input_file","file_id":"file-api-a"}]}]}`, false},
		{"claude tool image", "claude", `{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"a","content":[{"type":"image","source":{"type":"url","url":"https://example.com/a.png"}}]}]}]}`, true},
		{"claude text only", "claude", `{"messages":[{"role":"user","content":"hello"}]}`, true},
		{"claude wrong image shape", "claude", `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.com/a.png"}}]}]}`, false},
		{"unsupported media", "claude", `{"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/svg+xml","data":"AAAA"}}]}]}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateDeepSeekFlashImages([]byte(tc.body), tc.format)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v err=%v", tc.valid, err)
			}
		})
	}
}

func TestDeepSeekFlashCapabilityHostBoundary(t *testing.T) {
	for _, host := range []string{"https://api.deepseek.com.evil.test", "https://api.deepseek.com@relay.example.com", "https://akool.com"} {
		if DeepSeekFlashVisionEnabled("deepseek-flash", host) || OfficialDeepSeekModel("deepseek-v4.1-flash", host) != "deepseek-v4.1-flash" {
			t.Fatalf("third-party host acquired official capabilities: %s", host)
		}
	}
	if !DeepSeekFlashVisionEnabled("deepseek-v4-flash", "https://api.deepseek.com/anthropic") {
		t.Fatal("legacy Flash alias lost vision")
	}
	if DeepSeekFlashVisionEnabled("deepseek-v4-pro", "https://api.deepseek.com") {
		t.Fatal("Pro gained vision before its upstream migration")
	}
}
