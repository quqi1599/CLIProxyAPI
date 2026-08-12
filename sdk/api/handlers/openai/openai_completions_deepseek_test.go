package openai

import "testing"

func TestIsDeepSeekV4ProFIMRequest(t *testing.T) {
	tests := []struct {
		name string
		body string
		want bool
	}{
		{name: "canonical model", body: `{"model":"deepseek-v4-pro","prompt":"left","suffix":"right"}`, want: true},
		{name: "qualified model", body: `{"model":"DeepSeek-official/deepseek-v4-pro[1m]","prompt":"left"}`, want: true},
		{name: "missing prompt", body: `{"model":"deepseek-v4-pro","suffix":"right"}`},
		{name: "flash does not support FIM", body: `{"model":"deepseek-v4-flash","prompt":"left"}`},
		{name: "other completion model", body: `{"model":"gpt-3.5-turbo-instruct","prompt":"left"}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := isDeepSeekV4ProFIMRequest([]byte(test.body)); got != test.want {
				t.Fatalf("isDeepSeekV4ProFIMRequest() = %v, want %v", got, test.want)
			}
		})
	}
}
