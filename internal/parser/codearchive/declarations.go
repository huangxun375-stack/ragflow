package codearchive

import (
	"strings"
	"unicode/utf8"

	ts "github.com/tree-sitter/go-tree-sitter"
)

func declarationSymbol(n *ts.Node, source []byte, scope string) (symbol, bool) {
	nameNode := declarationName(n)
	if nameNode == nil {
		return symbol{}, false
	}
	name := strings.Join(strings.Fields(nameNode.Utf8Text(source)), " ")
	kind := n.Kind()
	if kind == "method_declaration" {
		if receiver := n.ChildByFieldName("receiver"); receiver != nil {
			if typ := receiverType(receiver, source); typ != "" {
				scope = typ
			}
		}
	}
	name = strings.ReplaceAll(name, "::", ".")
	if scope != "" && !strings.HasPrefix(name, scope+".") {
		name = scope + "." + name
	}
	unit := completeSpan(n, source)
	end := int(n.EndByte())
	if body := declarationBody(n); body != nil {
		end = int(body.StartByte())
	}
	signature := compactLine(string(source[int(n.StartByte()):end]), 240)
	doc := ""
	if unit.start < int(n.StartByte()) {
		prefix := strings.TrimSpace(string(source[unit.start:int(n.StartByte())]))
		if strings.HasPrefix(prefix, "//") || strings.HasPrefix(prefix, "/*") || strings.HasPrefix(prefix, "#") || strings.HasPrefix(prefix, "--") {
			doc, _, _ = strings.Cut(prefix, "\n")
			doc = compactLine(strings.Trim(doc, " /*#-\t"), 160)
		}
	}
	if doc == "" && (kind == "function_definition" || kind == "class_definition") {
		if body := n.ChildByFieldName("body"); body != nil && body.NamedChildCount() > 0 {
			first := strings.TrimSpace(body.NamedChild(0).Utf8Text(source))
			if strings.HasPrefix(first, "\"\"\"") || strings.HasPrefix(first, "'''") {
				first = strings.TrimLeft(first, "\"'")
				first, _, _ = strings.Cut(first, "\n")
				doc = compactLine(strings.TrimRight(first, "\"'"), 160)
			}
		}
	}
	return symbol{name: name, kind: kind, start: unit.start, end: unit.end, signature: signature, doc: doc}, true
}

func receiverType(n *ts.Node, source []byte) string {
	if n.Kind() == "type_identifier" {
		return n.Utf8Text(source)
	}
	for i := uint(0); i < n.NamedChildCount(); i++ {
		if name := receiverType(n.NamedChild(i), source); name != "" {
			return name
		}
	}
	return ""
}

func declarationBody(n *ts.Node) *ts.Node {
	if body := n.ChildByFieldName("body"); body != nil {
		return body
	}
	if value := n.ChildByFieldName("value"); value != nil {
		if body := value.ChildByFieldName("body"); body != nil {
			return body
		}
	}
	kind := n.Kind()
	if kind == "function_declaration" || kind == "function_definition" {
		for i := uint(0); i < n.NamedChildCount(); i++ {
			child := n.NamedChild(i)
			if child.Kind() == "block" {
				return child
			}
		}
	}
	if typ := n.ChildByFieldName("type"); typ != nil {
		if body := typ.ChildByFieldName("body"); body != nil {
			return body
		}
		for i := uint(0); i < typ.NamedChildCount(); i++ {
			child := typ.NamedChild(i)
			if child.Kind() == "field_declaration_list" || child.Kind() == "interface_body" {
				return child
			}
		}
	}
	return nil
}

// Expression callables own their locals even when the expression has an
// internal name (for example, a named JavaScript function expression).
func expressionCallable(n *ts.Node) bool {
	switch n.Kind() {
	case "arrow_function", "function_expression", "generator_function", "lambda", "lambda_expression", "func_literal":
		return true
	case "function_definition":
		return declarationName(n) == nil
	}
	return false
}

func callableField(n *ts.Node) bool {
	value := n.ChildByFieldName("value")
	return value != nil && expressionCallable(value)
}

func containsCallable(n *ts.Node) bool {
	if n.Kind() == "function_declarator" {
		return true
	}
	for i := uint(0); i < n.NamedChildCount(); i++ {
		if containsCallable(n.NamedChild(i)) {
			return true
		}
	}
	return false
}

func containerDeclaration(kind string) bool {
	switch kind {
	case "class_definition", "class_declaration", "interface_declaration", "enum_declaration", "record_declaration",
		"namespace_definition", "class_specifier", "struct_specifier", "enum_specifier", "union_specifier", "type_spec":
		return true
	}
	return false
}

func compactLine(text string, limit int) string {
	return truncateUTF8(strings.Join(strings.Fields(text), " "), limit)
}

func truncateUTF8(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	end := max(0, limit-len(" …"))
	for end > 0 && !utf8.RuneStart(text[end]) {
		end--
	}
	return strings.TrimRight(text[:end], " \t") + " …"
}
