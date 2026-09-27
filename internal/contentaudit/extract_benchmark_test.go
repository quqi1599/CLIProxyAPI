package contentaudit

import (
	"encoding/json"
	"runtime"
	"strings"
	"testing"
)

var auditBenchmarkSizes = []struct {
	name  string
	bytes int
}{
	{"8KiB", 8 << 10},
	{"1MiB", 1 << 20},
	{"16MiB", 16 << 20},
	{"64MiB", 64 << 20},
}

// Run with -cpu=1,2 to compare one and two concurrent requests. Fixtures are
// immutable and shared; all result references stay local to each worker.
func BenchmarkEvidenceWindows(b *testing.B) {
	for _, size := range auditBenchmarkSizes {
		for _, alphabet := range []struct{ name, text string }{{"ASCII", "a"}, {"UTF8", "中🙂"}} {
			for _, window := range []struct {
				name   string
				limit  int
				suffix bool
			}{{"prefix", maxEvidenceStringRunes, false}, {"suffix", 4096, true}} {
				for _, legacy := range []bool{true, false} {
					version := "bounded"
					if legacy {
						version = "legacy"
					}
					b.Run(size.name+"/"+alphabet.name+"/"+window.name+"/"+version, func(b *testing.B) {
						input := strings.Repeat(alphabet.text, size.bytes/len(alphabet.text))
						b.ReportAllocs()
						b.ResetTimer()
						b.RunParallel(func(pb *testing.PB) {
							for pb.Next() {
								var out string
								if legacy {
									out, _ = legacyEvidenceWindow(input, window.limit, window.suffix)
								} else if window.suffix {
									out, _ = evidenceSuffix(input, window.limit)
								} else {
									out, _ = evidencePrefix(input, window.limit)
								}
								runtime.KeepAlive(out)
							}
						})
					})
				}
			}
		}
	}
}

func BenchmarkURLPrefilter(b *testing.B) {
	for _, size := range auditBenchmarkSizes {
		for _, kind := range []struct{ name, text string }{{"lower", "plain text "}, {"mixed", "Plain TEXT "}, {"url", "https://example.test/a"}, {"data", "DATA:text/plain,abc"}} {
			for _, legacy := range []bool{true, false} {
				version := "prefilter"
				if legacy {
					version = "legacy"
				}
				b.Run(size.name+"/"+kind.name+"/"+version, func(b *testing.B) {
					input := kind.text
					if kind.name == "lower" || kind.name == "mixed" {
						input = strings.Repeat(input, size.bytes/len(input))
					}
					b.ReportAllocs()
					b.ResetTimer()
					b.RunParallel(func(pb *testing.PB) {
						for pb.Next() {
							var out bool
							if legacy {
								out = legacyIsURLOrData(input)
							} else {
								out = isURLOrData(input)
							}
							runtime.KeepAlive(out)
						}
					})
				})
			}
		}
	}
}

// This benchmark deliberately depends only on the public extraction entrypoint,
// so the same fixture can run against the parent revision as an end-to-end control.
func BenchmarkAuditExtraction(b *testing.B) {
	for _, size := range auditBenchmarkSizes {
		b.Run(size.name, func(b *testing.B) {
			body, err := json.Marshal(map[string]any{
				"model": "benchmark", "messages": []any{
					map[string]any{"role": "user", "content": strings.Repeat("Plain TEXT ", size.bytes/11)},
				},
			})
			if err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.SetBytes(int64(len(body)))
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					out := ExtractJSONRequestForPath(body, "/v1/chat/completions")
					runtime.KeepAlive(out)
				}
			})
		})
	}
}
