// Command rulecheck reports code that breaks the Go rules in AGENTS.md that
// gofmt, go vet and golangci-lint do not cover. It exits 1 when it finds any.
//
//	go run ./tools/rulecheck ./...
package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// maxNesting is the deepest allowed nesting of control statements in a function.
const maxNesting = 3

// ticketRef matches ticket, PR or review-finding numbers, which belong in commits, not code.
var ticketRef = regexp.MustCompile(`(?i)(finding|issue|ticket|pr)\s*#?\d+|#\d{2,}`)

type finding struct {
	pos  token.Position
	rule string
	msg  string
}

type checker struct {
	fset     *token.FileSet
	findings []finding
	envOK    bool // the current file may read the environment
}

func main() {
	root := "."
	if len(os.Args) > 1 {
		root = strings.TrimSuffix(os.Args[1], "/...")
	}
	c := &checker{fset: token.NewFileSet()}
	if err := filepath.WalkDir(root, c.visitPath); err != nil {
		fmt.Fprintln(os.Stderr, "rulecheck:", err)
		os.Exit(2)
	}
	sort.Slice(c.findings, func(i, j int) bool {
		a, b := c.findings[i].pos, c.findings[j].pos
		if a.Filename != b.Filename {
			return a.Filename < b.Filename
		}
		return a.Line < b.Line
	})
	for _, f := range c.findings {
		fmt.Printf("%s:%d: [%s] %s\n", f.pos.Filename, f.pos.Line, f.rule, f.msg)
	}
	if len(c.findings) > 0 {
		fmt.Printf("\n%d finding(s)\n", len(c.findings))
		os.Exit(1)
	}
}

func (c *checker) visitPath(path string, d fs.DirEntry, err error) error {
	if err != nil {
		return err
	}
	if d.IsDir() {
		name := d.Name()
		if path != "." && (strings.HasPrefix(name, ".") || name == "testdata" || name == "node_modules") {
			return filepath.SkipDir
		}
		return nil
	}
	if !strings.HasSuffix(path, ".go") {
		return nil
	}
	file, err := parser.ParseFile(c.fset, path, nil, parser.ParseComments)
	if err != nil {
		return err
	}
	c.checkFile(path, file)
	return nil
}

func (c *checker) report(n ast.Node, rule, format string, args ...any) {
	c.findings = append(c.findings, finding{pos: c.fset.Position(n.Pos()), rule: rule, msg: fmt.Sprintf(format, args...)})
}

func (c *checker) checkFile(path string, file *ast.File) {
	isTest := strings.HasSuffix(path, "_test.go")
	isMain := file.Name.Name == "main"
	// export_test.go is the sanctioned way to hand unexported names to black-box tests.
	isExport := filepath.Base(path) == "export_test.go"
	c.envOK = isMain
	if isTest && !isExport && !strings.HasSuffix(file.Name.Name, "_test") && !isMain {
		c.report(file.Name, "black-box-tests", "test file uses package %s; use package %s_test", file.Name.Name, file.Name.Name)
	}
	for _, cg := range file.Comments {
		if ticketRef.MatchString(cg.Text()) {
			c.report(cg, "no-ticket-refs", "comment refers to a ticket or finding number: %q", strings.TrimSpace(ticketRef.FindString(cg.Text())))
		}
	}
	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.GenDecl:
			c.checkGenDecl(d, isTest)
		case *ast.FuncDecl:
			c.checkFunc(d, isMain, isTest)
		}
	}
}

func (c *checker) checkGenDecl(d *ast.GenDecl, isTest bool) {
	if d.Tok != token.VAR || isTest {
		return
	}
	for _, spec := range d.Specs {
		vs, ok := spec.(*ast.ValueSpec)
		if !ok || allowedPackageVar(d, vs) {
			continue
		}
		for _, name := range vs.Names {
			c.report(name, "no-package-state", "package-level var %s; pass it in, or make it a const, sentinel error or compiled regexp", name.Name)
		}
	}
}

// allowedPackageVar reports whether a package-level var is one of the allowed
// kinds: blank interface checks, sentinel errors, compiled regexps and
// templates, //go:embed files, and read-only tables marked //rules:readonly
// (with a reason) because Go has no const for them.
func allowedPackageVar(d *ast.GenDecl, vs *ast.ValueSpec) bool {
	if len(vs.Names) == 1 && vs.Names[0].Name == "_" {
		return true
	}
	if hasDirective(d.Doc, "//go:embed") || hasDirective(vs.Doc, "//go:embed") ||
		hasDirective(d.Doc, "//rules:readonly") || hasDirective(vs.Doc, "//rules:readonly") {
		return true
	}
	for _, v := range vs.Values {
		call, ok := v.(*ast.CallExpr)
		if !ok {
			return false
		}
		fn := callName(call)
		if fn != "errors.New" && fn != "fmt.Errorf" && fn != "regexp.MustCompile" && fn != "template.Must" {
			return false
		}
	}
	return len(vs.Values) > 0
}

// hasDirective reports whether a comment group has a line starting with the
// directive. (CommentGroup.Text drops directive lines, so read the raw list.)
func hasDirective(cg *ast.CommentGroup, directive string) bool {
	if cg == nil {
		return false
	}
	for _, c := range cg.List {
		if strings.HasPrefix(c.Text, directive) {
			return true
		}
	}
	return false
}

func callName(call *ast.CallExpr) string {
	switch f := call.Fun.(type) {
	case *ast.SelectorExpr:
		if x, ok := f.X.(*ast.Ident); ok {
			return x.Name + "." + f.Sel.Name
		}
	case *ast.Ident:
		return f.Name
	}
	return ""
}

func (c *checker) checkFunc(fn *ast.FuncDecl, isMain, isTest bool) {
	if fn.Name.Name == "init" && fn.Recv == nil {
		c.report(fn, "no-init", "init() wiring; do it in a constructor or main")
	}
	if fn.Recv != nil && len(fn.Recv.List) > 0 && len(fn.Recv.List[0].Names) > 0 {
		if r := fn.Recv.List[0].Names[0].Name; r == "this" || r == "self" {
			c.report(fn.Recv, "receiver-name", "receiver named %s", r)
		}
	}
	if fn.Body == nil {
		return
	}
	isConstructor := strings.HasPrefix(fn.Name.Name, "New")
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		c.checkNode(n, fn, isMain, isTest, isConstructor)
		return true
	})
	c.checkNesting(fn.Body.List, 0, fn.Name.Name)
}

func (c *checker) checkNode(n ast.Node, fn *ast.FuncDecl, isMain, isTest, isConstructor bool) {
	switch x := n.(type) {
	case *ast.IfStmt:
		if x.Else != nil && endsInTerminal(x.Body) {
			c.report(x.Else, "no-else-after-return", "else after a block that returns; drop the else")
		}
	case *ast.RangeStmt:
		if _, ok := x.X.(*ast.CompositeLit); ok {
			c.report(x, "named-range-cases", "range over an inline composite literal; declare a named type and a variable first")
		}
	case *ast.BranchStmt:
		if x.Tok == token.GOTO {
			c.report(x, "no-goto", "goto")
		}
	case *ast.LabeledStmt:
		c.report(x, "no-labels", "label %s", x.Label.Name)
	case *ast.GoStmt:
		if isConstructor {
			c.report(x, "no-go-in-constructor", "%s starts a goroutine; expose Run(ctx) instead", fn.Name.Name)
		}
	case *ast.CallExpr:
		c.checkCall(x, isMain, isTest)
	}
}

func (c *checker) checkCall(call *ast.CallExpr, isMain, isTest bool) {
	switch name := callName(call); name {
	case "context.Background", "context.TODO":
		if !isMain && !isTest {
			c.report(call, "ctx-from-caller", "%s outside main; take ctx from the caller", name)
		}
	case "os.Getenv", "os.LookupEnv":
		if !c.envOK && !isTest {
			c.report(call, "env-in-config-only", "%s outside main/config", name)
		}
	case "time.Now":
		if !isTest && !isMain {
			c.report(call, "injected-clock", "time.Now(); take the time from an injected clock")
		}
	case "slog.Default", "log.Printf", "log.Println", "log.Print", "log.Fatal", "log.Fatalf":
		if !isTest && !isMain {
			c.report(call, "injected-logger", "%s; use an injected *slog.Logger", name)
		}
	}
}

// endsInTerminal reports whether a block always ends in return, panic, break, continue or os.Exit.
func endsInTerminal(b *ast.BlockStmt) bool {
	if b == nil || len(b.List) == 0 {
		return false
	}
	switch s := b.List[len(b.List)-1].(type) {
	case *ast.ReturnStmt:
		return true
	case *ast.BranchStmt:
		return s.Tok == token.BREAK || s.Tok == token.CONTINUE
	case *ast.ExprStmt:
		if call, ok := s.X.(*ast.CallExpr); ok {
			n := callName(call)
			return n == "panic" || n == "os.Exit" || n == "log.Fatal"
		}
	}
	return false
}

func (c *checker) checkNesting(stmts []ast.Stmt, depth int, fn string) {
	for _, s := range stmts {
		c.checkStmtNesting(s, depth, fn)
	}
}

func (c *checker) checkStmtNesting(s ast.Stmt, depth int, fn string) {
	inner := children(s)
	if inner == nil {
		return
	}
	if depth+1 > maxNesting {
		c.report(s, "max-nesting", "%s nests control statements %d deep (max %d); return early or extract a function", fn, depth+1, maxNesting)
		return
	}
	for _, block := range inner {
		c.checkNesting(block, depth+1, fn)
	}
}

// children returns the statement lists nested inside a control statement, or nil.
func children(s ast.Stmt) [][]ast.Stmt {
	switch x := s.(type) {
	case *ast.IfStmt:
		out := [][]ast.Stmt{x.Body.List}
		switch e := x.Else.(type) {
		case *ast.BlockStmt:
			out = append(out, e.List)
		case *ast.IfStmt:
			out = append(out, []ast.Stmt{e})
		}
		return out
	case *ast.ForStmt:
		return [][]ast.Stmt{x.Body.List}
	case *ast.RangeStmt:
		return [][]ast.Stmt{x.Body.List}
	case *ast.SwitchStmt:
		return clauses(x.Body)
	case *ast.TypeSwitchStmt:
		return clauses(x.Body)
	case *ast.SelectStmt:
		return clauses(x.Body)
	}
	return nil
}

func clauses(b *ast.BlockStmt) [][]ast.Stmt {
	var out [][]ast.Stmt
	for _, s := range b.List {
		switch cl := s.(type) {
		case *ast.CaseClause:
			out = append(out, cl.Body)
		case *ast.CommClause:
			out = append(out, cl.Body)
		}
	}
	return out
}
