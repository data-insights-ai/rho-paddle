package paddle

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// CONTRIBUTING states that wire types stay in internal/. The rule buys a
// compiler boundary: a Paddle JSON field can never be promoted into an exported
// signature by accident. It only holds if something checks, so this does.
func TestProviderWireShapesStayInternal(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.TYPE {
				continue
			}
			for _, spec := range gen.Specs {
				typeSpec, ok := spec.(*ast.TypeSpec)
				if !ok {
					continue
				}
				// Only an exported type can carry a provider field into the
				// module's compatibility contract. Unexported parse helpers and
				// this adapter's own durable formats are not provider wire.
				if !typeSpec.Name.IsExported() {
					continue
				}
				structType, ok := typeSpec.Type.(*ast.StructType)
				if !ok {
					continue
				}
				for _, field := range structType.Fields.List {
					if field.Tag == nil || !strings.Contains(field.Tag.Value, "json:") {
						continue
					}
					t.Errorf("%s: type %s carries a json tag; provider wire shapes belong in internal/paddlewire",
						filepath.Base(name), typeSpec.Name.Name)
					break
				}
			}
		}
	}
}
