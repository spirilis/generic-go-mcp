package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/spirilis/generic-go-mcp/transport"
)

// binaryAsset is deliberately not valid UTF-8, so it must travel as a base64 blob and its
// digest still has to match the raw bytes.
var binaryAsset = []byte{0x89, 0x50, 0x4e, 0x47, 0x00, 0xff, 0xfe, 0x01}

const refundsSKILL = `---
name: refunds
description: Process customer refund requests per company policy
license: Apache-2.0
metadata:
  version: 2.1.0
allowed-tools:
  - Bash
  - Read
---

Refund procedure body.
`

const pdfSKILL = `---
name: pdf-processing
description: Extract, fill, and assemble PDF documents
---

PDF body.
`

func skillMapFS() fstest.MapFS {
	return fstest.MapFS{
		"acme/billing/refunds/SKILL.md":                   &fstest.MapFile{Data: []byte(refundsSKILL)},
		"acme/billing/refunds/examples/email.md":          &fstest.MapFile{Data: []byte("# Email template\n")},
		"pdf-processing/SKILL.md":                         &fstest.MapFile{Data: []byte(pdfSKILL)},
		"pdf-processing/assets/logo.png":                  &fstest.MapFile{Data: binaryAsset},
		"pdf-processing/scripts/extract.py":               &fstest.MapFile{Data: []byte("print('hi')\n")},
		"pdf-processing/templates/invoice.md":             &fstest.MapFile{Data: []byte("invoice\n")},
		"pdf-processing/templates/regional/eu-invoice.md": &fstest.MapFile{Data: []byte("eu invoice\n")},
	}
}

// newSkillServer builds a server whose skill catalog is the fixture filesystem above, with
// resources/directory/read switched on.
func newSkillServer(t *testing.T) (*Server, *SkillRegistry, *ResourceRegistry) {
	t.Helper()
	skills, resources := newLoadedSkillRegistry(t)
	srv := NewServer(NewToolRegistry(), resources, &ServerConfig{
		Name:                "skill-test-server",
		Version:             "0.0.0",
		Skills:              skills,
		SkillsDirectoryRead: true,
	})
	return srv, skills, resources
}

func newLoadedSkillRegistry(t *testing.T) (*SkillRegistry, *ResourceRegistry) {
	t.Helper()
	resources := NewResourceRegistry()
	skills := NewSkillRegistry(resources)
	if err := skills.LoadFS(skillMapFS(), "skill://"); err != nil {
		t.Fatalf("LoadFS: %v", err)
	}
	return skills, resources
}

// newEmptySkillServer has a SkillRegistry with nothing in it — the resolver-backed /
// not-yet-populated case, which still declares the extension.
func newEmptySkillServer(t *testing.T, directoryRead bool) (*Server, *SkillRegistry, *ResourceRegistry) {
	t.Helper()
	resources := NewResourceRegistry()
	skills := NewSkillRegistry(resources)
	srv := NewServer(NewToolRegistry(), resources, &ServerConfig{
		Name:                "skill-test-server",
		Version:             "0.0.0",
		Skills:              skills,
		SkillsDirectoryRead: directoryRead,
	})
	return srv, skills, resources
}

func decodeResult(t *testing.T, env rpcResponseEnvelope, into interface{}) {
	t.Helper()
	if env.Error != nil {
		t.Fatalf("unexpected error: %+v", env.Error)
	}
	if err := json.Unmarshal(env.Result, into); err != nil {
		t.Fatalf("unmarshal result: %v (body: %s)", err, env.Result)
	}
}

func listSkills(t *testing.T, srv *Server) []Skill {
	t.Helper()
	env := call(t, srv, 1, "skills/list", map[string]interface{}{"_meta": validMeta()})
	var body struct {
		Skills []Skill `json:"skills"`
	}
	decodeResult(t, env, &body)
	return body.Skills
}

// readResourceBytes returns exactly the bytes a client would reconstruct from a
// resources/read result, whichever payload field carried them.
func readResourceBytes(t *testing.T, srv *Server, uri string) ([]byte, string) {
	t.Helper()
	env := call(t, srv, 1, "resources/read", map[string]interface{}{"_meta": validMeta(), "uri": uri})
	var body struct {
		Contents []struct {
			Text     string `json:"text"`
			Blob     string `json:"blob"`
			MimeType string `json:"mimeType"`
		} `json:"contents"`
	}
	decodeResult(t, env, &body)
	if len(body.Contents) != 1 {
		t.Fatalf("resources/read %s: got %d contents, want 1", uri, len(body.Contents))
	}
	c := body.Contents[0]
	if c.Blob != "" {
		raw, err := base64.StdEncoding.DecodeString(c.Blob)
		if err != nil {
			t.Fatalf("resources/read %s: blob is not base64: %v", uri, err)
		}
		return raw, c.MimeType
	}
	return []byte(c.Text), c.MimeType
}

func digestOf(b []byte) string { return fmt.Sprintf("sha256:%x", sha256.Sum256(b)) }

// ---------------------------------------------------------------------------
// skills/list
// ---------------------------------------------------------------------------

func TestSkillsListEmptyIsEmptyArray(t *testing.T) {
	srv, _, _ := newEmptySkillServer(t, false)
	env := call(t, srv, 1, "skills/list", map[string]interface{}{"_meta": validMeta()})
	if env.Error != nil {
		t.Fatalf("unexpected error: %+v", env.Error)
	}
	// The wire shape matters, not just the decoded value: a nil Go slice would marshal to
	// null, and a host must be able to tell "no skills listed" from "malformed".
	if !strings.Contains(string(env.Result), `"skills":[]`) {
		t.Errorf("result %s does not contain an empty skills array", env.Result)
	}
}

func TestSkillsListResultEnvelope(t *testing.T) {
	srv, _, _ := newSkillServer(t)
	env := call(t, srv, 1, "skills/list", map[string]interface{}{"_meta": validMeta()})
	var result struct {
		ResultType string                 `json:"resultType"`
		TTLMs      int64                  `json:"ttlMs"`
		CacheScope string                 `json:"cacheScope"`
		Meta       map[string]interface{} `json:"_meta"`
		Skills     []Skill                `json:"skills"`
	}
	decodeResult(t, env, &result)

	if result.ResultType != ResultTypeComplete {
		t.Errorf("resultType = %q, want %q", result.ResultType, ResultTypeComplete)
	}
	if result.TTLMs != defaultListTTLMs {
		t.Errorf("ttlMs = %d, want %d", result.TTLMs, defaultListTTLMs)
	}
	if result.CacheScope != CacheScopePublic {
		t.Errorf("cacheScope = %q, want %q", result.CacheScope, CacheScopePublic)
	}
	if _, ok := result.Meta[metaKeyServerInfo]; !ok {
		t.Errorf("_meta missing %s", metaKeyServerInfo)
	}
	if len(result.Skills) != 2 {
		t.Fatalf("got %d skills, want 2: %s", len(result.Skills), env.Result)
	}
	if result.Skills[0].URI != "skill://acme/billing/refunds/SKILL.md" {
		t.Errorf("skills[0].uri = %q", result.Skills[0].URI)
	}
	if result.Skills[1].URI != "skill://pdf-processing/SKILL.md" {
		t.Errorf("skills[1].uri = %q", result.Skills[1].URI)
	}
}

func TestSkillsListPreservesUnknownFrontmatterVerbatim(t *testing.T) {
	srv, _, _ := newSkillServer(t)
	skills := listSkills(t, srv)

	fm := skills[0].Frontmatter
	if fm["license"] != "Apache-2.0" {
		t.Errorf("license = %v, want Apache-2.0", fm["license"])
	}
	meta, ok := fm["metadata"].(map[string]interface{})
	if !ok {
		t.Fatalf("metadata = %T, want a nested object", fm["metadata"])
	}
	if meta["version"] != "2.1.0" {
		t.Errorf("metadata.version = %v, want 2.1.0", meta["version"])
	}
	tools, ok := fm["allowed-tools"].([]interface{})
	if !ok || len(tools) != 2 {
		t.Fatalf("allowed-tools = %#v, want a 2-element list preserved verbatim", fm["allowed-tools"])
	}
}

// TestSkillDigestsMatchResourceRead is the invariant most likely to rot: what skills/list
// publishes must be the sha256 of what resources/read actually serves, on both the text and
// the base64 blob path.
func TestSkillDigestsMatchResourceRead(t *testing.T) {
	srv, _, _ := newSkillServer(t)

	sawBlob := false
	for _, skill := range listSkills(t, srv) {
		if len(skill.Resources) == 0 {
			t.Fatalf("skill %s published no resources", skill.URI)
		}
		if skill.Resources[0].URI != skill.URI {
			t.Errorf("skill %s: resources[0] is %q, want the SKILL.md itself", skill.URI, skill.Resources[0].URI)
		}
		for _, res := range skill.Resources {
			body, mimeType := readResourceBytes(t, srv, res.URI)
			if got := digestOf(body); got != res.Digest {
				t.Errorf("%s: published digest %s, but resources/read returns %s", res.URI, res.Digest, got)
			}
			if strings.HasSuffix(res.URI, ".png") {
				sawBlob = true
				if mimeType != "image/png" {
					t.Errorf("%s: mimeType = %q, want image/png", res.URI, mimeType)
				}
			}
			if strings.HasSuffix(res.URI, "/SKILL.md") && mimeType != "text/markdown" {
				t.Errorf("%s: mimeType = %q, want text/markdown", res.URI, mimeType)
			}
		}
	}
	if !sawBlob {
		t.Fatal("fixture never exercised the binary/blob path")
	}
}

func TestSkillsListInvalidCursorIsInvalidParams(t *testing.T) {
	srv, _, _ := newSkillServer(t)
	env := call(t, srv, 1, "skills/list", map[string]interface{}{"_meta": validMeta(), "cursor": "not-a-cursor!!"})
	if env.Error == nil || env.Error.Code != transport.InvalidParams {
		t.Fatalf("want -32602 for a bad cursor, got %+v", env.Error)
	}
}

// ---------------------------------------------------------------------------
// skills/get
// ---------------------------------------------------------------------------

func TestSkillsGetWrapsEntryUnderSkillKey(t *testing.T) {
	srv, _, _ := newSkillServer(t)
	listed := listSkills(t, srv)[1]

	env := call(t, srv, 1, "skills/get", map[string]interface{}{
		"_meta": validMeta(), "uri": listed.URI,
	})
	if env.Error != nil {
		t.Fatalf("unexpected error: %+v", env.Error)
	}
	// SEP-2640 nests the entry under "skill"; a bare entry is the wrong shape.
	if !strings.Contains(string(env.Result), `"skill":{`) {
		t.Errorf("result %s does not nest the entry under a skill key", env.Result)
	}

	var body struct {
		Skill Skill `json:"skill"`
	}
	decodeResult(t, env, &body)
	if body.Skill.URI != listed.URI {
		t.Errorf("uri = %q, want %q", body.Skill.URI, listed.URI)
	}
	want, _ := json.Marshal(listed)
	got, _ := json.Marshal(body.Skill)
	if string(want) != string(got) {
		t.Errorf("skills/get entry differs from its skills/list element:\n get %s\nwant %s", got, want)
	}
}

func TestSkillsGetUnknownURIIsInvalidParams(t *testing.T) {
	srv, _, resources := newSkillServer(t)
	// A perfectly good resource that is not a skill. Skill-ness is established by
	// skills/list and skills/get, never by a URI's shape or scheme.
	resources.Register(Resource{URI: "skill://not-a-skill/SKILL.md", Name: "decoy"},
		func(context.Context) (ResourceContentResult, error) {
			return ResourceContentResult{Text: "decoy"}, nil
		})

	for _, uri := range []string{"skill://not-a-skill/SKILL.md", "test:///nope"} {
		env := call(t, srv, 1, "skills/get", map[string]interface{}{"_meta": validMeta(), "uri": uri})
		if env.Error == nil || env.Error.Code != transport.InvalidParams {
			t.Errorf("skills/get %s: want -32602, got %+v", uri, env.Error)
		}
	}
}

func TestSkillRegisteredResourceIsNotASkill(t *testing.T) {
	srv, _, resources := newSkillServer(t)
	resources.Register(Resource{URI: "skill://decoy/SKILL.md", Name: "decoy"},
		func(context.Context) (ResourceContentResult, error) {
			return ResourceContentResult{Text: "decoy"}, nil
		})

	for _, s := range listSkills(t, srv) {
		if s.URI == "skill://decoy/SKILL.md" {
			t.Fatal("a skill:// resource registered straight on the ResourceRegistry appeared in skills/list")
		}
	}
}

func TestSkillsGetResolverConsultedOnlyOnMiss(t *testing.T) {
	srv, skills, _ := newSkillServer(t)

	calls := 0
	skills.SetResolver(func(ctx context.Context, uri string) (Skill, bool, error) {
		calls++
		if uri != "skill://generated/SKILL.md" {
			return Skill{}, false, nil
		}
		return Skill{
			URI:         uri,
			Frontmatter: map[string]interface{}{"name": "generated", "description": "made up on demand"},
		}, true, nil
	})

	// A registered hit must not pay for the resolver.
	env := call(t, srv, 1, "skills/get", map[string]interface{}{
		"_meta": validMeta(), "uri": "skill://pdf-processing/SKILL.md",
	})
	if env.Error != nil {
		t.Fatalf("unexpected error: %+v", env.Error)
	}
	if calls != 0 {
		t.Errorf("resolver consulted %d times for a registered skill, want 0", calls)
	}

	env = call(t, srv, 2, "skills/get", map[string]interface{}{
		"_meta": validMeta(), "uri": "skill://generated/SKILL.md",
	})
	var body struct {
		Skill Skill `json:"skill"`
	}
	decodeResult(t, env, &body)
	if calls != 1 {
		t.Errorf("resolver consulted %d times, want 1", calls)
	}
	if body.Skill.URI != "skill://generated/SKILL.md" {
		t.Errorf("uri = %q", body.Skill.URI)
	}
	// A dynamically generated skill offers no integrity, so "resources" is omitted rather
	// than sent empty.
	if strings.Contains(string(env.Result), `"resources"`) {
		t.Errorf("result %s should omit resources for a resolver-generated skill", env.Result)
	}

	// A resolver that declines still produces -32602, not a fabricated entry.
	env = call(t, srv, 3, "skills/get", map[string]interface{}{
		"_meta": validMeta(), "uri": "skill://nothing/SKILL.md",
	})
	if env.Error == nil || env.Error.Code != transport.InvalidParams {
		t.Fatalf("want -32602 when the resolver declines, got %+v", env.Error)
	}
}

// ---------------------------------------------------------------------------
// Capability declaration and the opt-out
// ---------------------------------------------------------------------------

func discoverCapabilities(t *testing.T, srv *Server) map[string]json.RawMessage {
	t.Helper()
	env := call(t, srv, 1, "server/discover", map[string]interface{}{"_meta": validMeta()})
	var result struct {
		Capabilities map[string]json.RawMessage `json:"capabilities"`
	}
	decodeResult(t, env, &result)
	return result.Capabilities
}

func TestSkillsDisabledIsInvisible(t *testing.T) {
	srv, _, _ := newTestServer(t) // no ServerConfig.Skills at all

	if raw, present := discoverCapabilities(t, srv)["extensions"]; present {
		t.Errorf("server/discover declares extensions %s with no skill registry configured", raw)
	}

	for _, method := range []string{"skills/list", "skills/get", "resources/directory/read"} {
		env := call(t, srv, 1, method, map[string]interface{}{"_meta": validMeta(), "uri": "skill://x/SKILL.md"})
		if env.Error == nil || env.Error.Code != transport.MethodNotFound {
			t.Errorf("%s: want -32601 with skills disabled, got %+v", method, env.Error)
		}
	}
}

func TestSkillsCapabilityAndDirectoryReadSetting(t *testing.T) {
	for _, tc := range []struct {
		directoryRead bool
		want          string
	}{
		{false, `{"directoryRead":false}`},
		{true, `{"directoryRead":true}`},
	} {
		srv, _, _ := newEmptySkillServer(t, tc.directoryRead)

		var exts map[string]json.RawMessage
		raw, present := discoverCapabilities(t, srv)["extensions"]
		if !present {
			t.Fatalf("directoryRead=%v: server/discover declares no extensions", tc.directoryRead)
		}
		if err := json.Unmarshal(raw, &exts); err != nil {
			t.Fatalf("unmarshal extensions: %v", err)
		}
		got, present := exts[SkillsExtensionID]
		if !present {
			t.Fatalf("directoryRead=%v: extensions has no %s key: %s", tc.directoryRead, SkillsExtensionID, raw)
		}
		if string(got) != tc.want {
			t.Errorf("directoryRead=%v: settings = %s, want %s", tc.directoryRead, got, tc.want)
		}

		// The capability and the router must agree.
		env := call(t, srv, 1, "resources/directory/read", map[string]interface{}{
			"_meta": validMeta(), "uri": "skill://x",
		})
		if tc.directoryRead {
			if env.Error != nil && env.Error.Code == transport.MethodNotFound {
				t.Errorf("directoryRead=true but resources/directory/read answers -32601")
			}
		} else if env.Error == nil || env.Error.Code != transport.MethodNotFound {
			t.Errorf("directoryRead=false: want -32601, got %+v", env.Error)
		}
	}
}

// ---------------------------------------------------------------------------
// resources/directory/read
// ---------------------------------------------------------------------------

func directoryChildren(t *testing.T, srv *Server, uri string) []Resource {
	t.Helper()
	env := call(t, srv, 1, "resources/directory/read", map[string]interface{}{"_meta": validMeta(), "uri": uri})
	var body struct {
		Resources []Resource `json:"resources"`
	}
	decodeResult(t, env, &body)
	return body.Resources
}

func TestDirectoryReadListsDirectChildrenOnly(t *testing.T) {
	srv, _, _ := newSkillServer(t)

	children := directoryChildren(t, srv, "skill://pdf-processing")
	got := map[string]string{}
	for _, c := range children {
		got[c.Name] = c.MimeType
	}
	want := map[string]string{
		"SKILL.md":  "text/markdown",
		"assets":    mimeTypeDirectory,
		"scripts":   mimeTypeDirectory,
		"templates": mimeTypeDirectory,
	}
	if len(got) != len(want) {
		t.Fatalf("children = %#v, want %#v", got, want)
	}
	for name, mime := range want {
		if got[name] != mime {
			t.Errorf("child %q mimeType = %q, want %q", name, got[name], mime)
		}
	}

	// Non-recursive: a nested directory is one entry, not its contents.
	nested := directoryChildren(t, srv, "skill://pdf-processing/templates")
	if len(nested) != 2 {
		t.Fatalf("templates children = %#v, want invoice.md and regional", nested)
	}
	for _, c := range nested {
		switch c.Name {
		case "invoice.md":
			if c.URI != "skill://pdf-processing/templates/invoice.md" {
				t.Errorf("invoice.md uri = %q", c.URI)
			}
		case "regional":
			if c.MimeType != mimeTypeDirectory {
				t.Errorf("regional mimeType = %q, want %q", c.MimeType, mimeTypeDirectory)
			}
		default:
			t.Errorf("unexpected child %q", c.Name)
		}
	}
}

func TestDirectoryReadTrailingSlashIsIgnored(t *testing.T) {
	srv, _, _ := newSkillServer(t)
	if a, b := directoryChildren(t, srv, "skill://pdf-processing/templates"), directoryChildren(t, srv, "skill://pdf-processing/templates/"); len(a) != len(b) {
		t.Errorf("trailing slash changed the answer: %d vs %d children", len(a), len(b))
	}
}

func TestDirectoryReadNonDirectoryIsInvalidParams(t *testing.T) {
	srv, _, _ := newSkillServer(t)
	for _, uri := range []string{
		"skill://pdf-processing/SKILL.md", // a file, not a directory
		"skill://pdf-processing/nowhere",  // nothing under it
		"skill://does-not-exist",          // no such namespace
	} {
		env := call(t, srv, 1, "resources/directory/read", map[string]interface{}{"_meta": validMeta(), "uri": uri})
		if env.Error == nil || env.Error.Code != transport.InvalidParams {
			t.Errorf("directory/read %s: want -32602, got %+v", uri, env.Error)
		}
	}
}

func TestDirectoryReadIsSchemeAgnostic(t *testing.T) {
	srv, _, resources := newSkillServer(t)
	resources.Register(Resource{URI: "kube:///ns/default/pod/api", Name: "api", MimeType: "application/json"},
		func(context.Context) (ResourceContentResult, error) {
			return ResourceContentResult{Text: "{}"}, nil
		})

	children := directoryChildren(t, srv, "kube:///ns/default/pod")
	if len(children) != 1 || children[0].Name != "api" {
		t.Errorf("children = %#v, want a single api entry", children)
	}
}

func TestDirectoryReadPaginates(t *testing.T) {
	srv, _, resources := newSkillServer(t)
	for i := 0; i < defaultPageSize+5; i++ {
		uri := fmt.Sprintf("bulk:///dir/file-%03d", i)
		resources.Register(Resource{URI: uri, Name: uri, MimeType: "text/plain"},
			func(context.Context) (ResourceContentResult, error) {
				return ResourceContentResult{Text: "x"}, nil
			})
	}

	env := call(t, srv, 1, "resources/directory/read", map[string]interface{}{"_meta": validMeta(), "uri": "bulk:///dir"})
	var page struct {
		Resources  []Resource `json:"resources"`
		NextCursor string     `json:"nextCursor"`
	}
	decodeResult(t, env, &page)
	if len(page.Resources) != defaultPageSize || page.NextCursor == "" {
		t.Fatalf("first page: %d entries, nextCursor %q", len(page.Resources), page.NextCursor)
	}

	env = call(t, srv, 2, "resources/directory/read", map[string]interface{}{
		"_meta": validMeta(), "uri": "bulk:///dir", "cursor": page.NextCursor,
	})
	var rest struct {
		Resources  []Resource `json:"resources"`
		NextCursor string     `json:"nextCursor"`
	}
	decodeResult(t, env, &rest)
	if len(rest.Resources) != 5 || rest.NextCursor != "" {
		t.Fatalf("second page: %d entries, nextCursor %q", len(rest.Resources), rest.NextCursor)
	}
}

// ---------------------------------------------------------------------------
// Registration, files, notifications
// ---------------------------------------------------------------------------

func TestSkillFilesAreReadableResources(t *testing.T) {
	srv, _, _ := newSkillServer(t)

	body, mimeType := readResourceBytes(t, srv, "skill://acme/billing/refunds/examples/email.md")
	if string(body) != "# Email template\n" {
		t.Errorf("content = %q", body)
	}
	if mimeType != "text/markdown" {
		t.Errorf("mimeType = %q, want text/markdown", mimeType)
	}

	// The SKILL.md resource takes its catalog name and description from the frontmatter.
	env := call(t, srv, 1, "resources/list", map[string]interface{}{"_meta": validMeta()})
	var body2 struct {
		Resources []Resource `json:"resources"`
	}
	decodeResult(t, env, &body2)
	var found bool
	for _, res := range body2.Resources {
		if res.URI != "skill://pdf-processing/SKILL.md" {
			continue
		}
		found = true
		if res.Name != "pdf-processing" {
			t.Errorf("SKILL.md resource name = %q, want the frontmatter name", res.Name)
		}
		if res.Description != "Extract, fill, and assemble PDF documents" {
			t.Errorf("SKILL.md resource description = %q", res.Description)
		}
		if res.Size == nil || *res.Size != int64(len(pdfSKILL)) {
			t.Errorf("SKILL.md resource size = %v, want %d", res.Size, len(pdfSKILL))
		}
	}
	if !found {
		t.Fatal("skill files are not in resources/list")
	}
}

func TestUnregisterSkillRemovesItsFiles(t *testing.T) {
	srv, skills, _ := newSkillServer(t)

	if !skills.Unregister("skill://pdf-processing/SKILL.md") {
		t.Fatal("Unregister reported no such skill")
	}
	if skills.Unregister("skill://pdf-processing/SKILL.md") {
		t.Error("Unregister of an absent skill should report false")
	}

	for _, s := range listSkills(t, srv) {
		if s.URI == "skill://pdf-processing/SKILL.md" {
			t.Fatal("unregistered skill still in skills/list")
		}
	}
	for _, uri := range []string{
		"skill://pdf-processing/SKILL.md",
		"skill://pdf-processing/templates/invoice.md",
	} {
		env := call(t, srv, 1, "resources/read", map[string]interface{}{"_meta": validMeta(), "uri": uri})
		if env.Error == nil || env.Error.Code != transport.InvalidParams {
			t.Errorf("resources/read %s after unregister: want -32602, got %+v", uri, env.Error)
		}
	}
	// The other skill is untouched.
	if _, ok := skills.Get("skill://acme/billing/refunds/SKILL.md"); !ok {
		t.Error("unregistering one skill removed another")
	}
}

// TestSkillRegistrationNotifiesExactlyOnce guards the reason mutateBatch exists: a skill is
// many resources, and a client must be told the catalog changed once, not once per file.
func TestSkillRegistrationNotifiesExactlyOnce(t *testing.T) {
	srv, skills, _ := newEmptySkillServer(t, false)
	filter := map[string]interface{}{"resourcesListChanged": true}

	// want=2 so the wait runs its full window: if a regression fires one notification per
	// file, more than one lands here.
	notifications := observeDuringMutationN(t, srv, filter, 2, func() {
		if err := skills.LoadFS(skillMapFS(), "skill://"); err != nil {
			t.Errorf("LoadFS: %v", err)
		}
	})
	// Two skills in the fixture: exactly two list_changed, not seven.
	if len(notifications) != 2 {
		t.Fatalf("got %d notifications for two skills, want 2: %+v", len(notifications), notifications)
	}
	for _, n := range notifications {
		if n.Method != "notifications/resources/list_changed" {
			t.Errorf("unexpected notification %s", n.Method)
		}
	}

	notifications = observeDuringMutationN(t, srv, filter, 2, func() {
		skills.Unregister("skill://pdf-processing/SKILL.md")
	})
	if len(notifications) != 1 || !hasNotification(notifications, "notifications/resources/list_changed") {
		t.Fatalf("unregister fired %d notifications, want exactly 1 list_changed: %+v", len(notifications), notifications)
	}
}

func TestSkillsWorkUnderAnyScheme(t *testing.T) {
	resources := NewResourceRegistry()
	skills := NewSkillRegistry(resources)
	srv := NewServer(NewToolRegistry(), resources, &ServerConfig{Skills: skills, SkillsDirectoryRead: true})

	uri := "github://owner/repo/skills/refunds/SKILL.md"
	body := []byte("---\nname: refunds\ndescription: refund policy\n---\n\nbody\n")
	fm, err := parseFrontmatter(body)
	if err != nil {
		t.Fatalf("parseFrontmatter: %v", err)
	}
	if err := skills.Register(SkillDef{
		URI:         uri,
		Frontmatter: fm,
		Files:       map[string]SkillContent{uri: {Bytes: body}},
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	listed := listSkills(t, srv)
	if len(listed) != 1 || listed[0].URI != uri {
		t.Fatalf("skills = %#v", listed)
	}
	got, _ := readResourceBytes(t, srv, uri)
	if digestOf(got) != listed[0].Resources[0].Digest {
		t.Error("digest mismatch under a non-skill:// scheme")
	}
}

func TestRegisterRejectsMalformedDefinitions(t *testing.T) {
	good := []byte("---\nname: refunds\ndescription: refund policy\n---\n")
	fm, err := parseFrontmatter(good)
	if err != nil {
		t.Fatalf("parseFrontmatter: %v", err)
	}

	cases := []struct {
		name string
		def  SkillDef
		want string
	}{
		{
			name: "uri is not a SKILL.md",
			def:  SkillDef{URI: "skill://refunds/readme.md", Frontmatter: fm},
			want: "must end in",
		},
		{
			name: "files omit the SKILL.md itself",
			def: SkillDef{URI: "skill://refunds/SKILL.md", Frontmatter: fm,
				Files: map[string]SkillContent{"skill://refunds/other.md": {Bytes: []byte("x")}}},
			want: "must contain the SKILL.md uri",
		},
		{
			name: "file outside the skill root",
			def: SkillDef{URI: "skill://refunds/SKILL.md", Frontmatter: fm, Files: map[string]SkillContent{
				"skill://refunds/SKILL.md": {Bytes: good},
				"skill://elsewhere/x.md":   {Bytes: []byte("x")},
			}},
			want: "is not under the skill root",
		},
		{
			name: "final path segment does not equal the name",
			def: SkillDef{URI: "skill://refund/SKILL.md", Frontmatter: fm,
				Files: map[string]SkillContent{"skill://refund/SKILL.md": {Bytes: good}}},
			want: "does not equal frontmatter name",
		},
		{
			name: "no frontmatter",
			def: SkillDef{URI: "skill://refunds/SKILL.md",
				Files: map[string]SkillContent{"skill://refunds/SKILL.md": {Bytes: good}}},
			want: "frontmatter is required",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resources := NewResourceRegistry()
			err := NewSkillRegistry(resources).Register(tc.def)
			if err == nil {
				t.Fatalf("Register accepted %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
			if resources.HasResources() {
				t.Error("a rejected registration still registered resources")
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Nested skills
// ---------------------------------------------------------------------------

func nestedSkillFS() fstest.MapFS {
	return fstest.MapFS{
		"bundle/SKILL.md":       &fstest.MapFile{Data: []byte("---\nname: bundle\ndescription: outer\n---\n")},
		"bundle/notes.md":       &fstest.MapFile{Data: []byte("notes\n")},
		"bundle/inner/SKILL.md": &fstest.MapFile{Data: []byte("---\nname: inner\ndescription: nested\n---\n")},
		"bundle/inner/ref.md":   &fstest.MapFile{Data: []byte("ref\n")},
	}
}

func TestNestedSkillsPublishFlatAndShareFiles(t *testing.T) {
	resources := NewResourceRegistry()
	skills := NewSkillRegistry(resources)
	if err := skills.LoadFS(nestedSkillFS(), "skill://"); err != nil {
		t.Fatalf("LoadFS: %v", err)
	}
	srv := NewServer(NewToolRegistry(), resources, &ServerConfig{Skills: skills})

	listed := listSkills(t, srv)
	if len(listed) != 2 {
		t.Fatalf("got %d skills, want the outer and the nested one published flat: %#v", len(listed), listed)
	}

	// The enclosing skill's resources include the nested skill's files.
	outer := listed[0]
	if outer.URI != "skill://bundle/SKILL.md" {
		t.Fatalf("skills[0].uri = %q", outer.URI)
	}
	if len(outer.Resources) != 4 {
		t.Errorf("outer skill lists %d files, want all 4 including the nested skill's", len(outer.Resources))
	}
	inner := listed[1]
	if inner.URI != "skill://bundle/inner/SKILL.md" || len(inner.Resources) != 2 {
		t.Errorf("nested skill = %#v", inner)
	}

	// Removing the nested skill must not orphan files the enclosing skill still publishes.
	if !skills.Unregister("skill://bundle/inner/SKILL.md") {
		t.Fatal("Unregister reported no such skill")
	}
	if len(listSkills(t, srv)) != 1 {
		t.Error("nested skill did not leave skills/list")
	}
	for _, uri := range []string{"skill://bundle/inner/SKILL.md", "skill://bundle/inner/ref.md"} {
		if body, _ := readResourceBytes(t, srv, uri); len(body) == 0 {
			t.Errorf("%s stopped being readable, but the enclosing skill still publishes it", uri)
		}
	}
}

// ---------------------------------------------------------------------------
// LoadFS
// ---------------------------------------------------------------------------

func TestLoadFSRejects(t *testing.T) {
	cases := []struct {
		name string
		fsys fstest.MapFS
		want string
	}{
		{
			name: "name does not match its directory",
			fsys: fstest.MapFS{"refunds/SKILL.md": &fstest.MapFile{Data: []byte("---\nname: refund\ndescription: d\n---\n")}},
			want: "does not equal frontmatter name",
		},
		{
			name: "name with a double hyphen",
			fsys: fstest.MapFS{"a--b/SKILL.md": &fstest.MapFile{Data: []byte("---\nname: a--b\ndescription: d\n---\n")}},
			want: "must match",
		},
		{
			name: "uppercase name",
			fsys: fstest.MapFS{"Refunds/SKILL.md": &fstest.MapFile{Data: []byte("---\nname: Refunds\ndescription: d\n---\n")}},
			want: "must match",
		},
		{
			name: "name longer than 64 characters",
			fsys: fstest.MapFS{"x/SKILL.md": &fstest.MapFile{Data: []byte("---\nname: " + strings.Repeat("a", 65) + "\ndescription: d\n---\n")}},
			want: "must be 1-64 characters",
		},
		{
			name: "description longer than 1024 characters",
			fsys: fstest.MapFS{"x/SKILL.md": &fstest.MapFile{Data: []byte("---\nname: x\ndescription: " + strings.Repeat("d", 1025) + "\n---\n")}},
			want: "at most 1024 characters",
		},
		{
			name: "missing description",
			fsys: fstest.MapFS{"x/SKILL.md": &fstest.MapFile{Data: []byte("---\nname: x\n---\n")}},
			want: "description is required",
		},
		{
			name: "missing name",
			fsys: fstest.MapFS{"x/SKILL.md": &fstest.MapFile{Data: []byte("---\ndescription: d\n---\n")}},
			want: "name is required",
		},
		{
			name: "compatibility longer than 500 characters",
			fsys: fstest.MapFS{"x/SKILL.md": &fstest.MapFile{Data: []byte(
				"---\nname: x\ndescription: d\ncompatibility: " + strings.Repeat("c", 501) + "\n---\n")}},
			want: "at most 500 characters",
		},
		{
			name: "no SKILL.md anywhere",
			fsys: fstest.MapFS{"notes/readme.md": &fstest.MapFile{Data: []byte("hi")}},
			want: "no SKILL.md found",
		},
		{
			name: "SKILL.md at the filesystem root",
			fsys: fstest.MapFS{"SKILL.md": &fstest.MapFile{Data: []byte("---\nname: x\ndescription: d\n---\n")}},
			want: "must live in a directory",
		},
		{
			name: "file belonging to no skill",
			fsys: fstest.MapFS{
				"x/SKILL.md": &fstest.MapFile{Data: []byte("---\nname: x\ndescription: d\n---\n")},
				"stray.md":   &fstest.MapFile{Data: []byte("orphan")},
			},
			want: "belongs to no skill",
		},
		{
			name: "no frontmatter block",
			fsys: fstest.MapFS{"x/SKILL.md": &fstest.MapFile{Data: []byte("# just markdown\n")}},
			want: "does not begin with a --- line",
		},
		{
			name: "unterminated frontmatter",
			fsys: fstest.MapFS{"x/SKILL.md": &fstest.MapFile{Data: []byte("---\nname: x\ndescription: d\n")}},
			want: "unterminated",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := NewSkillRegistry(NewResourceRegistry()).LoadFS(tc.fsys, "skill://")
			if err == nil {
				t.Fatalf("LoadFS accepted %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

func TestLoadFSRejectsBadURIPrefix(t *testing.T) {
	err := NewSkillRegistry(NewResourceRegistry()).LoadFS(skillMapFS(), "skill:")
	if err == nil || !strings.Contains(err.Error(), "must end in") {
		t.Fatalf("want a uri prefix error, got %v", err)
	}
}

func TestLoadFSSkipsDotEntries(t *testing.T) {
	fsys := fstest.MapFS{
		"x/SKILL.md":  &fstest.MapFile{Data: []byte("---\nname: x\ndescription: d\n---\n")},
		"x/.DS_Store": &fstest.MapFile{Data: []byte("junk")},
		"x/.cache/c":  &fstest.MapFile{Data: []byte("junk")},
	}
	skills := NewSkillRegistry(NewResourceRegistry())
	if err := skills.LoadFS(fsys, "skill://"); err != nil {
		t.Fatalf("LoadFS: %v", err)
	}
	s, _ := skills.Get("skill://x/SKILL.md")
	if len(s.Resources) != 1 {
		t.Errorf("skill publishes %#v, want only SKILL.md", s.Resources)
	}
}

// ---------------------------------------------------------------------------
// _meta and annotations on a read payload (independent of skills)
// ---------------------------------------------------------------------------

func TestResourceReadCarriesMetaAndAnnotations(t *testing.T) {
	srv, _, resources := newTestServer(t)
	priority := 0.5
	resources.Register(Resource{URI: "test:///annotated", Name: "annotated", MimeType: "text/plain"},
		func(context.Context) (ResourceContentResult, error) {
			return ResourceContentResult{
				Text:        "body",
				Meta:        map[string]interface{}{SkillsMetaPrefix + "extra": "value"},
				Annotations: &Annotations{Audience: []string{"user"}, Priority: &priority},
			}, nil
		})

	env := call(t, srv, 1, "resources/read", map[string]interface{}{"_meta": validMeta(), "uri": "test:///annotated"})
	var body struct {
		Contents []struct {
			Meta        map[string]interface{} `json:"_meta"`
			Annotations *Annotations           `json:"annotations"`
		} `json:"contents"`
	}
	decodeResult(t, env, &body)
	if len(body.Contents) != 1 {
		t.Fatalf("got %d contents", len(body.Contents))
	}
	if body.Contents[0].Meta[SkillsMetaPrefix+"extra"] != "value" {
		t.Errorf("contents[0]._meta = %#v", body.Contents[0].Meta)
	}
	if body.Contents[0].Annotations == nil || len(body.Contents[0].Annotations.Audience) != 1 {
		t.Errorf("contents[0].annotations = %#v", body.Contents[0].Annotations)
	}
}
