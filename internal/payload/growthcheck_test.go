package payload

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func TestSJSONLoopAppendBaseline(t *testing.T) {
	want := map[string]int{
		"internal/runtime/executor/claude_executor.go":                                           9,
		"internal/runtime/executor/codex_openai_images.go":                                       3,
		"internal/runtime/executor/gemini_executor.go":                                           1,
		"internal/runtime/executor/helps/claude_tool_history.go":                                 11,
		"internal/runtime/executor/helps/payload_helpers.go":                                     1,
		"internal/runtime/executor/kimi_executor.go":                                             21,
		"internal/runtime/executor/openai_compat_images.go":                                      2,
		"internal/runtime/executor/xai_executor.go":                                              3,
		"internal/translator/antigravity/claude/antigravity_claude_request.go":                   25,
		"internal/translator/antigravity/claude/antigravity_claude_response.go":                  1,
		"internal/translator/antigravity/claude/web_search.go":                                   2,
		"internal/translator/antigravity/gemini/antigravity_gemini_request.go":                   6,
		"internal/translator/antigravity/gemini/antigravity_gemini_response.go":                  1,
		"internal/translator/antigravity/openai/chat-completions/antigravity_openai_request.go":  9,
		"internal/translator/antigravity/openai/chat-completions/antigravity_openai_response.go": 2,
		"internal/translator/claude/gemini/claude_gemini_response.go":                            1,
		"internal/translator/claude/openai/responses/claude_openai-responses_request.go":         6,
		"internal/translator/claude/openai/responses/claude_openai-responses_response.go":        2,
		"internal/translator/codex/claude/codex_claude_request.go":                               9,
		"internal/translator/codex/gemini/codex_gemini_request.go":                               11,
		"internal/translator/codex/gemini/codex_gemini_response.go":                              1,
		"internal/translator/codex/openai/chat-completions/codex_openai_request.go":              17,
		"internal/translator/codex/openai/chat-completions/codex_openai_response.go":             2,
		"internal/translator/gemini/claude/gemini_claude_request.go":                             1,
		"internal/translator/gemini/claude/gemini_claude_response.go":                            1,
		"internal/translator/gemini/openai/chat-completions/gemini_openai_request.go":            9,
		"internal/translator/gemini/openai/chat-completions/gemini_openai_response.go":           4,
		"internal/translator/gemini/openai/responses/gemini_openai-responses_request.go":         12,
		"internal/translator/gemini/openai/responses/gemini_openai-responses_response.go":        3,
		"internal/translator/openai/claude/openai_claude_request.go":                             3,
		"internal/translator/openai/claude/openai_claude_response.go":                            3,
		"internal/translator/openai/openai/responses/openai_openai-responses_request.go":         5,
		"internal/translator/openai/openai/responses/openai_openai-responses_response.go":        1,
		"sdk/api/handlers/openai/openai_images_handlers.go":                                      4,
		"sdk/api/handlers/openai/openai_images_minimax.go":                                       1,
		"sdk/api/handlers/openai/openai_videos_handlers.go":                                      2,
	}

	root := repositoryRoot(t)
	got := map[string]int{}
	for _, dir := range []string{"cmd", "internal", "sdk"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, entry fs.DirEntry, err error) error {
			if err != nil || entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			count, err := sjsonLoopAppendCount(path)
			if err != nil || count == 0 {
				return err
			}
			relative, err := filepath.Rel(root, path)
			if err == nil {
				got[filepath.ToSlash(relative)] = count
			}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sjson loop-append debt changed; remove new calls or lower the baseline after cleanup\ngot:  %#v\nwant: %#v", got, want)
	}
}

func sjsonLoopAppendCount(path string) (int, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		return 0, err
	}
	aliases := map[string]bool{}
	for _, spec := range file.Imports {
		importPath, _ := strconv.Unquote(spec.Path.Value)
		if importPath == "github.com/tidwall/sjson" {
			name := "sjson"
			if spec.Name != nil {
				name = spec.Name.Name
			}
			aliases[name] = true
		}
	}
	count := 0
	ast.Inspect(file, func(node ast.Node) bool {
		var body *ast.BlockStmt
		switch loop := node.(type) {
		case *ast.ForStmt:
			body = loop.Body
		case *ast.RangeStmt:
			body = loop.Body
		default:
			return true
		}
		ast.Inspect(body, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok || len(call.Args) < 2 || !containsAppendPath(call.Args[1]) {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := selector.X.(*ast.Ident)
			if ok && aliases[pkg.Name] && strings.HasPrefix(selector.Sel.Name, "Set") {
				count++
			}
			return true
		})
		return true
	})
	return count, nil
}

func containsAppendPath(expr ast.Expr) bool {
	found := false
	ast.Inspect(expr, func(node ast.Node) bool {
		literal, ok := node.(*ast.BasicLit)
		if !ok || literal.Kind != token.STRING {
			return true
		}
		value, _ := strconv.Unquote(literal.Value)
		if value == "-1" || strings.Contains(value, ".-1") {
			found = true
			return false
		}
		return true
	})
	return found
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("repository root not found")
		}
		dir = parent
	}
}
