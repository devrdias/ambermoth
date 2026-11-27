// Command crap computes the CRAP score of every function from a Go coverage
// profile and exits 1 if any function scores at or above the limit.
//
//	go test -coverprofile=cover.out ./... && go run ./tools/crap -profile cover.out
//
// CRAP = complexity² × (1 − coverage)³ + complexity, where complexity is the
// cyclomatic complexity (1 + branches and boolean operators) and coverage is
// the share of the function's statements that the tests ran.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

type block struct {
	startLine, endLine int
	stmts              int
	covered            bool
}

type funcScore struct {
	file       string
	line       int
	name       string
	complexity int
	coverage   float64
	crap       float64
}

func main() {
	profile := flag.String("profile", "cover.out", "coverage profile from go test -coverprofile")
	limit := flag.Float64("limit", 10, "fail on any function with CRAP at or above this")
	top := flag.Int("top", 15, "how many of the highest scores to print")
	flag.Parse()

	module, err := modulePath("go.mod")
	if err != nil {
		fail(err)
	}
	blocks, err := readProfile(*profile, module)
	if err != nil {
		fail(err)
	}
	scores, err := scoreFiles(blocks)
	if err != nil {
		fail(err)
	}
	sort.Slice(scores, func(i, j int) bool { return scores[i].crap > scores[j].crap })
	over := 0
	for i, s := range scores {
		if s.crap >= *limit {
			over++
		}
		if i < *top || s.crap >= *limit {
			fmt.Printf("%7.1f  cc=%-3d cov=%5.1f%%  %s:%d %s\n", s.crap, s.complexity, 100*s.coverage, s.file, s.line, s.name)
		}
	}
	fmt.Printf("\n%d functions, %d at or above CRAP %.0f\n", len(scores), over, *limit)
	if over > 0 {
		os.Exit(1)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "crap:", err)
	os.Exit(2)
}

func modulePath(gomod string) (string, error) {
	b, err := os.ReadFile(gomod)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "module ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "module ")), nil
		}
	}
	return "", fmt.Errorf("no module line in %s", gomod)
}

// readProfile returns coverage blocks per local file path. A block counts as
// covered if any test run executed it.
func readProfile(path, module string) (map[string][]block, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	out := map[string][]block{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "mode:") || line == "" {
			continue
		}
		file, b, err := parseProfileLine(line, module)
		if err != nil {
			return nil, err
		}
		out[file] = append(out[file], b)
	}
	return out, sc.Err()
}

// parseProfileLine parses "mod/pkg/file.go:12.3,15.4 2 1".
func parseProfileLine(line, module string) (string, block, error) {
	colon := strings.LastIndex(line, ":")
	fields := strings.Fields(line[colon+1:])
	if colon < 0 || len(fields) != 3 {
		return "", block{}, fmt.Errorf("bad profile line %q", line)
	}
	file := strings.TrimPrefix(strings.TrimPrefix(line[:colon], module), "/")
	span := strings.Split(fields[0], ",")
	start, _ := strconv.Atoi(strings.Split(span[0], ".")[0])
	end, _ := strconv.Atoi(strings.Split(span[1], ".")[0])
	stmts, _ := strconv.Atoi(fields[1])
	count, _ := strconv.Atoi(fields[2])
	return file, block{startLine: start, endLine: end, stmts: stmts, covered: count > 0}, nil
}

func scoreFiles(blocks map[string][]block) ([]funcScore, error) {
	merged := map[string][]block{}
	for file, bs := range blocks {
		merged[filepath.Clean(file)] = mergeBlocks(bs)
	}
	var out []funcScore
	fset := token.NewFileSet()
	for file, bs := range merged {
		af, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			return nil, err
		}
		for _, d := range af.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			out = append(out, score(fset, file, fn, bs))
		}
	}
	return out, nil
}

// mergeBlocks collapses repeated blocks (one per test binary) into one, covered if any run covered it.
func mergeBlocks(bs []block) []block {
	type key struct{ s, e, n int }
	seen := map[key]int{}
	var out []block
	for _, b := range bs {
		k := key{b.startLine, b.endLine, b.stmts}
		if i, ok := seen[k]; ok {
			out[i].covered = out[i].covered || b.covered
			continue
		}
		seen[k] = len(out)
		out = append(out, b)
	}
	return out
}

func score(fset *token.FileSet, file string, fn *ast.FuncDecl, bs []block) funcScore {
	start, end := fset.Position(fn.Pos()).Line, fset.Position(fn.End()).Line
	total, covered := 0, 0
	for _, b := range bs {
		if b.startLine >= start && b.endLine <= end {
			total += b.stmts
			if b.covered {
				covered += b.stmts
			}
		}
	}
	cov := 1.0
	if total > 0 {
		cov = float64(covered) / float64(total)
	}
	cc := complexity(fn)
	c := float64(cc)
	return funcScore{file: file, line: start, name: funcName(fn), complexity: cc, coverage: cov,
		crap: c*c*(1-cov)*(1-cov)*(1-cov) + c}
}

func funcName(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return fn.Name.Name
	}
	t := fn.Recv.List[0].Type
	if s, ok := t.(*ast.StarExpr); ok {
		t = s.X
	}
	if id, ok := t.(*ast.Ident); ok {
		return id.Name + "." + fn.Name.Name
	}
	return fn.Name.Name
}

// complexity is 1 plus one per if, for, range, non-default case, && and ||.
func complexity(fn *ast.FuncDecl) int {
	n := 1
	ast.Inspect(fn.Body, func(node ast.Node) bool {
		switch x := node.(type) {
		case *ast.IfStmt, *ast.ForStmt, *ast.RangeStmt:
			n++
		case *ast.CaseClause:
			if x.List != nil {
				n++
			}
		case *ast.CommClause:
			if x.Comm != nil {
				n++
			}
		case *ast.BinaryExpr:
			if x.Op == token.LAND || x.Op == token.LOR {
				n++
			}
		case *ast.FuncLit:
			return true
		}
		return true
	})
	return n
}
