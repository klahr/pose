package config

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"

	"gopkg.in/yaml.v3"
)

// AddDotfile appends a dotfile entry to the YAML file at path, creating the file
// if needed. Comments are kept; indentation is normalized to two spaces. The
// returned undo puts back the previous content.
func AddDotfile(path, source, target string) (undo func() error, err error) {
	return edit(path, func(root *yaml.Node) error {
		seq, err := mapValue(root, "dotfiles", yaml.SequenceNode)
		if err != nil {
			return err
		}
		entry := &yaml.Node{Kind: yaml.MappingNode, Content: []*yaml.Node{
			scalar("source"), scalar(source), scalar("target"), scalar(target),
		}}
		seq.Content = append(seq.Content, entry)
		return nil
	})
}

// AddPackages appends the packages not already listed under packages.<manager>
// in the YAML file at path and returns those it added; see AddDotfile.
func AddPackages(path, manager string, pkgs []string) (added []string, undo func() error, err error) {
	undo, err = edit(path, func(root *yaml.Node) error {
		byManager, err := mapValue(root, "packages", yaml.MappingNode)
		if err != nil {
			return err
		}
		seq, err := mapValue(byManager, manager, yaml.SequenceNode)
		if err != nil {
			return fmt.Errorf("packages: %w", err)
		}
		if len(seq.Content) == 0 {
			seq.Style = yaml.FlowStyle
		}
		for _, p := range pkgs {
			if slices.ContainsFunc(seq.Content, func(n *yaml.Node) bool { return n.Value == p }) || slices.Contains(added, p) {
				continue
			}
			seq.Content = append(seq.Content, scalar(p))
			added = append(added, p)
		}
		return nil
	})
	return added, undo, err
}

// edit loads path as a YAML document (a missing or empty file is an empty
// mapping), applies fn to its top-level mapping and writes it back.
func edit(path string, fn func(root *yaml.Node) error) (func() error, error) {
	orig, err := os.ReadFile(path)
	existed := err == nil
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(orig, &doc); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if doc.Kind == 0 {
		doc.Kind = yaml.DocumentNode
	}
	if len(doc.Content) == 0 {
		doc.Content = []*yaml.Node{{Kind: yaml.MappingNode}}
	}
	root := doc.Content[0]
	if root.Kind == yaml.ScalarNode && root.Tag == "!!null" {
		*root = yaml.Node{Kind: yaml.MappingNode, HeadComment: root.HeadComment, FootComment: root.FootComment}
	}
	if root.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%s: top level must be a mapping", path)
	}
	if err := fn(root); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		return nil, err
	}
	return func() error {
		if !existed {
			return os.Remove(path)
		}
		return os.WriteFile(path, orig, 0o644)
	}, nil
}

// mapValue returns the value of key in mapping m, adding an empty one of kind
// if the key is missing or null.
func mapValue(m *yaml.Node, key string, kind yaml.Kind) (*yaml.Node, error) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value != key {
			continue
		}
		v := m.Content[i+1]
		if v.Kind == yaml.ScalarNode && v.Tag == "!!null" {
			*v = yaml.Node{Kind: kind, LineComment: v.LineComment}
		}
		if v.Kind != kind {
			return nil, fmt.Errorf("%s: unexpected type", key)
		}
		return v, nil
	}
	v := &yaml.Node{Kind: kind}
	m.Content = append(m.Content, scalar(key), v)
	return v, nil
}

func scalar(s string) *yaml.Node { return &yaml.Node{Kind: yaml.ScalarNode, Value: s} }
