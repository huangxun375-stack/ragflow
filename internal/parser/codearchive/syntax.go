package codearchive

import (
	"bytes"
	"context"
	"path"
	"slices"
	"sort"
	"strings"

	ts "github.com/tree-sitter/go-tree-sitter"
	tspack "github.com/xberg-io/tree-sitter-language-pack/packages/go"
	"golang.org/x/net/html"
)

type byteSpan struct{ start, end int }
type symbol struct {
	name, kind     string
	start, end     int
	signature, doc string
}
type syntax struct {
	spans      []byteSpan
	symbols    []symbol
	fallback   bool
	errorLines map[int]struct{}
	language   string
	tree       *ts.Tree
	source     []byte
}

func (s *syntax) close() {
	if s.tree != nil {
		s.tree.Close()
		s.tree = nil
	}
	s.source = nil
}

// lineNode returns the largest named node starting at the first nonblank byte,
// or the enclosing named node when the line starts with a continuation token.
func (s *syntax) lineNode(start, end int) (*ts.Node, int) {
	line := bytes.TrimLeft(s.source[start:end], " \t\r\n\v\f")
	if len(line) == 0 {
		return nil, end
	}
	at := end - len(line)
	n := s.tree.RootNode().NamedDescendantForByteRange(uint(at), uint(at+1))
	if n == nil || int(n.StartByte()) != at {
		return n, at
	}
	for parent := n.Parent(); parent != nil && int(parent.StartByte()) == at; parent = n.Parent() {
		n = parent
	}
	return n, at
}

func (s *syntax) cutDepth(lines []int, line int) (int, bool) {
	if s == nil || s.tree == nil || line < 0 || line >= len(lines) {
		return 0, false
	}
	end := len(s.source)
	if line+1 < len(lines) {
		end = lines[line+1]
	}
	n, at := s.lineNode(lines[line], end)
	if n == nil || !n.IsNamed() || n.IsError() || n.IsMissing() || int(n.StartByte()) != at {
		return 0, false
	}
	for previous := line - 1; previous >= 0; previous-- {
		start, end := lines[previous], lines[previous+1]
		if len(bytes.TrimSpace(s.source[start:end])) == 0 {
			continue
		}
		p, _ := s.lineNode(start, end)
		if p != nil && commentKind(p.Kind()) && (int(p.EndByte()) >= end || len(bytes.TrimSpace(s.source[int(p.EndByte()):end])) == 0) {
			return 0, false
		}
		break
	}
	depth := 0
	for parent := n.Parent(); parent != nil; parent = parent.Parent() {
		depth++
	}
	return depth, true
}

// CheckGrammars verifies every grammar required by the policy without allowing
// the language pack's download fallback. Call it after configuring the cache.
func CheckGrammars(policy Policy) error {
	if err := policy.Validate(); err != nil {
		return err
	}
	required := make(map[string]bool)
	for _, extension := range policy.Extensions {
		language := languages[extension]
		if !slices.Contains(policy.Languages, language) {
			continue
		}
		if language == "vue" {
			for _, embedded := range []string{"html", "javascript", "typescript", "tsx"} {
				required[embedded] = true
			}
		} else {
			required[language] = true
		}
		if extension == ".h" {
			required["c"] = true
		}
	}
	for language := range required {
		tree, err := parseTree(context.Background(), language, nil)
		if err != nil {
			return err
		}
		tree.Close()
	}
	return nil
}

func parseTree(ctx context.Context, language string, source []byte) (*ts.Tree, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// GetLanguage can download a missing parser. Check the installed cache first
	// so unsupported deployment never invokes that network fallback.
	if !slices.Contains(tspack.DownloadedLanguages(), language) {
		return nil, fail("ZIP_UNSUPPORTED_DEPLOYMENT")
	}
	grammar, err := tspack.GetLanguage(language)
	if err != nil {
		return nil, fail("ZIP_UNSUPPORTED_DEPLOYMENT")
	}
	p := ts.NewParser()
	defer p.Close()
	if err := p.SetLanguage(grammar); err != nil {
		return nil, fail("ZIP_UNSUPPORTED_DEPLOYMENT")
	}
	tree := p.ParseWithOptions(func(offset int, _ ts.Point) []byte {
		if offset >= len(source) {
			return nil
		}
		return source[offset:]
	}, nil, &ts.ParseOptions{ProgressCallback: func(ts.ParseState) bool { return ctx.Err() != nil }})
	if err := ctx.Err(); err != nil {
		if tree != nil {
			tree.Close()
		}
		return nil, err
	}
	if tree == nil {
		return nil, fail("ZIP_PARSE_FAILED")
	}
	return tree, nil
}

func analyze(ctx context.Context, f SourceFile, errorRatio float64) (syntax, error) {
	if f.Language == "vue" {
		return analyzeVue(ctx, f.Text, errorRatio)
	}
	parsed, err := analyzeLanguage(ctx, f.Language, f.Text, errorRatio)
	if err != nil {
		return syntax{}, err
	}
	if strings.EqualFold(path.Ext(f.Path), ".h") && parsed.fallback {
		c, err := analyzeLanguage(ctx, "c", f.Text, errorRatio)
		if err != nil {
			parsed.close()
			return syntax{}, err
		}
		if len(c.errorLines) < len(parsed.errorLines) {
			parsed.close()
			parsed = c
		} else {
			c.close()
		}
	}
	return parsed, nil
}

func analyzeLanguage(ctx context.Context, language, text string, errorRatio float64) (syntax, error) {
	source := []byte(text)
	tree, err := parseTree(ctx, language, source)
	if err != nil {
		return syntax{}, err
	}
	parsed := syntax{language: language, errorLines: make(map[int]struct{}), tree: tree, source: source}
	transferred := false
	defer func() {
		if !transferred {
			parsed.close()
		}
	}()
	root := tree.RootNode()
	if root.HasError() {
		if err := collectErrorLines(ctx, root, len(lineStarts(text)), parsed.errorLines); err != nil {
			return syntax{}, err
		}
	}
	var walk func(*ts.Node, string) error
	walk = func(n *ts.Node, scope string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if n.IsError() {
			return nil
		}
		kind := n.Kind()
		if expressionCallable(n) {
			return nil
		}
		if kind == "field_declaration" && !containsCallable(n) {
			return nil
		}
		if (kind == "public_field_definition" || kind == "field_definition") && !callableField(n) {
			return nil
		}
		if declarationKind(kind) {
			if s, ok := declarationSymbol(n, source, scope); ok {
				parsed.spans = append(parsed.spans, byteSpan{s.start, s.end})
				parsed.symbols = append(parsed.symbols, s)
				if kind == "type_definition" {
					if body := declarationBody(n); body != nil {
						owner := typedefMemberName(n)
						memberScope := scope
						if owner != nil {
							memberScope = qualifiedSymbolName(owner, source, scope)
						}
						return walk(body, memberScope)
					}
				}
				if !containerDeclaration(kind) {
					// Local declarations belong to their enclosing callable, not
					// to the file's retrieval vocabulary.
					return nil
				}
				scope = s.name
			}
		} else if strings.HasPrefix(kind, "import") || kind == "preproc_include" {
			// Imports have no declaration bodies and can appear in an overview.
			parsed.symbols = append(parsed.symbols, symbol{name: n.Utf8Text(source), kind: "import", start: int(n.StartByte()), end: int(n.EndByte())})
			return nil
		}
		count := n.NamedChildCount()
		for i := uint(0); i < count; i++ {
			if err := walk(n.NamedChild(i), scope); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(root, ""); err != nil {
		return syntax{}, err
	}
	parsed.applyErrorRatio(text, errorRatio)
	parsed.spans = normalizeSpans(parsed.spans, len(source))
	transferred = true
	return parsed, nil
}

// Tree-sitter ranges are half-open. A node ending at the start of a line does
// not cover that line, while a missing token still marks its insertion line.
func collectErrorLines(ctx context.Context, node *ts.Node, totalLines int, lines map[int]struct{}) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if node.IsError() || node.IsMissing() {
		start, end := int(node.StartPosition().Row), int(node.EndPosition().Row)
		if node.EndByte() > node.StartByte() && node.EndPosition().Column == 0 && end > start {
			end--
		}
		start, end = min(start, totalLines-1), min(end, totalLines-1)
		for line := start; line <= end; line++ {
			lines[line] = struct{}{}
		}
		if node.IsError() {
			return nil
		}
	}
	for i := uint(0); i < node.ChildCount(); i++ {
		if err := collectErrorLines(ctx, node.Child(i), totalLines, lines); err != nil {
			return err
		}
	}
	return nil
}

func (s *syntax) applyErrorRatio(text string, errorRatio float64) {
	if float64(len(s.errorLines))/float64(len(lineStarts(text))) > errorRatio {
		s.fallback = true
		s.spans = nil
		s.symbols = nil
		s.close()
	}
}

func declarationKind(kind string) bool {
	switch kind {
	case "function_definition", "function_declaration", "generator_function_declaration", "method_declaration", "class_definition",
		"class_declaration", "abstract_class_declaration", "method_definition", "interface_declaration", "enum_declaration",
		"type_alias_declaration", "record_declaration", "constructor_declaration", "namespace_definition",
		"class_specifier", "struct_specifier", "enum_specifier", "union_specifier", "type_spec", "type_alias",
		"variable_declarator", "function_declaration_statement", "function_declarator", "type_definition",
		"method_signature", "abstract_method_signature", "method_elem", "public_field_definition", "field_definition":
		return true
	}
	return false
}

func declarationName(n *ts.Node) *ts.Node {
	if name := n.ChildByFieldName("name"); name != nil {
		return name
	}
	if n.Kind() == "field_definition" {
		return n.ChildByFieldName("property")
	}
	declarator := n.ChildByFieldName("declarator")
	for declarator != nil {
		declarator = unwrapDeclarator(declarator)
		switch declarator.Kind() {
		case "identifier", "field_identifier", "type_identifier", "qualified_identifier", "operator_name", "destructor_name":
			return declarator
		}
		declarator = declarator.ChildByFieldName("declarator")
	}
	return nil
}

func completeSpan(n *ts.Node, source []byte) byteSpan {
	for parent := n.Parent(); parent != nil; parent = n.Parent() {
		switch parent.Kind() {
		case "type_declaration":
			declarations := 0
			count := parent.NamedChildCount()
			for i := uint(0); i < count; i++ {
				if declarationKind(parent.NamedChild(i).Kind()) {
					declarations++
				}
			}
			if declarations > 1 {
				parent = nil
			} else {
				n = parent
			}
		case "decorated_definition", "export_statement", "template_declaration", "lexical_declaration",
			"variable_declaration", "declaration", "field_declaration":
			n = parent
		default:
			parent = nil
		}
		if parent == nil {
			break
		}
	}
	s := byteSpan{int(n.StartByte()), int(n.EndByte())}
	// Keep a declaration's adjacent leading comments in the same structural
	// unit. A blank line or code before a comment makes it a separate unit.
	if !commentKind(n.Kind()) {
		for previous := n.PrevNamedSibling(); previous != nil && commentKind(previous.Kind()); previous = previous.PrevNamedSibling() {
			start, end := int(previous.StartByte()), int(previous.EndByte())
			gap := string(source[end:s.start])
			if strings.TrimSpace(gap) != "" || strings.Count(gap, "\n") > 1 {
				break
			}
			lineStart := bytes.LastIndexByte(source[:start], '\n') + 1
			if strings.TrimSpace(string(source[lineStart:start])) != "" {
				break
			}
			s.start = start
		}
	}
	// The C/C++ class/struct node excludes its terminating semicolon.
	if s.end < len(source) {
		at := s.end
		for at < len(source) && (source[at] == ' ' || source[at] == '\t') {
			at++
		}
		if at < len(source) && source[at] == ';' {
			s.end = at + 1
		}
	}
	return s
}

func commentKind(kind string) bool {
	return kind == "comment" || kind == "line_comment" || kind == "block_comment"
}

func normalizeSpans(spans []byteSpan, size int) []byteSpan {
	sort.Slice(spans, func(i, j int) bool {
		if spans[i].start != spans[j].start {
			return spans[i].start < spans[j].start
		}
		return spans[i].end > spans[j].end
	})
	result := spans[:0]
	for _, s := range spans {
		if s.start < 0 || s.end > size || s.start >= s.end {
			continue
		}
		if len(result) > 0 && result[len(result)-1] == s {
			continue
		}
		result = append(result, s)
	}
	return result
}

func analyzeVue(ctx context.Context, text string, errorRatio float64) (syntax, error) {
	tree, err := parseTree(ctx, "html", []byte(text))
	if err != nil {
		return syntax{}, err
	}
	defer tree.Close()
	parsed := syntax{language: "vue", errorLines: make(map[int]struct{})}
	root := tree.RootNode()
	if root.HasError() {
		if err := collectErrorLines(ctx, root, len(lineStarts(text)), parsed.errorLines); err != nil {
			return syntax{}, err
		}
	}
	for i := uint(0); i < root.NamedChildCount(); i++ {
		block := root.NamedChild(i)
		if block.Kind() != "script_element" {
			continue
		}
		language, known := "javascript", true
		for j := uint(0); j < block.NamedChildCount(); j++ {
			child := block.NamedChild(j)
			if child.Kind() == "start_tag" {
				language, known, err = scriptLanguage(child.Utf8Text([]byte(text)))
				if err != nil {
					return syntax{}, err
				}
			}
			// A script in an unsupported language stays unparsed source text;
			// Vue files are split by lines, so no bytes are lost.
			if child.Kind() != "raw_text" || !known {
				continue
			}
			offset := int(child.StartByte())
			// Evaluate the union against the complete Vue file after all blocks.
			script, err := analyzeLanguage(ctx, language, child.Utf8Text([]byte(text)), 1)
			if err != nil {
				return syntax{}, err
			}
			script.close()
			for line := range script.errorLines {
				parsed.errorLines[line+int(child.StartPosition().Row)] = struct{}{}
			}
			for _, s := range script.spans {
				parsed.spans = append(parsed.spans, byteSpan{s.start + offset, s.end + offset})
			}
			for _, s := range script.symbols {
				s.start += offset
				s.end += offset
				parsed.symbols = append(parsed.symbols, s)
			}
		}
	}
	parsed.applyErrorRatio(text, errorRatio)
	parsed.spans = normalizeSpans(parsed.spans, len(text))
	return parsed, nil
}

func scriptLanguage(tag string) (string, bool, error) {
	tokenizer := html.NewTokenizer(strings.NewReader(tag))
	if tokenizer.Next() != html.StartTagToken {
		return "", false, fail("ZIP_PARSE_FAILED")
	}
	for _, attr := range tokenizer.Token().Attr {
		if attr.Key != "lang" {
			continue
		}
		switch strings.ToLower(attr.Val) {
		case "js", "javascript", "jsx":
			return "javascript", true, nil
		case "ts", "typescript":
			return "typescript", true, nil
		case "tsx":
			return "tsx", true, nil
		default:
			return "", false, nil
		}
	}
	return "javascript", true, nil
}
