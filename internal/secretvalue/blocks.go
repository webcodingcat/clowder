package secretvalue

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/webcodingcat/clowder/internal/yamlpath"
)

// header is the line that a value starts on, from the tag on: a literal
// block scalar with no indentation indicator, as secret encrypt writes it.
// Only such a block has lines of its own, which Rewrite can replace.
var header = regexp.MustCompile(`^!secret[ \t]+\|[-+]?[ \t]*(#.*)?$`)

// Document is one YAML document of a file, with its values taken out.
type Document struct {
	// Node is the document with each value's path in its place, as a plain
	// string, ready to decode.
	Node *yaml.Node
	// Secrets holds each value's armor by its path (yamlpath).
	Secrets map[string]string
}

// Extract reads every document of content and takes the values out. A tag
// on anything other than a block scalar is an error. The caller checks
// whether a path may hold a value.
func Extract(content []byte) ([]Document, error) {
	file, err := parse(content)
	if err != nil {
		return nil, err
	}
	documents := make([]Document, 0, len(file.documents))
	for index, node := range file.documents {
		secrets := map[string]string{}
		for _, found := range file.blocks[index] {
			if _, ok := secrets[found.path]; ok {
				return nil, fmt.Errorf("%s: two values at the same place", found.path)
			}
			secrets[found.path] = strings.TrimRight(found.node.Value, "\n") + "\n"
			found.node.Tag, found.node.Value, found.node.Style = "!!str", found.path, 0
		}
		documents = append(documents, Document{Node: node, Secrets: secrets})
	}
	return documents, nil
}

// Rewrite replaces the armor of every value in content with what change
// returns for it (document counts from 0). It leaves the rest as it is:
// the tag's line, indentation, comments, line endings and other documents.
func Rewrite(content []byte, change func(document int, path, armored string) (string, error)) ([]byte, error) {
	file, err := parse(content)
	if err != nil {
		return nil, err
	}
	var rewritten bytes.Buffer
	next := 0 // the first line not yet written
	for index, blocks := range file.blocks {
		for _, found := range blocks {
			armored, err := change(index, found.path, strings.TrimRight(found.node.Value, "\n")+"\n")
			if err != nil {
				return nil, fmt.Errorf("%s: %w", found.path, err)
			}
			replacement := strings.Split(strings.TrimRight(armored, "\n"), "\n")
			for _, line := range replacement {
				if strings.TrimSpace(line) == "" {
					return nil, fmt.Errorf("%s: the new armor has an empty line", found.path)
				}
			}
			for _, line := range file.lines[next:found.first] {
				rewritten.Write(line)
			}
			between, end := lineEnding(file.lines[found.first]), lineEnding(file.lines[found.last])
			for number, line := range replacement {
				rewritten.WriteString(found.indent + line)
				if number < len(replacement)-1 {
					rewritten.WriteString(between)
				} else {
					rewritten.WriteString(end)
				}
			}
			next = found.last + 1
		}
	}
	for _, line := range file.lines[next:] {
		rewritten.Write(line)
	}
	return rewritten.Bytes(), nil
}

// Block writes a value as it appears in a file, after its key: the tag on
// this line and the armor below it, each line after indent, which must be
// deeper than the key's indentation.
func Block(armored, indent string) string {
	var block strings.Builder
	block.WriteString(Tag + " |\n")
	for _, line := range strings.Split(strings.TrimRight(armored, "\n"), "\n") {
		block.WriteString(indent + line + "\n")
	}
	return block.String()
}

// parsed is a file as read for Extract and Rewrite: its lines with their
// endings, its documents, and per document its values in the order of
// their lines.
type parsed struct {
	lines     [][]byte
	documents []*yaml.Node
	blocks    [][]block
}

// block is a value in its file: the first and last lines of its armor
// (counted from 0, both included) and their indentation.
type block struct {
	path        string
	node        *yaml.Node
	first, last int
	indent      string
}

func parse(content []byte) (parsed, error) {
	file := parsed{lines: bytes.SplitAfter(content, []byte("\n"))}
	decoder := yaml.NewDecoder(bytes.NewReader(content))
	for {
		node := &yaml.Node{}
		err := decoder.Decode(node)
		if errors.Is(err, io.EOF) {
			return file, nil
		}
		if err != nil {
			return parsed{}, fmt.Errorf("read YAML: %w", err)
		}
		var blocks []block
		err = walk(node, "", true, func(tagged *yaml.Node, path string) error {
			found, err := file.locate(tagged, path)
			blocks = append(blocks, found)
			return err
		})
		if err != nil {
			return parsed{}, err
		}
		file.documents = append(file.documents, node)
		file.blocks = append(file.blocks, blocks)
	}
}

// walk calls visit for every node below node that is tagged !secret, in
// document order, with its path. A key that is not a scalar has no path,
// so a value below such a key is an error.
func walk(node *yaml.Node, path string, named bool, visit func(tagged *yaml.Node, path string) error) error {
	if node.Tag == Tag {
		if !named {
			return errors.New("a value below a key that is not a scalar")
		}
		if node.Kind != yaml.ScalarNode {
			return fmt.Errorf("%s: %s tags a block scalar, not a mapping or a list", path, Tag)
		}
		return visit(node, path)
	}
	switch node.Kind {
	case yaml.DocumentNode:
		for _, child := range node.Content {
			if err := walk(child, path, named, visit); err != nil {
				return err
			}
		}
	case yaml.SequenceNode:
		for index, child := range node.Content {
			if err := walk(child, yamlpath.Index(path, index), named, visit); err != nil {
				return err
			}
		}
	case yaml.MappingNode:
		for index := 0; index+1 < len(node.Content); index += 2 {
			key, value := node.Content[index], node.Content[index+1]
			if key.Tag == Tag {
				return fmt.Errorf("%s: %s tags a value, not a key", path, Tag)
			}
			isScalar := key.Kind == yaml.ScalarNode
			if err := walk(value, yamlpath.Key(path, key.Value), named && isScalar, visit); err != nil {
				return err
			}
		}
	}
	return nil
}

// locate finds a value's lines in the file and checks that they are laid
// out as secret encrypt writes them: the tag and the block's indicator on
// one line, and the armor below, indented further.
func (file parsed) locate(node *yaml.Node, path string) (block, error) {
	writeAs := fmt.Errorf("%s: write %s | and the armor below it, as clowder secret encrypt does", path, Tag)
	if node.Style&^yaml.TaggedStyle != yaml.LiteralStyle || node.Anchor != "" || node.Line < 1 || node.Line > len(file.lines) {
		return block{}, writeAs
	}
	tagLine := []rune(strings.TrimRight(string(file.lines[node.Line-1]), "\r\n"))
	if node.Column < 1 || node.Column > len(tagLine) || !header.MatchString(string(tagLine[node.Column-1:])) {
		return block{}, writeAs
	}
	// the armor starts on the next line that is not blank, indented more
	// than the tag's line
	first := node.Line
	for first < len(file.lines) && blank(file.lines[first]) {
		first++
	}
	if first == len(file.lines) || indentation(file.lines[first]) <= indentation(file.lines[node.Line-1]) {
		return block{}, fmt.Errorf("%s: the value is empty", path)
	}
	// it ends before the first line that is not blank and indented less, as
	// YAML ends a block scalar
	depth := indentation(file.lines[first])
	last := first
	for line := first + 1; line < len(file.lines); line++ {
		if blank(file.lines[line]) {
			continue
		}
		if indentation(file.lines[line]) < depth {
			break
		}
		last = line
	}
	found := block{path: path, node: node, first: first, last: last, indent: string(file.lines[first][:depth])}
	// the lines found must be the value YAML read, or Rewrite would cut the
	// file in the wrong place (a blank line before the armor, a tab)
	read := make([]string, 0, last-first+1)
	for _, line := range file.lines[first : last+1] {
		text := strings.TrimRight(string(line), "\r\n")
		if len(text) > depth {
			text = text[depth:]
		} else {
			text = ""
		}
		read = append(read, text)
	}
	if strings.Join(read, "\n") != strings.TrimRight(node.Value, "\n") {
		return block{}, writeAs
	}
	return found, nil
}

// indentation counts the spaces that a line starts with. YAML indents with
// spaces only.
func indentation(line []byte) int {
	return len(line) - len(bytes.TrimLeft(line, " "))
}

func blank(line []byte) bool {
	return len(bytes.TrimSpace(line)) == 0
}

// lineEnding returns the end of a line: "\r\n", "\n", or none on a file's
// last line.
func lineEnding(line []byte) string {
	switch {
	case bytes.HasSuffix(line, []byte("\r\n")):
		return "\r\n"
	case bytes.HasSuffix(line, []byte("\n")):
		return "\n"
	default:
		return ""
	}
}
