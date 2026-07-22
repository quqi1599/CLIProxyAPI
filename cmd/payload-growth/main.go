package main

import (
	"github.com/router-for-me/CLIProxyAPI/v7/internal/payload/growthlint"
	"golang.org/x/tools/go/analysis/singlechecker"
)

func main() {
	analyzer := growthlint.NewAnalyzer()
	if err := analyzer.Flags.Set("baseline", "internal/payload/growthlint/baseline.txt"); err != nil {
		panic(err)
	}
	singlechecker.Main(analyzer)
}
