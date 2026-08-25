package ai

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"testing"
)

// declaredCritiqueRuleIDs parses the rule constants out of critique_rules.go so
// the registry cannot silently drift from the declared identifiers.
func declaredCritiqueRuleIDs(t *testing.T) map[string]CritiqueRuleID {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "critique_rules.go", nil, 0)
	if err != nil {
		t.Fatalf("parse critique_rules.go: %v", err)
	}
	declared := map[string]CritiqueRuleID{}
	for _, decl := range file.Decls {
		genDecl, ok := decl.(*ast.GenDecl)
		if !ok || genDecl.Tok != token.CONST {
			continue
		}
		for _, spec := range genDecl.Specs {
			value, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			ident, ok := value.Type.(*ast.Ident)
			if !ok || ident.Name != "CritiqueRuleID" {
				continue
			}
			for i, name := range value.Names {
				literal, ok := value.Values[i].(*ast.BasicLit)
				if !ok || literal.Kind != token.STRING {
					t.Fatalf("%s is not a string literal", name.Name)
				}
				unquoted, err := strconv.Unquote(literal.Value)
				if err != nil {
					t.Fatalf("unquote %s: %v", name.Name, err)
				}
				declared[name.Name] = CritiqueRuleID(unquoted)
			}
		}
	}
	if len(declared) == 0 {
		t.Fatal("no CritiqueRuleID constants found")
	}
	return declared
}

func TestCritiqueRuleDescriptorsAreExhaustive(t *testing.T) {
	declared := declaredCritiqueRuleIDs(t)
	for name, rule := range declared {
		descriptor, ok := critiqueRuleDescriptors[rule]
		if !ok {
			t.Errorf("%s (%q) has no critiqueRuleDescriptors entry", name, rule)
			continue
		}
		if descriptor.Effect != critiqueEffectWithhold && descriptor.Warning == "" {
			t.Errorf("%s (%q) publishes without a warning code", name, rule)
		}
		if descriptor.Effect == critiqueEffectWithhold && descriptor.Warning != "" {
			t.Errorf("%s (%q) withholds the analysis but declares warning %q", name, rule, descriptor.Warning)
		}
	}
	if len(critiqueRuleDescriptors) != len(declared) {
		t.Errorf("critiqueRuleDescriptors has %d entries, want %d declared rules", len(critiqueRuleDescriptors), len(declared))
	}
}

func TestCritiqueRuleIDsAreUnique(t *testing.T) {
	seen := map[CritiqueRuleID]string{}
	for name, rule := range declaredCritiqueRuleIDs(t) {
		if rule == "" {
			t.Errorf("%s has an empty rule id", name)
		}
		if previous, ok := seen[rule]; ok {
			t.Errorf("%s and %s share rule id %q", previous, name, rule)
		}
		seen[rule] = name
	}
}
