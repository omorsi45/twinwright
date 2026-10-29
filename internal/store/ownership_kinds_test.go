package store

import (
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// OwnershipKinds is how the metrics show a kind that has not happened yet as zero,
// so it has to name every kind the log can hold. A kind added as a constant and
// left out of the list would be missing from the endpoint until its first row.
func TestOwnershipKindsListsEveryOwnershipConstant(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "ownership.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var declared []string
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			value := spec.(*ast.ValueSpec)
			for i, name := range value.Names {
				if !strings.HasPrefix(name.Name, "Ownership") || i >= len(value.Values) {
					continue
				}
				literal, ok := value.Values[i].(*ast.BasicLit)
				if !ok || literal.Kind != token.STRING {
					continue
				}
				kind, err := strconv.Unquote(literal.Value)
				if err != nil {
					t.Fatal(err)
				}
				declared = append(declared, kind)
			}
		}
	}
	listed := append([]string(nil), OwnershipKinds()...)
	sort.Strings(declared)
	sort.Strings(listed)
	if len(declared) == 0 || strings.Join(declared, ",") != strings.Join(listed, ",") {
		t.Fatalf("OwnershipKinds() = %v, but the Ownership constants in ownership.go are %v", listed, declared)
	}
}
