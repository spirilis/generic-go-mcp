package mcp

// Tests for the skill entry shape the released (Beta) Skills specification requires: size
// beside every digest, "resources" always present as an array or "dynamic", the per-skill
// limits, and a frontmatter that is the SKILL.md's own.

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/spirilis/generic-go-mcp/transport"
)

func TestSkillManifestCarriesSizeOfTheServedBytes(t *testing.T) {
	srv, _, _ := newSkillServer(t)
	for _, skill := range listSkills(t, srv) {
		if skill.Dynamic || len(skill.Resources) == 0 {
			t.Fatalf("%s: want a manifest, got dynamic=%v with %d resources", skill.URI, skill.Dynamic, len(skill.Resources))
		}
		for _, sr := range skill.Resources {
			b, _ := readResourceBytes(t, srv, sr.URI)
			if sr.Size != int64(len(b)) {
				t.Errorf("%s: size %d, resources/read returned %d bytes", sr.URI, sr.Size, len(b))
			}
			if sr.Digest != digestOf(b) {
				t.Errorf("%s: digest %s does not match the bytes read", sr.URI, sr.Digest)
			}
		}
	}
}

// "size" must be on the wire even for an empty file: 0 is a size, absence is not.
func TestSkillResourceSizeIsNeverOmitted(t *testing.T) {
	resources := NewResourceRegistry()
	skills := NewSkillRegistry(resources)
	err := skills.Register(SkillDef{URI: "skill://solo/SKILL.md", Files: map[string]SkillContent{
		"skill://solo/SKILL.md": {Bytes: skillMD("solo", "d", "")},
		"skill://solo/empty.md": {Bytes: nil},
	}})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	raw, err := json.Marshal(skills.List())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"uri":"skill://solo/empty.md","digest":"`+digestOf(nil)+`","size":0`) {
		t.Errorf("empty file should carry size 0: %s", raw)
	}
}

func TestSkillJSONRoundTrip(t *testing.T) {
	manifest := Skill{
		URI:         "skill://a/SKILL.md",
		Frontmatter: map[string]interface{}{"name": "a", "description": "d"},
		Resources:   []SkillResource{{URI: "skill://a/SKILL.md", Digest: digestOf([]byte("x")), Size: 1}},
		Meta:        map[string]interface{}{"com.example/reviewed": true},
	}
	dynamic := Skill{
		URI:         "skill://b/SKILL.md",
		Frontmatter: map[string]interface{}{"name": "b", "description": "d"},
		Dynamic:     true,
	}
	for _, want := range []Skill{manifest, dynamic} {
		raw, err := json.Marshal(want)
		if err != nil {
			t.Fatal(err)
		}
		var got Skill
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("unmarshal %s: %v", raw, err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("round trip of %s gave %+v", raw, got)
		}
	}

	raw, _ := json.Marshal(dynamic)
	if !strings.Contains(string(raw), `"resources":"dynamic"`) || strings.Contains(string(raw), "_meta") {
		t.Errorf("dynamic entry = %s", raw)
	}

	// An entry with no "resources", null, or any other string is invalid.
	for _, bad := range []string{
		`{"uri":"skill://a/SKILL.md","frontmatter":{}}`,
		`{"uri":"skill://a/SKILL.md","frontmatter":{},"resources":null}`,
		`{"uri":"skill://a/SKILL.md","frontmatter":{},"resources":"static"}`,
	} {
		var s Skill
		if err := json.Unmarshal([]byte(bad), &s); err == nil {
			t.Errorf("unmarshal accepted %s", bad)
		}
	}
}

func TestRegisterDynamicSkillListsEntryWithoutFiles(t *testing.T) {
	resources := NewResourceRegistry()
	skills := NewSkillRegistry(resources)
	err := skills.Register(SkillDef{
		URI:         "skill://reports/daily/SKILL.md",
		Frontmatter: map[string]interface{}{"name": "daily", "description": "Assemble today's report"},
		Dynamic:     true,
		Meta:        map[string]interface{}{"com.example/owner": "ops"},
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if resources.HasResources() {
		t.Error("a dynamic skill must not register resources; the caller serves them")
	}
	srv := newServerFor(skills)
	env := call(t, srv, 1, "skills/list", map[string]interface{}{"_meta": validMeta()})
	if env.Error != nil {
		t.Fatalf("skills/list: %+v", env.Error)
	}
	for _, want := range []string{`"resources":"dynamic"`, `"_meta":{"com.example/owner":"ops"}`} {
		if !strings.Contains(string(env.Result), want) {
			t.Errorf("skills/list result %s lacks %s", env.Result, want)
		}
	}
	if !skills.Unregister("skill://reports/daily/SKILL.md") || skills.Has() {
		t.Error("Unregister of a dynamic skill")
	}
}

func TestResolverEntryIsValidatedBeforeItIsSent(t *testing.T) {
	const uri = "skill://generated/SKILL.md"
	fmOK := map[string]interface{}{"name": "generated", "description": "d"}
	good := SkillResource{URI: uri, Digest: digestOf([]byte("x")), Size: 1}

	cases := map[string]Skill{
		"neither manifest nor dynamic": {URI: uri, Frontmatter: fmOK},
		"both manifest and dynamic":    {URI: uri, Frontmatter: fmOK, Dynamic: true, Resources: []SkillResource{good}},
		"a different uri":              {URI: "skill://other/SKILL.md", Frontmatter: map[string]interface{}{"name": "other", "description": "d"}, Dynamic: true},
		"name is not the last segment": {URI: uri, Frontmatter: map[string]interface{}{"name": "else", "description": "d"}, Dynamic: true},
		"manifest without SKILL.md":    {URI: uri, Frontmatter: fmOK, Resources: []SkillResource{{URI: "skill://generated/a.md", Digest: good.Digest, Size: 1}}},
		"file listed twice":            {URI: uri, Frontmatter: fmOK, Resources: []SkillResource{good, good}},
		"file outside the skill":       {URI: uri, Frontmatter: fmOK, Resources: []SkillResource{good, {URI: "skill://elsewhere/a.md", Digest: good.Digest, Size: 1}}},
		"uppercase digest":             {URI: uri, Frontmatter: fmOK, Resources: []SkillResource{{URI: uri, Digest: strings.ToUpper(good.Digest), Size: 1}}},
		"negative size":                {URI: uri, Frontmatter: fmOK, Resources: []SkillResource{{URI: uri, Digest: good.Digest, Size: -1}}},
	}
	for name, entry := range cases {
		t.Run(name, func(t *testing.T) {
			srv, skills, _ := newEmptySkillServer(t, false)
			skills.SetResolver(func(context.Context, string) (Skill, bool, error) { return entry, true, nil })
			env := call(t, srv, 1, "skills/get", map[string]interface{}{"_meta": validMeta(), "uri": uri})
			if env.Error == nil || env.Error.Code != transport.InternalError {
				t.Fatalf("want -32603 for a resolver entry with %s, got error=%+v result=%s", name, env.Error, env.Result)
			}
		})
	}

	// And a correct manifest passes.
	srv, skills, _ := newEmptySkillServer(t, false)
	skills.SetResolver(func(context.Context, string) (Skill, bool, error) {
		return Skill{URI: uri, Frontmatter: fmOK, Resources: []SkillResource{good}}, true, nil
	})
	env := call(t, srv, 1, "skills/get", map[string]interface{}{"_meta": validMeta(), "uri": uri})
	if env.Error != nil || !strings.Contains(string(env.Result), `"size":1`) {
		t.Fatalf("valid resolver entry: error=%+v result=%s", env.Error, env.Result)
	}
}

func TestRegisterEnforcesPerSkillLimits(t *testing.T) {
	def := func(extraFiles, bigBytes int) SkillDef {
		d := SkillDef{URI: "skill://big/SKILL.md", Files: map[string]SkillContent{
			"skill://big/SKILL.md": {Bytes: skillMD("big", "d", "")},
		}}
		for i := 0; i < extraFiles; i++ {
			d.Files[fmt.Sprintf("skill://big/f/%04d.txt", i)] = SkillContent{Bytes: []byte("x")}
		}
		if bigBytes > 0 {
			d.Files["skill://big/blob.bin"] = SkillContent{Bytes: make([]byte, bigBytes)}
		}
		return d
	}

	skills := NewSkillRegistry(NewResourceRegistry())
	if err := skills.Register(def(MaxSkillResources-1, 0)); err != nil {
		t.Fatalf("a skill of exactly %d files must register: %v", MaxSkillResources, err)
	}
	if err := skills.Register(def(MaxSkillResources, 0)); err == nil || !strings.Contains(err.Error(), "files exceeds") {
		t.Errorf("%d files: err = %v", MaxSkillResources+1, err)
	}
	if err := skills.Register(def(0, MaxSkillBytes)); err == nil || !strings.Contains(err.Error(), "bytes exceeds") {
		t.Errorf("over %d bytes: err = %v", MaxSkillBytes, err)
	}
	// A refused registration leaves the earlier, valid one in place.
	if got, _ := skills.Get("skill://big/SKILL.md"); len(got.Resources) != MaxSkillResources {
		t.Errorf("after a refused replacement the skill has %d files, want %d", len(got.Resources), MaxSkillResources)
	}

	skills.AllowOversizedSkills(true)
	if err := skills.Register(def(MaxSkillResources, 0)); err != nil {
		t.Errorf("AllowOversizedSkills should admit an oversized skill: %v", err)
	}
}

// A server that declares the extension must declare resources too, even before any skill or
// resource is registered: skill files are served through resources/read.
func TestSkillsExtensionAlwaysDeclaresResources(t *testing.T) {
	srv, _, _ := newEmptySkillServer(t, false)
	caps := discoverCapabilities(t, srv)
	if _, ok := caps["resources"]; !ok {
		t.Errorf("capabilities %v: the skills extension is declared without resources", caps)
	}
	if _, ok := caps["extensions"]; !ok {
		t.Errorf("capabilities %v: extension missing", caps)
	}
}

// The entry's frontmatter is what a YAML 1.2 host parser reads from the same file. Each line
// here is a case where decoding with yaml.v3's own rules would produce something else. The
// expected values were produced by the npm "yaml" package, which the MCP conformance suite
// uses, parsing this exact block.
const fidelityFrontmatter = `name: fidelity
description: "Checks YAML to JSON fidelity"
metadata:
  released: 2026-11-25
  stamp: 2026-11-25T10:00:00Z
  legacy-octal: 014
  octal: 0o14
  hex: 0x1F
  underscore: 1_000
  version: 1.10
  exponent: 1e3
  yes-word: yes
  off-word: off
  quoted-number: "42"
  quoted-bool: 'true'
  nothing: ~
  empty:
  flag: True
  big: 123456789012345678901234567890
  negative: -7
  1: numeric key
base: &base
  a: 1
merged:
  <<: *base
  b: 2
list: [1, "2", three, 4.5, null]
block: |
  line one
  line two
`

const fidelityWant = `{"name":"fidelity","description":"Checks YAML to JSON fidelity","metadata":{"released":"2026-11-25","stamp":"2026-11-25T10:00:00Z","legacy-octal":14,"octal":12,"hex":31,"underscore":"1_000","version":1.1,"exponent":1000,"yes-word":"yes","off-word":"off","quoted-number":"42","quoted-bool":"true","nothing":null,"empty":null,"flag":true,"big":1.2345678901234568e+29,"negative":-7,"1":"numeric key"},"base":{"a":1},"merged":{"<<":{"a":1},"b":2},"list":[1,"2","three",4.5,null],"block":"line one\nline two\n"}`

func TestFrontmatterIsWhatAYAML12HostParserReads(t *testing.T) {
	got, err := parseFrontmatter([]byte("---\n" + fidelityFrontmatter + "---\nbody\n"))
	if err != nil {
		t.Fatalf("parseFrontmatter: %v", err)
	}
	var want map[string]interface{}
	if err := json.Unmarshal([]byte(fidelityWant), &want); err != nil {
		t.Fatal(err)
	}
	if !sameJSON(got, want) {
		raw, _ := json.Marshal(got)
		t.Errorf("frontmatter as JSON:\n got  %s\n want %s", raw, fidelityWant)
	}
}

func TestParseFrontmatterRejects(t *testing.T) {
	cases := map[string]string{
		"no opening fence":            "name: a\ndescription: d\n",
		"opening fence with a suffix": "--- yaml\nname: a\n---\n",
		"opening fence with a space":  "--- \nname: a\n---\n",
		"no closing fence":            "---\nname: a\ndescription: d\n",
		"document end marker only":    "---\nname: a\ndescription: d\n...\n",
		"empty block":                 "---\n---\n",
		"not a mapping":               "---\n- a\n- b\n---\n",
		"duplicate key":               "---\nname: a\nname: b\n---\n",
		"infinity":                    "---\nname: a\nlimit: .inf\n---\n",
		"self-referencing alias":      "---\nname: &a [*a]\n---\n",
		"alias expansion":             "---\na: &a [x,x,x,x,x,x,x,x,x,x]\nb: &b [*a,*a,*a,*a,*a,*a,*a,*a,*a,*a]\nc: &c [*b,*b,*b,*b,*b,*b,*b,*b,*b,*b]\nd: &d [*c,*c,*c,*c,*c,*c,*c,*c,*c,*c]\ne: [*d,*d,*d,*d,*d,*d,*d,*d,*d,*d]\n---\n",
	}
	for name, doc := range cases {
		if fm, err := parseFrontmatter([]byte(doc)); err == nil {
			t.Errorf("%s: accepted, got %v", name, fm)
		}
	}

	// CRLF line endings and a byte-order mark are both fine.
	fm, err := parseFrontmatter([]byte("\ufeff---\r\nname: a\r\ndescription: d\r\n---\r\nbody\r\n"))
	if err != nil || fm["name"] != "a" || fm["description"] != "d" {
		t.Errorf("CRLF + BOM: fm=%v err=%v", fm, err)
	}
}

// A caller may still pass Frontmatter; it is accepted when it says what the file says,
// whatever Go types it uses to say it.
func TestRegisterAcceptsSuppliedFrontmatterEqualToTheFile(t *testing.T) {
	md := []byte("---\nname: solo\ndescription: d\nmetadata:\n  retries: 3\n---\n")
	skills := NewSkillRegistry(NewResourceRegistry())
	err := skills.Register(SkillDef{
		URI: "skill://solo/SKILL.md",
		Frontmatter: map[string]interface{}{
			"name": "solo", "description": "d",
			"metadata": map[string]interface{}{"retries": float64(3)},
		},
		Files: map[string]SkillContent{"skill://solo/SKILL.md": {Bytes: md}},
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
}
