package helps

import (
	"io"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestMiniMaxEmptyTruncationKeepsTerminalAndUsage(t *testing.T) {
	const req = `{"model":"MiniMax-M3.1-Flash-Preview","max_tokens":1}`
	const original = `{"id":"fixture","type":"message","role":"assistant","model":"MiniMax-M3.1-Flash-Preview","content":null,"stop_reason":"max_tokens","usage":{"input_tokens":32,"output_tokens":1,"cache_read_input_tokens":176}}`
	for _, tc := range []struct {
		base, path string
		value      any
		accept     bool
	}{
		{accept: true}, {path: "stop_reason", value: "end_turn"}, {path: "model", value: "MiniMax-M3"}, {path: "id", value: ""}, {path: "usage.output_tokens", value: -1}, {path: "usage.output_tokens", value: "1"}, {base: "https://fixture.invalid/anthropic"},
	} {
		base := tc.base
		if base == "" {
			base = "https://api.minimaxi.com/anthropic"
		}
		body := []byte(original)
		if tc.path != "" {
			body, _ = sjson.SetBytes(body, tc.path, tc.value)
		}
		out := NormalizeMiniMaxEmptyTruncation(base, []byte(req), body)
		if tc.accept {
			if gjson.GetBytes(out, "content").Raw != "[]" || gjson.GetBytes(out, "usage").Raw != gjson.GetBytes(body, "usage").Raw || gjson.GetBytes(out, "stop_reason").String() != "max_tokens" {
				t.Fatal("lost incomplete/usage", string(out))
			}
		} else if string(out) != string(body) {
			t.Fatal("unproven empty response normalized")
		}
	}
}

func TestMiniMaxTruncationReaderUsesOneProvenTerminal(t *testing.T) {
	start := `data: {"type":"message_start","message":{"id":"fixture","model":"MiniMax-M3.1-Flash-Preview","content":[]}}`
	stop := `data: {"type":"content_block_stop","index":0}`
	terminal := `data: {"type":"message_delta","delta":{"stop_reason":"max_tokens"},"usage":{"output_tokens":1}}`
	for _, tc := range []struct {
		name, base, model, next string
		accept                  bool
	}{
		{name: "measured", accept: true}, {name: "third party", base: "https://fixture.invalid/anthropic"}, {name: "other model", model: "MiniMax-M3"}, {name: "not truncated", next: strings.ReplaceAll(terminal, "max_tokens", "end_turn")}, {name: "usage mismatch", next: strings.ReplaceAll(terminal, `"output_tokens":1`, `"output_tokens":2`)}, {name: "wrong event name", next: "event: error\n" + terminal}, {name: "no terminal", next: "EOF"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base, model, next := tc.base, tc.model, tc.next
			if base == "" {
				base = "https://api.minimaxi.com/anthropic"
			}
			if model == "" {
				model = MiniMaxM31OfficialModel
			}
			if next == "" {
				next = terminal
			}
			frames := []string{start, stop}
			if next != "EOF" {
				frames = append(frames, next, `data: {"type":"message_stop"}`)
			}
			index := 0
			r := NewMiniMaxTruncationReader(func() ([]byte, error) {
				if index == len(frames) {
					return nil, io.EOF
				}
				f := []byte(frames[index])
				index++
				return f, nil
			}, base, []byte(`{"model":"`+model+`","max_tokens":1}`))
			var out []string
			for {
				b, err := r.ReadEvent()
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				out = append(out, string(b))
				if len(out) > 10 {
					t.Fatal("unbounded reader")
				}
			}
			want := frames
			if tc.accept {
				want = []string{start, terminal, `data: {"type":"message_stop"}`}
			}
			if strings.Join(out, "\n") != strings.Join(want, "\n") {
				t.Fatal("unexpected mutation", out)
			}
		})
	}
}
