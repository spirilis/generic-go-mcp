package mcp

import (
	"strings"
	"testing"
)

func fm(name, desc string) map[string]interface{} {
	return map[string]interface{}{"name": name, "description": desc}
}

// A nested skill that re-publishes an enclosing skill's file with DIFFERENT bytes would make
// skills/list advertise a digest resources/read can't reproduce. Register must reject it.
func TestRegisterRejectsConflictingSharedDigest(t *testing.T) {
	skills := NewSkillRegistry(NewResourceRegistry())

	parent := SkillDef{
		URI:         "skill://bundle/SKILL.md",
		Frontmatter: fm("bundle", "parent"),
		Files: map[string]SkillContent{
			"skill://bundle/SKILL.md":       {Bytes: []byte("parent skill")},
			"skill://bundle/inner/SKILL.md": {Bytes: []byte("VERSION A")},
		},
	}
	if err := skills.Register(parent); err != nil {
		t.Fatalf("parent Register: %v", err)
	}

	nested := SkillDef{
		URI:         "skill://bundle/inner/SKILL.md",
		Frontmatter: fm("inner", "nested"),
		Files: map[string]SkillContent{
			"skill://bundle/inner/SKILL.md": {Bytes: []byte("VERSION B")}, // divergent
		},
	}
	err := skills.Register(nested)
	if err == nil {
		t.Fatal("expected a conflict error registering a shared file with different bytes")
	}
	if !strings.Contains(err.Error(), "different digest") {
		t.Errorf("error = %q, want it to mention a digest conflict", err)
	}
}

// Identical bytes for the shared file are the normal nested case and must register cleanly.
func TestRegisterAcceptsMatchingSharedDigest(t *testing.T) {
	skills := NewSkillRegistry(NewResourceRegistry())
	shared := []byte("same bytes both places")

	parent := SkillDef{
		URI:         "skill://bundle/SKILL.md",
		Frontmatter: fm("bundle", "parent"),
		Files: map[string]SkillContent{
			"skill://bundle/SKILL.md":       {Bytes: []byte("parent skill")},
			"skill://bundle/inner/SKILL.md": {Bytes: shared},
		},
	}
	nested := SkillDef{
		URI:         "skill://bundle/inner/SKILL.md",
		Frontmatter: fm("inner", "nested"),
		Files:       map[string]SkillContent{"skill://bundle/inner/SKILL.md": {Bytes: shared}},
	}
	if err := skills.Register(parent); err != nil {
		t.Fatalf("parent Register: %v", err)
	}
	if err := skills.Register(nested); err != nil {
		t.Fatalf("nested Register with identical bytes should succeed: %v", err)
	}
	if got := len(skills.List()); got != 2 {
		t.Errorf("List len = %d, want 2", got)
	}
}

// Re-registering the SAME skill with changed bytes for a file only it owns is not a conflict:
// the guard excludes a skill's own prior incarnation.
func TestRegisterReplacementChangingOwnFileIsNotAConflict(t *testing.T) {
	skills := NewSkillRegistry(NewResourceRegistry())

	def := func(body string) SkillDef {
		return SkillDef{
			URI:         "skill://solo/SKILL.md",
			Frontmatter: fm("solo", "d"),
			Files: map[string]SkillContent{
				"skill://solo/SKILL.md": {Bytes: []byte("entry")},
				"skill://solo/ref.md":   {Bytes: []byte(body)},
			},
		}
	}
	if err := skills.Register(def("first")); err != nil {
		t.Fatalf("first Register: %v", err)
	}
	if err := skills.Register(def("second, changed")); err != nil {
		t.Fatalf("re-registering the same skill with a changed own file must succeed: %v", err)
	}
	b, _ := readResourceBytes(t, newServerFor(skills), "skill://solo/ref.md")
	if string(b) != "second, changed" {
		t.Errorf("ref.md = %q, want the updated bytes", b)
	}
}

// newServerFor wraps an already-populated SkillRegistry in a Server so the read helpers work.
func newServerFor(skills *SkillRegistry) *Server {
	return NewServer(NewToolRegistry(), skills.rr, &ServerConfig{Skills: skills, SkillsDirectoryRead: true})
}
