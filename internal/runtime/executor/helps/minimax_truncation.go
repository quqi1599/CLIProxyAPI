package helps

import (
	"bytes"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func miniMaxTruncationBudget(base string, request []byte) (string, int64) {
	model := gjson.GetBytes(request, "model").String()
	// The synthetic short name checks the same official endpoint boundary.
	if model != MiniMaxM31OfficialModel || OfficialMiniMaxModel("MiniMax-M3.1", base) != MiniMaxM31OfficialModel {
		return "", 0
	}
	budget, err := strconv.ParseInt(gjson.GetBytes(request, "max_tokens").Raw, 10, 64)
	if err != nil || budget <= 0 {
		return "", 0
	}
	return model, budget
}

// NormalizeMiniMaxEmptyTruncation preserves the supplier's incomplete terminal
// and accounting. It never raises the caller's budget or invents answer text.
func NormalizeMiniMaxEmptyTruncation(base string, request, response []byte) []byte {
	model, budget := miniMaxTruncationBudget(base, request)
	if budget == 0 || !gjson.ValidBytes(response) {
		return response
	}
	r := gjson.ParseBytes(response)
	if r.Get("model").String() != model || r.Get("content").Raw != "null" || r.Get("stop_reason").String() != "max_tokens" || r.Get("id").String() == "" || r.Get("type").String() != "message" || r.Get("role").String() != "assistant" {
		return response
	}
	for _, field := range []string{"usage.input_tokens", "usage.output_tokens"} {
		n, err := strconv.ParseInt(r.Get(field).Raw, 10, 64)
		if err != nil || n < 0 {
			return response
		}
	}
	out, err := sjson.SetRawBytes(response, "content", []byte("[]"))
	if err != nil {
		return response
	}
	return out
}

// MiniMaxTruncationReader holds at most one bounded event. Only an orphan stop
// immediately followed by a matching exhausted-budget terminal is removed.
type MiniMaxTruncationReader struct {
	read                       func() ([]byte, error)
	model                      string
	budget                     int64
	started, content, terminal bool
	pending                    []byte
	pendingErr                 error
	hasPending                 bool
}

func NewMiniMaxTruncationReader(read func() ([]byte, error), base string, request []byte) *MiniMaxTruncationReader {
	model, budget := miniMaxTruncationBudget(base, request)
	return &MiniMaxTruncationReader{read: read, model: model, budget: budget}
}

func miniMaxFrameJSON(event []byte) gjson.Result {
	var data []byte
	name := ""
	for _, line := range bytes.Split(event, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if bytes.HasPrefix(line, []byte("event:")) {
			name = strings.TrimSpace(string(line[6:]))
		}
		if bytes.HasPrefix(line, []byte("data:")) {
			if data != nil {
				return gjson.Result{}
			}
			data = bytes.TrimSpace(line[5:])
		}
	}
	if !gjson.ValidBytes(data) {
		return gjson.Result{}
	}
	r := gjson.ParseBytes(data)
	if name != "" && name != r.Get("type").String() {
		return gjson.Result{}
	}
	return r
}

func (r *MiniMaxTruncationReader) ReadEvent() ([]byte, error) {
	var event []byte
	var err error
	if r.hasPending {
		event, err = r.pending, r.pendingErr
		r.pending, r.pendingErr, r.hasPending = nil, nil, false
	} else {
		event, err = r.read()
	}
	if err != nil || r.budget == 0 || r.terminal {
		return event, err
	}
	data := miniMaxFrameJSON(event)
	switch data.Get("type").String() {
	case "message_start":
		r.started = data.Get("message.model").String() == r.model && data.Get("message.id").String() != "" && data.Get("message.content").IsArray() && len(data.Get("message.content").Array()) == 0
	case "content_block_start", "content_block_delta":
		r.content = true
	case "message_stop":
		r.terminal = true
	case "content_block_stop":
		if !r.started || r.content || data.Get("index").Raw != "0" {
			break
		}
		next, nextErr := r.read()
		n := miniMaxFrameJSON(next)
		output, countErr := strconv.ParseInt(n.Get("usage.output_tokens").Raw, 10, 64)
		if nextErr == nil && n.Get("type").String() == "message_delta" && n.Get("delta.stop_reason").String() == "max_tokens" && countErr == nil && output == r.budget {
			r.content = true
			return next, nil
		}
		r.pending, r.pendingErr, r.hasPending = next, nextErr, true
	}
	return event, err
}
