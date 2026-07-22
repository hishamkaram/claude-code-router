package main

import (
	"bytes"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	goProductionTarget = 600
	goProductionHard   = 800
	goTestTarget       = 700
	goTestHard         = 1000
	markdownTarget     = 1200
	markdownHard       = 1800

	functionTarget             = 40
	functionHard               = 80
	orchestratorFunctionTarget = 80
	orchestratorFunctionHard   = 120
)

type findingLevel string

const (
	levelHard findingLevel = "HARD"
	levelWarn findingLevel = "WARN"
)

type finding struct {
	level  findingLevel
	kind   string
	path   string
	name   string
	lines  int
	target int
	hard   int
}

func main() {
	root := flag.String("root", ".", "repository root to scan")
	strict := flag.Bool("strict", false, "fail on target warnings as well as hard failures")
	flag.Parse()

	findings, err := scanRepository(*root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "maintainability: scan failed: %v\n", err)
		os.Exit(1)
	}

	sortFindings(findings)
	hardCount, warnCount := 0, 0
	for _, item := range findings {
		if item.level == levelHard {
			hardCount++
		} else {
			warnCount++
		}
		name := item.name
		if name != "" {
			name = " " + name
		}
		fmt.Printf(
			"%s %s lines=%d target=%d hard=%d %s%s\n",
			item.level,
			item.kind,
			item.lines,
			item.target,
			item.hard,
			item.path,
			name,
		)
	}

	switch {
	case hardCount > 0:
		fmt.Fprintf(os.Stderr, "maintainability: failed with %d hard violation(s) and %d target warning(s)\n", hardCount, warnCount)
		os.Exit(1)
	case *strict && warnCount > 0:
		fmt.Fprintf(os.Stderr, "maintainability: strict mode failed with %d target warning(s)\n", warnCount)
		os.Exit(1)
	case warnCount > 0:
		fmt.Printf("maintainability: passed with %d target warning(s); hard violations=0\n", warnCount)
	default:
		fmt.Println("maintainability: passed; no target warnings or hard violations")
	}
}

func scanRepository(root string) ([]finding, error) {
	root = filepath.Clean(root)
	var findings []finding
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if shouldSkipDir(entry.Name()) {
				return filepath.SkipDir
			}
			return nil
		}

		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)

		switch {
		case strings.HasSuffix(path, ".go"):
			fileFindings, err := scanGoFile(fset, path, rel)
			if err != nil {
				return err
			}
			findings = append(findings, fileFindings...)
		case strings.HasSuffix(path, ".md"):
			fileFinding, err := scanTextFile(path, rel, "Markdown doc", markdownTarget, markdownHard)
			if err != nil {
				return err
			}
			findings = appendFinding(findings, fileFinding)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return findings, nil
}

func shouldSkipDir(name string) bool {
	switch name {
	case ".git", ".cache", ".claude", "bin", "dist", "graphify-out", "node_modules", "vendor":
		return true
	default:
		return false
	}
}

func scanGoFile(fset *token.FileSet, path, rel string) ([]finding, error) {
	target, hard, kind := goProductionTarget, goProductionHard, "Go production file"
	if strings.HasSuffix(path, "_test.go") {
		target, hard, kind = goTestTarget, goTestHard, "Go test file"
	}

	fileFinding, err := scanTextFile(path, rel, kind, target, hard)
	if err != nil {
		return nil, err
	}

	parsed, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", rel, err)
	}

	findings := make([]finding, 0, 1+len(parsed.Decls))
	findings = appendFinding(findings, fileFinding)
	for _, decl := range parsed.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		start := fset.Position(fn.Pos()).Line
		end := fset.Position(fn.End()).Line
		lines := end - start + 1
		fnTarget, fnHard, fnKind := functionTarget, functionHard, "Function"
		if isOrchestratorFunction(fn.Name.Name) {
			fnTarget, fnHard, fnKind = orchestratorFunctionTarget, orchestratorFunctionHard, "Orchestrator function"
		}
		findings = appendFinding(findings, classifyFinding(fnKind, rel, functionName(fn), lines, fnTarget, fnHard))
	}
	return findings, nil
}

func scanTextFile(path, rel, kind string, target, hard int) (finding, error) {
	file, err := os.Open(path)
	if err != nil {
		return finding{}, fmt.Errorf("open %s: %w", rel, err)
	}
	defer func() {
		_ = file.Close()
	}()

	lines, err := countLines(file)
	if err != nil {
		return finding{}, fmt.Errorf("count lines in %s: %w", rel, err)
	}
	return classifyFinding(kind, rel, "", lines, target, hard), nil
}

func countLines(reader io.Reader) (int, error) {
	buffer := make([]byte, 32*1024)
	lines := 0
	sawBytes := false
	lastByte := byte('\n')
	for {
		n, err := reader.Read(buffer)
		if n > 0 {
			chunk := buffer[:n]
			sawBytes = true
			lines += bytes.Count(chunk, []byte{'\n'})
			lastByte = chunk[n-1]
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return 0, err
		}
	}
	if sawBytes && lastByte != '\n' {
		lines++
	}
	return lines, nil
}

func classifyFinding(kind, path, name string, lines, target, hard int) finding {
	switch {
	case lines > hard:
		return finding{level: levelHard, kind: kind, path: path, name: name, lines: lines, target: target, hard: hard}
	case lines > target:
		return finding{level: levelWarn, kind: kind, path: path, name: name, lines: lines, target: target, hard: hard}
	default:
		return finding{}
	}
}

func appendFinding(findings []finding, item finding) []finding {
	if item.level == "" {
		return findings
	}
	return append(findings, item)
}

func functionName(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return fn.Name.Name
	}
	receiver := fn.Recv.List[0].Type
	return receiverName(receiver) + "." + fn.Name.Name
}

func receiverName(expr ast.Expr) string {
	switch typed := expr.(type) {
	case *ast.Ident:
		return typed.Name
	case *ast.StarExpr:
		return receiverName(typed.X)
	case *ast.IndexExpr:
		return receiverName(typed.X)
	case *ast.IndexListExpr:
		return receiverName(typed.X)
	case *ast.SelectorExpr:
		return typed.Sel.Name
	default:
		return "receiver"
	}
}

func isOrchestratorFunction(name string) bool {
	lower := strings.ToLower(name)
	for _, prefix := range []string{
		"build",
		"conformance",
		"discover",
		"doctor",
		"execute",
		"handle",
		"import",
		"launch",
		"manage",
		"migrate",
		"new",
		"open",
		"prompt",
		"proxy",
		"refresh",
		"route",
		"run",
		"serve",
		"start",
		"stream",
		"sync",
		"test",
		"watch",
	} {
		if strings.HasPrefix(lower, prefix) {
			return true
		}
	}
	return false
}

func sortFindings(findings []finding) {
	sort.Slice(findings, func(i, j int) bool {
		if findings[i].level != findings[j].level {
			return findings[i].level == levelHard
		}
		if findings[i].lines != findings[j].lines {
			return findings[i].lines > findings[j].lines
		}
		if findings[i].path != findings[j].path {
			return findings[i].path < findings[j].path
		}
		return findings[i].name < findings[j].name
	})
}
