package e2e

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// fatalHelpers are the helpers in this package that end the test by calling
// t.Fatalf. They are correct to call from the test goroutine and wrong to call
// from a spawned one.
var fatalHelpers = map[string]string{
	"do":              "use request, which returns the error instead",
	"addItem":         "use request with lineItemPath and lineItemBody",
	"createBill":      "use request with createBillBody",
	"billIDOf":        "read the id after the goroutines have been waited on",
	"waitForState":    "poll from the test goroutine",
	"totalMinorUnits": "read the total after the goroutines have been waited on",
	"requireAPI":      "call it before spawning anything",
}

// TestGoroutinesDoNotCallFatalHelpers enforces a rule the runtime cannot.
//
// t.Fatalf stops a test by killing the goroutine it runs on. From the test
// goroutine that is exactly right. From a spawned one it ends that goroutine
// quietly: the waitgroup still completes, the test carries on, and it reports a
// missing result as though the assertion had failed. A refused connection then
// reads as a lost write, and whoever investigates goes looking for a concurrency
// defect that was never there - which is precisely what happened the first time
// the concurrent line item test failed.
//
// go vet has a check for this, but it only sees t.Fatal written directly inside
// the go statement. Here the call is two frames deep - addItem to do to Fatalf -
// so it passes. This looks for the helpers themselves.
func TestGoroutinesDoNotCallFatalHelpers(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", nil, 0)
	if err != nil {
		t.Fatalf("parsing the e2e package: %v", err)
	}

	checked := 0
	for _, pkg := range pkgs {
		for name, file := range pkg.Files {
			ast.Inspect(file, func(n ast.Node) bool {
				gostmt, ok := n.(*ast.GoStmt)
				if !ok {
					return true
				}
				checked++
				ast.Inspect(gostmt, func(inner ast.Node) bool {
					call, ok := inner.(*ast.CallExpr)
					if !ok {
						return true
					}
					ident, ok := call.Fun.(*ast.Ident)
					if !ok {
						return true
					}
					if advice, bad := fatalHelpers[ident.Name]; bad {
						t.Errorf("%s: goroutine calls %s, which ends the test with t.Fatalf\n\n"+
							"From a spawned goroutine that kills only that goroutine, so the failure "+
							"is reported against the wrong assertion. Instead: %s.",
							fset.Position(call.Pos()), ident.Name, advice)
					}
					return true
				})
				return true
			})
			_ = name
		}
	}

	if checked == 0 {
		t.Error("no goroutines found in the e2e package; this guard is checking nothing")
	}
	t.Logf("checked %d goroutines", checked)
}
