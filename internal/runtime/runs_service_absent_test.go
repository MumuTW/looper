package runtime

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Contract (#32): run transitions stay on the operation-lease + role runners +
// storage.RunsRepository path. A parallel runs.Service must not return.
func TestServicesHasNoRunsTransitionService(t *testing.T) {
	t.Parallel()

	field, ok := reflect.TypeOf(Services{}).FieldByName("Runs")
	if ok {
		t.Fatalf("Services.Runs field present (%v); run transitions must not use a parallel service seam", field.Type)
	}
}

func TestRuntimeDoesNotImportRunsServicePackage(t *testing.T) {
	t.Parallel()

	src, err := os.ReadFile("runtime.go")
	if err != nil {
		t.Fatalf("read runtime.go: %v", err)
	}
	if strings.Contains(string(src), "github.com/nexu-io/looper/internal/runs") {
		t.Fatal("runtime must not import internal/runs; that package was the unused transition seam")
	}
}

func TestRunsServicePackageDoesNotExist(t *testing.T) {
	t.Parallel()

	entries, err := os.ReadDir(filepath.Join("..", "runs"))
	if err == nil && len(entries) > 0 {
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Fatalf("internal/runs still contains files %v; unused runs.Service package must stay deleted", names)
	}
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("stat internal/runs: %v", err)
	}
}

func TestNoParallelRunTransitionServiceType(t *testing.T) {
	t.Parallel()

	root := filepath.Join("..")
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			name := d.Name()
			if name == "testdata" || name == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		file, parseErr := parser.ParseFile(fset, path, nil, 0)
		if parseErr != nil {
			return parseErr
		}
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.TYPE {
				continue
			}
			for _, spec := range gen.Specs {
				typeSpec, ok := spec.(*ast.TypeSpec)
				if !ok || typeSpec.Name == nil || typeSpec.Name.Name != "Service" {
					continue
				}
				methods := serviceMethodNames(file, "Service")
				if methods["StartRun"] && methods["RecordStep"] && methods["Complete"] {
					t.Fatalf("%s defines Service with StartRun/RecordStep/Complete; that is a second run transition authority", path)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk internal packages: %v", err)
	}
}

func serviceMethodNames(file *ast.File, typeName string) map[string]bool {
	out := map[string]bool{}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv == nil || len(fn.Recv.List) == 0 || fn.Name == nil {
			continue
		}
		recv := fn.Recv.List[0].Type
		switch t := recv.(type) {
		case *ast.StarExpr:
			if ident, ok := t.X.(*ast.Ident); ok && ident.Name == typeName {
				out[fn.Name.Name] = true
			}
		case *ast.Ident:
			if t.Name == typeName {
				out[fn.Name.Name] = true
			}
		}
	}
	return out
}
