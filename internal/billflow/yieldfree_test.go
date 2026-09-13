package billflow_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// yieldPoints are the workflow APIs that suspend a coroutine. A handler that
// reaches one of these is no longer atomic with respect to the period-end timer.
var yieldPoints = map[string]bool{
	"ExecuteActivity":      true,
	"ExecuteLocalActivity": true,
	"ExecuteChildWorkflow": true,
	"NewTimer":             true,
	"Sleep":                true,
	"Await":                true,
	"AwaitWithTimeout":     true,
	"GetSignalChannel":     true,
}

// TestUpdateHandlersAreYieldFree enforces the rule the whole close-race design
// rests on.
//
// Handlers and the main loop share a thread and interleave only at yield points,
// so a handler with no yield point is atomic with respect to the period-end timer
// and the close race resolves without a lock. A handler that awaited an activity
// would suspend mid-transition and let the timer fire inside that window.
//
// Timing-dependent, so no runtime assertion catches it reliably; checked
// structurally against the source instead.
func TestUpdateHandlersAreYieldFree(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "workflow.go", nil, 0)
	if err != nil {
		t.Fatalf("parsing workflow.go: %v", err)
	}

	checked := 0

	check := func(name string, body ast.Node) {
		checked++
		ast.Inspect(body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			// workflow.X(...) where X suspends.
			if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "workflow" && yieldPoints[sel.Sel.Name] {
				t.Errorf("%s calls workflow.%s: update handlers must not yield, or the close race stops being atomic",
					name, sel.Sel.Name)
			}
			// selector.Select(...) suspends too.
			if sel.Sel.Name == "Select" {
				t.Errorf("%s calls Select: update handlers must not yield", name)
			}
			return true
		})
	}

	ast.Inspect(file, func(n ast.Node) bool {
		// The two update registrations: handler is arg 2, validator lives in the
		// options literal at arg 3.
		if call, ok := n.(*ast.CallExpr); ok {
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok &&
				sel.Sel.Name == "SetUpdateHandlerWithOptions" && len(call.Args) >= 3 {

				updateName := "update"
				if lit, ok := call.Args[1].(*ast.Ident); ok {
					updateName = lit.Name
				}
				if fn, ok := call.Args[2].(*ast.FuncLit); ok {
					check(updateName+" handler", fn.Body)
				}
				if opts, ok := call.Args[3].(*ast.CompositeLit); ok {
					for _, elt := range opts.Elts {
						kv, ok := elt.(*ast.KeyValueExpr)
						if !ok {
							continue
						}
						if key, ok := kv.Key.(*ast.Ident); ok && key.Name == "Validator" {
							if fn, ok := kv.Value.(*ast.FuncLit); ok {
								check(updateName+" validator", fn.Body)
							}
						}
					}
				}
			}
		}
		// beginClose is called from the close handler, so it inherits the rule.
		if assign, ok := n.(*ast.AssignStmt); ok && len(assign.Lhs) == 1 && len(assign.Rhs) == 1 {
			if id, ok := assign.Lhs[0].(*ast.Ident); ok && id.Name == "beginClose" {
				if fn, ok := assign.Rhs[0].(*ast.FuncLit); ok {
					check("beginClose", fn.Body)
				}
			}
		}
		return true
	})

	// Four closures carry the rule: two handlers, one validator, and beginClose.
	// If the workflow is restructured so they no longer exist under these shapes,
	// this test must be updated rather than silently checking nothing.
	if want := 4; checked != want {
		t.Errorf("checked %d closures, want %d: the guard is no longer finding what it should", checked, want)
	}
}

// TestWorkflowUsesDeterministicClock guards the other way an in-flight bill gets
// broken: wall-clock time inside workflow code is non-deterministic on replay,
// so a worker restart mid-period would fail to reconstruct the bill.
func TestWorkflowUsesDeterministicClock(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "workflow.go", nil, 0)
	if err != nil {
		t.Fatalf("parsing workflow.go: %v", err)
	}

	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		pkg, ok := sel.X.(*ast.Ident)
		if !ok {
			return true
		}
		if pkg.Name == "time" && (sel.Sel.Name == "Now" || sel.Sel.Name == "Since") {
			pos := fset.Position(call.Pos())
			t.Errorf("workflow.go:%d calls time.%s: workflow code must read the clock through workflow.Now, "+
				"which replays deterministically", pos.Line, sel.Sel.Name)
		}
		return true
	})

	// And confirm the period timer is actually built from the deterministic clock.
	src, err := parser.ParseFile(token.NewFileSet(), "workflow.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("re-parsing workflow.go: %v", err)
	}
	found := false
	ast.Inspect(src, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "NewTimer" {
			var buf strings.Builder
			ast.Fprint(&buf, token.NewFileSet(), call.Args, nil)
			if strings.Contains(buf.String(), "Now") {
				found = true
			}
		}
		return true
	})
	if !found {
		t.Error("the period timer is not derived from workflow.Now")
	}
}
