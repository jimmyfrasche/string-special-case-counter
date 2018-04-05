package main

import (
	"bufio"
	"flag"
	"fmt"
	"go/ast"
	"go/build"
	"go/token"
	"go/types"
	"log"
	"os"
	"os/exec"
	"path"
	"strings"

	"golang.org/x/tools/go/ast/astutil"
	"golang.org/x/tools/go/buildutil"
	"golang.org/x/tools/go/loader"
)

func goList(tags, pkgs []string) ([]string, error) {
	args := []string{"list"}
	if len(tags) > 0 {
		args = append(args, strings.Join(tags, " "))
	}
	args = append(args, "--")
	args = append(args, pkgs...)

	cmd := exec.Command("go", args...)
	cmd.Stderr = os.Stderr

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("coult not create stdout pipe to go list: %v", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("could not exec go list: %v", err)
	}

	scan := bufio.NewScanner(stdout)
	var acc []string
	for scan.Scan() {
		name := scan.Text()
		if !strings.Contains("/"+name+"/", "/vendor/") {
			acc = append(acc, name)
		}
	}
	if err := scan.Err(); err != nil {
		_ = cmd.Wait()
		return nil, fmt.Errorf("could not read stdout from go list: %v", err)
	}

	if err := cmd.Wait(); err != nil {
		return nil, fmt.Errorf("could not exec go list: %v", err)
	}

	return acc, nil
}

func load(ctx *build.Context, pkgs []string) (*loader.Program, []*loader.PackageInfo, error) {
	cfg := &loader.Config{
		AllowErrors: true,
		Build:       ctx,
	}

	for _, pkg := range pkgs {
		cfg.ImportWithTests(pkg)
	}

	prog, err := cfg.Load()
	if err != nil {
		return nil, nil, fmt.Errorf("could not parse pkgs: %v", err)
	}

	var acc []*loader.PackageInfo
	for _, pkg := range pkgs {
		P := prog.Package(pkg)
		if P == nil || !P.TransitivelyErrorFree {
			log.Printf("could not examine %s", pkg)
			continue
		}
		acc = append(acc, P)

		P = prog.Package(pkg + "_test")
		if P == nil {
			continue
		}
		if !P.TransitivelyErrorFree {
			log.Printf("coult not examine %s_test", pkg)
			continue
		}
		acc = append(acc, P)
	}

	return prog, acc, nil
}

type Kind uint

const (
	Other Kind = iota
	String
	Bytes
	Runes
	Byte
	Rune
)

func (k Kind) String() string {
	switch k {
	case String:
		return "string"
	case Bytes:
		return "[]byte"
	case Runes:
		return "[]rune"
	case Byte:
		return "byte"
	case Rune:
		return "rune"
	default:
		return "<other>"
	}
}

func KindOf(t types.Type) Kind {
	switch t := t.(type) {
	case *types.Basic:
		switch t.Kind() {
		case types.String, types.UntypedString:
			return String
		case types.Byte:
			return Byte
		case types.Rune, types.UntypedRune, types.UntypedInt:
			// NB, UntypedInt catches things like string(42).
			// This would cause false positives except that we only examine
			// in specific cases where a false positive would be illegal since
			// the program type checks.
			return Rune
		default:
			return Other
		}

	case *types.Slice:
		switch KindOf(t.Elem()) {
		case Byte:
			return Bytes
		case Rune:
			return Runes
		default:
			return Other
		}

	case *types.Named:
		return KindOf(t.Underlying())

	default:
		return Other
	}
}

//isConversion tests whether n is a conversion and returns the type being converted to.
//This misses a number of cases but covers all that this program cares about.
func isConversion(pi *loader.PackageInfo, n ast.Node) (types.Type, ast.Expr, bool) {
	call, ok := n.(*ast.CallExpr)
	if !ok {
		return nil, nil, false
	}
	if len(call.Args) != 1 {
		return nil, nil, false
	}
	if call.Ellipsis != token.NoPos {
		return nil, nil, false
	}

	to := pi.Types[call.Fun].Type

	conv := astutil.Unparen(call.Fun)

	// Only interested in the y from x.y and only if x is a package.
	if se, ok := conv.(*ast.SelectorExpr); ok {
		if pi.Selections[se] != nil {
			return nil, nil, false
		}

		conv = se.Sel
	}

	switch F := conv.(type) {
	case *ast.ArrayType, *ast.StarExpr: // NB. StarExpr for weird ones like *(*string)(&x)
		// Must be a conversion.

	case *ast.Ident:
		switch to := to.(type) {
		case *types.Basic:
			// If this is something we care about, this is string.
		case *types.Named:
			// Accept if the name is the same as the same type
			if to.Obj().Name() != F.Name {
				return nil, nil, false
			}

		default:
			return nil, nil, false
		}

	default:
		return nil, nil, false
	}

	return to, call.Args[0], true
}

type count struct {
	fs *token.FileSet

	logStr2bs bool
	str2bs    int // []byte(string)

	logBs2Str bool
	bs2str    int // string([]byte)

	logStr2rs bool
	str2rs    int // []rune(string)

	logR2str bool
	r2str    int // string(rune)

	logB2str bool
	b2str    int // string(byte)

	logAppend bool
	append    int // append([]byte, string...)

	logCopy bool
	copy    int // copy([]byte, string)

	logRAppend bool
	rAppend    int // append([]byte, []byte(string)...)

	logRCopy bool
	rCopy    int // copy([]byte, []byte(string))

	lloc int // logical lines of code inspected
}

func (c *count) log(logIt bool, what, imp string, poser interface{ Pos() token.Pos }) {
	if !logIt {
		return
	}
	pos := c.fs.Position(poser.Pos())
	log.Printf("%s %s:%s:%d", what, imp, path.Base(pos.Filename), pos.Line)
}

func (c *count) builtin(imp string, pi *loader.PackageInfo, call *ast.CallExpr) (counted bool) {
	id, ok := call.Fun.(*ast.Ident)
	if !ok {
		return false
	}

	bi, ok := pi.Uses[id]
	if !ok {
		return false
	}

	// Determine if this is copy or an append we're interested in.
	isAppend := false
	switch bi.Name() {
	case "append":
		// Only want append(X, Y...).
		if len(call.Args) != 2 || call.Ellipsis == token.NoPos {
			return
		}
		isAppend = true

	case "copy":

	default:
		return false
	}
	// In either case,  need arg₀ = []byte, arg₁ = string or []byte(string).
	if KindOf(pi.Types[call.Args[0]].Type) != Bytes {
		return false
	}

	k := KindOf(pi.Types[call.Args[1]].Type)
	if k == Bytes {
		// count redundant []byte(string)
		_, arg, ok := isConversion(pi, call.Args[1])
		if !ok {
			return false
		}
		if KindOf(pi.Types[arg].Type) != String {
			return false
		}

		if isAppend {
			c.log(c.logRAppend, "append([]byte, []byte(string)...)", imp, call)
			c.rAppend++
		} else {
			c.log(c.logRCopy, "copy([]byte, []byte(string))", imp, call)
			c.rCopy++
		}
		return true
	}
	if k != String {
		return false
	}

	if isAppend {
		c.log(c.logAppend, "append([]byte, string...)", imp, call)
		c.append++
	} else {
		c.log(c.logCopy, "copy([]byte, string)", imp, call)
		c.copy++
	}
	return true
}

func (c *count) node(imp string, pi *loader.PackageInfo, n ast.Node) {
	call, ok := n.(*ast.CallExpr)
	if !ok {
		return
	}

	// Test for builtins.
	if c.builtin(imp, pi, call) {
		return
	}

	toType, _, ok := isConversion(pi, call)
	if !ok {
		return
	}

	to := KindOf(toType)
	if to == Other {
		return
	}

	from := KindOf(pi.Types[call.Args[0]].Type)
	if from == Other {
		return
	}

	switch to {
	case String:
		switch from {
		case Byte:
			c.log(c.logB2str, "string(byte)", imp, call)
			c.b2str++
		case Rune:
			c.log(c.logR2str, "string(rune)", imp, call)
			c.r2str++
		case Bytes:
			c.log(c.logBs2Str, "string([]byte)", imp, call)
			c.bs2str++
		}

	case Bytes:
		if from == String {
			c.log(c.logStr2bs, "[]byte(string)", imp, call)
			c.str2bs++
		}

	case Runes:
		if from == String {
			c.log(c.logStr2rs, "[]rune(string)", imp, call)
			c.str2rs++
		}
	}
}

// lines must not be reused for different files
type lines struct {
	fs   *token.FileSet
	last int
}

func (l *lines) delta(n ast.Node) int {
	if n == nil {
		return 0
	}
	new := l.fs.Position(n.Pos()).Line
	if new == l.last {
		return 0
	}
	l.last = new
	return 1
}

func chk(err error) {
	if err != nil {
		log.Fatal(err)
	}
}

// run on std cmd golang.org/x/...
func main() {
	log.SetFlags(0)

	ctx := build.Default

	flag.Var((*buildutil.TagsFlag)(&ctx.BuildTags), "tags", buildutil.TagsFlagDoc)
	flag.Parse()

	imports, err := goList(ctx.BuildTags, flag.Args())
	chk(err)

	prog, pkgs, err := load(&ctx, imports)
	chk(err)

	c := count{
		fs:         prog.Fset,
		logRAppend: true,
		logRCopy:   true,
	}
	for _, pkg := range pkgs {
		imp := pkg.Pkg.Path()
		for _, file := range pkg.Files {
			lines := lines{
				fs:   prog.Fset,
				last: prog.Fset.Position(file.Pos()).Line,
			}
			ast.Inspect(file, func(n ast.Node) bool {
				c.lloc += lines.delta(n)
				c.node(imp, pkg, n)
				return true
			})
		}
	}

	fmt.Println("[]byte(string):", c.str2bs)
	fmt.Println("string([]byte):", c.bs2str)
	fmt.Println("[]rune(string):", c.str2rs)
	fmt.Println()
	fmt.Println("string(rune):", c.r2str)
	fmt.Println("string(byte):", c.b2str)
	fmt.Println()
	fmt.Println("append([]byte, string...):", c.append)
	fmt.Println("copy([]byte, string):", c.copy)
	fmt.Println()
	fmt.Println("append([]byte, []byte(string)...):", c.rAppend)
	fmt.Println("copy([]byte, []byte(string)):", c.rCopy)
	fmt.Println()
	fmt.Println("packages examined:", len(pkgs))
	fmt.Println("lloc examined:", c.lloc)
}
