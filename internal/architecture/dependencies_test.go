package architecture

import (
	"fmt"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
)

const modulePath = "github.com/router-for-me/CLIProxyAPI/v7"

type dependencyRule struct {
	name                  string
	sourcePrefixes        []string
	forbiddenImports      []string
	forbiddenExactImports []string
	allowedImports        []string
}

var dependencyRules = []dependencyRule{
	{
		name: "api packages use executor capabilities instead of runtime implementations",
		sourcePrefixes: []string{
			modulePath + "/internal/api",
			modulePath + "/sdk/api",
		},
		forbiddenImports: []string{
			modulePath + "/internal/runtime/executor",
		},
		allowedImports: []string{
			modulePath + "/internal/runtime/executor/helps",
		},
	},
	{
		name: "translators remain pure and do not perform network IO",
		sourcePrefixes: []string{
			modulePath + "/internal/translator",
			modulePath + "/sdk/translator",
		},
		forbiddenImports: []string{
			"crypto/tls",
			"github.com/gorilla/websocket",
			"golang.org/x/net",
			"net/http",
			modulePath + "/internal/httpfetch",
			modulePath + "/internal/runtime/executor",
			modulePath + "/internal/transport",
			modulePath + "/sdk/proxyutil",
		},
		forbiddenExactImports: []string{"net"},
	},
}

func TestRepositoryDependencyBoundaries(t *testing.T) {
	repoRoot := repositoryRoot(t)
	scanRoots := []string{
		"internal/api",
		"sdk/api",
		"internal/translator",
		"sdk/translator",
	}

	var violations []string
	for _, scanRoot := range scanRoots {
		root := filepath.Join(repoRoot, filepath.FromSlash(scanRoot))
		errWalk := filepath.WalkDir(root, func(path string, entry fs.DirEntry, errWalk error) error {
			if errWalk != nil {
				return errWalk
			}
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
				return nil
			}

			relativeDir, errRelative := filepath.Rel(repoRoot, filepath.Dir(path))
			if errRelative != nil {
				return fmt.Errorf("resolve package path for %s: %w", path, errRelative)
			}
			packagePath := modulePath + "/" + filepath.ToSlash(relativeDir)
			fileSet := token.NewFileSet()
			parsed, errParse := parser.ParseFile(fileSet, path, nil, parser.ImportsOnly)
			if errParse != nil {
				return fmt.Errorf("parse imports from %s: %w", path, errParse)
			}
			for _, importSpec := range parsed.Imports {
				importPath, errUnquote := strconv.Unquote(importSpec.Path.Value)
				if errUnquote != nil {
					return fmt.Errorf("decode import in %s: %w", path, errUnquote)
				}
				for _, ruleName := range violatedRules(packagePath, importPath) {
					position := fileSet.Position(importSpec.Pos())
					relativeFile, errFileRelative := filepath.Rel(repoRoot, position.Filename)
					if errFileRelative != nil {
						return fmt.Errorf("resolve source path for %s: %w", position.Filename, errFileRelative)
					}
					violations = append(violations, fmt.Sprintf("%s:%d imports %q: %s", filepath.ToSlash(relativeFile), position.Line, importPath, ruleName))
				}
			}
			return nil
		})
		if errWalk != nil {
			t.Fatalf("scan %s: %v", scanRoot, errWalk)
		}
	}

	if len(violations) > 0 {
		sort.Strings(violations)
		t.Fatalf("dependency boundary violations:\n%s", strings.Join(violations, "\n"))
	}
}

func TestDependencyRulesRejectForbiddenEdges(t *testing.T) {
	tests := []struct {
		name       string
		source     string
		importPath string
		wantRule   bool
	}{
		{
			name:       "api cannot import runtime executor",
			source:     modulePath + "/sdk/api/handlers/openai",
			importPath: modulePath + "/internal/runtime/executor",
			wantRule:   true,
		},
		{
			name:       "api cannot import provider executor package",
			source:     modulePath + "/internal/api",
			importPath: modulePath + "/internal/runtime/executor/provider",
			wantRule:   true,
		},
		{
			name:       "api may use shared executor helpers during migration",
			source:     modulePath + "/sdk/api/handlers",
			importPath: modulePath + "/internal/runtime/executor/helps",
		},
		{
			name:       "api may use executor capability types",
			source:     modulePath + "/sdk/api/handlers",
			importPath: modulePath + "/sdk/cliproxy/executor",
		},
		{
			name:       "translator cannot import net http",
			source:     modulePath + "/internal/translator/openai/claude",
			importPath: "net/http",
			wantRule:   true,
		},
		{
			name:       "translator cannot import project transport",
			source:     modulePath + "/sdk/translator",
			importPath: modulePath + "/internal/transport/http2pool",
			wantRule:   true,
		},
		{
			name:       "translator may use URL value parsing",
			source:     modulePath + "/internal/translator/common",
			importPath: "net/url",
		},
		{
			name:       "unrelated packages are outside these rules",
			source:     modulePath + "/internal/runtime/executor",
			importPath: "net/http",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotRule := len(violatedRules(tt.source, tt.importPath)) > 0
			if gotRule != tt.wantRule {
				t.Fatalf("violatedRules(%q, %q) matched = %t, want %t", tt.source, tt.importPath, gotRule, tt.wantRule)
			}
		})
	}
}

func violatedRules(source, importPath string) []string {
	var violated []string
	for _, rule := range dependencyRules {
		if !matchesAnyPrefix(source, rule.sourcePrefixes) || matchesAnyPrefix(importPath, rule.allowedImports) {
			continue
		}
		if matchesAnyPrefix(importPath, rule.forbiddenImports) || contains(rule.forbiddenExactImports, importPath) {
			violated = append(violated, rule.name)
		}
	}
	return violated
}

func matchesAnyPrefix(value string, prefixes []string) bool {
	for _, prefix := range prefixes {
		if value == prefix || strings.HasPrefix(value, prefix+"/") {
			return true
		}
	}
	return false
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve architecture test location")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(filename), "..", ".."))
}
