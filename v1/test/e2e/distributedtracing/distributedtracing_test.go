// Copyright 2021 The OPA Authors.  All rights reserved.
// Use of this source code is governed by an Apache2
// license that can be found in the LICENSE file.

package distributedtracing

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/open-policy-agent/opa/v1/logging/test"
	"github.com/open-policy-agent/opa/v1/plugins/bundle"
	"github.com/open-policy-agent/opa/v1/plugins/discovery"
	"github.com/open-policy-agent/opa/v1/plugins/logs"
	"github.com/open-policy-agent/opa/v1/plugins/status"
	"github.com/open-policy-agent/opa/v1/runtime"
	opasdktest "github.com/open-policy-agent/opa/v1/sdk/test"
	"github.com/open-policy-agent/opa/v1/server"
	"github.com/open-policy-agent/opa/v1/test/e2e"
	"github.com/open-policy-agent/opa/v1/tracing"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

var testRuntime *e2e.TestRuntime
var spanExporter *tracetest.InMemoryExporter

func TestMain(m *testing.M) {
	spanExporter = tracetest.NewInMemoryExporter()
	options := tracing.NewOptions(
		otelhttp.WithTracerProvider(trace.NewTracerProvider(trace.WithSpanProcessor(trace.NewSimpleSpanProcessor(spanExporter)))),
	)

	flag.Parse()
	testServerParams := e2e.NewAPIServerTestParams()
	testServerParams.DistributedTracingOpts = options
	testServerParams.Addrs = &[]string{"localhost:0"}

	var err error
	testRuntime, err = e2e.NewTestRuntime(testServerParams)

	if err != nil {
		os.Exit(1)
	}

	os.Exit(testRuntime.RunTests(m))
}

// TestServerSpan exemplarily asserts that the server handlers emit OpenTelemetry spans
// with the correct attributes. It does NOT exercise all handlers, but contains one test
// with a GET and one with a POST.
func TestServerSpan(t *testing.T) {
	spanExporter.Reset()

	t.Run("POST v0/data", func(t *testing.T) {
		t.Cleanup(spanExporter.Reset)

		mr, err := http.Post(testRuntime.URL()+"/v0/data", "application/json", nil)
		if err != nil {
			t.Fatal(err)
		}
		defer mr.Body.Close()

		spans := spanExporter.GetSpans()
		if got, expected := len(spans), 1; got != expected {
			t.Fatalf("got %d span(s), expected %d", got, expected)
		}
		if !spans[0].SpanContext.IsValid() {
			t.Fatalf("invalid span created: %#v", spans[0].SpanContext)
		}
		if got, expected := spans[0].Name, "v0/data"; got != expected {
			t.Fatalf("Expected span name to be %q but got %q", expected, got)
		}
		if got, expected := spans[0].SpanKind.String(), "server"; got != expected {
			t.Fatalf("Expected span kind to be %q but got %q", expected, got)
		}

		u, err := url.Parse(testRuntime.URL())
		if err != nil {
			t.Fatal(err)
		}
		port, err := strconv.Atoi(u.Port())
		if err != nil {
			t.Fatal(err)
		}
		expected := []any{
			attribute.String("server.address", u.Hostname()),
			attribute.Int("server.port", port),
			attribute.String("network.protocol.version", "1.1"),
			attribute.String("network.peer.address", "127.0.0.1"),
			attribute.Key("network.peer.port"),
			attribute.String("http.request.method", "POST"),
			attribute.String("url.scheme", "http"),
			attribute.String("url.path", "/v0/data"),
			attribute.Int("http.response.status_code", 200),
			attribute.Int("http.response.body.size", 3),
			attribute.String("user_agent.original", "Go-http-client/1.1"),
		}

		compareSpanAttributes(t, expected, attribute.NewSet(spans[0].Attributes...))
	})

	t.Run("GET v1/data", func(t *testing.T) {
		t.Cleanup(spanExporter.Reset)

		mr, err := http.Get(testRuntime.URL() + "/v1/data")
		if err != nil {
			t.Fatal(err)
		}
		defer mr.Body.Close()

		spans := spanExporter.GetSpans()
		if got, expected := len(spans), 1; got != expected {
			t.Fatalf("got %d span(s), expected %d", got, expected)
		}
		if !spans[0].SpanContext.IsValid() {
			t.Fatalf("invalid span created: %#v", spans[0].SpanContext)
		}
		if got, expected := spans[0].Name, "v1/data"; got != expected {
			t.Fatalf("Expected span name to be %q but got %q", expected, got)
		}
		if got, expected := spans[0].SpanKind.String(), "server"; got != expected {
			t.Fatalf("Expected span kind to be %q but got %q", expected, got)
		}

		u, err := url.Parse(testRuntime.URL())
		if err != nil {
			t.Fatal(err)
		}
		port, err := strconv.Atoi(u.Port())
		if err != nil {
			t.Fatal(err)
		}
		expected := []any{
			attribute.String("server.address", u.Hostname()),
			attribute.Int("server.port", port),
			attribute.String("network.protocol.version", "1.1"),
			attribute.String("network.peer.address", "127.0.0.1"),
			attribute.Key("network.peer.port"),
			attribute.String("http.request.method", "GET"),
			attribute.String("url.scheme", "http"),
			attribute.String("url.path", "/v1/data"),
			attribute.Int("http.response.status_code", 200),
			attribute.Int("http.response.body.size", 67),
			attribute.String("user_agent.original", "Go-http-client/1.1"),
		}
		compareSpanAttributes(t, expected, attribute.NewSet(spans[0].Attributes...))
	})
}

func TestServerSpanWithDecisionLogging(t *testing.T) {
	// setup
	spanExp := tracetest.NewInMemoryExporter()
	options := tracing.NewOptions(
		otelhttp.WithTracerProvider(trace.NewTracerProvider(trace.WithSpanProcessor(trace.NewSimpleSpanProcessor(spanExp)))),
	)

	testServerParams := e2e.NewAPIServerTestParams()
	testServerParams.ConfigOverrides = []string{
		"decision_logs.console=true",
	}

	// Ensure decisions are logged regardless of regular log level
	testServerParams.Logging = runtime.LoggingConfig{Level: "error"}
	consoleLogger := test.New()
	testServerParams.ConsoleLogger = consoleLogger

	testServerParams.DistributedTracingOpts = options

	e2e.WithRuntime(t, e2e.TestRuntimeOpts{}, testServerParams, func(rt *e2e.TestRuntime) {

		spanExp.Reset()
		rt.ConsoleLogger = consoleLogger

		mr, err := http.Post(rt.URL()+"/v1/data", "application/json", nil)
		if err != nil {
			t.Fatal(err)
		}
		defer mr.Body.Close()

		if mr.StatusCode != http.StatusOK {
			t.Fatalf("expected status %v but got %v", http.StatusOK, mr.StatusCode)
		}

		spans := spanExp.GetSpans()
		if got, expected := len(spans), 1; got != expected {
			t.Fatalf("got %d span(s), expected %d", got, expected)
		}
		if !spans[0].SpanContext.IsValid() {
			t.Fatalf("invalid span created: %#v", spans[0].SpanContext)
		}

		if got, expected := spans[0].SpanKind.String(), "server"; got != expected {
			t.Fatalf("Expected span kind to be %q but got %q", expected, got)
		}

		var entry test.LogEntry
		var found bool

		for _, entry = range rt.ConsoleLogger.Entries() {
			if entry.Message == "Decision Log" {
				found = true
			}
		}

		if !found {
			t.Fatalf("Did not find 'Decision Log' event in captured log entries")
		}

		// Check for some important fields
		expectedFields := map[string]*struct {
			found bool
			match func(*testing.T, string)
		}{
			"labels":      {},
			"decision_id": {},
			"trace_id":    {},
			"span_id":     {},
			"result":      {},
			"timestamp":   {},
			"type": {match: func(t *testing.T, actual string) {
				if actual != "openpolicyagent.org/decision_logs" {
					t.Fatalf("Expected field 'type' to be 'openpolicyagent.org/decision_logs'")
				}
			}},
		}

		// Ensure expected fields exist
		for fieldName, rawField := range entry.Fields {
			if fd, ok := expectedFields[fieldName]; ok {
				if fieldValue, ok := rawField.(string); ok && fd.match != nil {
					fd.match(t, fieldValue)
				}
				fd.found = true
			}
		}

		for field, fd := range expectedFields {
			if !fd.found {
				t.Errorf("Missing expected field in decision log: %s\n\nEntry: %+v\n\n", field, entry)
			}
		}
	})
}

// TestClientSpan asserts that for all handlers that end up evaluating policies, the
// http.send calls will emit the proper spans related to the incoming requests.
//
// NOTE(sr): `{GET,POST} v1/query` are omitted, http.send is forbidden for ad-hoc queries
func TestClientSpan(t *testing.T) {
	type resp struct {
		DecisionID string `json:"decision_id"`
	}

	policy := `
	package test

	response := http.send({"method": "get", "url": "%s/health"})
	`

	policy = fmt.Sprintf(policy, testRuntime.URL())
	err := testRuntime.UploadPolicy(t.Name(), strings.NewReader(policy))
	if err != nil {
		t.Fatal(err)
	}
	spanExporter.Reset()

	t.Run("POST v0/data", func(t *testing.T) {
		t.Cleanup(spanExporter.Reset)

		mr, err := http.Post(testRuntime.URL()+"/v0/data/test", "application/json", nil)
		if err != nil {
			t.Fatal(err)
		}
		defer mr.Body.Close()

		spans := spanExporter.GetSpans()

		// Ordered by span emission, which is the reverse of the processing
		// code flow:
		// 3 = GET /health (HTTP server handler)
		//     + http.send (HTTP client instrumentation)
		//     + GET /v1/data/test (HTTP server handler)
		if got, expected := len(spans), 3; got != expected {
			t.Fatalf("got %d span(s), expected %d", got, expected)
		}
		if !spans[1].SpanContext.IsValid() {
			t.Fatalf("invalid span created: %#v", spans[1].SpanContext)
		}
		if got, expected := spans[1].SpanKind.String(), "client"; got != expected {
			t.Fatalf("Expected span kind to be %q but got %q", expected, got)
		}

		parentSpanID := spans[2].SpanContext.SpanID()
		if got, expected := spans[1].Parent.SpanID(), parentSpanID; got != expected {
			t.Errorf("expected span to be child of %v, got parent %v", expected, got)
		}

		expected := []any{
			attribute.String("http.request.method", "GET"),
			attribute.String("url.full", testRuntime.URL()+"/health"),
			attribute.Int("http.response.status_code", 200),
			attribute.String("server.address", "127.0.0.1"),
			attribute.Key("server.port"),
			attribute.String("network.protocol.version", "1.1"),
		}
		compareSpanAttributes(t, expected, attribute.NewSet(spans[1].Attributes...))
	})

	t.Run("GET v1/data", func(t *testing.T) {
		t.Cleanup(spanExporter.Reset)

		mr, err := http.Get(testRuntime.URL() + "/v1/data/test")
		if err != nil {
			t.Fatal(err)
		}
		defer mr.Body.Close()
		var r resp
		if err := json.NewDecoder(mr.Body).Decode(&r); err != nil {
			t.Fatal(err)
		}
		if r.DecisionID == "" {
			t.Fatal("expected decision id")
		}

		spans := spanExporter.GetSpans()
		if got, expected := len(spans), 3; got != expected {
			t.Fatalf("got %d span(s), expected %d", got, expected)
		}
		if !spans[1].SpanContext.IsValid() {
			t.Fatalf("invalid span created: %#v", spans[1].SpanContext)
		}
		if got, expected := spans[1].SpanKind.String(), "client"; got != expected {
			t.Fatalf("Expected span kind to be %q but got %q", expected, got)
		}

		parentSpanID := spans[2].SpanContext.SpanID()
		if got, expected := spans[1].Parent.SpanID(), parentSpanID; got != expected {
			t.Errorf("expected span to be child of %v, got parent %v", expected, got)
		}

		expected := []any{
			attribute.String("http.request.method", "GET"),
			attribute.String("url.full", testRuntime.URL()+"/health"),
			attribute.Int("http.response.status_code", 200),
			attribute.String("server.address", "127.0.0.1"),
			attribute.Key("server.port"),
			attribute.String("network.protocol.version", "1.1"),
		}
		compareSpanAttributes(t, expected, attribute.NewSet(spans[1].Attributes...))

		// The (parent) server span carries the decision ID
		expected = []any{
			attribute.String("opa.decision_id", r.DecisionID),
		}
		compareSpanAttributes(t, expected, attribute.NewSet(spans[2].Attributes...))
	})

	t.Run("POST v1/data", func(t *testing.T) {
		t.Cleanup(spanExporter.Reset)

		payload := strings.NewReader(`{"input": "meow"}`)
		mr, err := http.Post(testRuntime.URL()+"/v1/data/test", "application/json", payload)
		if err != nil {
			t.Fatal(err)
		}
		defer mr.Body.Close()
		var r resp
		if err := json.NewDecoder(mr.Body).Decode(&r); err != nil {
			t.Fatal(err)
		}
		if r.DecisionID == "" {
			t.Fatal("expected decision id")
		}

		spans := spanExporter.GetSpans()
		if got, expected := len(spans), 3; got != expected {
			t.Fatalf("got %d span(s), expected %d", got, expected)
		}
		if !spans[1].SpanContext.IsValid() {
			t.Fatalf("invalid span created: %#v", spans[1].SpanContext)
		}
		if got, expected := spans[1].SpanKind.String(), "client"; got != expected {
			t.Fatalf("Expected span kind to be %q but got %q", expected, got)
		}

		parentSpanID := spans[2].SpanContext.SpanID()
		if got, expected := spans[1].Parent.SpanID(), parentSpanID; got != expected {
			t.Errorf("expected span to be child of %v, got parent %v", expected, got)
		}

		expected := []any{
			attribute.String("http.request.method", "GET"),
			attribute.String("url.full", testRuntime.URL()+"/health"),
			attribute.Int("http.response.status_code", 200),
			attribute.String("server.address", "127.0.0.1"),
			attribute.Key("server.port"),
			attribute.String("network.protocol.version", "1.1"),
		}
		compareSpanAttributes(t, expected, attribute.NewSet(spans[1].Attributes...))

		// The (parent) server span carries the decision ID
		expected = []any{
			attribute.String("opa.decision_id", r.DecisionID),
		}
		compareSpanAttributes(t, expected, attribute.NewSet(spans[2].Attributes...))
	})

	t.Run("POST /", func(t *testing.T) {
		t.Cleanup(spanExporter.Reset)

		main := fmt.Sprintf(`
		package system.main

		response := http.send({"method": "get", "url": "%s/health"})
		`, testRuntime.URL())
		err := testRuntime.UploadPolicy("system.main", strings.NewReader(main))
		if err != nil {
			t.Fatal(err)
		}
		spanExporter.Reset()

		mr, err := http.Post(testRuntime.URL()+"/", "application/json", nil)
		if err != nil {
			t.Fatal(err)
		}
		defer mr.Body.Close()

		spans := spanExporter.GetSpans()
		if got, expected := len(spans), 3; got != expected {
			t.Fatalf("got %d span(s), expected %d", got, expected)
		}
		if !spans[1].SpanContext.IsValid() {
			t.Fatalf("invalid span created: %#v", spans[1].SpanContext)
		}
		if got, expected := spans[1].SpanKind.String(), "client"; got != expected {
			t.Fatalf("Expected span kind to be %q but got %q", expected, got)
		}

		parentSpanID := spans[2].SpanContext.SpanID()
		if got, expected := spans[1].Parent.SpanID(), parentSpanID; got != expected {
			t.Errorf("expected span to be child of %v, got parent %v", expected, got)
		}

		expected := []any{
			attribute.String("http.request.method", "GET"),
			attribute.String("url.full", testRuntime.URL()+"/health"),
			attribute.Int("http.response.status_code", 200),
			attribute.String("server.address", "127.0.0.1"),
			attribute.Key("server.port"),
			attribute.String("network.protocol.version", "1.1"),
		}
		compareSpanAttributes(t, expected, attribute.NewSet(spans[1].Attributes...))
	})
}

func TestClientSpanWithDecisionLogging(t *testing.T) {
	// setup
	spanExp := tracetest.NewInMemoryExporter()
	options := tracing.NewOptions(
		otelhttp.WithTracerProvider(trace.NewTracerProvider(trace.WithSpanProcessor(trace.NewSimpleSpanProcessor(spanExp)))),
	)

	testServerParams := e2e.NewAPIServerTestParams()
	testServerParams.ConfigOverrides = []string{
		"decision_logs.console=true",
	}

	// Ensure decisions are logged regardless of regular log level
	testServerParams.Logging = runtime.LoggingConfig{Level: "error"}
	consoleLogger := test.New()
	testServerParams.ConsoleLogger = consoleLogger

	testServerParams.DistributedTracingOpts = options

	e2e.WithRuntime(t, e2e.TestRuntimeOpts{}, testServerParams, func(rt *e2e.TestRuntime) {

		spanExp.Reset()
		rt.ConsoleLogger = consoleLogger

		policy := `
		package test

		response := http.send({"method": "get", "url": "%s/health"})
		`

		policy = fmt.Sprintf(policy, testRuntime.URL())
		err := rt.UploadPolicy(t.Name(), strings.NewReader(policy))
		if err != nil {
			t.Fatal(err)
		}

		mr, err := http.Post(rt.URL()+"/v1/data/test", "application/json", nil)
		if err != nil {
			t.Fatal(err)
		}
		defer mr.Body.Close()

		if mr.StatusCode != http.StatusOK {
			t.Fatalf("expected status %v but got %v", http.StatusOK, mr.StatusCode)
		}

		spans := spanExp.GetSpans()
		// Ordered by span emission, which is the reverse of the processing
		// code flow:
		// 3 = GET /health (HTTP server handler)
		//     + http.send (HTTP client instrumentation)
		//     + GET /v1/data/test (HTTP server handler)
		if got, expected := len(spans), 3; got != expected {
			t.Fatalf("got %d span(s), expected %d", got, expected)
		}

		if !spans[1].SpanContext.IsValid() {
			t.Fatalf("invalid span created: %#v", spans[1].SpanContext)
		}
		if got, expected := spans[1].SpanKind.String(), "client"; got != expected {
			t.Fatalf("Expected span kind to be %q but got %q", expected, got)
		}

		parentTraceID := spans[2].SpanContext.TraceID()
		parentSpanID := spans[2].SpanContext.SpanID()
		if got, expected := spans[1].Parent.SpanID(), parentSpanID; got != expected {
			t.Errorf("expected span to be child of %v, got parent %v", expected, got)
		}

		var entry test.LogEntry
		var found bool

		for _, entry = range rt.ConsoleLogger.Entries() {
			if entry.Message == "Decision Log" {
				found = true
			}
		}

		if !found {
			t.Fatalf("Did not find 'Decision Log' event in captured log entries")
		}

		// Check for some important fields
		expectedFields := map[string]*struct {
			found bool
			match func(*testing.T, string)
		}{
			"labels":      {},
			"decision_id": {},
			"trace_id": {match: func(t *testing.T, actual string) {
				if actual != parentTraceID.String() {
					t.Fatalf("Expected field 'trace_id' to be %v", parentTraceID.String())
				}
			}},
			"span_id": {match: func(t *testing.T, actual string) {
				if actual != parentSpanID.String() {
					t.Fatalf("Expected field 'span_id' to be %v", parentSpanID.String())
				}
			}},
			"result":    {},
			"timestamp": {},
			"type": {match: func(t *testing.T, actual string) {
				if actual != "openpolicyagent.org/decision_logs" {
					t.Fatalf("Expected field 'type' to be 'openpolicyagent.org/decision_logs'")
				}
			}},
		}

		// Ensure expected fields exist
		for fieldName, rawField := range entry.Fields {
			if fd, ok := expectedFields[fieldName]; ok {
				if fieldValue, ok := rawField.(string); ok && fd.match != nil {
					fd.match(t, fieldValue)
				}
				fd.found = true
			}
		}

		for field, fd := range expectedFields {
			if !fd.found {
				t.Errorf("Missing expected field in decision log: %s\n\nEntry: %+v\n\n", field, entry)
			}
		}
	})
}

func TestServerSpanWithSystemAuthzPolicy(t *testing.T) {

	// setup
	spanExp := tracetest.NewInMemoryExporter()
	options := tracing.NewOptions(
		otelhttp.WithTracerProvider(trace.NewTracerProvider(trace.WithSpanProcessor(trace.NewSimpleSpanProcessor(spanExp)))),
	)

	authzPolicy := []byte(`package system.authz
import rego.v1
default allow = false
allow if {
	input.path = ["health"]
}`)

	tmpfile, err := os.CreateTemp(t.TempDir(), "authz.*.rego")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmpfile.Name())

	if _, err := tmpfile.Write(authzPolicy); err != nil {
		t.Fatal(err)
	}
	if err := tmpfile.Close(); err != nil {
		t.Fatal(err)
	}

	testServerParams := e2e.NewAPIServerTestParams()
	testServerParams.DistributedTracingOpts = options
	testServerParams.Authorization = server.AuthorizationBasic
	testServerParams.Paths = []string{"system.authz:" + tmpfile.Name()}

	e2e.WithRuntime(t, e2e.TestRuntimeOpts{}, testServerParams, func(rt *e2e.TestRuntime) {

		spanExp.Reset()

		mr, err := http.Post(rt.URL()+"/v1/data", "application/json", nil)
		if err != nil {
			t.Fatal(err)
		}
		defer mr.Body.Close()

		if mr.StatusCode != http.StatusUnauthorized {
			t.Fatalf("expected status %v but got %v", http.StatusUnauthorized, mr.StatusCode)
		}

		spans := spanExp.GetSpans()
		if got, expected := len(spans), 1; got != expected {
			t.Fatalf("got %d span(s), expected %d", got, expected)
		}
		if !spans[0].SpanContext.IsValid() {
			t.Fatalf("invalid span created: %#v", spans[0].SpanContext)
		}
		if got, expected := spans[0].Name, server.PromHandlerAPIAuthz; got != expected {
			t.Fatalf("Expected span name to be %q but got %q", expected, got)
		}
		if got, expected := spans[0].SpanKind.String(), "server"; got != expected {
			t.Fatalf("Expected span kind to be %q but got %q", expected, got)
		}

		u := mr.Request.URL
		port, err := strconv.Atoi(u.Port())
		if err != nil {
			t.Fatal(err)
		}

		expected := []any{
			attribute.String("server.address", u.Hostname()),
			attribute.Int("server.port", port),
			attribute.String("network.protocol.version", "1.1"),
			attribute.String("network.peer.address", "127.0.0.1"),
			attribute.Key("network.peer.port"),
			attribute.String("http.request.method", "POST"),
			attribute.String("url.scheme", "http"),
			attribute.String("url.path", "/v1/data"),
			attribute.Int("http.response.status_code", 401),
			attribute.Int("http.response.body.size", 87),
			attribute.String("user_agent.original", "Go-http-client/1.1"),
		}
		compareSpanAttributes(t, expected, attribute.NewSet(spans[0].Attributes...))

	})
}

func TestControlPlaneSpans(t *testing.T) {
	// setup
	spanExp := tracetest.NewInMemoryExporter()
	options := tracing.NewOptions(
		otelhttp.WithTracerProvider(trace.NewTracerProvider(trace.WithSpanProcessor(trace.NewSimpleSpanProcessor(spanExp)))),
	)

	opaControlPlane := opasdktest.MustNewServer(
		opasdktest.MockBundle("/bundles/test", map[string]string{
			"main.rego": `
				package main

				default allow = false
			`,
		}),
		opasdktest.MockBundle("/bundles/discovery", map[string]string{
			"data.json": `
				{"discovery":{"bundles":{"bundles/test":{"persist":false,"resource":"bundles/test","service":"bundleregistry", "trigger":"manual"}}}}
			`,
		}),
	)
	defer opaControlPlane.Stop()

	controlPlaneURL, err := url.Parse(opaControlPlane.URL())
	if err != nil {
		t.Fatal(err)
	}

	controlPlanePort, err := strconv.Atoi(controlPlaneURL.Port())
	if err != nil {
		t.Fatal(err)
	}

	ts := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
	}))
	defer ts.Close()

	statusURL, err := url.Parse(ts.URL)
	if err != nil {
		t.Fatal(err)
	}

	statusPort, err := strconv.Atoi(statusURL.Port())
	if err != nil {
		t.Fatal(err)
	}

	testServerParams := e2e.NewAPIServerTestParams()
	testServerParams.ConfigOverrides = []string{
		"services.bundleregistry.url=" + opaControlPlane.URL(),
		"services.observability.url=" + ts.URL,
		"discovery.name=discovery",
		"discovery.resource=/bundles/discovery",
		"discovery.service=bundleregistry",
		"discovery.trigger=manual",
		"status.service=observability",
		"status.trigger=manual",
		"decision_logs.service=bundleregistry",
		"decision_logs.reporting.trigger=manual",
	}

	testServerParams.DistributedTracingOpts = options
	testServerParams.ReadyTimeout = 5
	testServerParams.Logging = runtime.LoggingConfig{Level: "debug"}

	manualTriggers := func(rt *e2e.TestRuntime) error {
		err := discovery.Lookup(rt.Runtime.Manager).Trigger(rt.Ctx)
		if err != nil {
			return err
		}
		err = bundle.Lookup(rt.Runtime.Manager).Trigger(rt.Ctx)
		if err != nil {
			return err
		}
		return status.Lookup(rt.Runtime.Manager).Trigger(rt.Ctx)
	}

	e2e.WithRuntime(t, e2e.TestRuntimeOpts{PostServeActions: manualTriggers}, testServerParams, func(rt *e2e.TestRuntime) {
		// We expect 3 spans:
		// 1. GET /bundles/discovery (client)
		// 2. GET /bundles/test (client)
		// 3. POST /status (client)
		// 4. health check (server)

		spans := spanExp.GetSpans()
		if got, expected := len(spans), 4; got != expected {
			t.Fatalf("got %d span(s), expected %d", got, expected)
		}
		for _, span := range spans {
			if !span.SpanContext.IsValid() {
				t.Fatalf("invalid span created: %#v", span.SpanContext)
			}
		}

		for idx := range 3 {
			if got, expected := spans[idx].SpanKind.String(), "client"; got != expected {
				t.Fatalf("Expected span kind to be %q but got %q", expected, got)
			}
		}

		u := controlPlaneURL
		port := controlPlanePort

		expected := []any{
			attribute.String("server.address", u.Hostname()),
			attribute.Int("server.port", port),
			attribute.String("http.request.method", "GET"),
			attribute.String("url.full", u.String()+"/bundles/discovery"),
			attribute.Int("http.response.status_code", 200),
			attribute.String("network.protocol.version", "1.1"),
		}
		compareSpanAttributes(t, expected, attribute.NewSet(spans[0].Attributes...))

		expected = []any{
			attribute.String("server.address", u.Hostname()),
			attribute.Int("server.port", port),
			attribute.String("http.request.method", "GET"),
			attribute.String("url.full", u.String()+"/bundles/test"),
			attribute.Int("http.response.status_code", 200),
			attribute.String("network.protocol.version", "1.1"),
		}
		compareSpanAttributes(t, expected, attribute.NewSet(spans[1].Attributes...))

		expected = []any{
			attribute.String("server.address", statusURL.Hostname()),
			attribute.Int("server.port", statusPort),
			attribute.String("http.request.method", "POST"),
			attribute.String("url.full", statusURL.String()+"/status"),
			attribute.String("network.protocol.version", "1.1"),
		}
		compareSpanAttributes(t, expected, attribute.NewSet(spans[2].Attributes...))

		spanExp.Reset()

		mr, err := http.Post(rt.URL()+"/v1/data/main", "application/json", nil)
		if err != nil {
			t.Fatal(err)
		}
		defer mr.Body.Close()

		_ = logs.Lookup(rt.Runtime.Manager).Trigger(context.Background())

		spans = spanExp.GetSpans()
		// Expect 2 spans:
		// 1. POST /v1/data/main (server)
		// 2. POST /v1/logs (client)

		if got, expected := len(spans), 2; got != expected {
			t.Fatalf("got %d span(s), expected %d", got, expected)
		}
		for _, span := range spans {
			if !span.SpanContext.IsValid() {
				t.Fatalf("invalid span created: %#v", span.SpanContext)
			}
		}
		if got, expected := spans[0].Name, "v1/data"; got != expected {
			t.Fatalf("Expected span name to be %q but got %q", expected, got)
		}
		if got, expected := spans[0].SpanKind.String(), "server"; got != expected {
			t.Fatalf("Expected span kind to be %q but got %q", expected, got)
		}
		if got, expected := spans[1].Name, "HTTP POST"; got != expected {
			t.Fatalf("Expected span name to be %q but got %q", expected, got)
		}
		if got, expected := spans[1].SpanKind.String(), "client"; got != expected {
			t.Fatalf("Expected span kind to be %q but got %q", expected, got)
		}

		u, err = url.Parse(rt.URL())
		if err != nil {
			t.Fatal(err)
		}
		port, err = strconv.Atoi(u.Port())
		if err != nil {
			t.Fatal(err)
		}

		expected = []any{
			attribute.String("server.address", u.Hostname()),
			attribute.Int("server.port", port),
			attribute.String("network.protocol.version", "1.1"),
			attribute.String("network.peer.address", "127.0.0.1"),
			attribute.Key("network.peer.port"),
			attribute.String("http.request.method", "POST"),
			attribute.String("url.scheme", "http"),
			attribute.String("url.path", "/v1/data/main"),
			attribute.Int("http.response.status_code", 200),
			attribute.Int("http.response.body.size", 168),
			attribute.String("user_agent.original", "Go-http-client/1.1"),
		}

		compareSpanAttributes(t, expected, attribute.NewSet(spans[0].Attributes...))

		expected = []any{
			attribute.String("server.address", controlPlaneURL.Hostname()),
			attribute.Int("server.port", controlPlanePort),
			attribute.String("http.request.method", "POST"),
			attribute.String("url.full", controlPlaneURL.String()+"/logs"),
			attribute.Int("http.response.status_code", 500),
			attribute.String("error.type", "500"),
			attribute.String("network.protocol.version", "1.1"),
		}

		compareSpanAttributes(t, expected, attribute.NewSet(spans[1].Attributes...))
	})
}

func compareSpanAttributes(t *testing.T, expectedAttributes []any, spanAttributes attribute.Set) {
	t.Helper()
	ok := true
	for _, exp := range expectedAttributes {
		var expKey attribute.Key
		var expValue *attribute.Value

		switch exp := exp.(type) {
		case attribute.KeyValue:
			expKey = exp.Key
			expValue = &exp.Value
		case attribute.Key:
			expKey = exp
		}

		value, exists := spanAttributes.Value(expKey)
		if !exists {
			t.Errorf("Expected span attributes to contain %q key", expKey)
			ok = false
		} else if expValue != nil && value != *expValue {
			t.Errorf("Expected %q attribute to be %s but got %s", expKey, expValue.Emit(), value.Emit())
			ok = false
		}
	}

	if !ok {
		txt, _ := spanAttributes.MarshalJSON()
		t.Fatalf("Span attributes mismatch.\n\nGot:\n\n%s", txt)
	}
}
