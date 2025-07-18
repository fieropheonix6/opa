// Copyright 2023 The OPA Authors.  All rights reserved.
// Use of this source code is governed by an Apache2
// license that can be found in the LICENSE file.

package rego

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/open-policy-agent/opa/v1/ast"
	"github.com/open-policy-agent/opa/v1/ir"
	"github.com/open-policy-agent/opa/v1/topdown"
	"github.com/open-policy-agent/opa/v1/tracing"
	"github.com/open-policy-agent/opa/v1/types"
)

type testPlugin struct {
	builtinFuncs map[string]*topdown.Builtin
	state        string
}

func (*testPlugin) IsTarget(t string) bool {
	return t == "foo"
}

func (*testPlugin) PrepareForEval(_ context.Context, _ *ir.Policy, po ...PrepareOption) (TargetPluginEval, error) {
	pc := &PrepareConfig{}
	for _, o := range po {
		o(pc)
	}
	return &testPlugin{
		builtinFuncs: pc.BuiltinFuncs(),
		state:        "newstate",
	}, nil
}

func (t *testPlugin) Eval(_ context.Context, _ *EvalContext, rt ast.Value) (ast.Value, error) {
	if rt != nil {
		return ast.NewSet(ast.NewTerm(ast.NewObject(
			[2]*ast.Term{ast.StringTerm("^term1"), ast.ObjectTerm([2]*ast.Term{ast.StringTerm(t.state), ast.NewTerm(rt)})},
		))), nil
	}
	return ast.NewSet(ast.NewTerm(ast.NewObject(
		[2]*ast.Term{ast.StringTerm("^term1"), ast.StringTerm(t.state)},
	))), nil
}

// Warning(philipc): This test modifies package variables, which means it cannot
// be run safely in parallel with other tests.
func TestTargetViaPlugin(t *testing.T) {
	tp := testPlugin{}
	RegisterPlugin("rego.target.foo", &tp)
	t.Cleanup(resetPlugins)
	r := New(
		Query("input"),
		Input("original-input"),
		Target("foo"),
		Runtime(ast.StringTerm("runtime")),
	)
	assertEval(t, r, `[[{"newstate": "runtime"}]]`)
}

type defaultPlugin struct {
	testPlugin
}

func (*defaultPlugin) IsTarget(t string) bool { return t == "" || t == "foo" }

// Warning(philipc): This test modifies package variables, which means it cannot
// be run safely in parallel with other tests.
func TestTargetViaDefaultPlugin(t *testing.T) {
	t.Run("no target", func(t *testing.T) {
		tp := defaultPlugin{testPlugin{}}
		RegisterPlugin("rego.target.foo", &tp)
		t.Cleanup(resetPlugins)
		r := New(
			Query("input"),
			Input("original-input"),
		)
		assertEval(t, r, `[["newstate"]]`)
	})

	t.Run("other target NOT overridden", func(t *testing.T) {
		tp := defaultPlugin{testPlugin{}}
		RegisterPlugin("rego.target.foo", &tp)
		t.Cleanup(resetPlugins)
		r := New(
			Query("input"),
			Input("original-input"),
			Target("rego"),
		)
		assertEval(t, r, `[["original-input"]]`)
	})
}

// Warning(philipc): This test modifies package variables, which means it cannot
// be run safely in parallel with other tests.
func TestPluginPrepareOptions(t *testing.T) {
	ctx := context.Background()
	tp := testPlugin{}
	RegisterPlugin("rego.target.foo", &tp)
	t.Cleanup(resetPlugins)

	t.Run("passed to PrepareForEval", func(t *testing.T) {
		r := New(
			Query("input"),
			Input("original-input"),
			Target("foo"),
			Runtime(ast.StringTerm("runtime")),
		)
		bi := map[string]*topdown.Builtin{
			"count": {
				Decl: ast.BuiltinMap["count"],
				Func: topdown.GetBuiltin("count"),
			},
		}
		pq, err := r.PrepareForEval(ctx, WithBuiltinFuncs(bi))
		if err != nil {
			t.Fatalf("PrepareForEval: %v", err)
		}
		assertPreparedEvalQueryEval(t, pq, nil, `[[{"newstate": "runtime"}]]`)

		// NOTE(sr): To assert what we want, we'll have to reach into the internals
		// here. Typically, the _effect_ of the PrepareOptions passed to the plugin
		// would be in the evalution done by the plugin. But our test plugin here does
		// not really do anything.
		internals := r.targetPrepState.(*testPlugin)
		act, exp := internals.builtinFuncs, bi
		if diff := cmp.Diff(exp, act,
			cmpopts.IgnoreUnexported(ast.Builtin{}, types.Function{}),
			cmpopts.IgnoreFields(topdown.Builtin{}, "Func")); diff != "" {
			t.Errorf("unexpected result (-want, +got):\n%s", diff)
		}
	})

	t.Run("passed to New", func(t *testing.T) {
		cpy := ast.BuiltinMap["count"]
		cpy.Description = ""
		cpy.Categories = nil
		bi := map[string]*topdown.Builtin{
			"count": {
				Decl: cpy,
				Func: topdown.GetBuiltin("count"),
			},
		}
		r := New(
			Query("input"),
			Input("original-input"),
			Target("foo"),
			Runtime(ast.StringTerm("runtime")),
			Function1(&Function{
				Name: "count",
				Decl: bi["count"].Decl.Decl,
			}, func(BuiltinContext, *ast.Term) (*ast.Term, error) { return nil, nil }),
		)
		assertEval(t, r, `[[{"newstate": "runtime"}]]`)

		// NOTE(sr): To assert what we want, we'll have to reach into the internals
		// here. Typically, the _effect_ of the PrepareOptions passed to the plugin
		// would be in the evalution done by the plugin. But our test plugin here does
		// not really do anything.
		internals := r.targetPrepState.(*testPlugin)
		act, exp := internals.builtinFuncs, bi
		if diff := cmp.Diff(exp, act,
			cmpopts.IgnoreUnexported(ast.Builtin{}, types.Function{}),
			cmpopts.IgnoreFields(topdown.Builtin{}, "Func")); diff != "" {
			t.Errorf("unexpected result (-want, +got):\n%s", diff)
		}
	})
}

// Warning(philipc): This test modifies package variables, which means it cannot
// be run safely in parallel with other tests.
func TestDistributedTracingOptsOnEvalContext(t *testing.T) {
	tp := testPluginDT{}
	RegisterPlugin("rego.target.foo_dt", &tp)
	t.Cleanup(resetPlugins)
	r := New(
		Query("input"),
		Target("foo_dt"),
		Runtime(ast.StringTerm("runtime")),
		DistributedTracingOpts(tracing.NewOptions("hey")),
	)
	assertEval(t, r, `[[{"x":"hey"}]]`)
}

type testPluginDT struct{}

func (*testPluginDT) IsTarget(t string) bool {
	return t == "foo_dt"
}

func (t *testPluginDT) PrepareForEval(context.Context, *ir.Policy, ...PrepareOption) (TargetPluginEval, error) {
	return t, nil
}

func (*testPluginDT) Eval(_ context.Context, ectx *EvalContext, _ ast.Value) (ast.Value, error) {
	if l := len(ectx.TracingOpts()); l != 1 {
		return nil, fmt.Errorf("expected ectx.TracingOpts of len 1, got %d", l)
	}
	return ast.NewSet(ast.NewTerm(ast.NewObject(
		[2]*ast.Term{
			ast.StringTerm("^term1"),
			ast.ObjectTerm(
				[2]*ast.Term{
					ast.StringTerm("x"),
					ast.StringTerm(fmt.Sprintf("%v", ectx.TracingOpts()[0])),
				}),
		}))), nil
}

func resetPlugins() {
	targetPlugins = map[string]TargetPlugin{}
}
