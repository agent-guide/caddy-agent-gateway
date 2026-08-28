package runtime_test

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestProtocolPackagesDoNotImportAgentControlPlane(t *testing.T) {
	t.Parallel()

	repositoryRoot := filepath.Clean(filepath.Join("..", "..", ".."))
	for _, relativeRoot := range []string{"pkg/acp", "pkg/llm", "pkg/mcp"} {
		relativeRoot := relativeRoot
		t.Run(relativeRoot, func(t *testing.T) {
			t.Parallel()
			root := filepath.Join(repositoryRoot, filepath.FromSlash(relativeRoot))
			err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
				if walkErr != nil {
					return walkErr
				}
				if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") {
					return nil
				}
				files := token.NewFileSet()
				file, err := parser.ParseFile(files, path, nil, parser.ImportsOnly)
				if err != nil {
					return err
				}
				for _, imported := range file.Imports {
					importPath, err := strconv.Unquote(imported.Path.Value)
					if err != nil {
						return err
					}
					if importPath == "github.com/agent-guide/agent-gateway/pkg/agent" ||
						strings.HasPrefix(importPath, "github.com/agent-guide/agent-gateway/pkg/agent/") {
						line := files.Position(imported.Pos()).Line
						t.Errorf("%s:%d imports forbidden control-plane package %q", path, line, importPath)
					}
				}
				return nil
			})
			if err != nil {
				t.Fatalf("inspect %s imports: %v", relativeRoot, err)
			}
		})
	}
}

func TestRuntimeImportsOnlyAgentContract(t *testing.T) {
	t.Parallel()

	root := filepath.Join("..", "..", "..", "pkg", "agent", "runtime")
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") ||
			strings.HasSuffix(entry.Name(), "_test.go") {
			return nil
		}
		files := token.NewFileSet()
		file, err := parser.ParseFile(files, path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, imported := range file.Imports {
			importPath, err := strconv.Unquote(imported.Path.Value)
			if err != nil {
				return err
			}
			if isStandardLibraryImport(importPath) ||
				importPath == "github.com/agent-guide/agent-gateway/pkg/agent" ||
				importPath == "github.com/agent-guide/agent-gateway/pkg/agent/runtime" {
				continue
			}
			line := files.Position(imported.Pos()).Line
			t.Errorf("%s:%d imports forbidden runtime implementation package %q", path, line, importPath)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("inspect agent runtime imports: %v", err)
	}
}

func isStandardLibraryImport(importPath string) bool {
	first, _, _ := strings.Cut(importPath, "/")
	return !strings.Contains(first, ".")
}
