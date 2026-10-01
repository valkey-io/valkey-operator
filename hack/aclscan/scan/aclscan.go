/*
Copyright 2025 Valkey Contributors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package aclscan statically discovers the Valkey commands the operator
// issues, by scanning its source for valkey-go client calls. It exists so
// the operator's own ACL for its "_operator" system user can be checked
// against what the code actually does, rather than a hand-maintained list
// that can silently drift as commands are added.
//
// Two call shapes are recognized:
//   - the valkey-go builder pattern, e.g. `client.B().ClusterInfo()...Build()`
//   - raw `client.B().Arbitrary("CLUSTER", "GETSLOTMIGRATIONS")` calls
//
// Builder method names are resolved to command tokens by parsing valkey-go's
// own generated command builders on disk, so the mapping tracks whichever
// valkey-go version the operator is built against instead of being
// hand-maintained here.
package aclscan

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

const (
	operatorModule = "github.com/valkey-io/valkey-operator"
	valkeyGoModule = "github.com/valkey-io/valkey-go"
)

// operatorScanDirs are the source directories, relative to the operator
// module root, that make up the operator's reconciliation code and are
// therefore in scope for command discovery.
var operatorScanDirs = []string{"cmd", "internal"}

// Command is a single Valkey command/subcommand, e.g. []string{"CLUSTER", "INFO"},
// together with the source location it was discovered at.
type Command struct {
	Tokens []string
	Pos    string
}

func (c Command) String() string {
	return strings.Join(c.Tokens, " ")
}

// OperatorCommands returns the set of Valkey commands issued by the
// operator's own reconciliation code (cmd/ and internal/), discovered via
// static analysis rather than a hand-maintained list.
func OperatorCommands() ([]Command, error) {
	operatorDir, err := moduleDir(operatorModule)
	if err != nil {
		return nil, fmt.Errorf("locating %s module: %w", operatorModule, err)
	}
	valkeyGoDir, err := moduleDir(valkeyGoModule)
	if err != nil {
		return nil, fmt.Errorf("locating %s module: %w", valkeyGoModule, err)
	}

	builderTokens, err := builderCommandTokens(filepath.Join(valkeyGoDir, "internal", "cmds"))
	if err != nil {
		return nil, fmt.Errorf("parsing valkey-go command builders: %w", err)
	}

	var commands []Command
	for _, dir := range operatorScanDirs {
		found, err := scanDir(filepath.Join(operatorDir, dir), builderTokens)
		if err != nil {
			return nil, fmt.Errorf("scanning %s: %w", dir, err)
		}
		commands = append(commands, found...)
	}

	return dedupe(commands), nil
}

// moduleDir resolves the on-disk directory of a Go module via the local
// module graph/cache, so results always match the version actually in use.
func moduleDir(modulePath string) (string, error) {
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", modulePath).Output()
	if err != nil {
		return "", fmt.Errorf("go list -m %s: %w", modulePath, err)
	}
	dir := strings.TrimSpace(string(out))
	if dir == "" {
		return "", fmt.Errorf("module %s resolved to no directory", modulePath)
	}
	return dir, nil
}

// builderCommandTokens parses valkey-go's command builder source and returns
// a map of Builder entry-point method name (e.g. "ClusterSetConfigEpoch") to
// the literal command tokens it sends (e.g. []string{"CLUSTER", "SET-CONFIG-EPOCH"}).
func builderCommandTokens(cmdsDir string) (map[string][]string, error) {
	entries, err := os.ReadDir(cmdsDir)
	if err != nil {
		return nil, err
	}

	tokens := make(map[string][]string)
	fset := token.NewFileSet()
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(cmdsDir, name), nil, 0)
		if err != nil {
			return nil, err
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || !isBuilderEntryPoint(fn) {
				continue
			}
			if cmd := firstAppendLiterals(fn.Body); cmd != nil {
				tokens[fn.Name.Name] = cmd
			}
		}
	}
	return tokens, nil
}

// isBuilderEntryPoint reports whether fn is a method on valkey-go's
// `Builder` type, i.e. the entry point of a command chain reachable as
// `client.B().Xxx()`. Methods on the various `Incomplete`-derived chain
// types (e.g. the return of ClusterInfo) are intentionally excluded: they
// continue a command already identified by its entry point.
func isBuilderEntryPoint(fn *ast.FuncDecl) bool {
	if fn.Recv == nil || len(fn.Recv.List) != 1 {
		return false
	}
	ident, ok := fn.Recv.List[0].Type.(*ast.Ident)
	return ok && ident.Name == "Builder"
}

// firstAppendLiterals finds the first `x.cs.s = append(x.cs.s, "A", "B", ...)`
// statement in body and returns the literal string tokens being appended, or
// nil if the command's tokens aren't static literals (e.g. Arbitrary, which
// forwards a caller-supplied slice).
func firstAppendLiterals(body *ast.BlockStmt) []string {
	if body == nil {
		return nil
	}
	for _, stmt := range body.List {
		assign, ok := stmt.(*ast.AssignStmt)
		if !ok || len(assign.Rhs) != 1 {
			continue
		}
		call, ok := assign.Rhs[0].(*ast.CallExpr)
		if !ok {
			continue
		}
		fun, ok := call.Fun.(*ast.Ident)
		if !ok || fun.Name != "append" || len(call.Args) < 2 {
			continue
		}
		if tokens := stringLiteralArgs(call.Args[1:]); tokens != nil {
			return tokens
		}
	}
	return nil
}

// scanDir walks dir for non-test Go source, looking for `x.B().Method(...)`
// builder-pattern calls and `x.B().Arbitrary("A", "B", ...)` calls, resolving
// them to Valkey command tokens via builderTokens (see scanFunc). A bare
// `y.Method(...)` call is only treated as a command when y is itself an
// inline `x.B()` call; other receivers (including a variable previously
// assigned `x.B()`) are not builder calls and are either ignored or, for the
// variable case, reported as an error so the gap is visible rather than
// silently under-reporting commands. Every function body — a top-level
// *ast.FuncDecl or a *ast.FuncLit wherever one appears, e.g. a package-level
// `var handler = func() {...}` — is scanned independently via scanFunc, so
// builder-variable tracking can't leak from one function's scope into an
// unrelated one that happens to reuse the same variable name.
func scanDir(dir string, builderTokens map[string][]string) ([]Command, error) {
	var commands []Command
	fset := token.NewFileSet()
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		var scanErr error
		ast.Inspect(file, func(n ast.Node) bool {
			var body *ast.BlockStmt
			switch fn := n.(type) {
			case *ast.FuncDecl:
				body = fn.Body
			case *ast.FuncLit:
				body = fn.Body
			default:
				return true
			}
			if body == nil {
				return true
			}
			found, err := scanFunc(body, fset, builderTokens)
			if err != nil {
				scanErr = err
				return false
			}
			commands = append(commands, found...)
			// scanFunc's own ast.Inspect already covers this function's
			// entire body, including any nested func literals, so don't
			// descend into it again from here.
			return false
		})
		return scanErr
	})
	if err != nil {
		return nil, err
	}
	return commands, nil
}

// scanFunc scans a single function body for builder-pattern and Arbitrary
// calls, resolving them to Valkey command tokens via builderTokens.
// builderVars, which tracks local variables assigned a bare `x.B()` result
// (whether via `b := client.B()` or `var b = client.B()`), is scoped to this
// one function body: see scanDir's doc comment for why. A nested func
// literal shares its enclosing function's builderVars rather than getting
// its own (scanDir only calls scanFunc at the outermost function it finds),
// which is a conservative approximation of real lexical scoping but not one
// that matters in practice for this codebase's style.
func scanFunc(body *ast.BlockStmt, fset *token.FileSet, builderTokens map[string][]string) ([]Command, error) {
	var commands []Command
	var scanErr error
	builderVars := make(map[string]bool)
	ast.Inspect(body, func(n ast.Node) bool {
		if assign, ok := n.(*ast.AssignStmt); ok && len(assign.Lhs) == 1 && len(assign.Rhs) == 1 {
			if ident, ok := assign.Lhs[0].(*ast.Ident); ok {
				recordBuilderVar(ident.Name, assign.Rhs[0], builderVars)
			}
		}
		if spec, ok := n.(*ast.ValueSpec); ok && len(spec.Names) == 1 && len(spec.Values) == 1 {
			recordBuilderVar(spec.Names[0].Name, spec.Values[0], builderVars)
		}
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		pos := fset.Position(call.Pos()).String()
		switch {
		case isBuilderCall(sel.X):
			if sel.Sel.Name == "Arbitrary" {
				tok := leadingStringLiterals(call.Args)
				if len(tok) == 0 {
					scanErr = fmt.Errorf("%s: Arbitrary call whose command is not a string literal; aclscan cannot resolve it", pos)
					return false
				}
				commands = append(commands, Command{Tokens: tok, Pos: pos})
				return true
			}
			if tok, ok := builderTokens[sel.Sel.Name]; ok {
				commands = append(commands, Command{Tokens: tok, Pos: pos})
			}
		case isBuilderVarRef(sel.X, builderVars):
			scanErr = fmt.Errorf(
				"%s: builder call made through a variable (e.g. `b := client.B(); b.%s(...)`); "+
					"aclscan only recognizes the inline client.B().Method() form", pos, sel.Sel.Name)
			return false
		}
		return true
	})
	return commands, scanErr
}

// isBuilderCall reports whether expr is a call to a niladic `B()` method,
// i.e. the receiver of the call being inspected is `x.B()`.
func isBuilderCall(expr ast.Expr) bool {
	call, ok := expr.(*ast.CallExpr)
	if !ok || len(call.Args) != 0 {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	return ok && sel.Sel.Name == "B"
}

// isBuilderVarRef reports whether expr is a reference to a local variable
// previously assigned a bare `x.B()` result, per builderVars.
func isBuilderVarRef(expr ast.Expr, builderVars map[string]bool) bool {
	ident, ok := expr.(*ast.Ident)
	return ok && builderVars[ident.Name]
}

// recordBuilderVar marks name in builderVars if value is a bare `x.B()`
// call, e.g. the right-hand side of `b := client.B()` or `var b = client.B()`.
func recordBuilderVar(name string, value ast.Expr, builderVars map[string]bool) {
	if isBuilderCall(value) {
		builderVars[name] = true
	}
}

// stringLiteralArgs returns the unquoted values of args if every one of them
// is a string literal, or nil otherwise (e.g. a variable or spread argument).
func stringLiteralArgs(args []ast.Expr) []string {
	if len(args) == 0 {
		return nil
	}
	tokens := make([]string, 0, len(args))
	for _, arg := range args {
		lit, ok := arg.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return nil
		}
		value, err := strconv.Unquote(lit.Value)
		if err != nil {
			return nil
		}
		tokens = append(tokens, value)
	}
	return tokens
}

// leadingStringLiterals returns the leading string-literal arguments of
// args, stopping at the first non-literal (e.g. a slot number formatted via
// strconv.Itoa) and capped at two, since an ACL only ever needs to know the
// command and subcommand, not the arguments that follow.
func leadingStringLiterals(args []ast.Expr) []string {
	n := min(len(args), 2)
	tokens := make([]string, 0, n)
	for _, arg := range args[:n] {
		lit, ok := arg.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			break
		}
		value, err := strconv.Unquote(lit.Value)
		if err != nil {
			break
		}
		tokens = append(tokens, value)
	}
	return tokens
}

func dedupe(commands []Command) []Command {
	seen := make(map[string]Command, len(commands))
	for _, c := range commands {
		key := strings.Join(c.Tokens, " ")
		if _, ok := seen[key]; !ok {
			seen[key] = c
		}
	}
	result := make([]Command, 0, len(seen))
	for _, c := range seen {
		result = append(result, c)
	}
	slices.SortFunc(result, func(a, b Command) int {
		return strings.Compare(a.String(), b.String())
	})
	return result
}
