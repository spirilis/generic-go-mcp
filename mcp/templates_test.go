package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/spirilis/generic-go-mcp/transport"
)

// mustCompile compiles a template that the test expects to be valid.
func mustCompile(t *testing.T, tmpl string) *compiledTemplate {
	t.Helper()
	re, vars, err := compileURITemplate(tmpl)
	if err != nil {
		t.Fatalf("compileURITemplate(%q): unexpected error: %v", tmpl, err)
	}
	return &compiledTemplate{tmpl: ResourceTemplate{URITemplate: tmpl}, re: re, vars: vars}
}

// assertNoCompile asserts a template is rejected, and that the message mentions want.
func assertNoCompile(t *testing.T, tmpl, want string) {
	t.Helper()
	_, _, err := compileURITemplate(tmpl)
	if err == nil {
		t.Fatalf("compileURITemplate(%q): expected an error, got none", tmpl)
	}
	if !strings.Contains(err.Error(), want) {
		t.Errorf("compileURITemplate(%q): error %q does not mention %q", tmpl, err.Error(), want)
	}
}

func TestTemplateMatchesAndBindsVariables(t *testing.T) {
	c := mustCompile(t, "mcp+kubectl://{context}/pod/{namespace}/{name}")

	vars, ok := c.match("mcp+kubectl://prod/pod/default/web-7")
	if !ok {
		t.Fatal("expected a match")
	}
	want := map[string]string{"context": "prod", "namespace": "default", "name": "web-7"}
	for k, v := range want {
		if vars[k] != v {
			t.Errorf("var %q = %q, want %q", k, vars[k], v)
		}
	}
	if len(vars) != len(want) {
		t.Errorf("got %d vars, want %d", len(vars), len(want))
	}
}

// A simple {var} is one path segment. Allowing it to swallow separators would make
// "x://{a}/{b}" match a URI with any number of segments, binding them arbitrarily.
func TestSimpleVariableDoesNotSpanSlash(t *testing.T) {
	c := mustCompile(t, "test:///env/{name}")

	if _, ok := c.match("test:///env/a/b"); ok {
		t.Error("{name} matched across a /, it must not")
	}
	if _, ok := c.match("test:///env/HOME"); !ok {
		t.Error("{name} failed to match a single segment")
	}
}

func TestReservedVariableSpansSegments(t *testing.T) {
	c := mustCompile(t, "file:///{+path}")

	vars, ok := c.match("file:///etc/nginx/nginx.conf")
	if !ok {
		t.Fatal("expected {+path} to match a multi-segment path")
	}
	if vars["path"] != "etc/nginx/nginx.conf" {
		t.Errorf("path = %q, want %q", vars["path"], "etc/nginx/nginx.conf")
	}
}

func TestTemplateVariablesArePercentDecoded(t *testing.T) {
	c := mustCompile(t, "test:///env/{name}")

	vars, ok := c.match("test:///env/a%20b%2Fc")
	if !ok {
		t.Fatal("expected a match")
	}
	// %2F decodes to "/" only after matching, so it does not defeat the segment rule.
	if vars["name"] != "a b/c" {
		t.Errorf("name = %q, want %q", vars["name"], "a b/c")
	}
}

// PathUnescape, not QueryUnescape: in a path a "+" is a literal plus, not a space.
func TestPlusIsLiteralInDecodedVariable(t *testing.T) {
	c := mustCompile(t, "test:///env/{name}")

	vars, ok := c.match("test:///env/a+b")
	if !ok {
		t.Fatal("expected a match")
	}
	if vars["name"] != "a+b" {
		t.Errorf("name = %q, want %q (a + in a path is literal)", vars["name"], "a+b")
	}
}

// A malformed percent-escape must fall through to the next template rather than erroring,
// so it cannot mask a template that would legitimately have matched.
func TestUndecodableVariableIsNoMatch(t *testing.T) {
	c := mustCompile(t, "test:///env/{name}")

	if _, ok := c.match("test:///env/%zz"); ok {
		t.Error("an undecodable percent-escape matched; it must fall through")
	}
}

func TestNonTerminalReservedExpansionIsRejected(t *testing.T) {
	// Followed by a literal...
	assertNoCompile(t, "file:///{+path}/meta", "must be the final expression")
	// ...and followed by another expression.
	assertNoCompile(t, "file:///{+path}/{name}", "must be the final expression")
}

func TestUnsupportedOperatorsAreRejected(t *testing.T) {
	assertNoCompile(t, "test:///x{#frag}", "not supported")
	assertNoCompile(t, "test:///x{?query}", "not supported")
	assertNoCompile(t, "test:///x{&param}", "not supported")
	assertNoCompile(t, "test:///x{/seg}", "not supported")
	assertNoCompile(t, "test:///x{.ext}", "not supported")
	assertNoCompile(t, "test:///x{;p}", "not supported")
}

func TestModifiersAndMultiVariableExpressionsAreRejected(t *testing.T) {
	assertNoCompile(t, "test:///{a,b}", "multi-variable")
	assertNoCompile(t, "test:///{a*}", "explode modifier")
	assertNoCompile(t, "test:///{a:3}", "prefix modifier")
}

func TestMalformedTemplatesAreRejected(t *testing.T) {
	assertNoCompile(t, "test:///{a", "unterminated")
	assertNoCompile(t, "test:///a}", "unbalanced")
	assertNoCompile(t, "test:///{}", "empty expression")
	assertNoCompile(t, "test:///{a-b}", "invalid variable name")
	assertNoCompile(t, "test:///{a}/{a}", "duplicate variable")
	assertNoCompile(t, "test:///concrete", "no variable expressions")
}

// Literal spans must be regex-quoted, or a "." in a scheme or hostname would match anything.
func TestLiteralSpansAreRegexQuoted(t *testing.T) {
	c := mustCompile(t, "https://example.com/{id}")

	if _, ok := c.match("https://exampleXcom/42"); ok {
		t.Error("a literal '.' behaved as a regex wildcard")
	}
	if _, ok := c.match("https://example.com/42"); !ok {
		t.Error("the literal template failed to match itself")
	}
}

// The matcher is anchored at both ends: a template is a whole-URI claim.
func TestTemplateIsAnchored(t *testing.T) {
	c := mustCompile(t, "test:///env/{name}")

	if _, ok := c.match("prefix-test:///env/HOME"); ok {
		t.Error("matched with a leading prefix; the pattern must be anchored")
	}
	if _, ok := c.match("test:///env/HOME/extra"); ok {
		t.Error("matched with a trailing suffix; the pattern must be anchored")
	}
}

func TestRegisterTemplateRejectsBadTemplateAndDoesNotMutate(t *testing.T) {
	r := NewResourceRegistry()
	err := r.RegisterTemplate(ResourceTemplate{URITemplate: "test:///{a,b}", Name: "bad"},
		func(context.Context, *ResourceReadRequest) (ResourceContentResult, error) {
			return ResourceContentResult{}, nil
		})
	if err == nil {
		t.Fatal("expected RegisterTemplate to reject a multi-variable template")
	}
	if r.HasTemplates() {
		t.Error("a rejected template was still added to the registry")
	}
}

func TestRegisterTemplateRejectsNilFunction(t *testing.T) {
	r := NewResourceRegistry()
	if err := r.RegisterTemplate(ResourceTemplate{URITemplate: "test:///{a}", Name: "x"}, nil); err == nil {
		t.Fatal("expected RegisterTemplate to reject a nil function")
	}
}

// First-match-wins in registration order is the documented contract; this pins the
// shadowing behaviour it implies.
func TestTemplateMatchingIsFirstRegisteredWins(t *testing.T) {
	r := NewResourceRegistry()
	read := func(text string) ResourceTemplateFunction {
		return func(context.Context, *ResourceReadRequest) (ResourceContentResult, error) {
			return ResourceContentResult{Text: text}, nil
		}
	}

	if err := r.RegisterTemplate(ResourceTemplate{URITemplate: "test:///{+rest}", Name: "general"}, read("general")); err != nil {
		t.Fatalf("register general: %v", err)
	}
	if err := r.RegisterTemplate(ResourceTemplate{URITemplate: "test:///{a}/detail", Name: "specific"}, read("specific")); err != nil {
		t.Fatalf("register specific: %v", err)
	}

	tmpl, _, _, ok := r.MatchTemplate("test:///x/detail")
	if !ok {
		t.Fatal("expected a match")
	}
	if tmpl.Name != "general" {
		t.Errorf("matched %q, want %q — the earlier registration must win", tmpl.Name, "general")
	}
}

func TestReRegisterTemplateReplacesAndMovesToEnd(t *testing.T) {
	r := NewResourceRegistry()
	fn := func(context.Context, *ResourceReadRequest) (ResourceContentResult, error) {
		return ResourceContentResult{}, nil
	}

	for _, u := range []string{"test:///a/{x}", "test:///b/{x}"} {
		if err := r.RegisterTemplate(ResourceTemplate{URITemplate: u, Name: u}, fn); err != nil {
			t.Fatalf("register %s: %v", u, err)
		}
	}
	if err := r.RegisterTemplate(ResourceTemplate{URITemplate: "test:///a/{x}", Name: "renamed"}, fn); err != nil {
		t.Fatalf("re-register: %v", err)
	}

	list := r.ListTemplates()
	if len(list) != 2 {
		t.Fatalf("got %d templates, want 2 (re-registration must replace, not append)", len(list))
	}
	if list[1].URITemplate != "test:///a/{x}" || list[1].Name != "renamed" {
		t.Errorf("last entry = %+v, want the replaced test:///a/{x} moved to the end", list[1])
	}
}

// Re-registering to edit metadata must not silently disable completion.
func TestReRegisterTemplatePreservesCompleter(t *testing.T) {
	r := NewResourceRegistry()
	fn := func(context.Context, *ResourceReadRequest) (ResourceContentResult, error) {
		return ResourceContentResult{}, nil
	}
	if err := r.RegisterTemplate(ResourceTemplate{URITemplate: "test:///{a}", Name: "x"}, fn); err != nil {
		t.Fatalf("register: %v", err)
	}
	completer := CompletionFunc(func(context.Context, *CompletionRequest) (CompletionResult, error) {
		return CompletionResult{}, nil
	})
	if err := r.SetTemplateCompleter("test:///{a}", completer); err != nil {
		t.Fatalf("SetTemplateCompleter: %v", err)
	}

	if err := r.RegisterTemplate(ResourceTemplate{URITemplate: "test:///{a}", Name: "renamed"}, fn); err != nil {
		t.Fatalf("re-register: %v", err)
	}
	if !r.hasTemplateCompleters() {
		t.Error("re-registering a template dropped its completion provider")
	}
}

func TestSetTemplateCompleterRejectsUnknownTemplate(t *testing.T) {
	r := NewResourceRegistry()
	err := r.SetTemplateCompleter("test:///{nope}", CompletionFunc(
		func(context.Context, *CompletionRequest) (CompletionResult, error) {
			return CompletionResult{}, nil
		}))
	if err == nil {
		t.Fatal("expected an error for a template that was never registered")
	}
}

func TestUnregisterTemplate(t *testing.T) {
	r := NewResourceRegistry()
	fn := func(context.Context, *ResourceReadRequest) (ResourceContentResult, error) {
		return ResourceContentResult{}, nil
	}
	if err := r.RegisterTemplate(ResourceTemplate{URITemplate: "test:///{a}", Name: "x"}, fn); err != nil {
		t.Fatalf("register: %v", err)
	}

	if !r.UnregisterTemplate("test:///{a}") {
		t.Error("UnregisterTemplate returned false for a registered template")
	}
	if r.HasTemplates() {
		t.Error("template survived UnregisterTemplate")
	}
	if r.UnregisterTemplate("test:///{a}") {
		t.Error("UnregisterTemplate returned true for an absent template")
	}
}

// ListTemplates feeds resources/templates/list, where an empty catalog must serialize as []
// and never null — which requires a non-nil slice.
func TestListTemplatesIsNonNilWhenEmpty(t *testing.T) {
	r := NewResourceRegistry()
	if list := r.ListTemplates(); list == nil {
		t.Error("ListTemplates returned nil; it must return an empty non-nil slice")
	}
}

// --- protocol-level tests: resources/templates/list, read dispatch, completions ---

// newTemplateServer is newTestServer's counterpart for the template surface: the same
// concrete test:///hello resource, plus a template family and one template whose handler
// always fails.
func newTemplateServer(t *testing.T) (*Server, *ResourceRegistry) {
	t.Helper()
	tools := NewToolRegistry()
	resources := NewResourceRegistry()

	resources.Register(Resource{URI: "test:///env/PINNED", Name: "pinned", MimeType: "text/plain"},
		func(ctx context.Context) (ResourceContentResult, error) {
			return ResourceContentResult{Text: "concrete"}, nil
		})

	if err := resources.RegisterTemplate(
		ResourceTemplate{
			URITemplate: "test:///env/{name}",
			Name:        "Environment variable",
			Title:       "Env",
			Description: "One environment variable by name",
			MimeType:    "text/plain",
		},
		func(ctx context.Context, req *ResourceReadRequest) (ResourceContentResult, error) {
			return ResourceContentResult{Text: "value-of-" + req.Vars["name"]}, nil
		}); err != nil {
		t.Fatalf("register template: %v", err)
	}

	if err := resources.RegisterTemplate(
		ResourceTemplate{URITemplate: "test:///boom/{id}", Name: "boom"},
		func(ctx context.Context, req *ResourceReadRequest) (ResourceContentResult, error) {
			return ResourceContentResult{}, errors.New("handler exploded")
		}); err != nil {
		t.Fatalf("register boom template: %v", err)
	}

	srv := NewServer(tools, resources, &ServerConfig{Name: "test-server", Version: "0.0.0"})
	return srv, resources
}

func TestResourcesTemplatesListEmptyIsEmptyArray(t *testing.T) {
	srv, _, _ := newTestServer(t) // has resources, no templates
	env := call(t, srv, 1, "resources/templates/list", map[string]interface{}{"_meta": validMeta()})
	if env.Error != nil {
		t.Fatalf("unexpected error: %+v", env.Error)
	}
	// The wire shape matters, not just the decoded value: a nil Go slice would marshal to
	// null, which is not an empty list.
	if !strings.Contains(string(env.Result), `"resourceTemplates":[]`) {
		t.Errorf("result %s does not contain an empty resourceTemplates array", env.Result)
	}
}

func TestResourcesTemplatesListEnvelopeAndEntries(t *testing.T) {
	srv, _ := newTemplateServer(t)
	env := call(t, srv, 1, "resources/templates/list", map[string]interface{}{"_meta": validMeta()})
	if env.Error != nil {
		t.Fatalf("unexpected error: %+v", env.Error)
	}

	var result struct {
		ResultType        string                 `json:"resultType"`
		TTLMs             int64                  `json:"ttlMs"`
		CacheScope        string                 `json:"cacheScope"`
		Meta              map[string]interface{} `json:"_meta"`
		ResourceTemplates []ResourceTemplate     `json:"resourceTemplates"`
	}
	if err := json.Unmarshal(env.Result, &result); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}

	if result.ResultType != ResultTypeComplete {
		t.Errorf("resultType = %q, want %q", result.ResultType, ResultTypeComplete)
	}
	if result.CacheScope != CacheScopePublic {
		t.Errorf("cacheScope = %q, want %q", result.CacheScope, CacheScopePublic)
	}
	if result.TTLMs < 0 {
		t.Errorf("ttlMs = %d, want >= 0", result.TTLMs)
	}
	if _, ok := result.Meta[metaKeyServerInfo]; !ok {
		t.Errorf("_meta missing %s", metaKeyServerInfo)
	}
	if len(result.ResourceTemplates) != 2 {
		t.Fatalf("got %d templates, want 2", len(result.ResourceTemplates))
	}
	first := result.ResourceTemplates[0]
	if first.URITemplate != "test:///env/{name}" || first.Name != "Environment variable" {
		t.Errorf("first template = %+v, want the env template in registration order", first)
	}
	if first.MimeType != "text/plain" || first.Title != "Env" {
		t.Errorf("first template lost metadata: %+v", first)
	}
}

func TestResourcesTemplatesListPaginates(t *testing.T) {
	srv, resources := newTemplateServer(t)
	fn := func(context.Context, *ResourceReadRequest) (ResourceContentResult, error) {
		return ResourceContentResult{}, nil
	}
	// 2 already registered; push past defaultPageSize.
	for i := 0; i < defaultPageSize; i++ {
		u := "test:///bulk" + strconv.Itoa(i) + "/{id}"
		if err := resources.RegisterTemplate(ResourceTemplate{URITemplate: u, Name: u}, fn); err != nil {
			t.Fatalf("register %s: %v", u, err)
		}
	}

	env := call(t, srv, 1, "resources/templates/list", map[string]interface{}{"_meta": validMeta()})
	var page1 ResourcesTemplatesListResult
	if err := json.Unmarshal(env.Result, &page1); err != nil {
		t.Fatalf("unmarshal page 1: %v", err)
	}
	if len(page1.ResourceTemplates) != defaultPageSize {
		t.Fatalf("page 1 had %d entries, want %d", len(page1.ResourceTemplates), defaultPageSize)
	}
	if page1.NextCursor == "" {
		t.Fatal("page 1 has no nextCursor but more entries exist")
	}

	env = call(t, srv, 2, "resources/templates/list",
		map[string]interface{}{"_meta": validMeta(), "cursor": page1.NextCursor})
	var page2 ResourcesTemplatesListResult
	if err := json.Unmarshal(env.Result, &page2); err != nil {
		t.Fatalf("unmarshal page 2: %v", err)
	}
	if len(page2.ResourceTemplates) != 2 {
		t.Errorf("page 2 had %d entries, want 2", len(page2.ResourceTemplates))
	}
	if page2.NextCursor != "" {
		t.Errorf("page 2 nextCursor = %q, want empty (last page)", page2.NextCursor)
	}
}

func TestResourcesTemplatesListInvalidCursor(t *testing.T) {
	srv, _ := newTemplateServer(t)
	env := call(t, srv, 1, "resources/templates/list",
		map[string]interface{}{"_meta": validMeta(), "cursor": "not-a-cursor"})
	if env.Error == nil {
		t.Fatal("expected an error for a malformed cursor")
	}
	if env.Error.Code != transport.InvalidParams {
		t.Errorf("code = %d, want %d", env.Error.Code, transport.InvalidParams)
	}
}

// A URI registered as a concrete resource must not be captured by a template that also
// matches it.
func TestConcreteResourceBeatsTemplate(t *testing.T) {
	srv, _ := newTemplateServer(t)
	env := call(t, srv, 1, "resources/read",
		map[string]interface{}{"_meta": validMeta(), "uri": "test:///env/PINNED"})
	if env.Error != nil {
		t.Fatalf("unexpected error: %+v", env.Error)
	}

	var result ResourcesReadResult
	if err := json.Unmarshal(env.Result, &result); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if len(result.Contents) != 1 {
		t.Fatalf("got %d contents, want 1", len(result.Contents))
	}
	if result.Contents[0].Text != "concrete" {
		t.Errorf("text = %q, want %q — the concrete resource must win", result.Contents[0].Text, "concrete")
	}
}

func TestResourcesReadThroughTemplate(t *testing.T) {
	srv, _ := newTemplateServer(t)
	env := call(t, srv, 1, "resources/read",
		map[string]interface{}{"_meta": validMeta(), "uri": "test:///env/HOME"})
	if env.Error != nil {
		t.Fatalf("unexpected error: %+v", env.Error)
	}

	var result ResourcesReadResult
	if err := json.Unmarshal(env.Result, &result); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if len(result.Contents) != 1 {
		t.Fatalf("got %d contents, want 1", len(result.Contents))
	}
	c := result.Contents[0]
	// contents[0].uri is the concrete URI the client asked for, never the template.
	if c.URI != "test:///env/HOME" {
		t.Errorf("uri = %q, want the concrete request URI", c.URI)
	}
	if c.Text != "value-of-HOME" {
		t.Errorf("text = %q, want %q", c.Text, "value-of-HOME")
	}
	if c.Name != "Environment variable" || c.Title != "Env" {
		t.Errorf("name/title = %q/%q, want them taken from the template", c.Name, c.Title)
	}
	if c.MimeType != "text/plain" {
		t.Errorf("mimeType = %q, want the template's text/plain", c.MimeType)
	}
}

func TestTemplateReadDecodesVariables(t *testing.T) {
	srv, _ := newTemplateServer(t)
	env := call(t, srv, 1, "resources/read",
		map[string]interface{}{"_meta": validMeta(), "uri": "test:///env/A%20B"})
	if env.Error != nil {
		t.Fatalf("unexpected error: %+v", env.Error)
	}
	var result ResourcesReadResult
	if err := json.Unmarshal(env.Result, &result); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if result.Contents[0].Text != "value-of-A B" {
		t.Errorf("text = %q, want the percent-decoded variable", result.Contents[0].Text)
	}
}

// No template matched: an error, never contents: [].
func TestUnmatchedTemplateURIIsInvalidParams(t *testing.T) {
	srv, _ := newTemplateServer(t)
	env := call(t, srv, 1, "resources/read",
		map[string]interface{}{"_meta": validMeta(), "uri": "test:///nothing/here/at/all"})
	if env.Error == nil {
		t.Fatal("expected an error for a URI matching neither a resource nor a template")
	}
	if env.Error.Code != transport.InvalidParams {
		t.Errorf("code = %d, want %d (-32002 must never be emitted)", env.Error.Code, transport.InvalidParams)
	}
	if env.Result != nil {
		t.Errorf("got a result alongside the error: %s", env.Result)
	}
}

func TestTemplateHandlerErrorIsInternalError(t *testing.T) {
	srv, _ := newTemplateServer(t)
	env := call(t, srv, 1, "resources/read",
		map[string]interface{}{"_meta": validMeta(), "uri": "test:///boom/1"})
	if env.Error == nil {
		t.Fatal("expected an error from the failing template handler")
	}
	if env.Error.Code != transport.InternalError {
		t.Errorf("code = %d, want %d", env.Error.Code, transport.InternalError)
	}
}

func TestRegisterTemplateFiresListChanged(t *testing.T) {
	srv, resources := newTemplateServer(t)
	notes := observeDuringMutation(t, srv, map[string]interface{}{"resourcesListChanged": true}, func() {
		_ = resources.RegisterTemplate(ResourceTemplate{URITemplate: "test:///new/{id}", Name: "new"},
			func(context.Context, *ResourceReadRequest) (ResourceContentResult, error) {
				return ResourceContentResult{}, nil
			})
	})
	if !hasNotification(notes, "notifications/resources/list_changed") {
		t.Error("registering a template did not fire resources/list_changed")
	}
}

func TestUnregisterTemplateFiresListChanged(t *testing.T) {
	srv, resources := newTemplateServer(t)
	notes := observeDuringMutation(t, srv, map[string]interface{}{"resourcesListChanged": true}, func() {
		resources.UnregisterTemplate("test:///boom/{id}")
	})
	if !hasNotification(notes, "notifications/resources/list_changed") {
		t.Error("unregistering a template did not fire resources/list_changed")
	}
}

func TestUnregisterAbsentTemplateIsSilent(t *testing.T) {
	srv, resources := newTemplateServer(t)
	notes := observeDuringMutation(t, srv, map[string]interface{}{"resourcesListChanged": true}, func() {
		resources.UnregisterTemplate("test:///never/{registered}")
	})
	if hasNotification(notes, "notifications/resources/list_changed") {
		t.Error("unregistering an absent template fired a notification; it must be a no-op")
	}
}

// A consumer watching an upstream needs to announce updates for URIs it never registered
// individually — that is the whole point of a template family.
func TestNotifyUpdatedWorksForTemplateURI(t *testing.T) {
	srv, resources := newTemplateServer(t)
	const uri = "test:///env/WATCHED"

	notes := observeDuringMutation(t, srv,
		map[string]interface{}{"resourceSubscriptions": []string{uri}}, func() {
			if !resources.NotifyUpdated(uri) {
				t.Error("NotifyUpdated returned false for a URI matching a registered template")
			}
		})

	params := notificationParams(t, notes, "notifications/resources/updated")
	if params["uri"] != uri {
		t.Errorf("updated uri = %v, want %q", params["uri"], uri)
	}
}

func TestNotifyUpdatedStillIgnoresUnmatchedURI(t *testing.T) {
	_, resources := newTemplateServer(t)
	if resources.NotifyUpdated("test:///matches/nothing/whatsoever") {
		t.Error("NotifyUpdated returned true for a URI matching neither a resource nor a template")
	}
}

// A templates-only registry must still declare the resources capability, or a client will
// never think to probe resources/templates/list.
func TestTemplatesOnlyRegistryAdvertisesResources(t *testing.T) {
	resources := NewResourceRegistry()
	if err := resources.RegisterTemplate(ResourceTemplate{URITemplate: "test:///{a}", Name: "x"},
		func(context.Context, *ResourceReadRequest) (ResourceContentResult, error) {
			return ResourceContentResult{}, nil
		}); err != nil {
		t.Fatalf("register template: %v", err)
	}
	srv := NewServer(NewToolRegistry(), resources, &ServerConfig{Name: "t", Version: "0"})

	env := call(t, srv, 1, "server/discover", map[string]interface{}{"_meta": validMeta()})
	var result DiscoverResult
	if err := json.Unmarshal(env.Result, &result); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if result.Capabilities.Resources == nil {
		t.Error("a templates-only registry did not advertise the resources capability")
	}
	if result.Capabilities.Completions != nil {
		t.Error("completions advertised with no CompletionProvider registered")
	}
}

// --- completion/complete ---

func envCompleter(values []string) CompletionFunc {
	return func(ctx context.Context, req *CompletionRequest) (CompletionResult, error) {
		var out []string
		for _, v := range values {
			if strings.HasPrefix(v, req.Value) {
				out = append(out, v)
			}
		}
		return CompletionResult{Values: out}, nil
	}
}

func TestCompletionUnsupportedWithoutProvider(t *testing.T) {
	srv, _ := newTemplateServer(t)
	env := call(t, srv, 1, "completion/complete", map[string]interface{}{
		"_meta":    validMeta(),
		"ref":      map[string]interface{}{"type": "ref/resource", "uri": "test:///env/{name}"},
		"argument": map[string]interface{}{"name": "name", "value": ""},
	})
	if env.Error == nil {
		t.Fatal("expected an error with no completion provider registered")
	}
	// -32601 is exactly the probe clients use to detect that completions are unsupported.
	if env.Error.Code != transport.MethodNotFound {
		t.Errorf("code = %d, want %d", env.Error.Code, transport.MethodNotFound)
	}
}

func TestCompletionReturnsValuesAndAdvertisesCapability(t *testing.T) {
	srv, resources := newTemplateServer(t)
	if err := resources.SetTemplateCompleter("test:///env/{name}",
		envCompleter([]string{"HOME", "HOSTNAME", "PATH"})); err != nil {
		t.Fatalf("SetTemplateCompleter: %v", err)
	}

	env := call(t, srv, 1, "server/discover", map[string]interface{}{"_meta": validMeta()})
	var discover DiscoverResult
	if err := json.Unmarshal(env.Result, &discover); err != nil {
		t.Fatalf("unmarshal discover: %v", err)
	}
	if discover.Capabilities.Completions == nil {
		t.Error("completions capability not advertised after registering a provider")
	}

	env = call(t, srv, 2, "completion/complete", map[string]interface{}{
		"_meta":    validMeta(),
		"ref":      map[string]interface{}{"type": "ref/resource", "uri": "test:///env/{name}"},
		"argument": map[string]interface{}{"name": "name", "value": "HO"},
	})
	if env.Error != nil {
		t.Fatalf("unexpected error: %+v", env.Error)
	}
	var result CompletionCompleteResult
	if err := json.Unmarshal(env.Result, &result); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if result.ResultType != ResultTypeComplete {
		t.Errorf("resultType = %q, want %q", result.ResultType, ResultTypeComplete)
	}
	if len(result.Completion.Values) != 2 {
		t.Fatalf("values = %v, want the two HO-prefixed entries", result.Completion.Values)
	}
	if result.Completion.HasMore {
		t.Error("hasMore set on a complete list")
	}
}

// context.arguments carries already-resolved variables, which is how a provider narrows a
// later variable's domain.
func TestCompletionReceivesContextArguments(t *testing.T) {
	srv, resources := newTemplateServer(t)
	var gotContext map[string]string
	if err := resources.SetTemplateCompleter("test:///env/{name}", CompletionFunc(
		func(ctx context.Context, req *CompletionRequest) (CompletionResult, error) {
			gotContext = req.Context
			return CompletionResult{Values: []string{"x"}}, nil
		})); err != nil {
		t.Fatalf("SetTemplateCompleter: %v", err)
	}

	call(t, srv, 1, "completion/complete", map[string]interface{}{
		"_meta":    validMeta(),
		"ref":      map[string]interface{}{"type": "ref/resource", "uri": "test:///env/{name}"},
		"argument": map[string]interface{}{"name": "name", "value": ""},
		"context":  map[string]interface{}{"arguments": map[string]interface{}{"cluster": "prod"}},
	})

	if gotContext["cluster"] != "prod" {
		t.Errorf("context.arguments not delivered to the provider: %v", gotContext)
	}
}

func TestCompletionTruncatesAtHundred(t *testing.T) {
	srv, resources := newTemplateServer(t)
	many := make([]string, 150)
	for i := range many {
		many[i] = "v" + strconv.Itoa(i)
	}
	if err := resources.SetTemplateCompleter("test:///env/{name}", CompletionFunc(
		func(ctx context.Context, req *CompletionRequest) (CompletionResult, error) {
			return CompletionResult{Values: many}, nil
		})); err != nil {
		t.Fatalf("SetTemplateCompleter: %v", err)
	}

	env := call(t, srv, 1, "completion/complete", map[string]interface{}{
		"_meta":    validMeta(),
		"ref":      map[string]interface{}{"type": "ref/resource", "uri": "test:///env/{name}"},
		"argument": map[string]interface{}{"name": "name", "value": ""},
	})
	var result CompletionCompleteResult
	if err := json.Unmarshal(env.Result, &result); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if len(result.Completion.Values) != maxCompletionValues {
		t.Errorf("got %d values, want the %d cap", len(result.Completion.Values), maxCompletionValues)
	}
	if !result.Completion.HasMore {
		t.Error("hasMore not set after truncation")
	}
}

func TestCompletionRejectsUnknownRefAndArgument(t *testing.T) {
	srv, resources := newTemplateServer(t)
	if err := resources.SetTemplateCompleter("test:///env/{name}", envCompleter(nil)); err != nil {
		t.Fatalf("SetTemplateCompleter: %v", err)
	}

	// A template that was never registered.
	env := call(t, srv, 1, "completion/complete", map[string]interface{}{
		"_meta":    validMeta(),
		"ref":      map[string]interface{}{"type": "ref/resource", "uri": "test:///nope/{x}"},
		"argument": map[string]interface{}{"name": "x", "value": ""},
	})
	if env.Error == nil || env.Error.Code != transport.InvalidParams {
		t.Errorf("unknown ref: got %+v, want -32602", env.Error)
	}

	// A variable the template does not have.
	env = call(t, srv, 2, "completion/complete", map[string]interface{}{
		"_meta":    validMeta(),
		"ref":      map[string]interface{}{"type": "ref/resource", "uri": "test:///env/{name}"},
		"argument": map[string]interface{}{"name": "nosuchvar", "value": ""},
	})
	if env.Error == nil || env.Error.Code != transport.InvalidParams {
		t.Errorf("unknown argument: got %+v, want -32602", env.Error)
	}

	// A registered template with no completer of its own.
	env = call(t, srv, 3, "completion/complete", map[string]interface{}{
		"_meta":    validMeta(),
		"ref":      map[string]interface{}{"type": "ref/resource", "uri": "test:///boom/{id}"},
		"argument": map[string]interface{}{"name": "id", "value": ""},
	})
	if env.Error == nil || env.Error.Code != transport.InvalidParams {
		t.Errorf("template without a completer: got %+v, want -32602", env.Error)
	}

	// ref/prompt: prompts are not implemented by this library.
	env = call(t, srv, 4, "completion/complete", map[string]interface{}{
		"_meta":    validMeta(),
		"ref":      map[string]interface{}{"type": "ref/prompt", "name": "whatever"},
		"argument": map[string]interface{}{"name": "x", "value": ""},
	})
	if env.Error == nil || env.Error.Code != transport.InvalidParams {
		t.Errorf("ref/prompt: got %+v, want -32602", env.Error)
	}
}

func TestCompletionProviderErrorIsInternalError(t *testing.T) {
	srv, resources := newTemplateServer(t)
	if err := resources.SetTemplateCompleter("test:///env/{name}", CompletionFunc(
		func(ctx context.Context, req *CompletionRequest) (CompletionResult, error) {
			return CompletionResult{}, errors.New("index unavailable")
		})); err != nil {
		t.Fatalf("SetTemplateCompleter: %v", err)
	}

	env := call(t, srv, 1, "completion/complete", map[string]interface{}{
		"_meta":    validMeta(),
		"ref":      map[string]interface{}{"type": "ref/resource", "uri": "test:///env/{name}"},
		"argument": map[string]interface{}{"name": "name", "value": ""},
	})
	if env.Error == nil || env.Error.Code != transport.InternalError {
		t.Errorf("got %+v, want -32603", env.Error)
	}
}

// A provider returning nothing must still produce "values":[] on the wire, not null.
func TestCompletionEmptyValuesIsEmptyArray(t *testing.T) {
	srv, resources := newTemplateServer(t)
	if err := resources.SetTemplateCompleter("test:///env/{name}", envCompleter(nil)); err != nil {
		t.Fatalf("SetTemplateCompleter: %v", err)
	}

	env := call(t, srv, 1, "completion/complete", map[string]interface{}{
		"_meta":    validMeta(),
		"ref":      map[string]interface{}{"type": "ref/resource", "uri": "test:///env/{name}"},
		"argument": map[string]interface{}{"name": "name", "value": "zzz"},
	})
	if env.Error != nil {
		t.Fatalf("unexpected error: %+v", env.Error)
	}
	if !strings.Contains(string(env.Result), `"values":[]`) {
		t.Errorf("result %s does not contain an empty values array", env.Result)
	}
}

// A template claims a whole URI shape, so a URI naming an absent member reaches the handler
// rather than failing to match. The handler saying so must read as -32602 ("no such
// resource"), not -32603 ("this server is broken").
func TestTemplateHandlerNotFoundIsInvalidParams(t *testing.T) {
	resources := NewResourceRegistry()
	if err := resources.RegisterTemplate(ResourceTemplate{URITemplate: "test:///pod/{name}", Name: "pod"},
		func(ctx context.Context, req *ResourceReadRequest) (ResourceContentResult, error) {
			return ResourceContentResult{}, fmt.Errorf("pod %q: %w", req.Vars["name"], ErrResourceNotFound)
		}); err != nil {
		t.Fatalf("register template: %v", err)
	}
	srv := NewServer(NewToolRegistry(), resources, &ServerConfig{Name: "t", Version: "0"})

	env := call(t, srv, 1, "resources/read",
		map[string]interface{}{"_meta": validMeta(), "uri": "test:///pod/absent"})
	if env.Error == nil {
		t.Fatal("expected an error")
	}
	if env.Error.Code != transport.InvalidParams {
		t.Errorf("code = %d, want %d", env.Error.Code, transport.InvalidParams)
	}
	if env.Result != nil {
		t.Errorf("got a result alongside the error: %s", env.Result)
	}
}

// The same sentinel works for a concrete resource whose backing content has gone away.
func TestConcreteResourceNotFoundIsInvalidParams(t *testing.T) {
	resources := NewResourceRegistry()
	resources.Register(Resource{URI: "test:///gone", Name: "gone"},
		func(ctx context.Context) (ResourceContentResult, error) {
			return ResourceContentResult{}, ErrResourceNotFound
		})
	srv := NewServer(NewToolRegistry(), resources, &ServerConfig{Name: "t", Version: "0"})

	env := call(t, srv, 1, "resources/read",
		map[string]interface{}{"_meta": validMeta(), "uri": "test:///gone"})
	if env.Error == nil || env.Error.Code != transport.InvalidParams {
		t.Errorf("got %+v, want -32602", env.Error)
	}
}
