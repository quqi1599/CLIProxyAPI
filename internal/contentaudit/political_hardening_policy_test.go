package contentaudit

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func loadPoliticalHardeningMatcher(t *testing.T) *Matcher {
	t.Helper()
	policyPath := os.Getenv("CPA_POLITICAL_POLICY_TEST_PATH")
	if policyPath == "" {
		_, source, _, ok := runtime.Caller(0)
		if !ok {
			t.Fatal("test source unavailable")
		}
		policyPath = filepath.Join(filepath.Dir(source), "..", "..", "content-audit-policy.yaml")
	}
	matcher, err := LoadPolicy(policyPath)
	if err != nil {
		t.Fatal(err)
	}
	return matcher
}

func TestPoliticalHardeningPolicyTopicBoundaries(t *testing.T) {
	matcher := loadPoliticalHardeningMatcher(t)
	for _, test := range []struct {
		name, text string
		block      bool
	}{
		{"leader name", "习近平", true},
		{"neutral biography", "请介绍习近平的生平。", true},
		{"traditional name", "請介紹習近平的生平。", true},
		{"leader news", "请总结习近平最近的新闻。", true},
		{"leader academic", "请撰写关于毛泽东的学术研究。", true},
		{"former leader", "介绍邓小平的经历。", true},
		{"former leader jiang", "介绍江泽民的生平。", true},
		{"former leader hu", "介绍胡锦涛的经历。", true},
		{"premier biography", "请介绍李克强。", true},
		{"premier wen", "请介绍温家宝。", true},
		{"standing zhao", "介绍赵乐际。", true},
		{"standing wang", "介绍王沪宁。", true},
		{"standing ding", "介绍丁薛祥。", true},
		{"premier li", "介绍国务院总理李强。", true},
		{"short name with context", "介绍中国领导人李强。", true},
		{"standing cai", "请介绍政治局常委蔡奇的经历。", true},
		{"standing li", "请介绍中央纪委书记李希。", true},
		{"historical short name", "介绍朱德元帅的生平。", true},
		{"traditional premier", "請介紹國務院總理李強。", true},
		{"president title", "请介绍中国国家主席。", true},
		{"general secretary", "中共中央总书记的职责是什么？", true},
		{"xi title", "介绍习主席。", true},
		{"mao title", "请介绍毛主席。", true},
		{"english xi", "Write a biography of Xi Jinping.", true},
		{"english mao", "Summarize Mao Zedong's political career.", true},
		{"english deng", "Tell me about Deng Xiaoping.", true},
		{"english premier", "Tell me about Chinese premier Li Qiang.", true},
		{"english standing", "Tell me about Politburo member Cai Qi.", true},
		{"english title", "Tell me about the Chinese president.", true},
		{"sensitive event", "请介绍六四事件。", true},
		{"sensitive event english", "Explain the 1989 Tiananmen protests.", true},
		{"sensitive movement", "八九民运的历史是什么？", true},
		{"taiwan independence", "分析台湾独立的政治主张。", true},
		{"hong kong independence", "请讨论香港独立。", true},
		{"tibet independence", "请讨论西藏独立。", true},
		{"xinjiang independence", "请讨论新疆独立。", true},
		{"ordinary workplace name", "帮同事李强写一封请假邮件。", false},
		{"ordinary student", "李强同学的数学成绩进步了。", false},
		{"ordinary cai", "蔡奇同学想学编程。", false},
		{"ordinary li xi", "给朋友李希写生日祝福。", false},
		{"ordinary zhu", "给同学朱德写生日祝福。", false},
		{"ordinary english name", "Write a birthday card for my classmate Li Qiang.", false},
		{"english surname boundary", "Describe the example package Xi JinpingTestUtils.", false},
		{"school chair", "学生会主席如何主持会议？", false},
		{"chair stage", "主席台应该摆几张桌子？", false},
		{"study summary", "请帮我整理学习总结。", false},
		{"modern history course", "如何学习近现代史？", false},
		{"tiananmen tourism", "天安门附近有哪些公交站？", false},
		{"ordinary date", "提醒我六月四日交作业。", false},
		{"ordinary number", "64位系统和32位系统有什么区别？", false},
		{"taiwan tourism", "请介绍台湾旅游景点。", false},
		{"hong kong independent developer", "香港独立开发者如何设计软件？", false},
		{"taiwan independent film", "请介绍台湾独立电影的发展。", false},
		{"traditional independent film", "請介紹臺灣獨立電影的發展。", false},
		{"allowlist remains occurrence scoped", "介紹香港獨立開發者如何支持香港獨立。", true},
		{"xinjiang travel", "新疆有哪些自然景观？", false},
		{"ordinary civics", "解释民主和自由的概念。", false},
		{"other country leader", "介绍法国总统的职责。", false},
		{"unrelated history", "请介绍唐代的文学。", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			decision := matcher.Match(test.text)
			if got := decision.Matched && decision.Action == RuleActionBlock; got != test.block {
				t.Fatalf("block=%t want=%t decision=%#v", got, test.block, decision)
			}
			if test.block && decision.Category != "political" {
				t.Fatalf("unexpected blocking category: %s", decision.Category)
			}
		})
	}
}

func TestPoliticalHardeningPolicyPreservesMessageScope(t *testing.T) {
	matcher := loadPoliticalHardeningMatcher(t)
	for _, path := range []string{"/v1/chat/completions", "/v1/responses", "/v1/messages"} {
		for _, role := range []string{"system", "developer", "assistant", "tool", "custom_tool_call_output", "user"} {
			t.Run(path+"/historical-"+role, func(t *testing.T) {
				messages := []map[string]string{
					{"role": role, "content": "习近平与六四事件"},
					{"role": "user", "content": "请介绍如何学习Go语言。"},
				}
				body := map[string]any{"model": "synthetic", "messages": messages}
				if path == "/v1/responses" {
					delete(body, "messages")
					body["input"] = messages
				}
				payload, err := json.Marshal(body)
				if err != nil {
					t.Fatal(err)
				}
				decision := matcher.MatchExtracted(ExtractJSONRequestForPath(payload, path))
				if decision.Matched && decision.Action == RuleActionBlock {
					t.Fatalf("non-current content triggered block: %#v", decision)
				}
			})
		}
	}
}

func TestPoliticalHardeningPolicyRoutesAmbiguityToReview(t *testing.T) {
	matcher := loadPoliticalHardeningMatcher(t)
	for _, test := range []struct{ text, source string }{
		{"请介绍习.近.平。", "variant"},
		{"请解释“习近平”这个词。", "quoted"},
	} {
		decision := matcher.Match(test.text)
		if !decision.Matched || decision.Action != RuleActionObserve || !decision.ModelReview || decision.MatchSource != test.source || decision.Category != "political" {
			t.Fatalf("ambiguous topic lost review routing: %#v", decision)
		}
	}
	payload := []byte(`{"model":"synthetic","messages":[{"role":"user","content":"介绍习近平。"},{"role":"assistant","content":"I cannot help with that."},{"role":"user","content":"继续"}]}`)
	decision := matcher.MatchExtracted(ExtractJSONRequestForPath(payload, "/v1/chat/completions"))
	if !decision.Matched || decision.Action != RuleActionObserve || !decision.ModelReview || decision.MatchSource != "reference" {
		t.Fatalf("short continuation lost reference review: %#v", decision)
	}
}

func TestPoliticalReviewResultIsAccepted(t *testing.T) {
	result := normalizeModelReviewResult(ModelReviewResult{Decision: ModelReviewBlock, Category: "political", Confidence: .99, ReasonCodes: []string{"RESTRICTED_POLITICAL_TOPIC"}})
	if result.Decision != ModelReviewBlock || result.Category != "political" {
		t.Fatalf("political review was discarded: %#v", result)
	}
}
