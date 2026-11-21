package codearchive

import (
	"sort"
	"strings"
	"unicode/utf8"

	ts "github.com/tree-sitter/go-tree-sitter"
)

func declarationSymbol(n *ts.Node, source []byte, scope string) (symbol, bool) {
	nameNode := declarationName(n)
	if nameNode == nil {
		return symbol{}, false
	}
	kind := n.Kind()
	if kind == "method_declaration" {
		if receiver := n.ChildByFieldName("receiver"); receiver != nil {
			if typ := receiverType(receiver, source); typ != "" {
				scope = typ
			}
		}
	}
	name := qualifiedSymbolName(nameNode, source, scope)
	body := declarationBody(n)
	declarator := n
	if kind == "function_declarator" {
		n = declarationOwner(n)
	}
	unit := completeSpan(n, source)
	end := int(n.EndByte())
	if body != nil {
		end = int(body.StartByte())
	}
	var signature string
	if kind == "function_declarator" {
		signature = declaratorSignature(declarator, n, source)
	} else {
		signature = signatureText(n, source, int(n.StartByte()), end)
	}
	signature = compactLine(signature, 240)
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

// C/C++ function declarators can sit below pointer or reference wrappers.
// Resolve their owning statement without crossing parameters or initializers.
func declarationOwner(n *ts.Node) *ts.Node {
	for parent := n.Parent(); parent != nil; parent = parent.Parent() {
		switch parent.Kind() {
		case "declaration", "field_declaration":
			return parent
		case "pointer_declarator", "reference_declarator", "parenthesized_declarator",
			"array_declarator", "attributed_declarator", "function_declarator":
			continue
		case "init_declarator":
			child := parent.ChildByFieldName("declarator")
			if child == nil || child.StartByte() > n.StartByte() || child.EndByte() < n.EndByte() {
				return n
			}
		default:
			return n
		}
	}
	return n
}

// The statement owns the source unit; each declarator owns its overview entry.
func declaratorSignature(n, owner *ts.Node, source []byte) string {
	var first, selected *ts.Node
	for i := uint(0); i < owner.NamedChildCount(); i++ {
		if owner.FieldNameForNamedChild(uint32(i)) != "declarator" {
			continue
		}
		child := owner.NamedChild(i)
		if first == nil {
			first = child
		}
		if child.StartByte() <= n.StartByte() && child.EndByte() >= n.EndByte() {
			selected = child
		}
	}
	if first == nil || selected == nil {
		return signatureText(n, source, int(n.StartByte()), int(n.EndByte()))
	}
	if selected.Kind() == "init_declarator" {
		selected = selected.ChildByFieldName("declarator")
	}
	text := signatureText(owner, source, int(owner.StartByte()), int(first.StartByte())) +
		signatureText(selected, source, int(selected.StartByte()), int(selected.EndByte()))
	if value := owner.ChildByFieldName("default_value"); value != nil && value.Kind() == "number_literal" && value.Utf8Text(source) == "0" {
		text += " = 0"
	}
	if owner.EndByte() > 0 && source[owner.EndByte()-1] == ';' {
		text += ";"
	}
	return text
}

// Default arguments and inline return types may themselves contain callables.
// Their bodies are source, but are never part of a structural signature.
func signatureText(root *ts.Node, source []byte, start, end int) string {
	var bodies []byteSpan
	var visit func(*ts.Node)
	visit = func(n *ts.Node) {
		if int(n.EndByte()) <= start || int(n.StartByte()) >= end {
			return
		}
		var excluded byteSpan
		if body := declarationBody(n); body != nil {
			kind := body.Kind()
			if expressionCallable(n) || kind == "block" || kind == "compound_statement" || kind == "statement_block" || kind == "constructor_body" {
				span := byteSpan{int(body.StartByte()), int(body.EndByte())}
				if span.start >= start && span.start < span.end && span.end <= end {
					bodies = append(bodies, span)
					excluded = span
				}
			}
		}
		for i := uint(0); i < n.NamedChildCount(); i++ {
			child := n.NamedChild(i)
			if excluded.start < excluded.end && int(child.StartByte()) >= excluded.start && int(child.EndByte()) <= excluded.end {
				continue
			}
			visit(child)
		}
	}
	visit(root)
	sort.Slice(bodies, func(i, j int) bool {
		if bodies[i].start == bodies[j].start {
			return bodies[i].end > bodies[j].end
		}
		return bodies[i].start < bodies[j].start
	})
	var text strings.Builder
	for _, body := range bodies {
		if body.start < start {
			continue
		}
		text.Write(source[start:body.start])
		if source[body.start] == '{' {
			text.WriteString("{ … }")
		} else {
			text.WriteString("…")
		}
		start = body.end
	}
	text.Write(source[start:end])
	return text.String()
}

func qualifiedSymbolName(node *ts.Node, source []byte, scope string) string {
	if node == nil {
		return ""
	}
	name := strings.ReplaceAll(strings.Join(strings.Fields(node.Utf8Text(source)), " "), "::", ".")
	if scope != "" && !strings.HasPrefix(name, scope+".") {
		return scope + "." + name
	}
	return name
}

func unwrapDeclarator(n *ts.Node) *ts.Node {
	for n.Kind() == "parenthesized_declarator" && n.NamedChildCount() == 1 {
		n = n.NamedChild(0)
	}
	return n
}

// A direct alias names the aggregate itself. Pointer and array aliases do not
// own its methods; use the tag, or leave the members unqualified if anonymous.
func typedefMemberName(n *ts.Node) *ts.Node {
	for i := uint(0); i < n.NamedChildCount(); i++ {
		if n.FieldNameForNamedChild(uint32(i)) == "declarator" {
			if d := unwrapDeclarator(n.NamedChild(i)); d.Kind() == "type_identifier" {
				return d
			}
		}
	}
	if typ := n.ChildByFieldName("type"); typ != nil {
		return typ.ChildByFieldName("name")
	}
	return nil
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
	case "class_definition", "class_declaration", "abstract_class_declaration", "interface_declaration", "enum_declaration", "record_declaration",
		"namespace_definition", "class_specifier", "struct_specifier", "enum_specifier", "union_specifier", "type_spec", "type_alias", "annotation_type_declaration":
		return true
	}
	return false
}

func classInitializer(n *ts.Node) bool {
	switch n.Kind() {
	case "static_initializer", "class_static_block":
		return true
	case "block":
		parent := n.Parent()
		return parent != nil && parent.Kind() == "class_body"
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
