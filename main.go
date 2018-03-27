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
		if !strings.Contains(name+"/", "/vendor/") {
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
			log.Print("coult not examine %s_test", pkg)
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

func possibleConversion(call *ast.CallExpr) bool {
	return len(call.Args) == 1 && call.Ellipsis == token.NoPos
}

type count struct {
	str2bs int // []byte(string)
	bs2str int // string([]byte)
	str2rs int // []rune(string)
	r2str  int // string(rune)
	b2str  int // string(byte)
	append int // append([]byte, string...)
	copy   int // copy([]byte, string)
	lloc   int // logical lines of code inspected
}

func (c *count) node(pi *loader.PackageInfo, n ast.Node) {
	call, ok := n.(*ast.CallExpr)
	if !ok {
		return
	}

	// Test for builtins.
	if id, ok := call.Fun.(*ast.Ident); ok { // TODO: move this into own func?
		if bi, ok := pi.Uses[id].(*types.Builtin); ok {
			isAppend := false
			switch bi.Name() {
			case "append":
				// Only want append([]byte, string...).
				if len(call.Args) != 2 || call.Ellipsis == token.NoPos {
					return
				}
				isAppend = true

			case "copy":

			default:
				return
			}

			// In either case,  need arg₀ = []byte, arg₁ = string.
			if KindOf(pi.Types[call.Args[0]].Type) != Bytes {
				return
			}
			if KindOf(pi.Types[call.Args[1]].Type) != String {
				return
			}

			if isAppend {
				c.append++
			} else {
				c.copy++
			}
			return
		}
	}

	if !possibleConversion(call) {
		return
	}

	from := KindOf(pi.Types[call.Args[0]].Type)

	// If this is a conversion, it's not one we are interested in.
	if from == Other {
		return
	}

	convType := pi.Types[call.Fun].Type
	to := KindOf(convType)
	if to == Other {
		return
	}

	// Grab the conv part from conv(X).
	conv := astutil.Unparen(call.Fun)
	if se, ok := conv.(*ast.SelectorExpr); ok {
		if pi.Selections[se] != nil {
			// Definitely not a conversion.
			return
		}
		conv = se.Sel
	}

	// We know we have a type we're interested in,
	// but not whether the current expression is definitely a conversion yet.
	switch F := conv.(type) {
	case *ast.ArrayType, *ast.StarExpr: // NB. StarExpr for weird ones like *(*string)(&x)
		// Must be a conversion.
	case *ast.Ident:
		switch convType := convType.(type) {
		case *types.Named:
			// Accept if type name == conv
			if convType.Obj().Name() != F.Name {
				return
			}

		case *types.Basic:
			// Must be a string.

		default:
			return
		}
	}

	// If we found a conversion we're interested in, count it.
	switch to {
	case String:
		switch from {
		case Byte:
			c.b2str++
		case Rune:
			c.r2str++
		case Bytes:
			c.bs2str++
		}

	case Bytes:
		if from == String {
			c.str2bs++
		}

	case Runes:
		if from == String {
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

	var c count
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			lines := lines{
				fs:   prog.Fset,
				last: prog.Fset.Position(file.Pos()).Line,
			}
			ast.Inspect(file, func(n ast.Node) bool {
				c.lloc += lines.delta(n)
				c.node(pkg, n)
				return true
			})
		}
	}

	fmt.Println("[]byte(string):", c.str2bs)
	fmt.Println("string([]byte):", c.bs2str)
	fmt.Println("[]rune(string):", c.str2rs)
	fmt.Println("string(rune):", c.r2str)
	fmt.Println("string(byte):", c.b2str)
	fmt.Println("append:", c.append)
	fmt.Println("copy:", c.copy)
	fmt.Println()
	fmt.Println("packages examined:", len(pkgs))
	fmt.Println("lloc examined:", c.lloc)
}
