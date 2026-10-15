package model_test

import (
	"go/ast"
	"go/constant"
	"go/parser"
	"go/token"
	"go/types"
	"strings"
	"testing"

	"github.com/labring/aiproxy/core/model"
)

// TestChannelTypeConstantsAreUnique parses chtype.go so a new constant that
// reuses a number (an upstream channel landing on one of ours, e.g. fal=59)
// fails here even if someone also drops its name-map entry to make the
// duplicate-key compile error go away.
func TestChannelTypeConstantsAreUnique(t *testing.T) {
	fset := token.NewFileSet()

	file, err := parser.ParseFile(fset, "chtype.go", nil, 0)
	if err != nil {
		t.Fatalf("parse chtype.go: %v", err)
	}

	conf := types.Config{Importer: nil, Error: func(error) {}}
	info := &types.Info{Defs: map[*ast.Ident]types.Object{}}
	// Type-check errors (unresolved imports) are ignored: only constant values
	// are needed and they do not depend on imports.
	_, _ = conf.Check("model", fset, []*ast.File{file}, info)

	seen := map[int64]string{}
	count := 0

	for ident, obj := range info.Defs {
		c, ok := obj.(*types.Const)
		if !ok || c.Type() == nil || c.Type().String() != "model.ChannelType" {
			continue
		}

		v, exact := constant.Int64Val(c.Val())
		if !exact {
			t.Fatalf("%s: non-integer channel type", ident.Name)
		}

		count++

		if prev, dup := seen[v]; dup {
			t.Errorf("channel type %d used by both %s and %s", v, prev, ident.Name)
			continue
		}

		seen[v] = ident.Name

		if name := model.ChannelType(v).String(); strings.HasPrefix(name, "unknow(") {
			t.Errorf("%s (%d) has no entry in channelTypeNames", ident.Name, v)
		}
	}

	if count < 40 {
		t.Fatalf("found only %d ChannelType constants; parser did not see chtype.go", count)
	}

	if name := seen[int64(model.ChannelTypeFal)]; name != "ChannelTypeFal" {
		t.Fatalf("channel type %d belongs to %q, want ChannelTypeFal", model.ChannelTypeFal, name)
	}
}
