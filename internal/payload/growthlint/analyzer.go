// Package growthlint detects payload transformations that can become quadratic.
package growthlint

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"fmt"
	"go/ast"
	"go/build"
	"go/constant"
	"go/format"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/passes/inspect"
	"golang.org/x/tools/go/ast/inspector"
)

const (
	ruleLoopAppend         = "PG001"
	ruleBodyRewrite        = "PG002"
	ruleLoopMarshal        = "PG003"
	ruleManualRawJSONArray = "PG004"
)

var suppressionPattern = regexp.MustCompile(`^//nolint:payload-growth benchmark=(BenchmarkPayloadGrowth[A-Za-z0-9_]*) (\S.*)$`)

// NewAnalyzer returns an analyzer with no historical baseline configured.
func NewAnalyzer() *analysis.Analyzer {
	var baselinePath string
	analyzer := &analysis.Analyzer{
		Name:     "payloadgrowth",
		Doc:      "detects loop-carried JSON rewrites, marshaling of growing containers, and duplicate raw JSON array builders",
		Requires: []*analysis.Analyzer{inspect.Analyzer},
	}
	analyzer.Flags.StringVar(&baselinePath, "baseline", "", "path to the historical finding baseline")
	analyzer.Run = func(pass *analysis.Pass) (any, error) {
		return nil, run(pass, baselinePath)
	}
	return analyzer
}

// Analyzer is suitable for use from multichecker and tests.
var Analyzer = NewAnalyzer()

type finding struct {
	rule            string
	message         string
	file            *ast.File
	node            ast.Node
	fingerprintNode ast.Node
}

type baselineKey struct {
	rule        string
	path        string
	fingerprint string
}

func run(pass *analysis.Pass, baselinePath string) error {
	root := ""
	baseline := make(map[baselineKey]int)
	if strings.TrimSpace(baselinePath) != "" {
		var err error
		root, err = repositoryRoot(pass)
		if err != nil {
			return err
		}
		baseline, err = readBaseline(root, baselinePath)
		if err != nil {
			return err
		}
	}

	seen := make(map[baselineKey]int)
	reportFindings := func(items []finding) {
		for _, item := range items {
			filename := pass.Fset.PositionFor(item.node.Pos(), false).Filename
			fingerprint := findingFingerprint(pass, item)
			key := baselineKey{rule: item.rule, path: relativeFilename(root, filename), fingerprint: fingerprint}
			seen[key]++
			if seen[key] <= baseline[key] || validSuppression(pass, item) {
				continue
			}
			pass.Reportf(item.node.Pos(), "%s payload growth risk: %s; use a linear builder or justify with //nolint:payload-growth benchmark=BenchmarkPayloadGrowthName <reason> [fingerprint=%s]", item.rule, item.message, fingerprint)
		}
	}

	inspectResult := pass.ResultOf[inspect.Analyzer].(*inspector.Inspector)
	inspectResult.Preorder([]ast.Node{(*ast.ForStmt)(nil), (*ast.RangeStmt)(nil)}, func(node ast.Node) {
		var body *ast.BlockStmt
		switch loop := node.(type) {
		case *ast.ForStmt:
			body = loop.Body
		case *ast.RangeStmt:
			body = loop.Body
		}
		reportFindings(inspectLoop(pass, body, node.Pos()))
	})

	seenCallbacks := make(map[*ast.FuncLit]bool)
	inspectResult.Preorder([]ast.Node{(*ast.CallExpr)(nil)}, func(node ast.Node) {
		call := node.(*ast.CallExpr)
		file := fileForPosition(pass.Files, call.Pos())
		callback := semanticLoopCallback(pass, file, call)
		if callback == nil || seenCallbacks[callback] {
			return
		}
		seenCallbacks[callback] = true
		reportFindings(inspectLoop(pass, callback.Body, callback.Pos()))
	})

	reportedRawJSONArrayCalls := make(map[*ast.CallExpr]bool)
	inspectResult.Preorder([]ast.Node{(*ast.BinaryExpr)(nil)}, func(node ast.Node) {
		reportFindings(manualRawJSONArrayFindings(pass, node.(*ast.BinaryExpr), reportedRawJSONArrayCalls))
	})
	if root != "" {
		for _, file := range pass.Files {
			path := relativeFilename(root, pass.Fset.PositionFor(file.Pos(), false).Filename)
			for key, expected := range baseline {
				if key.path == path && expected > seen[key] {
					pass.Reportf(file.Package, "%s payload growth baseline is stale for %s fingerprint %s: found %d, baseline %d; lower or remove the baseline entry", key.rule, path, key.fingerprint, seen[key], expected)
				}
			}
		}
	}
	return nil
}

func manualRawJSONArrayFindings(pass *analysis.Pass, expression *ast.BinaryExpr, reported map[*ast.CallExpr]bool) []finding {
	if expression == nil || expression.Op != token.ADD {
		return nil
	}

	var operands []ast.Expr
	flattenAddition(expression, &operands)
	var findings []finding
	for index := 1; index+1 < len(operands); index++ {
		call, ok := operands[index].(*ast.CallExpr)
		if !ok || reported[call] || !isPackageFunction(pass, call, "strings", "Join") || len(call.Args) != 2 {
			continue
		}
		separator, okSeparator := constantString(pass, call.Args[1])
		opening, okOpening := constantString(pass, operands[index-1])
		closing, okClosing := constantString(pass, operands[index+1])
		if !okSeparator || separator != "," || !okOpening || opening != "[" || !okClosing || closing != "]" {
			continue
		}
		reported[call] = true
		findings = append(findings, finding{
			rule:            ruleManualRawJSONArray,
			message:         "manual raw JSON array assembly duplicates internal/payload.BuildRaw",
			file:            fileForPosition(pass.Files, call.Pos()),
			node:            call,
			fingerprintNode: expression,
		})
	}
	return findings
}

func flattenAddition(expression ast.Expr, operands *[]ast.Expr) {
	switch typed := expression.(type) {
	case *ast.ParenExpr:
		flattenAddition(typed.X, operands)
	case *ast.BinaryExpr:
		if typed.Op != token.ADD {
			*operands = append(*operands, expression)
			return
		}
		flattenAddition(typed.X, operands)
		flattenAddition(typed.Y, operands)
	default:
		*operands = append(*operands, expression)
	}
}

func constantString(pass *analysis.Pass, expression ast.Expr) (string, bool) {
	value := pass.TypesInfo.Types[expression].Value
	if value == nil || value.Kind() != constant.String {
		return "", false
	}
	return constant.StringVal(value), true
}

func findingFingerprint(pass *analysis.Pass, item finding) string {
	node := item.fingerprintNode
	if node == nil {
		node = item.node
	}
	var normalized bytes.Buffer
	_ = format.Node(&normalized, pass.Fset, node)
	identity := item.rule + "\x00" + enclosingFunction(pass.Fset, item.file, item.node.Pos()) + "\x00" + normalized.String()
	digest := sha256.Sum256([]byte(identity))
	return fmt.Sprintf("%x", digest[:8])
}

func enclosingFunction(fset *token.FileSet, file *ast.File, position token.Pos) string {
	if file == nil {
		return "package"
	}
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Pos() > position || position > function.End() {
			continue
		}
		name := function.Name.Name
		if function.Recv != nil && len(function.Recv.List) > 0 {
			var receiver bytes.Buffer
			_ = format.Node(&receiver, fset, function.Recv.List[0].Type)
			name = receiver.String() + "." + name
		}
		return name
	}
	return "package"
}

func relativeFilename(root, filename string) string {
	if root != "" {
		if relative, err := filepath.Rel(root, filename); err == nil {
			filename = relative
		}
	}
	return filepath.ToSlash(filename)
}

func inspectLoop(pass *analysis.Pass, body *ast.BlockStmt, loopPos token.Pos) []finding {
	if body == nil {
		return nil
	}
	file := fileForPosition(pass.Files, body.Pos())
	definitions := expressionDefinitions(pass, file)
	appendCalls := make(map[*ast.CallExpr]bool)
	var findings []finding

	inspectDirectLoop(body, func(node ast.Node) {
		call, ok := node.(*ast.CallExpr)
		if !ok || !isSJSONMutation(pass, call) || len(call.Args) < 2 || !isAppendPath(pass, call, definitions) {
			return
		}
		input, ok := expressionReference(pass, call.Args[0])
		if !ok || !expressionDeclaredBefore(input, loopPos) {
			return
		}
		appendCalls[call] = true
		findings = append(findings, finding{
			rule:    ruleLoopAppend,
			message: "sjson append path rewrites a growing JSON value inside a loop",
			file:    file,
			node:    call,
		})
	})

	findings = append(findings, bodyRewriteFindings(pass, body, loopPos, file, appendCalls)...)

	growing := growingContainers(pass, body, loopPos)
	if len(growing) == 0 {
		return findings
	}
	inspectDirectLoop(body, func(node ast.Node) {
		call, ok := node.(*ast.CallExpr)
		if !ok || !isPackageFunction(pass, call, "encoding/json", "Marshal") || len(call.Args) != 1 {
			return
		}
		container, ok := expressionReference(pass, call.Args[0])
		if !ok || !growing[container] {
			return
		}
		findings = append(findings, finding{
			rule:    ruleLoopMarshal,
			message: "json.Marshal serializes a container that grows across loop iterations",
			file:    file,
			node:    call,
		})
	})
	return findings
}

type expressionRef struct {
	base types.Object
	path string
}

type mutationAlias struct {
	root expressionRef
	call *ast.CallExpr
}

func bodyRewriteFindings(pass *analysis.Pass, body *ast.BlockStmt, loopPos token.Pos, file *ast.File, appendCalls map[*ast.CallExpr]bool) []finding {
	var assignments []*ast.AssignStmt
	inspectDirectLoop(body, func(node ast.Node) {
		if assignment, ok := node.(*ast.AssignStmt); ok {
			assignments = append(assignments, assignment)
		}
	})
	sort.Slice(assignments, func(left, right int) bool { return assignments[left].Pos() < assignments[right].Pos() })

	aliases := make(map[expressionRef]mutationAlias)
	reported := make(map[*ast.CallExpr]bool)
	var findings []finding
	addFinding := func(call *ast.CallExpr) {
		if call == nil || appendCalls[call] || reported[call] {
			return
		}
		reported[call] = true
		findings = append(findings, finding{
			rule:    ruleBodyRewrite,
			message: "loop-carried JSON value is rewritten on every iteration",
			file:    file,
			node:    call,
		})
	}

	for _, assignment := range assignments {
		if len(assignment.Lhs) > 0 && len(assignment.Rhs) == 1 {
			if call, ok := assignment.Rhs[0].(*ast.CallExpr); ok && isSJSONMutation(pass, call) && len(call.Args) > 0 {
				output, okOutput := expressionReference(pass, assignment.Lhs[0])
				input, okInput := expressionReference(pass, call.Args[0])
				if okOutput && okInput {
					source := mutationAlias{root: input}
					if existing, okAlias := aliases[input]; okAlias {
						source.root = existing.root
					}
					source.call = call
					aliases[output] = source
					if output == source.root && expressionDeclaredBefore(source.root, loopPos) {
						addFinding(call)
					}
					continue
				}
			}
		}

		if len(assignment.Lhs) != len(assignment.Rhs) {
			continue
		}
		for index, lhs := range assignment.Lhs {
			output, okOutput := expressionReference(pass, lhs)
			input, okInput := expressionReference(pass, assignment.Rhs[index])
			if !okOutput || !okInput {
				if okOutput {
					delete(aliases, output)
				}
				continue
			}
			source := mutationAlias{root: input}
			if existing, okAlias := aliases[input]; okAlias {
				source = existing
			}
			aliases[output] = source
			if source.call != nil && output == source.root && expressionDeclaredBefore(source.root, loopPos) {
				addFinding(source.call)
			}
		}
	}
	return findings
}

func expressionReference(pass *analysis.Pass, expression ast.Expr) (expressionRef, bool) {
	switch typed := expression.(type) {
	case *ast.Ident:
		object := objectOf(pass, typed)
		return expressionRef{base: object}, object != nil && typed.Name != "_"
	case *ast.SelectorExpr:
		base, ok := expressionReference(pass, typed.X)
		if !ok {
			return expressionRef{}, false
		}
		field := pass.TypesInfo.Uses[typed.Sel]
		if selection := pass.TypesInfo.Selections[typed]; selection != nil {
			field = selection.Obj()
		}
		if field == nil {
			return expressionRef{}, false
		}
		base.path += "." + types.Id(field.Pkg(), field.Name()) + ":" + field.Type().String()
		return base, true
	case *ast.ParenExpr:
		return expressionReference(pass, typed.X)
	case *ast.UnaryExpr:
		if typed.Op == token.AND || typed.Op == token.MUL {
			return expressionReference(pass, typed.X)
		}
	case *ast.SliceExpr:
		if typed.Low == nil && typed.High == nil && typed.Max == nil {
			return expressionReference(pass, typed.X)
		}
	case *ast.CallExpr:
		if len(typed.Args) == 1 && pass.TypesInfo.Types[typed.Fun].IsType() {
			return expressionReference(pass, typed.Args[0])
		}
		if len(typed.Args) == 1 && isPackageFunction(pass, typed, "bytes", "Clone") {
			return expressionReference(pass, typed.Args[0])
		}
	}
	return expressionRef{}, false
}

func expressionDeclaredBefore(reference expressionRef, position token.Pos) bool {
	return reference.base != nil && reference.base.Pos() < position
}

func inspectDirectLoop(body *ast.BlockStmt, visit func(ast.Node)) {
	ast.Inspect(body, func(node ast.Node) bool {
		if node == nil {
			return true
		}
		if node != body {
			switch node.(type) {
			case *ast.ForStmt, *ast.RangeStmt, *ast.FuncLit:
				return false
			}
		}
		visit(node)
		return true
	})
}

func semanticLoopCallback(pass *analysis.Pass, file *ast.File, call *ast.CallExpr) *ast.FuncLit {
	if file == nil || len(call.Args) != 1 || !isPackageFunction(pass, call, "github.com/tidwall/gjson", "ForEach") {
		return nil
	}
	return resolveFunctionLiteral(pass, call.Args[0], expressionDefinitions(pass, file), call.Pos(), make(map[types.Object]bool))
}

func resolveFunctionLiteral(pass *analysis.Pass, expression ast.Expr, definitions map[types.Object][]ast.Expr, before token.Pos, seen map[types.Object]bool) *ast.FuncLit {
	switch typed := expression.(type) {
	case *ast.FuncLit:
		return typed
	case *ast.ParenExpr:
		return resolveFunctionLiteral(pass, typed.X, definitions, before, seen)
	case *ast.Ident:
		object := objectOf(pass, typed)
		if object == nil || seen[object] {
			return nil
		}
		seen[object] = true
		values := definitions[object]
		for index := len(values) - 1; index >= 0; index-- {
			value := values[index]
			if value.Pos() >= before {
				continue
			}
			if literal := resolveFunctionLiteral(pass, value, definitions, value.Pos(), seen); literal != nil {
				return literal
			}
		}
	}
	return nil
}

func growingContainers(pass *analysis.Pass, body *ast.BlockStmt, loopPos token.Pos) map[expressionRef]bool {
	growing := make(map[expressionRef]bool)
	resets := unconditionalContainerResets(pass, body)
	inspectDirectLoop(body, func(node ast.Node) {
		assignment, ok := node.(*ast.AssignStmt)
		if !ok {
			return
		}
		for index, lhs := range assignment.Lhs {
			switch target := lhs.(type) {
			case *ast.Ident, *ast.SelectorExpr:
				if index >= len(assignment.Rhs) {
					continue
				}
				call, okCall := assignment.Rhs[index].(*ast.CallExpr)
				if !okCall || !isBuiltin(pass, call, "append") || len(call.Args) == 0 {
					continue
				}
				targetRef, okTarget := expressionReference(pass, target)
				sourceRef, okSource := expressionReference(pass, call.Args[0])
				if okTarget && okSource && targetRef == sourceRef && expressionDeclaredBefore(targetRef, loopPos) && !resetDominates(resets, targetRef, assignment.Pos()) {
					growing[targetRef] = true
				}
			case *ast.IndexExpr:
				container, okContainer := expressionReference(pass, target.X)
				if !okContainer || pass.TypesInfo.Types[target.Index].Value != nil {
					continue
				}
				if expressionDeclaredBefore(container, loopPos) && !resetDominates(resets, container, assignment.Pos()) {
					growing[container] = true
				}
			}
		}
	})
	return growing
}

func unconditionalContainerResets(pass *analysis.Pass, body *ast.BlockStmt) map[expressionRef]token.Pos {
	resets := make(map[expressionRef]token.Pos)
	for _, statement := range body.List {
		switch typed := statement.(type) {
		case *ast.AssignStmt:
			if len(typed.Lhs) != len(typed.Rhs) {
				continue
			}
			for index, lhs := range typed.Lhs {
				target, ok := expressionReference(pass, lhs)
				if ok && !resets[target].IsValid() && isContainerReset(pass, typed.Rhs[index], target) {
					resets[target] = typed.Pos()
				}
			}
		case *ast.ExprStmt:
			call, ok := typed.X.(*ast.CallExpr)
			if !ok || !isBuiltin(pass, call, "clear") || len(call.Args) != 1 {
				continue
			}
			if target, okTarget := expressionReference(pass, call.Args[0]); okTarget && !resets[target].IsValid() {
				resets[target] = typed.Pos()
			}
		}
	}
	return resets
}

func resetDominates(resets map[expressionRef]token.Pos, target expressionRef, growth token.Pos) bool {
	position := resets[target]
	return position.IsValid() && position < growth
}

func isContainerReset(pass *analysis.Pass, expression ast.Expr, target expressionRef) bool {
	if identifier, ok := expression.(*ast.Ident); ok && identifier.Name == "nil" {
		return true
	}
	if call, ok := expression.(*ast.CallExpr); ok && isBuiltin(pass, call, "make") {
		return true
	}
	slice, ok := expression.(*ast.SliceExpr)
	if !ok || slice.Low != nil || slice.Max != nil || slice.High == nil {
		return false
	}
	source, ok := expressionReference(pass, slice.X)
	if !ok || source != target {
		return false
	}
	value := pass.TypesInfo.Types[slice.High].Value
	return value != nil && value.Kind() == constant.Int && constant.Sign(value) == 0
}

func isAppendPath(pass *analysis.Pass, call *ast.CallExpr, definitions map[types.Object][]ast.Expr) bool {
	if !strings.HasPrefix(calledFunction(pass, call), "Set") {
		return false
	}
	path := call.Args[1]
	if value := pass.TypesInfo.Types[path].Value; value != nil && value.Kind() == constant.String {
		return appendPath(constant.StringVal(value))
	}
	return expressionContainsAppendMarker(pass, path, definitions, make(map[types.Object]bool))
}

func appendPath(path string) bool {
	return path == "-1" || strings.HasSuffix(path, ".-1")
}

func expressionContainsAppendMarker(pass *analysis.Pass, expression ast.Expr, definitions map[types.Object][]ast.Expr, seen map[types.Object]bool) bool {
	found := false
	ast.Inspect(expression, func(node ast.Node) bool {
		if call, ok := node.(*ast.CallExpr); ok && callProducesAppendMarker(pass, call) {
			found = true
			return false
		}
		identifier, okIdentifier := node.(*ast.Ident)
		if okIdentifier {
			object := objectOf(pass, identifier)
			if len(definitions[object]) > 0 && !seen[object] {
				seen[object] = true
				for _, definition := range definitions[object] {
					found = expressionContainsAppendMarker(pass, definition, definitions, seen)
					if found {
						return false
					}
				}
			}
		}
		literal, ok := node.(*ast.BasicLit)
		if !ok || literal.Kind != token.STRING {
			return true
		}
		value, err := strconv.Unquote(literal.Value)
		if err == nil && (value == "-1" || strings.Contains(value, ".-1")) {
			found = true
			return false
		}
		return true
	})
	return found
}

func callProducesAppendMarker(pass *analysis.Pass, call *ast.CallExpr) bool {
	if isPackageFunction(pass, call, "strconv", "Itoa") && len(call.Args) == 1 {
		return constantMinusOne(pass, call.Args[0])
	}
	if isPackageFunction(pass, call, "strconv", "FormatInt") && len(call.Args) == 2 {
		return constantMinusOne(pass, call.Args[0])
	}
	if isPackageFunction(pass, call, "fmt", "Sprint") && len(call.Args) == 1 {
		return constantMinusOne(pass, call.Args[0])
	}
	if !isPackageFunction(pass, call, "fmt", "Sprintf") || len(call.Args) < 2 {
		return false
	}
	formatValue := pass.TypesInfo.Types[call.Args[0]].Value
	if formatValue == nil || formatValue.Kind() != constant.String {
		return false
	}
	formatString := constant.StringVal(formatValue)
	if !formatEndsInAppendVerb(formatString) || simpleFormatVerbCount(formatString) != len(call.Args)-1 {
		return false
	}
	return constantMinusOne(pass, call.Args[len(call.Args)-1])
}

func constantMinusOne(pass *analysis.Pass, expression ast.Expr) bool {
	value := pass.TypesInfo.Types[expression].Value
	if value == nil {
		return false
	}
	if value.Kind() == constant.String {
		return constant.StringVal(value) == "-1"
	}
	if value.Kind() != constant.Int {
		return false
	}
	return constant.Compare(value, token.EQL, constant.MakeInt64(-1))
}

func formatEndsInAppendVerb(formatString string) bool {
	for _, suffix := range []string{"%d", "%s", "%v"} {
		if formatString == suffix || strings.HasSuffix(formatString, "."+suffix) {
			return true
		}
	}
	return false
}

func simpleFormatVerbCount(formatString string) int {
	count := 0
	for index := 0; index < len(formatString); index++ {
		if formatString[index] != '%' || index+1 >= len(formatString) {
			continue
		}
		if formatString[index+1] == '%' {
			index++
			continue
		}
		count++
	}
	return count
}

func expressionDefinitions(pass *analysis.Pass, file *ast.File) map[types.Object][]ast.Expr {
	definitions := make(map[types.Object][]ast.Expr)
	if file == nil {
		return definitions
	}
	ast.Inspect(file, func(node ast.Node) bool {
		switch declaration := node.(type) {
		case *ast.AssignStmt:
			if len(declaration.Lhs) != len(declaration.Rhs) {
				return true
			}
			for index, lhs := range declaration.Lhs {
				identifier, ok := lhs.(*ast.Ident)
				if !ok {
					continue
				}
				object := objectOf(pass, identifier)
				if object != nil {
					definitions[object] = append(definitions[object], declaration.Rhs[index])
				}
			}
		case *ast.ValueSpec:
			if len(declaration.Names) != len(declaration.Values) {
				return true
			}
			for index, name := range declaration.Names {
				if pass.TypesInfo.Defs[name] != nil {
					object := pass.TypesInfo.Defs[name]
					definitions[object] = append(definitions[object], declaration.Values[index])
				}
			}
		}
		return true
	})
	return definitions
}

func isSJSONMutation(pass *analysis.Pass, call *ast.CallExpr) bool {
	function := calledFunctionObject(pass, call)
	if function == nil || function.Pkg() == nil || function.Pkg().Path() != "github.com/tidwall/sjson" {
		return false
	}
	return strings.HasPrefix(function.Name(), "Set") || strings.HasPrefix(function.Name(), "Delete")
}

func isPackageFunction(pass *analysis.Pass, call *ast.CallExpr, packagePath, name string) bool {
	function := calledFunctionObject(pass, call)
	return function != nil && function.Pkg() != nil && function.Pkg().Path() == packagePath && function.Name() == name
}

func calledFunction(pass *analysis.Pass, call *ast.CallExpr) string {
	if function := calledFunctionObject(pass, call); function != nil {
		return function.Name()
	}
	return ""
}

func calledFunctionObject(pass *analysis.Pass, call *ast.CallExpr) *types.Func {
	var object types.Object
	switch function := call.Fun.(type) {
	case *ast.Ident:
		object = pass.TypesInfo.Uses[function]
	case *ast.SelectorExpr:
		object = pass.TypesInfo.Uses[function.Sel]
	}
	typed, _ := object.(*types.Func)
	return typed
}

func isBuiltin(pass *analysis.Pass, call *ast.CallExpr, name string) bool {
	identifier, ok := call.Fun.(*ast.Ident)
	if !ok || identifier.Name != name {
		return false
	}
	_, ok = pass.TypesInfo.Uses[identifier].(*types.Builtin)
	return ok
}

func objectOf(pass *analysis.Pass, identifier *ast.Ident) types.Object {
	if object := pass.TypesInfo.Uses[identifier]; object != nil {
		return object
	}
	return pass.TypesInfo.Defs[identifier]
}

func fileForPosition(files []*ast.File, position token.Pos) *ast.File {
	for _, file := range files {
		if file.Pos() <= position && position <= file.End() {
			return file
		}
	}
	return nil
}

func validSuppression(pass *analysis.Pass, item finding) bool {
	if item.file == nil {
		return false
	}
	line := pass.Fset.PositionFor(item.node.Pos(), false).Line
	targetFunction := enclosingFunction(pass.Fset, item.file, item.node.Pos())
	if separator := strings.LastIndex(targetFunction, "."); separator >= 0 {
		targetFunction = targetFunction[separator+1:]
	}
	for _, group := range item.file.Comments {
		start := pass.Fset.PositionFor(group.Pos(), false).Line
		end := pass.Fset.PositionFor(group.End(), false).Line
		if end != line-1 && start != line {
			continue
		}
		for _, comment := range group.List {
			match := suppressionPattern.FindStringSubmatch(strings.TrimSpace(comment.Text))
			if len(match) == 3 && benchmarkExists(pass.Fset.PositionFor(item.node.Pos(), false).Filename, item.file.Name.Name, match[1], targetFunction) {
				return true
			}
		}
	}
	return false
}

func benchmarkExists(sourcePath, sourcePackage, benchmark, targetFunction string) bool {
	if targetFunction == "" || targetFunction == "package" {
		return false
	}
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(sourcePath), "*_test.go"))
	if err != nil {
		return false
	}
	for _, match := range matches {
		matched, errMatch := build.Default.MatchFile(filepath.Dir(match), filepath.Base(match))
		if errMatch != nil || !matched {
			continue
		}
		file, errParse := parser.ParseFile(token.NewFileSet(), match, nil, 0)
		if errParse != nil || file.Name.Name != sourcePackage {
			continue
		}
		testingAliases := make(map[string]bool)
		for _, spec := range file.Imports {
			importPath, errUnquote := strconv.Unquote(spec.Path.Value)
			if errUnquote != nil || importPath != "testing" {
				continue
			}
			name := "testing"
			if spec.Name != nil {
				name = spec.Name.Name
			}
			testingAliases[name] = true
		}
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if ok && function.Recv == nil && function.Name.Name == benchmark && isBenchmarkParameter(function, testingAliases) && benchmarkCovers(function, targetFunction) {
				return true
			}
		}
	}
	return false
}

func benchmarkCovers(function *ast.FuncDecl, targetFunction string) bool {
	benchmarkVariable := "b"
	if len(function.Type.Params.List) == 1 && len(function.Type.Params.List[0].Names) == 1 {
		benchmarkVariable = function.Type.Params.List[0].Names[0].Name
	}
	skips := false
	ast.Inspect(function.Body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		if called, okSelector := call.Fun.(*ast.SelectorExpr); okSelector {
			if receiver, okReceiver := called.X.(*ast.Ident); okReceiver && receiver.Name == benchmarkVariable {
				skips = skips || strings.HasPrefix(called.Sel.Name, "Skip")
			}
		}
		return true
	})
	if skips {
		return false
	}
	if functionShadowsName(function, targetFunction) {
		return false
	}
	for _, statement := range function.Body.List {
		if _, exits := statement.(*ast.ReturnStmt); exits {
			return false
		}
		loop, ok := statement.(*ast.ForStmt)
		if !ok || !isBenchmarkLoop(loop, benchmarkVariable) {
			continue
		}
		return benchmarkLoopCovers(loop.Body, targetFunction)
	}
	return false
}

func functionShadowsName(function *ast.FuncDecl, name string) bool {
	shadowed := false
	ast.Inspect(function.Body, func(node ast.Node) bool {
		switch typed := node.(type) {
		case *ast.AssignStmt:
			if typed.Tok != token.DEFINE {
				return true
			}
			for _, lhs := range typed.Lhs {
				identifier, ok := lhs.(*ast.Ident)
				shadowed = shadowed || ok && identifier.Name == name
			}
		case *ast.DeclStmt:
			declaration, ok := typed.Decl.(*ast.GenDecl)
			if !ok {
				return true
			}
			for _, spec := range declaration.Specs {
				value, okValue := spec.(*ast.ValueSpec)
				if !okValue {
					continue
				}
				for _, declared := range value.Names {
					shadowed = shadowed || declared.Name == name
				}
			}
		}
		return !shadowed
	})
	return shadowed
}

func benchmarkLoopCovers(body *ast.BlockStmt, targetFunction string) bool {
	covered := false
	for _, statement := range body.List {
		if benchmarkStatementExits(statement) {
			return false
		}
		switch typed := statement.(type) {
		case *ast.ExprStmt:
			covered = covered || directCallTo(typed.X, targetFunction)
		case *ast.AssignStmt:
			for _, expression := range typed.Rhs {
				covered = covered || directCallTo(expression, targetFunction)
			}
		}
	}
	return covered
}

func benchmarkStatementExits(statement ast.Stmt) bool {
	exits := false
	ast.Inspect(statement, func(node ast.Node) bool {
		if _, nestedFunction := node.(*ast.FuncLit); nestedFunction {
			return false
		}
		switch node.(type) {
		case *ast.BranchStmt, *ast.ReturnStmt:
			exits = true
			return false
		}
		return !exits
	})
	return exits
}

func directCallTo(expression ast.Expr, name string) bool {
	call, ok := expression.(*ast.CallExpr)
	if !ok {
		return false
	}
	identifier, ok := call.Fun.(*ast.Ident)
	return ok && identifier.Name == name
}

func isBenchmarkLoop(loop *ast.ForStmt, benchmarkVariable string) bool {
	if loop.Cond == nil {
		return false
	}
	if call, ok := loop.Cond.(*ast.CallExpr); ok {
		selector, okSelector := call.Fun.(*ast.SelectorExpr)
		if okSelector {
			receiver, okReceiver := selector.X.(*ast.Ident)
			if okReceiver && receiver.Name == benchmarkVariable && selector.Sel.Name == "Loop" && len(call.Args) == 0 {
				return loop.Init == nil && loop.Post == nil
			}
		}
	}
	initialization, ok := loop.Init.(*ast.AssignStmt)
	if !ok || initialization.Tok != token.DEFINE || len(initialization.Lhs) != 1 || len(initialization.Rhs) != 1 {
		return false
	}
	index, ok := initialization.Lhs[0].(*ast.Ident)
	initialValue := passlessConstantInt(initialization.Rhs[0])
	if !ok || initialValue != 0 {
		return false
	}
	condition, ok := loop.Cond.(*ast.BinaryExpr)
	if !ok || condition.Op != token.LSS {
		return false
	}
	conditionIndex, okIndex := condition.X.(*ast.Ident)
	limit, okLimit := condition.Y.(*ast.SelectorExpr)
	if !okIndex || !okLimit || conditionIndex.Name != index.Name || limit.Sel.Name != "N" {
		return false
	}
	receiver, okReceiver := limit.X.(*ast.Ident)
	if !okReceiver || receiver.Name != benchmarkVariable {
		return false
	}
	increment, ok := loop.Post.(*ast.IncDecStmt)
	if !ok {
		return false
	}
	incrementIndex, okIncrement := increment.X.(*ast.Ident)
	return okIncrement && increment.Tok == token.INC && incrementIndex.Name == index.Name
}

func passlessConstantInt(expression ast.Expr) int64 {
	literal, ok := expression.(*ast.BasicLit)
	if !ok || literal.Kind != token.INT {
		return -1
	}
	value, err := strconv.ParseInt(literal.Value, 0, 64)
	if err != nil {
		return -1
	}
	return value
}

func isBenchmarkParameter(function *ast.FuncDecl, testingAliases map[string]bool) bool {
	if function.Type.Params == nil || len(function.Type.Params.List) != 1 {
		return false
	}
	pointer, ok := function.Type.Params.List[0].Type.(*ast.StarExpr)
	if !ok {
		return false
	}
	selector, ok := pointer.X.(*ast.SelectorExpr)
	if !ok || selector.Sel.Name != "B" {
		return false
	}
	identifier, ok := selector.X.(*ast.Ident)
	return ok && testingAliases[identifier.Name]
}

func repositoryRoot(pass *analysis.Pass) (string, error) {
	if directory, err := os.Getwd(); err == nil {
		if root := findModuleRoot(directory); root != "" {
			return root, nil
		}
	}
	for _, file := range pass.Files {
		directory := filepath.Dir(pass.Fset.PositionFor(file.Pos(), false).Filename)
		if root := findModuleRoot(directory); root != "" {
			return root, nil
		}
	}
	return "", fmt.Errorf("payloadgrowth: repository root not found for %s", pass.Pkg.Path())
}

func findModuleRoot(directory string) string {
	for {
		if _, err := os.Stat(filepath.Join(directory, "go.mod")); err == nil {
			return directory
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			return ""
		}
		directory = parent
	}
}

func readBaseline(root, path string) (map[baselineKey]int, error) {
	baseline := make(map[baselineKey]int)
	if strings.TrimSpace(path) == "" {
		return baseline, nil
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("payloadgrowth: open baseline: %w", err)
	}

	scanner := bufio.NewScanner(strings.NewReader(string(contents)))
	for line := 1; scanner.Scan(); line++ {
		text := strings.TrimSpace(scanner.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		fields := strings.Fields(text)
		if len(fields) != 4 {
			return nil, fmt.Errorf("payloadgrowth: invalid baseline line %d", line)
		}
		count, errCount := strconv.Atoi(fields[3])
		if errCount != nil || count < 1 {
			return nil, fmt.Errorf("payloadgrowth: invalid baseline count on line %d", line)
		}
		path := filepath.ToSlash(fields[1])
		if _, errStat := os.Stat(filepath.Join(root, filepath.FromSlash(path))); errStat != nil {
			return nil, fmt.Errorf("payloadgrowth: baseline path on line %d: %w", line, errStat)
		}
		baseline[baselineKey{rule: fields[0], path: path, fingerprint: fields[2]}] = count
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("payloadgrowth: read baseline: %w", err)
	}
	return baseline, nil
}
