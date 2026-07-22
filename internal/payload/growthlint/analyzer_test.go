package growthlint

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/tools/go/analysis/analysistest"
)

func TestAnalyzer(t *testing.T) {
	analysistest.Run(t, analysistest.TestData(), NewAnalyzer(), "a", "callbackpositive", "callbacknegative", "arraybuilderpositive", "arraybuildernegative")
}

func TestAnalyzerBaselineOnlyAllowsKnownCount(t *testing.T) {
	path := filepath.Join(t.TempDir(), "baseline.txt")
	entry := "PG001 internal/payload/growthlint/testdata/src/baseline/baseline.go 729655ecbc5c8913 1\n"
	if err := os.WriteFile(path, []byte(entry), 0o600); err != nil {
		t.Fatal(err)
	}
	analyzer := NewAnalyzer()
	if err := analyzer.Flags.Set("baseline", path); err != nil {
		t.Fatal(err)
	}
	analysistest.Run(t, analysistest.TestData(), analyzer, "baseline")
}

func TestAnalyzerBaselineCountsSemanticCallbackLoops(t *testing.T) {
	path := filepath.Join(t.TempDir(), "baseline.txt")
	entry := "PG001 internal/payload/growthlint/testdata/src/callbackbaseline/callbackbaseline.go 1c95c7dcacc50010 1\n"
	if err := os.WriteFile(path, []byte(entry), 0o600); err != nil {
		t.Fatal(err)
	}
	analyzer := NewAnalyzer()
	if err := analyzer.Flags.Set("baseline", path); err != nil {
		t.Fatal(err)
	}
	analysistest.Run(t, analysistest.TestData(), analyzer, "callbackbaseline")
}

func TestAnalyzerRejectsStaleBaseline(t *testing.T) {
	path := filepath.Join(t.TempDir(), "baseline.txt")
	entry := "PG001 internal/payload/growthlint/testdata/src/stale/stale.go c8c856fb09bbc2d5 2\n"
	if err := os.WriteFile(path, []byte(entry), 0o600); err != nil {
		t.Fatal(err)
	}
	analyzer := NewAnalyzer()
	if err := analyzer.Flags.Set("baseline", path); err != nil {
		t.Fatal(err)
	}
	analysistest.Run(t, analysistest.TestData(), analyzer, "stale")
}

func TestReadBaseline(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "baseline.txt")
	if err := os.MkdirAll(filepath.Join(root, "internal"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "internal", "a.go"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("# known debt\nPG001 internal/a.go abc123 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	baseline, err := readBaseline(root, "baseline.txt")
	if err != nil {
		t.Fatal(err)
	}
	if got := baseline[baselineKey{rule: ruleLoopAppend, path: "internal/a.go", fingerprint: "abc123"}]; got != 2 {
		t.Fatalf("baseline count = %d, want 2", got)
	}
}

func TestAppendPath(t *testing.T) {
	for _, path := range []string{"-1", "response.output.-1"} {
		if !appendPath(path) {
			t.Fatalf("appendPath(%q) = false", path)
		}
	}
	for _, path := range []string{"1", "response.-10", "response.output"} {
		if appendPath(path) {
			t.Fatalf("appendPath(%q) = true", path)
		}
	}
}
