package sink

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// forbiddenDials are the package-level names a sink must not use to open a
// connection: it dials through netguard.Client.DialContext, which applies
// the SSRF guard and the per-host limiter (ADR 0039).
var forbiddenDials = map[string]map[string]bool{
	"net":        {"Dial": true, "DialTimeout": true, "Dialer": true, "DialTCP": true, "DialIP": true, "DialUDP": true, "DialUnix": true},
	"crypto/tls": {"Dial": true, "DialWithDialer": true, "Dialer": true},
}

// directDials returns "file:line: pkg.Name" for each use of a forbidden
// name in the Go source src.
func directDials(fset *token.FileSet, name string, src any) ([]string, error) {
	f, err := parser.ParseFile(fset, name, src, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	imports := map[string]string{} // local name -> import path
	for _, imp := range f.Imports {
		path, _ := strconv.Unquote(imp.Path.Value)
		if forbiddenDials[path] == nil {
			continue
		}
		local := path[strings.LastIndexByte(path, '/')+1:]
		if imp.Name != nil {
			local = imp.Name.Name
		}
		imports[local] = path
	}
	var found []string
	ast.Inspect(f, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		id, ok := sel.X.(*ast.Ident)
		if !ok {
			return true
		}
		if path, ok := imports[id.Name]; ok && forbiddenDials[path][sel.Sel.Name] {
			found = append(found, fset.Position(sel.Pos()).String()+": "+path+"."+sel.Sel.Name)
		}
		return true
	})
	return found, nil
}

// AC (#93, 93-B): no sink opens a connection with net.Dial, net.DialTimeout,
// net.Dialer or tls.Dial; sinks dial through netguard.
func TestSinksDoNotDialDirectly(t *testing.T) {
	fset := token.NewFileSet()
	files := 0
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == "testdata" {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		files++
		found, err := directDials(fset, path, nil)
		if err != nil {
			return err
		}
		for _, f := range found {
			t.Errorf("%s: sinks dial through netguard.Client.DialContext", f)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if files < 10 {
		t.Fatalf("scanned only %d Go files; is the walk rooted at internal/sink?", files)
	}
}

func TestDirectDialsDetects(t *testing.T) {
	src := `package x
import (
	"net"
	stdtls "crypto/tls"
)
func f() {
	_, _ = net.Dial("tcp", "h:1")
	var d net.Dialer
	_ = d
	_, _ = stdtls.Dial("tcp", "h:1", nil)
	_, _ = net.Listen("tcp", ":0")
	_ = net.JoinHostPort("h", "1")
}`
	found, err := directDials(token.NewFileSet(), "x.go", src)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"net.Dial", "net.Dialer", "crypto/tls.Dial"}
	if len(found) != len(want) {
		t.Fatalf("found %v, want %v", found, want)
	}
	for i, w := range want {
		if !strings.HasSuffix(found[i], ": "+w) {
			t.Errorf("finding %d = %q, want %s", i, found[i], w)
		}
	}
}
