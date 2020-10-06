// impl generates method stubs for implementing an interface.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"go/ast"
	"go/build"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/template"

	"golang.org/x/tools/imports"
	"go/format"
)

const usage = `kitboiler <iface>

kitboiler generates Go kit (https://gokit.io) endpoints, request/response types, request decoders and http handlers 
based on an interface that defines a service.

Given a service definition/interface in github.com/me/mypkg/api/somefile.go:

type MyService interface {
	MyFirstFunction(name string) (err error)
	MyFirstQuery() (results []*model.QueryResult, err error)
	MySecondQuery() (result *somepkg.FooBar, err error)
}

You should call kitboiler like this:

kitboiler github.com/me/mypkg/api.MyService 

NOTE: you HAVE to provide names for both the parameters and the return vars in your interface definition as
those are used by kitboiler. Choose the names wisely as they will become part of your public interface.

Implementation is based on the impl package: https://github.com/josharian/impl and inspiration was generously provided 
by SQLBoiler (https://github.com/volatiletech/sqlboiler)
`

var (
	flagSrcDir = flag.String("dir", "", "package source directory, useful for vendored code")
	flagPkgName = flag.String("pkg", "endpoints", "name of resulting package")
)

// findInterface returns the import path and identifier of an interface.
// For example, given "http.ResponseWriter", findInterface returns
// "net/http", "ResponseWriter".
// If a fully qualified interface is given, such as "net/http.ResponseWriter",
// it simply parses the input.
func findInterface(iface string, srcDir string) (path string, id string, err error) {
	if len(strings.Fields(iface)) != 1 {
		return "", "", fmt.Errorf("couldn't parse interface: %s", iface)
	}

	srcPath := filepath.Join(srcDir, "__go_impl__.go")

	if slash := strings.LastIndex(iface, "/"); slash > -1 {
		// package path provided
		dot := strings.LastIndex(iface, ".")
		// make sure iface does not end with "/" (e.g. reject net/http/)
		if slash+1 == len(iface) {
			return "", "", fmt.Errorf("interface name cannot end with a '/' character: %s", iface)
		}
		// make sure iface does not end with "." (e.g. reject net/http.)
		if dot+1 == len(iface) {
			return "", "", fmt.Errorf("interface name cannot end with a '.' character: %s", iface)
		}
		// make sure iface has exactly one "." after "/" (e.g. reject net/http/httputil)
		if strings.Count(iface[slash:], ".") != 1 {
			return "", "", fmt.Errorf("invalid interface name: %s", iface)
		}
		return iface[:dot], iface[dot+1:], nil
	}

	src := []byte("package hack\n" + "var i " + iface)
	// If we couldn't determine the import path, goimports will
	// auto fix the import path.
	imp, err := imports.Process(srcPath, src, nil)
	if err != nil {
		return "", "", fmt.Errorf("couldn't parse interface: %s", iface)
	}

	// imp should now contain an appropriate import.
	// Parse out the import and the identifier.
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, srcPath, imp, 0)
	if err != nil {
		panic(err)
	}
	if len(f.Imports) == 0 {
		return "", "", fmt.Errorf("unrecognized interface: %s", iface)
	}
	raw := f.Imports[0].Path.Value   // "io"
	path, err = strconv.Unquote(raw) // io
	if err != nil {
		panic(err)
	}
	decl := f.Decls[1].(*ast.GenDecl)      // var i io.Reader
	spec := decl.Specs[0].(*ast.ValueSpec) // i io.Reader
	sel := spec.Type.(*ast.SelectorExpr)   // io.Reader
	id = sel.Sel.Name                      // Reader
	return path, id, nil
}

// Pkg is a parsed build.Package.
type Pkg struct {
	*build.Package
	*token.FileSet
	srcDir string
}

// typeSpec locates the *ast.TypeSpec for type id in the import path.
func typeSpec(path string, id string, srcDir string) (Pkg, *ast.TypeSpec, error) {
	pkg, err := build.Import(path, srcDir, 0)
	if err != nil {
		return Pkg{}, nil, fmt.Errorf("couldn't find package %s: %v", path, err)
	}

	fset := token.NewFileSet() // share one fset across the whole package
	for _, file := range pkg.GoFiles {
		f, err := parser.ParseFile(fset, filepath.Join(pkg.Dir, file), nil, 0)
		if err != nil {
			continue
		}

		for _, decl := range f.Decls {
			decl, ok := decl.(*ast.GenDecl)
			if !ok || decl.Tok != token.TYPE {
				continue
			}
			for _, spec := range decl.Specs {
				spec := spec.(*ast.TypeSpec)
				if spec.Name.Name != id {
					continue
				}
				return Pkg{Package: pkg, FileSet: fset, srcDir: srcDir}, spec, nil
			}
		}
	}
	return Pkg{}, nil, fmt.Errorf("type %s not found in %s", id, path)
}

// gofmt pretty-prints e.
func (p Pkg) gofmt(e ast.Expr) string {
	var buf bytes.Buffer
	_ = printer.Fprint(&buf, p.FileSet, e)
	return buf.String()
}

// fullType returns the fully qualified type of e.
// Examples, assuming package net/http:
// 	fullType(int) => "int"
// 	fullType(Handler) => "http.Handler"
// 	fullType(io.Reader) => "io.Reader"
// 	fullType(*Request) => "*http.Request"
func (p Pkg) fullType(e ast.Expr) string {
	ast.Inspect(e, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.Ident:
			// Using typeSpec instead of IsExported here would be
			// more accurate, but it'd be crazy expensive, and if
			// the type isn't exported, there's no point trying
			// to implement it anyway.
			if n.IsExported() {
				n.Name = p.Package.Name + "." + n.Name
			}
		case *ast.SelectorExpr:
			return false
		}
		return true
	})
	return p.gofmt(e)
}

func (p Pkg) generateOptionSetters(name, typ string) []string {
	var optionSetters []string
	if strings.HasPrefix(typ, "...") && strings.HasSuffix(typ,"Setter") {
		typ = typ[3:len(typ)-6]
		srcPkg := p.Name
		importPath := p.ImportPath
		bareType := typ
		if strings.Contains(typ, ".") {
			bareType = typ[strings.Index(typ, ".")+1:]
			srcPkg = typ[:strings.Index(typ, ".")]
			if !strings.HasSuffix(importPath, srcPkg) {
				for _, ip := range p.Imports {
					if strings.HasSuffix(ip, srcPkg) {
						importPath = ip
						break
					}
				}
			}
		}

		_, spec, err := typeSpec(importPath, bareType, p.srcDir)
		if err != nil { panic(err) }
		if idecl, ok := spec.Type.(*ast.StructType); ok {
			for _, field := range idecl.Fields.List {
				optionSetters = append(optionSetters, fmt.Sprintf("\nfunc(v %v) func(*%s) { return func(opts *%s) { opts.%s = v } }(req.%s.%s)",
					field.Type, typ, typ, field.Names[0], name, field.Names[0]))
			}
		}

	}
	return optionSetters
}

func (p Pkg) generateOptionStructName(typ string) string {
	if strings.HasPrefix(typ, "...") && strings.HasSuffix(typ,"Setter") {
		typ = typ[3:len(typ)-6]
	}
	return typ
}

func (p Pkg) params(field *ast.Field) []Param {
	var params []Param
	typ := p.fullType(field.Type)

	for _, name := range field.Names {
		params = append(params, Param{Name: name.Name, Type: typ})
	}
	// Handle anonymous params
	if len(params) == 0 {
		params = []Param{Param{Type: typ}}
	}
	return params
}

type Service struct {
	Pkg string
	IFace string
	Imports map[string]string
	Funcs []Func
}

// Func represents a function signature.
type Func struct {
	Name   string
	Params []Param
	Res    []Param
	RequiredImports []string
	OptionSetters []string
}

// Param represents a parameter in a function or method signature.
type Param struct {
	Name string
	Type string
}

func (p Pkg) funcsig(f *ast.Field) Func {
	fn := Func{Name: f.Names[0].Name,}
	typ := f.Type.(*ast.FuncType)
	if typ.Params != nil {
		for _, field := range typ.Params.List {
			fn.Params = append(fn.Params, p.params(field)...)
		}
	}
	for _, param := range fn.Params {
		if IsOptionSetter(param.Type) {
			fn.OptionSetters = append(fn.OptionSetters, p.generateOptionSetters(param.Name, param.Type)...)
		}
	}
	if typ.Results != nil {
		for _, field := range typ.Results.List {
			fn.Res = append(fn.Res, p.params(field)...)
		}
	}
	for _, i := range p.Imports {
		k := i[strings.LastIndex(i, "/")+1:]
		for _, param := range fn.Params {
			if strings.Contains(param.Type, k) {
				fn.RequiredImports = append(fn.RequiredImports, i)
			}
		}
		for _, res := range fn.Res {
			if strings.Contains(res.Type, k) {
				fn.RequiredImports = append(fn.RequiredImports, i)
			}
		}
	}

	return fn
}

// funcs returns the set of methods required to implement iface.
// It is called funcs rather than methods because the
// function descriptions are functions; there is no receiver.
func funcs(iface string, srcDir string) ([]Func, error) {
	// Locate the interface.
	path, id, err := findInterface(iface, srcDir)
	if err != nil {
		return nil, err
	}

	// Parse the package and find the interface declaration.
	p, spec, err := typeSpec(path, id, srcDir)
	if err != nil {
		return nil, fmt.Errorf("interface %s not found: %s", iface, err)
	}
	idecl, ok := spec.Type.(*ast.InterfaceType)
	if !ok {
		return nil, fmt.Errorf("not an interface: %s", iface)
	}

	if idecl.Methods == nil {
		return nil, fmt.Errorf("empty interface: %s", iface)
	}

	//fmt.Printf("imports: %v\n", p.Imports)
	var fns []Func
	for _, fndecl := range idecl.Methods.List {
		if len(fndecl.Names) == 0 {
			// Embedded interface: recurse
			embedded, err := funcs(p.fullType(fndecl.Type), srcDir)
			if err != nil {
				return nil, err
			}
			fns = append(fns, embedded...)
			continue
		}

		fn := p.funcsig(fndecl)
		fns = append(fns, fn)
	}
	return fns, nil
}

const stub = `
// Code generated by KitBoiler (https://github.com/jeroenvand/kitboiler). DO NOT EDIT.
// This file is meant to be re-generated in place and/or deleted at any time.

package {{ .Pkg }}
{{ $svc := . }}
import ({{ range $imp, $alias := .Imports }}{{ $alias }} "{{ $imp }}"
{{ end }}
)
{{ range $fun := .Funcs }}


type {{$fun.Name}}Request struct { {{ range .Params}}{{.Name}} {{ OptionSetterStruct .Type}} 
{{end}} }

type {{.Name}}Response struct { {{ range FilterError .Res }}{{ .Name }} {{.Type}}
{{end}} }

func {{.Name}}EndPoint(svc {{$svc.IFace}}) endpoint.Endpoint {
	return func(ctx context.Context, request interface{}) (interface{}, error) { {{ if TakesParams $fun }}
		req := request.({{.Name}}Request){{ end }}
		{{ JoinParams .Res }} := svc.{{.Name}}({{ GenerateFuncParams $fun }})
		return {{.Name}}Response{
			{{ range FilterError .Res  }}{{.Name}}: {{.Name}},
			{{end}}
		}, err
	}
}

func {{.Name}}HTTPJSONHandler(e endpoint.Endpoint) http.Handler {
	return httptransport.NewServer(
		e,
		Decode{{.Name}}Request,
		EncodeResponse,
	)
}

func Decode{{.Name}}Request(_ context.Context, r *http.Request) (interface{}, error) {
	var request {{.Name}}Request
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		return nil, err
	}
	return request, nil
}

{{ end }}

func EncodeResponse(_ context.Context, w http.ResponseWriter, response interface{}) error {
	return json.NewEncoder(w).Encode(response)
}

`

func IsOptionSetter(typ string) bool {
	return strings.HasPrefix(typ, "...") && strings.HasSuffix(typ,"Setter")
}

func GenerateFuncParams(f Func) string {
	params := []string{}
	for _, p := range f.Params {
		if p.Type == "context.Context" {
			params = append(params, fmt.Sprintf("ctx"))
			continue
		}
		if !IsOptionSetter(p.Type) {
			params = append(params, fmt.Sprintf("req.%s", p.Name))
		}
	}
	for _, optSetter := range f.OptionSetters {
		params = append(params, optSetter)
	}
	return strings.Join(params, ", ")
}


func OptionSetterStruct(typ string) string {
	if strings.HasPrefix(typ, "...") && strings.HasSuffix(typ,"Setter") {
		typ = typ[3:len(typ)-6]
	}
	return typ
}

func TakesParams(f Func) bool {
	return len(f.Params) > 0
}


func FilterError(params []Param) []Param {
	var newParams []Param
	for _, p := range params {
		if p.Type != "error" {
			newParams = append(newParams, p)
		}
	}
	return newParams
}

func JoinParams(params []Param) string {
	var names []string
	for _, p := range params {
		names = append(names, p.Name)
	}
	return strings.Join(names, ",")
}

var tmpl = template.Must(template.New("test").Funcs(template.FuncMap{
	"JoinParams": JoinParams,
	"FilterError": FilterError,
	"TakesParams": TakesParams,
	"IsOptionSetter": IsOptionSetter,
	"OptionSetterStruct": OptionSetterStruct,
	"GenerateFuncParams": GenerateFuncParams,
}).Parse(stub))

// genStubs prints nicely formatted method stubs
// for fns using receiver expression recv.
// If recv is not a valid receiver expression,
// genStubs will panic.
func genStubs(iface, pkg string, fns []Func) []byte {
	var buf bytes.Buffer
	ifaceName := iface[strings.LastIndex(iface, "/")+1:]
	ifacePkg := iface[:strings.LastIndex(iface, ".")]

	importMap := map[string]string{
		"context": "",
		"encoding/json": "",
		"net/http": "",
		"github.com/go-kit/kit/transport/http": "httptransport",
		"github.com/go-kit/kit/endpoint": "",
		ifacePkg: "",
	}
	for _, f := range fns {
		for _, i := range f.RequiredImports {
			if _, ok := importMap[i]; !ok {
				importMap[i] = ""
			}
		}
	}
	svc := Service{Funcs: fns, IFace: ifaceName, Imports: importMap, Pkg: pkg}
	err := tmpl.Execute(&buf, svc)
	if err != nil {
		panic(err)
	}

	pretty, err := format.Source(buf.Bytes())
	if err != nil {
		return buf.Bytes()
	}
	return pretty
}

func main() {
	flag.Parse()
	fmt.Println("// " + *flagPkgName)
	fmt.Println("// " + *flagSrcDir)
	//os.Exit(0)
	if len(flag.Args()) < 1 {
		_, _ = fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}

	iface := flag.Arg(0)


	if *flagSrcDir == "" {
		if dir, err := os.Getwd(); err == nil {
			*flagSrcDir = dir
		}
	}
	fns, err := funcs(iface, *flagSrcDir)
	if err != nil {
		fatal(err)
	}

	src := genStubs(iface, *flagPkgName, fns)
	fmt.Print(string(src))
}

func fatal(msg interface{}) {
	_, _ = fmt.Fprintln(os.Stderr, msg)
	os.Exit(1)
}
