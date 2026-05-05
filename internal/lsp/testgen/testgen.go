package testgen

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/open-policy-agent/opa/v1/ast"
	"github.com/open-policy-agent/opa/v1/dependencies"
	"github.com/open-policy-agent/opa/v1/format"
	"github.com/open-policy-agent/regal/internal/compile"
	rio "github.com/open-policy-agent/regal/internal/io"
	"github.com/open-policy-agent/regal/internal/lsp/uri"
	rparse "github.com/open-policy-agent/regal/internal/parse"
)

type TestGenerationOptions struct {
	RuleName      string
	PackagePath   string
	WorkspacePath string
	FileURI       string
	Rule          *ast.Rule
	AllModules    map[string]*ast.Module
}

func analyzeDependencies(opts TestGenerationOptions) ([]string, error) {
	compiler := compile.NewCompilerWithRegalBuiltins()
	compiler.Compile(opts.AllModules)

	if compiler.Failed() {
		return nil, fmt.Errorf("compilation failed: %v", compiler.Errors)
	}

	refs, err := dependencies.Base(compiler, opts.Rule)
	if err != nil {
		return nil, fmt.Errorf("failed to analyze rule dependencies: %w", err)
	}

	headRefs, err := dependencies.Base(compiler, opts.Rule.Head)
	if err != nil {
		return nil, fmt.Errorf("failed to analyze rule head dependencies: %w", err)
	}

	refs = append(refs, headRefs...)

	filePath := uri.ToPath(opts.FileURI)
	_, inputData := rio.FindInput(filePath, opts.WorkspacePath)
	dataData := findDataFile(opts.WorkspacePath)

	var withClauses []string

	for _, ref := range refs {
		if len(ref) < 2 {
			continue
		}

		if ref[0].Equal(ast.InputRootDocument) {
			path := buildRefPath("input", ref[1:])
			if value := lookupValueFromData(inputData, ref[1:]); value != nil {
				clause := fmt.Sprintf("%s as %s", path, formatValue(value))
				withClauses = append(withClauses, clause)
			}
		}

		if ref[0].Equal(ast.DefaultRootDocument) {
			path := buildRefPath("data", ref[1:])
			if value := lookupValueFromData(dataData, ref[1:]); value != nil {
				clause := fmt.Sprintf("%s as %s", path, formatValue(value))
				withClauses = append(withClauses, clause)
			}
		}
	}

	return withClauses, nil
}

func buildRefPath(root string, terms ast.Ref) string {
	var parts []string
	parts = append(parts, root)

	for _, term := range terms {
		key := strings.Trim(term.Value.String(), `"`)
		parts = append(parts, key)
	}

	return strings.Join(parts, ".")
}

func lookupValueFromData(data map[string]any, terms ast.Ref) any {
	if data == nil || len(terms) == 0 {
		return nil
	}

	node := data
	for _, term := range terms[:len(terms)-1] {
		key := strings.Trim(term.Value.String(), `"`)
		if child, ok := node[key].(map[string]any); ok {
			node = child
		} else {
			return nil
		}
	}

	leaf := strings.Trim(terms[len(terms)-1].Value.String(), `"`)
	return node[leaf]
}

func formatValue(value any) string {
	switch v := value.(type) {
	case string:
		return fmt.Sprintf(`"%s"`, v)
	case float64, bool:
		return fmt.Sprintf("%v", v)
	default:
		if jsonBytes, err := json.Marshal(v); err == nil {
			return string(jsonBytes)
		}
		return `"unknown"`
	}
}

func buildTestFunction(opts TestGenerationOptions, withClauses []string) string {
	testName := generateTestName(opts.RuleName)
	ruleCall := generateRuleCall(opts.PackagePath, opts.RuleName)

	var withClause string
	if len(withClauses) > 0 {
		withClause = " with " + strings.Join(withClauses, " with ")
	}

	return fmt.Sprintf(`%s if {
    %s%s
}`, testName, ruleCall, withClause)
}

func BuildTestHeader(packagePath string) string {
	testPackage := generateTestPackage(packagePath)
	return fmt.Sprintf(`package %s

import %s`, testPackage, packagePath)
}

func GenerateTestFunction(opts TestGenerationOptions) (string, error) {
	if opts.Rule == nil || opts.AllModules == nil {
		return "", fmt.Errorf("dependency analysis requires Rule and AllModules to be provided")
	}

	withClauses, err := analyzeDependencies(opts)
	if err != nil {
		return "", fmt.Errorf("failed to analyze dependencies: %w", err)
	}

	testFunction := buildTestFunction(opts, withClauses)

	completeTest := BuildTestHeader(opts.PackagePath) + "\n\n" + testFunction
	if _, err := rparse.Module("test.rego", completeTest); err != nil {
		// Array access syntax like input.permissions[0] can cause parsing issues
		// So for now I just fall back to a basic test skeleton. Surely a more
		// graceful way of handling this.
		testFunction = buildTestFunction(opts, []string{})
		completeTest = BuildTestHeader(opts.PackagePath) + "\n\n" + testFunction
		if _, err := rparse.Module("test.rego", completeTest); err != nil {
			return "", fmt.Errorf("failed to parse even basic test: %w", err)
		}
	}

	module, err := rparse.Module("test.rego", completeTest)
	if err != nil {
		return "", fmt.Errorf("failed to parse test: %w", err)
	}

	formatted, err := format.Ast(module)
	if err != nil {
		return "", fmt.Errorf("failed to format test: %w", err)
	}

	lines := strings.Split(string(formatted), "\n")
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "test_") {
			return strings.Join(lines[i:], "\n"), nil
		}
	}

	return testFunction, nil
}

func findDataFile(workspacePath string) map[string]any {
	dataPath := filepath.Join(workspacePath, "data.json")
	if content, err := os.ReadFile(dataPath); err == nil {
		var data map[string]any
		if err := json.Unmarshal(content, &data); err == nil {
			return data
		}
	}

	return nil
}

func getLastPackagePart(packagePath string) string {
	parts := strings.Split(packagePath, ".")
	if len(parts) == 0 {
		return ""
	}
	return parts[len(parts)-1]
}

func generateTestPackage(packagePath string) string {
	lastPart := getLastPackagePart(packagePath)
	if lastPart == "" {
		return "test"
	}
	return lastPart + "_test"
}

func generateTestName(ruleName string) string {
	return fmt.Sprintf("test_%s", ruleName)
}

func generateRuleCall(packagePath, ruleName string) string {
	packageName := getLastPackagePart(packagePath)
	if packageName == "" {
		return ruleName
	}
	return fmt.Sprintf("%s.%s", packageName, ruleName)
}
