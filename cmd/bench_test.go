// Copyright 2020 The OPA Authors.  All rights reserved.
// Use of this source code is governed by an Apache2
// license that can be found in the LICENSE file.

package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/open-policy-agent/opa/cmd/formats"
	"github.com/open-policy-agent/opa/internal/presentation"
	"github.com/open-policy-agent/opa/v1/ast"
	"github.com/open-policy-agent/opa/v1/bundle"
	"github.com/open-policy-agent/opa/v1/rego"
	"github.com/open-policy-agent/opa/v1/util"
	"github.com/open-policy-agent/opa/v1/util/test"
)

// Minimize the number of tests that *actually* run the benchmarks, they are pretty slow.
// Have one test that exercises the whole flow.
func TestRunBenchmark(t *testing.T) {
	if testing.Short() {
		t.Skip("too slow for testing.Short")
	}

	t.Parallel()

	params := testBenchParams()

	args := []string{"1 + 1"}
	var buf bytes.Buffer

	rc, err := benchMain(args, params, &buf, &goBenchRunner{})
	if err != nil {
		t.Fatalf("Unexpected error: %s", err)
	}

	if rc != 0 {
		t.Fatalf("Unexpected return code %d, expected 0", rc)
	}

	// Expect a json serialized benchmark result with histogram fields
	var br testing.BenchmarkResult
	err = util.UnmarshalJSON(buf.Bytes(), &br)
	if err != nil {
		t.Fatalf("Unexpected error unmarshalling output: %s", err)
	}

	if br.N == 0 || br.T == 0 || br.MemAllocs == 0 || br.MemBytes == 0 {
		t.Fatalf("Expected benchmark results to be non-zero, got: %+v", br)
	}

	if _, ok := br.Extra["histogram_timer_rego_query_eval_ns_count"]; !ok {
		t.Fatalf("Expected benchmark results to contain histogram_timer_rego_query_eval_ns_count, got: %+v", br)
	}

	if float64(br.N) != br.Extra["histogram_timer_rego_query_eval_ns_count"] {
		t.Fatalf("Expected 'histogram_timer_rego_query_eval_ns_count' to be equal to N")
	}
}

func TestRunBenchmarkWithQueryImport(t *testing.T) {
	if testing.Short() {
		t.Skip("too slow for testing.Short")
	}

	t.Parallel()

	params := testBenchParams()
	// We add the rego.v1 import ..
	params.imports = newrepeatedStringFlag([]string{"rego.v1"})

	// .. which provides the 'in' keyword
	args := []string{`"a" in ["a", "b", "c"]`}
	var buf bytes.Buffer

	rc, err := benchMain(args, params, &buf, &goBenchRunner{})
	if err != nil {
		t.Fatalf("Unexpected error: %s", err)
	}

	if rc != 0 {
		t.Fatalf("Unexpected return code %d, expected 0", rc)
	}

	// Expect a json serialized benchmark result with histogram fields
	var br testing.BenchmarkResult
	err = util.UnmarshalJSON(buf.Bytes(), &br)
	if err != nil {
		t.Fatalf("Unexpected error unmarshalling output: %s", err)
	}

	if br.N == 0 || br.T == 0 || br.MemAllocs == 0 || br.MemBytes == 0 {
		t.Fatalf("Expected benchmark results to be non-zero, got: %+v", br)
	}

	if _, ok := br.Extra["histogram_timer_rego_query_eval_ns_count"]; !ok {
		t.Fatalf("Expected benchmark results to contain histogram_timer_rego_query_eval_ns_count, got: %+v", br)
	}

	if float64(br.N) != br.Extra["histogram_timer_rego_query_eval_ns_count"] {
		t.Fatalf("Expected 'histogram_timer_rego_query_eval_ns_count' to be equal to N")
	}
}

func TestRunBenchmarkE2E(t *testing.T) {
	if testing.Short() {
		t.Skip("too slow for testing.Short")
	}

	t.Parallel()

	params := testBenchParams()
	params.e2e = true

	args := []string{"1 + 1"}
	var buf bytes.Buffer

	rc, err := benchMain(args, params, &buf, &goBenchRunner{})
	if err != nil {
		t.Fatalf("Unexpected error: %s", err)
	}

	if rc != 0 {
		t.Fatalf("Unexpected return code %d, expected 0", rc)
	}

	// Expect a json serialized benchmark result with histogram fields
	var br testing.BenchmarkResult
	err = util.UnmarshalJSON(buf.Bytes(), &br)
	if err != nil {
		t.Fatalf("Unexpected error unmarshalling output: %s", err)
	}

	if br.N == 0 || br.T == 0 || br.MemAllocs == 0 || br.MemBytes == 0 {
		t.Fatalf("Expected benchmark results to be non-zero, got: %+v", br)
	}

	if _, ok := br.Extra["histogram_timer_rego_query_eval_ns_count"]; !ok {
		t.Fatalf("Expected benchmark results to contain 'histogram_timer_rego_query_eval_ns_count', got: %+v", br)
	}

	if float64(br.N) != br.Extra["histogram_timer_rego_query_eval_ns_count"] {
		t.Fatalf("Expected 'histogram_timer_rego_query_eval_ns_count' to be equal to N")
	}

	if _, ok := br.Extra["histogram_timer_server_handler_ns_count"]; !ok {
		t.Fatalf("Expected benchmark results to contain 'histogram_timer_server_handler_ns_count', got: %+v", br)
	}

	if float64(br.N) != br.Extra["histogram_timer_server_handler_ns_count"] {
		t.Fatalf("Expected 'histogram_timer_server_handler_ns_count' to be equal to N")
	}
}

func TestRunBenchmarkE2EWithOPAConfigFile(t *testing.T) {
	if testing.Short() {
		t.Skip("too slow for testing.Short")
	}

	t.Parallel()

	fs := map[string]string{
		"/config.yaml": `{"decision_logs": {"console": true}}`,
	}

	test.WithTempFS(fs, func(testDirRoot string) {

		params := testBenchParams()
		params.e2e = true
		params.configFile = filepath.Join(testDirRoot, "config.yaml")

		args := []string{"1 + 1"}
		var buf bytes.Buffer

		rc, err := benchMain(args, params, &buf, &goBenchRunner{})
		if err != nil {
			t.Fatalf("Unexpected error: %s", err)
		}

		if rc != 0 {
			t.Fatalf("Unexpected return code %d, expected 0", rc)
		}

		// Expect a json serialized benchmark result with histogram fields
		var br testing.BenchmarkResult
		err = util.UnmarshalJSON(buf.Bytes(), &br)
		if err != nil {
			t.Fatalf("Unexpected error unmarshalling output: %s", err)
		}

		if br.N == 0 || br.T == 0 || br.MemAllocs == 0 || br.MemBytes == 0 {
			t.Fatalf("Expected benchmark results to be non-zero, got: %+v", br)
		}

		if _, ok := br.Extra["histogram_timer_rego_query_eval_ns_count"]; !ok {
			t.Fatalf("Expected benchmark results to contain 'histogram_timer_rego_query_eval_ns_count', got: %+v", br)
		}

		if float64(br.N) != br.Extra["histogram_timer_rego_query_eval_ns_count"] {
			t.Fatalf("Expected 'histogram_timer_rego_query_eval_ns_count' to be equal to N")
		}

		if _, ok := br.Extra["histogram_timer_server_handler_ns_count"]; !ok {
			t.Fatalf("Expected benchmark results to contain 'histogram_timer_server_handler_ns_count', got: %+v", br)
		}

		if float64(br.N) != br.Extra["histogram_timer_server_handler_ns_count"] {
			t.Fatalf("Expected 'histogram_timer_server_handler_ns_count' to be equal to N")
		}
	})
}

func TestRunBenchmarkFailFastE2E(t *testing.T) {
	if testing.Short() {
		t.Skip("too slow for testing.Short")
	}

	t.Parallel()

	params := testBenchParams()
	params.fail = true // configured to fail on undefined results
	params.e2e = true

	args := []string{"a := 1; a > 2"}
	var buf bytes.Buffer

	rc, err := benchMain(args, params, &buf, &goBenchRunner{})
	if err != nil {
		t.Fatalf("Unexpected error: %s", err)
	}

	if rc != 1 {
		t.Fatalf("Unexpected return code %d, expected 1", rc)
	}

	// Expect a json serialized benchmark result with histogram fields
	var pr presentation.Output
	err = util.UnmarshalJSON(buf.Bytes(), &pr)
	if err != nil {
		t.Fatalf("Unexpected error unmarshalling output: %s", err)
	}

	if len(pr.Errors) != 1 {
		t.Fatalf("Expected 1 error in result, got:\n\n%s\n", buf.String())
	}
}

func TestBenchPartialE2E(t *testing.T) {
	if testing.Short() {
		t.Skip("too slow for testing.Short")
	}

	t.Parallel()

	params := testBenchParams()
	params.partial = true
	params.fail = true
	params.e2e = true
	params.unknowns = []string{"input"}
	args := []string{"input.x > 0"}
	var buf bytes.Buffer

	rc, err := benchMain(args, params, &buf, &goBenchRunner{})
	if err != nil {
		t.Fatalf("Unexpected error: %s", err)
	}

	if rc != 0 {
		t.Fatalf("Unexpected return code %d, expected 0", rc)
	}

	var br testing.BenchmarkResult
	err = util.UnmarshalJSON(buf.Bytes(), &br)
	if err != nil {
		t.Fatalf("Unexpected error unmarshalling output: %s", err)
	}

	if br.N == 0 || br.T == 0 || br.MemAllocs == 0 || br.MemBytes == 0 {
		t.Fatalf("Expected benchmark results to be non-zero, got: %+v", br)
	}

	if _, ok := br.Extra["histogram_timer_rego_partial_eval_ns_count"]; !ok {
		t.Fatalf("Expected benchmark results to contain 'histogram_timer_rego_partial_eval_ns_count', got: %+v", br)
	}

	if float64(br.N) != br.Extra["histogram_timer_rego_partial_eval_ns_count"] {
		t.Fatalf("Expected 'histogram_timer_rego_partial_eval_ns_count' to be equal to N")
	}

	if _, ok := br.Extra["histogram_timer_server_handler_ns_count"]; !ok {
		t.Fatalf("Expected benchmark results to contain 'histogram_timer_server_handler_ns_count', got: %+v", br)
	}

	if float64(br.N) != br.Extra["histogram_timer_server_handler_ns_count"] {
		t.Fatalf("Expected 'histogram_timer_server_handler_ns_count' to be equal to N")
	}
}

func TestRunBenchmarkPartialFailFastE2E(t *testing.T) {
	if testing.Short() {
		t.Skip("too slow for testing.Short")
	}

	t.Parallel()

	params := testBenchParams()
	params.partial = true
	params.unknowns = []string{}
	params.fail = true
	params.e2e = true
	args := []string{"1 == 2"}
	var buf bytes.Buffer

	rc, err := benchMain(args, params, &buf, &goBenchRunner{})
	if err != nil {
		t.Fatalf("Unexpected error: %s", err)
	}

	if rc != 1 {
		t.Fatalf("Unexpected return code %d, expected 1", rc)
	}

	actual := buf.String()
	expected := `{
  "errors": [
    {
      "message": "undefined result"
    }
  ]
}
`

	if actual != expected {
		t.Fatalf("\nExpected:\n%s\n\nGot:\n%s\n", expected, actual)
	}
}

func TestRunBenchmarkFailFast(t *testing.T) {
	t.Parallel()

	params := testBenchParams()
	params.fail = true // configured to fail on undefined results

	args := []string{"a := 1; a > 2"}
	var buf bytes.Buffer

	rc, err := benchMain(args, params, &buf, &goBenchRunner{})
	if err != nil {
		t.Fatalf("Unexpected error: %s", err)
	}

	if rc != 1 {
		t.Fatalf("Unexpected return code %d, expected 1", rc)
	}

	// Expect a json serialized benchmark result with histogram fields
	var pr presentation.Output
	err = util.UnmarshalJSON(buf.Bytes(), &pr)
	if err != nil {
		t.Fatalf("Unexpected error unmarshalling output: %s", err)
	}

	if len(pr.Errors) != 1 {
		t.Fatalf("Expected 1 error in result, got:\n\n%s\n", buf.String())
	}
}

// mockBenchRunner lets us test the bench CLI operations without having to wait ~10 seconds
// while the actual benchmark runner does its thing.
type mockBenchRunner struct {
	onRun func(ctx context.Context, ectx *evalContext, params benchmarkCommandParams, f func(context.Context, ...rego.EvalOption) error) (testing.BenchmarkResult, error)
}

func (r *mockBenchRunner) run(ctx context.Context, ectx *evalContext, params benchmarkCommandParams, f func(context.Context, ...rego.EvalOption) error) (testing.BenchmarkResult, error) {
	if r.onRun != nil {
		return r.onRun(ctx, ectx, params, f)
	}
	return testing.BenchmarkResult{}, nil
}

func TestBenchPartial(t *testing.T) {
	t.Parallel()

	params := testBenchParams()
	params.partial = true
	params.fail = true
	args := []string{"input=1"}
	var buf bytes.Buffer

	rc, err := benchMain(args, params, &buf, &mockBenchRunner{})
	if err != nil {
		t.Fatalf("Unexpected error: %s", err)
	}

	if rc != 0 {
		t.Fatalf("Unexpected return code %d, expected 0", rc)
	}
}

func TestBenchMainErrPreparing(t *testing.T) {
	t.Parallel()

	params := testBenchParams()
	args := []string{"???"} // query compile error
	var buf bytes.Buffer

	rc, err := benchMain(args, params, &buf, &mockBenchRunner{})
	if err != nil {
		t.Fatalf("Unexpected error: %s", err)
	}

	if rc != 1 {
		t.Fatalf("Unexpected return code %d, expected 1", rc)
	}
}

func TestBenchMainErrRunningBenchmark(t *testing.T) {
	t.Parallel()

	params := testBenchParams()
	args := []string{"1+1"}
	var buf bytes.Buffer

	mockRunner := &mockBenchRunner{}
	mockRunner.onRun = func(_ context.Context, _ *evalContext, _ benchmarkCommandParams, _ func(context.Context, ...rego.EvalOption) error) (testing.BenchmarkResult, error) {
		return testing.BenchmarkResult{}, errors.New("error error error")
	}

	rc, err := benchMain(args, params, &buf, mockRunner)
	if err != nil {
		t.Fatalf("Unexpected error: %s", err)
	}

	if rc != 1 {
		t.Fatalf("Unexpected return code %d, expected 1", rc)
	}
}

func TestBenchMainWithCount(t *testing.T) {
	t.Parallel()

	params := testBenchParams()
	args := []string{"1+1"}
	var buf bytes.Buffer

	mockRunner := &mockBenchRunner{}

	params.count = 25
	actualCount := 0
	mockRunner.onRun = func(_ context.Context, _ *evalContext, _ benchmarkCommandParams, _ func(context.Context, ...rego.EvalOption) error) (testing.BenchmarkResult, error) {
		actualCount++
		return testing.BenchmarkResult{}, nil
	}

	rc, err := benchMain(args, params, &buf, mockRunner)
	if err != nil {
		t.Fatalf("Unexpected error: %s", err)
	}

	if rc != 0 {
		t.Fatalf("Unexpected return code %d, expected 0", rc)
	}

	if actualCount != params.count {
		t.Fatalf("Expected benchmark to be run %d times, only ran %d", params.count, actualCount)
	}
}

func TestBenchMainWithNegativeCount(t *testing.T) {
	t.Parallel()

	params := testBenchParams()
	args := []string{"1+1"}
	var buf bytes.Buffer

	mockRunner := &mockBenchRunner{}

	params.count = -1
	actualCount := 0
	mockRunner.onRun = func(_ context.Context, _ *evalContext, _ benchmarkCommandParams, _ func(context.Context, ...rego.EvalOption) error) (testing.BenchmarkResult, error) {
		actualCount++
		return testing.BenchmarkResult{}, nil
	}

	rc, err := benchMain(args, params, &buf, mockRunner)
	if err != nil {
		t.Fatalf("Unexpected error: %s", err)
	}

	if rc != 0 {
		t.Fatalf("Unexpected return code %d, expected 0", rc)
	}

	if actualCount != 0 {
		t.Fatalf("Expected benchmark to not be run, instead ran %d times", actualCount)
	}
}

func validateBenchMainPrep(t *testing.T, args []string, params benchmarkCommandParams) {
	t.Helper()

	var buf bytes.Buffer

	mockRunner := &mockBenchRunner{}

	mockRunner.onRun = func(ctx context.Context, ectx *evalContext, _ benchmarkCommandParams, _ func(context.Context, ...rego.EvalOption) error) (testing.BenchmarkResult, error) {

		// cheat and use the ectx to evalute the query to ensure the input setup on it was valid
		r := rego.New(ectx.regoArgs...)
		pq, err := r.PrepareForEval(ctx)
		if err != nil {
			return testing.BenchmarkResult{}, err
		}

		rs, err := pq.Eval(ctx, ectx.evalArgs...)
		if err != nil {
			return testing.BenchmarkResult{}, err
		}

		if len(rs) == 0 {
			return testing.BenchmarkResult{}, errors.New("expected result, got none")
		}

		return testing.BenchmarkResult{}, nil
	}

	rc, err := benchMain(args, params, &buf, mockRunner)
	if err != nil {
		t.Fatalf("Unexpected error: %s", err)
	}
	if rc != 0 {
		t.Fatalf("Unexpected return code %d, expected 0", rc)
	}
}

func TestBenchMainWithJSONInputFile(t *testing.T) {
	t.Parallel()

	params := testBenchParams()
	files := map[string]string{
		"/input.json": `{"x": 42}`,
	}
	args := []string{"input.x == 42"}
	test.WithTempFS(files, func(path string) {
		params.inputPath = filepath.Join(path, "input.json")

		validateBenchMainPrep(t, args, params)
	})
}

func TestBenchMainWithYAMLInputFile(t *testing.T) {
	t.Parallel()

	params := testBenchParams()
	files := map[string]string{
		"/input.yaml": `x: 42`,
	}
	args := []string{"input.x == 42"}
	test.WithTempFS(files, func(path string) {
		params.inputPath = filepath.Join(path, "input.yaml")

		validateBenchMainPrep(t, args, params)
	})
}

func TestBenchMainInvalidInputFile(t *testing.T) {
	t.Parallel()

	params := testBenchParams()
	files := map[string]string{
		"/input.yaml": `x: 42`,
	}
	args := []string{"1+1"}
	test.WithTempFS(files, func(path string) {
		params.inputPath = filepath.Join(path, "definitely", "not", "input.yaml")

		var buf bytes.Buffer

		rc, err := benchMain(args, params, &buf, &mockBenchRunner{})
		if err != nil {
			t.Fatalf("Unexpected error: %s", err)
		}
		if rc != 1 {
			t.Fatalf("Unexpected return code %d, expected 1", rc)
		}
	})
}

func TestBenchMainWithJSONInputFileE2E(t *testing.T) {
	if testing.Short() {
		t.Skip("too slow for testing.Short")
	}

	t.Parallel()

	params := testBenchParams()
	params.e2e = true
	files := map[string]string{
		"/input.json": `{"x": 42}`,
	}
	args := []string{"input.x == 42"}
	test.WithTempFS(files, func(path string) {
		params.inputPath = filepath.Join(path, "input.json")

		var buf bytes.Buffer

		rc, err := benchMain(args, params, &buf, &goBenchRunner{})
		if err != nil {
			t.Fatalf("Unexpected error: %s", err)
		}

		if rc != 0 {
			t.Fatalf("Unexpected return code %d, expected 0", rc)
		}
	})
}

func TestBenchMainWithYAMLInputFileE2E(t *testing.T) {
	if testing.Short() {
		t.Skip("too slow for testing.Short")
	}

	t.Parallel()

	params := testBenchParams()
	params.e2e = true
	files := map[string]string{
		"/input.yaml": `x: 42`,
	}
	args := []string{"input.x == 42"}
	test.WithTempFS(files, func(path string) {
		params.inputPath = filepath.Join(path, "input.yaml")

		var buf bytes.Buffer

		rc, err := benchMain(args, params, &buf, &goBenchRunner{})
		if err != nil {
			t.Fatalf("Unexpected error: %s", err)
		}

		if rc != 0 {
			t.Fatalf("Unexpected return code %d, expected 0", rc)
		}
	})
}

func TestBenchMainInvalidInputFileE2E(t *testing.T) {
	t.Parallel()

	params := testBenchParams()
	params.e2e = true
	files := map[string]string{
		"/input.yaml": `x: 42`,
	}
	args := []string{"1+1"}
	test.WithTempFS(files, func(path string) {
		params.inputPath = filepath.Join(path, "definitely", "not", "input.yaml")

		var buf bytes.Buffer

		rc, err := benchMain(args, params, &buf, &goBenchRunner{})
		if err != nil {
			t.Fatalf("Unexpected error: %s", err)
		}

		if rc != 1 {
			t.Fatalf("Unexpected return code %d, expected 1", rc)
		}
	})
}

func TestBenchMainWithBundleData(t *testing.T) {
	t.Parallel()

	params := testBenchParams()

	b := testBundle()

	files := map[string]string{
		"bundle.tar.gz": "",
	}

	test.WithTempFS(files, func(path string) {
		bundlePath := filepath.Join(path, "bundle.tar.gz")
		f, err := os.OpenFile(bundlePath, os.O_WRONLY, os.ModePerm)
		if err != nil {
			t.Fatalf("Unexpected error: %s", err)
		}

		err = bundle.Write(f, b)
		if err != nil {
			t.Fatalf("Unexpected error: %s", err)
		}

		err = params.bundlePaths.Set(bundlePath)
		if err != nil {
			t.Fatalf("Unexpected error: %s", err)
		}

		args := []string{"data.a.b.x"}

		validateBenchMainPrep(t, args, params)
	})
}

func TestBenchMainWithBundleDataE2E(t *testing.T) {
	if testing.Short() {
		t.Skip("too slow for testing.Short")
	}

	t.Parallel()

	params := testBenchParams()
	params.e2e = true

	b := testBundle()

	files := map[string]string{
		"bundle.tar.gz": "",
	}

	test.WithTempFS(files, func(path string) {
		bundlePath := filepath.Join(path, "bundle.tar.gz")
		f, err := os.OpenFile(bundlePath, os.O_WRONLY, os.ModePerm)
		if err != nil {
			t.Fatalf("Unexpected error: %s", err)
		}

		err = bundle.Write(f, b)
		if err != nil {
			t.Fatalf("Unexpected error: %s", err)
		}

		err = params.bundlePaths.Set(bundlePath)
		if err != nil {
			t.Fatalf("Unexpected error: %s", err)
		}

		args := []string{"data.a.b.x"}

		var buf bytes.Buffer

		rc, err := benchMain(args, params, &buf, &goBenchRunner{})
		if err != nil {
			t.Fatalf("Unexpected error: %s", err)
		}

		if rc != 0 {
			t.Fatalf("Unexpected return code %d, expected 0", rc)
		}
	})
}

func TestBenchMainWithDataE2E(t *testing.T) {
	if testing.Short() {
		t.Skip("too slow for testing.Short")
	}

	t.Parallel()

	params := testBenchParams()
	params.e2e = true

	mod := `package a.b
	import rego.v1

	x if {
	   data.a.b.c == 42
	}
	`

	files := map[string]string{
		"p.rego": mod,
	}

	test.WithTempFS(files, func(path string) {
		err := params.dataPaths.Set(filepath.Join(path, "p.rego"))
		if err != nil {
			t.Fatalf("Unexpected error: %s", err)
		}

		args := []string{"data.a.b.x"}

		var buf bytes.Buffer

		rc, err := benchMain(args, params, &buf, &goBenchRunner{})
		if err != nil {
			t.Fatalf("Unexpected error: %s", err)
		}

		if rc != 0 {
			t.Fatalf("Unexpected return code %d, expected 0", rc)
		}
	})
}

func TestBenchMainBadQueryE2E(t *testing.T) {
	if testing.Short() {
		t.Skip("too slow for testing.Short")
	}

	t.Parallel()

	params := testBenchParams()
	params.e2e = true
	args := []string{"foo.bar"}

	var buf bytes.Buffer

	rc, err := benchMain(args, params, &buf, &goBenchRunner{})
	if err != nil {
		t.Fatalf("Unexpected error: %s", err)
	}

	if rc != 1 {
		t.Fatalf("Unexpected return code %d, expected 1", rc)
	}
}

func TestBenchMain_DefaultRegoVersion(t *testing.T) {
	if testing.Short() {
		t.Skip("too slow for testing.Short")
	}

	t.Parallel()

	tests := []struct {
		note    string
		module  string
		query   string
		expErrs []string
	}{
		// These tests are slow, so we're not being completely exhaustive here.
		{
			note: "v0 module",
			module: `package test
a[x] {
	x := 42
}`,
			query: `data.test.a`,
			expErrs: []string{
				"mod.rego:2: rego_parse_error: `if` keyword is required before rule body",
				"mod.rego:2: rego_parse_error: `contains` keyword is required for partial set rules",
			},
		},
		{
			note: "v1 module",
			module: `package test
a contains x if {
	x := 42
}`,
			query: `data.test.a`,
		},
	}

	modes := []struct {
		name string
		e2e  bool
	}{
		{
			name: "run",
		},
		{
			name: "e2e",
			e2e:  true,
		},
	}

	for _, mode := range modes {
		for _, tc := range tests {
			t.Run(fmt.Sprintf("%s, %s", tc.note, mode.name), func(t *testing.T) {
				t.Parallel()

				files := map[string]string{
					"mod.rego": tc.module,
				}

				test.WithTempFS(files, func(path string) {
					params := testBenchParams()
					_ = params.outputFormat.Set(formats.Pretty)
					params.e2e = mode.e2e

					for n := range files {
						err := params.dataPaths.Set(filepath.Join(path, n))
						if err != nil {
							t.Fatalf("Unexpected error: %s", err)
						}
					}

					args := []string{tc.query}

					var buf bytes.Buffer
					rc, err := benchMain(args, params, &buf, &goBenchRunner{})

					if len(tc.expErrs) > 0 {
						if rc == 0 {
							t.Fatalf("Expected non-zero return code")
						}

						output := buf.String()
						for _, expErr := range tc.expErrs {
							if !strings.Contains(output, expErr) {
								t.Fatalf("Expected error:\n\n%s\n\ngot:\n\n%s", expErr, output)
							}
						}
					} else {
						if err != nil {
							t.Fatalf("Unexpected error: %s", err)
						}
						if rc != 0 {
							t.Fatalf("Unexpected return code %d, expected 0", rc)
						}
					}
				})
			})
		}
	}
}

func TestBenchMainCompatibleFlags(t *testing.T) {
	if testing.Short() {
		t.Skip("too slow for testing.Short")
	}

	t.Parallel()

	tests := []struct {
		note         string
		v0Compatible bool
		v1Compatible bool
		module       string
		query        string
		expErrs      []string
	}{
		// These tests are slow, so we're not being completely exhaustive here.
		{
			note:         "v0, keywords not used",
			v0Compatible: true,
			module: `package test
a[4] {
	1 == 1
}`,
			query: `data.test.a`,
		},
		{
			note:         "v0, no keywords imported",
			v0Compatible: true,
			module: `package test
a contains 4 if {
	1 == 1
}`,
			query: `data.test.a`,
			expErrs: []string{
				"rego_parse_error: var cannot be used for rule name",
				"rego_parse_error: number cannot be used for rule name",
			},
		},
		{
			note:         "v1, keywords not used",
			v1Compatible: true,
			module: `package test
a[4] {
	1 == 1
}`,
			query: `data.test.a`,
			expErrs: []string{
				"rego_parse_error: `if` keyword is required before rule body",
				"rego_parse_error: `contains` keyword is required for partial set rules",
			},
		},
		{
			note:         "v1, no keywords imported",
			v1Compatible: true,
			module: `package test
a contains 4 if {
	1 == 1
}`,
			query: `data.test.a`,
		},
		{
			note:         "v0+v1, keywords not used (v0 takes precedence)",
			v0Compatible: true,
			v1Compatible: true,
			module: `package test
a[4] {
	1 == 1
}`,
			query: `data.test.a`,
		},
	}

	modes := []struct {
		name string
		e2e  bool
	}{
		{
			name: "run",
		},
		{
			name: "e2e",
			e2e:  true,
		},
	}

	for _, mode := range modes {
		for _, tc := range tests {
			t.Run(fmt.Sprintf("%s, %s", tc.note, mode.name), func(t *testing.T) {
				t.Parallel()

				files := map[string]string{
					"mod.rego": tc.module,
				}

				test.WithTempFS(files, func(path string) {
					params := testBenchParams()
					_ = params.outputFormat.Set(formats.Pretty)
					params.v0Compatible = tc.v0Compatible
					params.v1Compatible = tc.v1Compatible
					params.e2e = mode.e2e

					for n := range files {
						err := params.dataPaths.Set(filepath.Join(path, n))
						if err != nil {
							t.Fatalf("Unexpected error: %s", err)
						}
					}

					args := []string{tc.query}

					var buf bytes.Buffer
					rc, err := benchMain(args, params, &buf, &goBenchRunner{})

					if len(tc.expErrs) > 0 {
						if rc == 0 {
							t.Fatalf("Expected non-zero return code")
						}

						output := buf.String()
						for _, expErr := range tc.expErrs {
							if !strings.Contains(output, expErr) {
								t.Fatalf("Expected error:\n\n%s\n\ngot:\n\n%s", expErr, output)
							}
						}
					} else {
						if err != nil {
							t.Fatalf("Unexpected error: %s", err)
						}
						if rc != 0 {
							t.Fatalf("Unexpected return code %d, expected 0", rc)
						}
					}
				})
			})
		}
	}
}

func TestBenchMainWithBundleRegoVersion(t *testing.T) {
	if testing.Short() {
		t.Skip("too slow for testing.Short")
	}

	t.Parallel()

	tests := []struct {
		note                   string
		bundleRegoVersion      int
		bundleFileRegoVersions map[string]int
		modules                map[string]string
		query                  string
		expErrs                []string
	}{
		// These tests are slow, so we're not being completely exhaustive here.
		{
			note:              "v0 bundle",
			bundleRegoVersion: 0,
			modules: map[string]string{
				"test.rego": `package test
a[4] {
	1 == 1
}`,
			},
			query: `data.test.a`,
		},
		{
			note:              "v0 bundle, no keywords imported",
			bundleRegoVersion: 0,
			modules: map[string]string{
				"test.rego": `package test
a contains 4 if {
	1 == 1
}`,
			},
			query: `data.test.a`,
			expErrs: []string{
				"rego_parse_error: var cannot be used for rule name",
				"rego_parse_error: number cannot be used for rule name",
			},
		},
		{
			note:              "v0 bundle, v1 per-file override",
			bundleRegoVersion: 0,
			bundleFileRegoVersions: map[string]int{
				"*/test2.rego": 1,
			},
			modules: map[string]string{
				"test1.rego": `package test
a[4] {
	1 == 1
}`,
				"test2.rego": `package test
b contains 4 if {
	1 == 1
}`,
			},
			query: `data.test.a`,
		},
		{
			note:              "v1 bundle, keywords not used",
			bundleRegoVersion: 1,
			modules: map[string]string{
				"test.rego": `package test
a[4] {
	1 == 1
}`,
			},
			query: `data.test.a`,
			expErrs: []string{
				"rego_parse_error: `if` keyword is required before rule body",
				"rego_parse_error: `contains` keyword is required for partial set rules",
			},
		},
		{
			note:              "v1, no keywords imported",
			bundleRegoVersion: 1,
			modules: map[string]string{
				"test.rego": `package test
a contains 4 if {
	1 == 1
}`,
			},
			query: `data.test.a`,
		},
	}

	bundleTypeCases := []struct {
		note string
		tar  bool
	}{
		{
			"bundle dir", false,
		},
		{
			"bundle tar", true,
		},
	}

	modes := []struct {
		name string
		e2e  bool
	}{
		{
			name: "run",
		},
		{
			name: "e2e",
			e2e:  true,
		},
	}

	for _, bundleType := range bundleTypeCases {
		for _, mode := range modes {
			for _, tc := range tests {
				t.Run(fmt.Sprintf("%s, %s, %s", bundleType.note, tc.note, mode.name), func(t *testing.T) {
					t.Parallel()

					files := map[string]string{}

					if bundleType.tar {
						files["bundle.tar.gz"] = ""
					} else {
						maps.Copy(files, tc.modules)

						manifest := bundle.Manifest{
							RegoVersion:      &tc.bundleRegoVersion,
							FileRegoVersions: tc.bundleFileRegoVersions,
						}
						manifest.Init()
						if b, err := json.Marshal(manifest); err != nil {
							t.Fatalf("Unexpected error: %s", err)
						} else {
							files[".manifest"] = string(b)
						}
					}

					test.WithTempFS(files, func(root string) {
						p := root
						if bundleType.tar {
							b := bundle.Bundle{
								Manifest: bundle.Manifest{
									RegoVersion:      &tc.bundleRegoVersion,
									FileRegoVersions: tc.bundleFileRegoVersions,
								},
								Data: map[string]any{},
							}
							for k, v := range tc.modules {
								b.Modules = append(b.Modules, bundle.ModuleFile{
									Path: k,
									Raw:  []byte(v),
								})
							}
							p = filepath.Join(root, "bundle.tar.gz")
							f, err := os.OpenFile(p, os.O_WRONLY, os.ModePerm)
							if err != nil {
								t.Fatalf("Unexpected error: %s", err)
							}
							err = bundle.Write(f, b)
							if err != nil {
								t.Fatalf("Unexpected error: %s", err)
							}
						}

						params := testBenchParams()
						_ = params.outputFormat.Set(formats.Pretty)

						params.e2e = mode.e2e
						err := params.bundlePaths.Set(p)
						if err != nil {
							t.Fatalf("Unexpected error: %s", err)
						}

						args := []string{tc.query}

						var buf bytes.Buffer
						rc, err := benchMain(args, params, &buf, &goBenchRunner{})

						if len(tc.expErrs) > 0 {
							if rc == 0 {
								t.Fatalf("Expected non-zero return code")
							}

							output := buf.String()
							for _, expErr := range tc.expErrs {
								if !strings.Contains(output, expErr) {
									t.Fatalf("Expected error:\n\n%s\n\ngot:\n\n%s", expErr, output)
								}
							}
						} else {
							if err != nil {
								t.Fatalf("Unexpected error: %s", err)
							}
							if rc != 0 {
								t.Fatalf("Unexpected return code %d, expected 0", rc)
							}
						}
					})
				})
			}
		}
	}
}

func TestRenderBenchmarkResultJSONOutput(t *testing.T) {
	t.Parallel()

	params := testBenchParams()
	err := params.outputFormat.Set(formats.JSON)
	if err != nil {
		t.Fatalf("Unexpected error: %s", err)
	}

	br := fakeBenchResults()

	var buf bytes.Buffer
	renderBenchmarkResult(params, br, &buf)

	actual := buf.String()

	expected := `{
  "N": 134844,
  "T": 1088294120,
  "Bytes": 0,
  "MemAllocs": 8360721,
  "MemBytes": 449906736,
  "Extra": {
    "histogram_timer_rego_query_eval_ns_75%": 4953.75,
    "histogram_timer_rego_query_eval_ns_90%": 6309.6,
    "histogram_timer_rego_query_eval_ns_95%": 7872.55,
    "histogram_timer_rego_query_eval_ns_99%": 14947.34000000001,
    "histogram_timer_rego_query_eval_ns_99.9%": 174377.08200000023,
    "histogram_timer_rego_query_eval_ns_99.99%": 176301,
    "histogram_timer_rego_query_eval_ns_count": 134844,
    "histogram_timer_rego_query_eval_ns_max": 176301,
    "histogram_timer_rego_query_eval_ns_mean": 5118.3706225680935,
    "histogram_timer_rego_query_eval_ns_median": 4312,
    "histogram_timer_rego_query_eval_ns_min": 3553,
    "histogram_timer_rego_query_eval_ns_stddev": 6587.830963916497
  }
}
`
	if actual != expected {
		t.Fatalf("\nExpected:\n%s\n\nGot:\n%s\n", expected, actual)
	}
}

func TestRenderBenchmarkResultPrettyOutput(t *testing.T) {
	t.Parallel()

	params := testBenchParams()
	params.benchMem = false
	err := params.outputFormat.Set(formats.Pretty)
	if err != nil {
		t.Fatalf("Unexpected error: %s", err)
	}

	br := fakeBenchResults()

	var buf bytes.Buffer
	renderBenchmarkResult(params, br, &buf)

	actual := buf.String()

	expected := `+-------------------------------------------+------------+
| samples                                   |     134844 |
| ns/op                                     |       8071 |
| histogram_timer_rego_query_eval_ns_75%    |       4954 |
| histogram_timer_rego_query_eval_ns_90%    |       6310 |
| histogram_timer_rego_query_eval_ns_95%    |       7873 |
| histogram_timer_rego_query_eval_ns_99%    |      14947 |
| histogram_timer_rego_query_eval_ns_99.9%  |     174377 |
| histogram_timer_rego_query_eval_ns_99.99% |     176301 |
| histogram_timer_rego_query_eval_ns_count  |     134844 |
| histogram_timer_rego_query_eval_ns_max    |     176301 |
| histogram_timer_rego_query_eval_ns_mean   |       5118 |
| histogram_timer_rego_query_eval_ns_median |       4312 |
| histogram_timer_rego_query_eval_ns_min    |       3553 |
| histogram_timer_rego_query_eval_ns_stddev |       6588 |
+-------------------------------------------+------------+
`
	if actual != expected {
		t.Fatalf("\nExpected:\n%s\n\nGot:\n%s\n", expected, actual)
	}
}

func TestRenderBenchmarkResultPrettyOutputShowAllocs(t *testing.T) {
	t.Parallel()

	params := testBenchParams()
	params.benchMem = true
	err := params.outputFormat.Set(formats.Pretty)
	if err != nil {
		t.Fatalf("Unexpected error: %s", err)
	}

	br := fakeBenchResults()

	var buf bytes.Buffer
	renderBenchmarkResult(params, br, &buf)

	actual := buf.String()

	expected := `+-------------------------------------------+------------+
| samples                                   |     134844 |
| ns/op                                     |       8071 |
| B/op                                      |       3336 |
| allocs/op                                 |         62 |
| histogram_timer_rego_query_eval_ns_75%    |       4954 |
| histogram_timer_rego_query_eval_ns_90%    |       6310 |
| histogram_timer_rego_query_eval_ns_95%    |       7873 |
| histogram_timer_rego_query_eval_ns_99%    |      14947 |
| histogram_timer_rego_query_eval_ns_99.9%  |     174377 |
| histogram_timer_rego_query_eval_ns_99.99% |     176301 |
| histogram_timer_rego_query_eval_ns_count  |     134844 |
| histogram_timer_rego_query_eval_ns_max    |     176301 |
| histogram_timer_rego_query_eval_ns_mean   |       5118 |
| histogram_timer_rego_query_eval_ns_median |       4312 |
| histogram_timer_rego_query_eval_ns_min    |       3553 |
| histogram_timer_rego_query_eval_ns_stddev |       6588 |
+-------------------------------------------+------------+
`
	if actual != expected {
		t.Fatalf("\nExpected:\n%s\n\nGot:\n%s\n", expected, actual)
	}
}

func TestRenderBenchmarkResultGoBenchOutputShowAllocs(t *testing.T) {
	t.Parallel()

	params := testBenchParams()
	params.benchMem = true
	err := params.outputFormat.Set(formats.GoBench)
	if err != nil {
		t.Fatalf("Unexpected error: %s", err)
	}

	br := fakeBenchResults()

	var buf bytes.Buffer
	renderBenchmarkResult(params, br, &buf)

	actual := buf.String()

	if !strings.HasPrefix(actual, "Benchmark") {
		t.Fatalf("Expected line output to start with 'Benchmark', got: \n\n%s\n", actual)
	}

	if len(strings.Split(strings.TrimSpace(actual), "\n")) != 1 {
		t.Fatalf("Expected only a single line of output")
	}
}

func TestRenderBenchmarkErrorJSONOutput(t *testing.T) {
	t.Parallel()

	params := testBenchParams()
	err := params.outputFormat.Set(formats.JSON)
	if err != nil {
		t.Fatalf("Unexpected error: %s", err)
	}

	var buf bytes.Buffer

	_, err = ast.ParseBody("???")

	err = renderBenchmarkError(params, err, &buf)
	if err != nil {
		t.Fatalf("Unexpected error %v", err)
	}

	actual := buf.String()
	expected := `{
  "errors": [
    {
      "message": "illegal token",
      "code": "rego_parse_error",
      "location": {
        "file": "",
        "row": 1,
        "col": 1
      },
      "details": {
        "line": "???",
        "idx": 0
      }
    }
  ]
}
`

	if actual != expected {
		t.Fatalf("\nExpected:\n%s\n\nGot:\n%s\n", expected, actual)
	}
}

func TestRenderBenchmarkErrorPrettyOutput(t *testing.T) {
	t.Parallel()

	params := testBenchParams()
	err := params.outputFormat.Set(formats.Pretty)
	if err != nil {
		t.Fatalf("Unexpected error: %s", err)
	}

	testPrettyBenchmarkOutput(t, params)
}

func TestRenderBenchmarkErrorGoBenchOutput(t *testing.T) {
	t.Parallel()

	params := testBenchParams()
	err := params.outputFormat.Set(formats.GoBench)
	if err != nil {
		t.Fatalf("Unexpected error: %s", err)
	}

	testPrettyBenchmarkOutput(t, params)
}

func testPrettyBenchmarkOutput(t *testing.T, params benchmarkCommandParams) {
	var buf bytes.Buffer

	_, err := ast.ParseBody("???")

	err = renderBenchmarkError(params, err, &buf)
	if err != nil {
		t.Fatalf("Unexpected error %v", err)
	}

	actual := buf.String()
	expected := `1 error occurred: 1:1: rego_parse_error: illegal token
	???
	^
`
	if actual != expected {
		t.Fatalf("\nExpected:\n%s\n\nGot:\n%s\n", expected, actual)
	}
}

func testBenchParams() benchmarkCommandParams {
	params := newBenchmarkEvalParams()
	params.benchMem = true
	params.metrics = true
	_ = params.outputFormat.Set(formats.JSON)
	params.count = 1
	return params
}

func fakeBenchResults() testing.BenchmarkResult {
	return testing.BenchmarkResult{
		N:         134844,
		T:         1088294120,
		Bytes:     0,
		MemAllocs: 8360721,
		MemBytes:  449906736,
		Extra: map[string]float64{
			"histogram_timer_rego_query_eval_ns_75%":    4953.75,
			"histogram_timer_rego_query_eval_ns_90%":    6309.6,
			"histogram_timer_rego_query_eval_ns_95%":    7872.55,
			"histogram_timer_rego_query_eval_ns_99%":    14947.34000000001,
			"histogram_timer_rego_query_eval_ns_99.9%":  174377.08200000023,
			"histogram_timer_rego_query_eval_ns_99.99%": 176301,
			"histogram_timer_rego_query_eval_ns_count":  134844,
			"histogram_timer_rego_query_eval_ns_max":    176301,
			"histogram_timer_rego_query_eval_ns_mean":   5118.3706225680935,
			"histogram_timer_rego_query_eval_ns_median": 4312,
			"histogram_timer_rego_query_eval_ns_min":    3553,
			"histogram_timer_rego_query_eval_ns_stddev": 6587.830963916497,
		},
	}
}

func testBundle() bundle.Bundle {
	mod := `package a.b
	import rego.v1

	x if {
	   data.a.b.c == 42
	}
	`

	return bundle.Bundle{
		Manifest: bundle.Manifest{},
		Data: map[string]any{
			"a": map[string]any{
				"b": map[string]any{
					"c": 42,
				},
			},
		},
		Modules: []bundle.ModuleFile{
			{
				Path:   "/a/b/policy.rego",
				Raw:    []byte(mod),
				Parsed: ast.MustParseModule(mod),
			},
		},
	}
}
