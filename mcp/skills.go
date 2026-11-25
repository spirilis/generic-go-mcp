package mcp

// Skills: the io.modelcontextprotocol/skills extension (SEP-2640).
//
// BETA. SEP-2640 is Final and the extension is published at stage Beta (SEP-3392): real use
// is welcome, and a breaking change is possible but expected to be rare. This file follows
// specification/beta/skills.mdx in modelcontextprotocol/ext-skills as read on 2026-11-25.
// The extension is not part of the 2026-07-28 core specification.
//
// The whole surface is opt-in: with ServerConfig.Skills nil there is no capability key and
// skills/list, skills/get, and resources/directory/read all answer -32601, so a server that
// does not serve skills is byte-for-byte the server it was before this file existed.
//
// Two rules from the specification constrain the design and must not be softened:
//
//  1. The skill:// scheme is not privileged. A server MAY serve skills under any scheme
//     native to its domain, so nothing here inspects or requires a particular scheme.
//  2. Skill-ness is established only by skills/list and skills/get. This package must never
//     synthesize a skill entry by scanning the resource registry for a URI shape — a
//     SkillRegistry is the sole source of skills.

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"net/url"
	"path"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/spirilis/generic-go-mcp/transport"
	"gopkg.in/yaml.v3"
)

// SkillsExtensionID is the extension identifier under which this server declares skills
// support in ServerCapabilities.Extensions.
const SkillsExtensionID = "io.modelcontextprotocol/skills"

// SkillsMetaPrefix is the reserved reverse-domain prefix SEP-2640 sets aside for exposing
// additional skill frontmatter on a resource's _meta. This library never populates it —
// skills/list already carries the frontmatter verbatim — but a consumer that wants to may,
// through ResourceContentResult.Meta.
const SkillsMetaPrefix = "io.modelcontextprotocol.skills/"

// skillFileName is the entry point every skill must have at its root, per the Agent Skills
// specification.
const skillFileName = "SKILL.md"

// mimeTypeDirectory marks a directory entry in a resources/directory/read result.
const mimeTypeDirectory = "inode/directory"

// skillResourcesDynamic is what a skill entry's "resources" carries in place of a manifest
// when the skill's content is generated and has no stable digests.
const skillResourcesDynamic = "dynamic"

// The two per-skill limits the specification fixes. Every conforming host accepts a skill
// up to both; nothing promises that any host accepts a larger one, so Register refuses to
// publish one unless AllowOversizedSkills is set.
const (
	// MaxSkillResources bounds the entries of one skill's manifest, SKILL.md included.
	MaxSkillResources = 512
	// MaxSkillBytes bounds the summed size of one skill's files: 16 MiB.
	MaxSkillBytes = 16 << 20
)

// skillsExtensionSettings is the value of the extension's capability object. directoryRead
// carries no omitempty: the specification gives it a default of false, and saying so explicitly is
// friendlier to a client than an absent key it has to know the default for.
type skillsExtensionSettings struct {
	DirectoryRead bool `json:"directoryRead"`
}

// Skill is one skill entry, the shape skills/list and skills/get both return. A
// SkillRegistry produces it; only a SkillResolver assembles one by hand.
//
// Frontmatter is the SKILL.md YAML rendered verbatim as JSON. Unknown keys survive
// deliberately: the Agent Skills format permits license, metadata, compatibility,
// allowed-tools and more, and hosts ship supersets of their own. This library validates the
// two required keys and passes everything else through uninterpreted.
type Skill struct {
	URI         string
	Frontmatter map[string]interface{}
	// Resources is the complete manifest: SKILL.md and every supporting file, each once,
	// with its digest and size. It is what a host verifies reads against and what a user's
	// approval binds to. Empty exactly when Dynamic is set.
	Resources []SkillResource
	// Dynamic marks a skill whose content is generated, so no stable digests exist. On the
	// wire "resources" is then the string "dynamic" instead of an array. Such a skill has
	// no content integrity, and hosts may decline to load it.
	Dynamic bool
	// Meta is the entry's optional _meta. Keys this extension defines start with
	// SkillsMetaPrefix; use your own reverse-domain prefix for anything else.
	Meta map[string]interface{}
}

// skillWire is Skill as JSON. "resources" is a union, an array or the string "dynamic",
// which a struct tag cannot express.
type skillWire struct {
	URI         string                 `json:"uri"`
	Frontmatter map[string]interface{} `json:"frontmatter"`
	Resources   interface{}            `json:"resources"`
	Meta        map[string]interface{} `json:"_meta,omitempty"`
}

// MarshalJSON writes "resources" as the manifest array or as "dynamic". The key is never
// omitted and never null: the specification makes an entry without it invalid.
func (s Skill) MarshalJSON() ([]byte, error) {
	w := skillWire{URI: s.URI, Frontmatter: s.Frontmatter, Meta: s.Meta}
	switch {
	case s.Dynamic:
		w.Resources = skillResourcesDynamic
	case s.Resources == nil:
		w.Resources = []SkillResource{}
	default:
		w.Resources = s.Resources
	}
	return json.Marshal(w)
}

// UnmarshalJSON accepts either form of "resources".
func (s *Skill) UnmarshalJSON(b []byte) error {
	var w struct {
		URI         string                 `json:"uri"`
		Frontmatter map[string]interface{} `json:"frontmatter"`
		Resources   json.RawMessage        `json:"resources"`
		Meta        map[string]interface{} `json:"_meta"`
	}
	if err := json.Unmarshal(b, &w); err != nil {
		return err
	}
	out := Skill{URI: w.URI, Frontmatter: w.Frontmatter, Meta: w.Meta}
	var marker string
	switch {
	case len(w.Resources) == 0 || string(w.Resources) == "null":
		return fmt.Errorf("skill %q: resources is required: an array or %q", w.URI, skillResourcesDynamic)
	case json.Unmarshal(w.Resources, &marker) == nil:
		if marker != skillResourcesDynamic {
			return fmt.Errorf("skill %q: resources is %q, want an array or %q", w.URI, marker, skillResourcesDynamic)
		}
		out.Dynamic = true
	default:
		if err := json.Unmarshal(w.Resources, &out.Resources); err != nil {
			return fmt.Errorf("skill %q: resources: %w", w.URI, err)
		}
	}
	*s = out
	return nil
}

// SkillResource names one file belonging to a skill.
type SkillResource struct {
	URI string `json:"uri"`
	// Digest is "sha256:" followed by 64 lowercase hex characters over the file's RAW
	// BYTES, and matches what resources/read returns for URI. Clients verify it; this
	// library computes it, so the two cannot drift.
	Digest string `json:"digest"`
	// Size is the length in bytes of the same raw content. A host treats a read of any
	// other length as a verification failure before it computes the digest.
	Size int64 `json:"size"`
}

// SkillContent is one skill file's bytes. The digest, the size, and the choice between a
// text and a base64 blob payload are all derived from Bytes; MimeType, if set, overrides
// this package's extension-based detection.
type SkillContent struct {
	Bytes    []byte
	MimeType string
}

// SkillDef is the input side of registration: everything a consumer supplies. Files is
// keyed by absolute resource URI and is the single source of truth for the published
// digests and sizes, for the frontmatter, and for what resources/read serves. That is what
// makes it impossible to publish an entry that disagrees with the bytes.
type SkillDef struct {
	// URI is the skill's SKILL.md and must end in "/SKILL.md". The last segment of the
	// path above it is the skill root and must equal the frontmatter name.
	URI string
	// Frontmatter is optional for a skill with Files: the registry parses it from the
	// SKILL.md bytes. If it is set anyway it must equal what those bytes contain, or
	// Register fails. A Dynamic skill has no bytes to parse, so there it is required.
	Frontmatter map[string]interface{}
	// Files must contain URI plus every other file the skill publishes. Each key must be
	// under the skill root. Empty for a Dynamic skill.
	Files map[string]SkillContent
	// Dynamic lists a skill whose content is generated: the entry carries
	// "resources": "dynamic" and the registry registers no files. Serving the SKILL.md
	// and everything else through the ResourceRegistry (usually a ResourceTemplate over
	// the same URI namespace) is then the caller's job, and so is keeping the served
	// frontmatter equal to Frontmatter.
	Dynamic bool
	// Meta becomes the entry's _meta.
	Meta map[string]interface{}
}

// SkillResolver backs skills/get for catalogs too large or too dynamic to enumerate, which
// is how the rule "a server MUST answer for every skill it serves, listed or not"
// becomes implementable without materializing the keyspace. It is consulted only after a
// registered lookup misses.
//
// A resolver returns the entry directly and therefore owns digest and size correctness
// itself: either a complete Resources manifest, or Dynamic. It is also responsible for
// making the skill's files readable — pair it with a ResourceTemplate registered over the
// same URI namespace, since nothing is registered on the ResourceRegistry on its behalf.
//
// The entry is checked before it is sent (see validateSkillEntry). One that fails is a
// server bug, logged and answered with -32603, never passed to a host that would refuse it.
type SkillResolver func(ctx context.Context, uri string) (Skill, bool, error)

// SkillRegistry holds the skills this server serves and keeps the resource registry in step
// with them: registering a skill also registers every file it publishes, so skills/list and
// resources/read can never disagree about what exists.
type SkillRegistry struct {
	mu        sync.RWMutex
	rr        *ResourceRegistry
	skills    []Skill             // registration order, stable
	owns      map[string][]string // skill URI -> the file URIs it registered
	refs      map[string]int      // file URI -> how many skills publish it
	digests   map[string]string   // file URI -> published sha256 digest, for conflict detection
	resolver  SkillResolver
	oversized bool // AllowOversizedSkills
}

// NewSkillRegistry creates a skill registry backed by rr.
//
// It takes the resource registry because a skill's files ARE resources: registering a skill
// registers its files so resources/read can serve them, and unregistering it takes them
// away again. rr must not be nil.
func NewSkillRegistry(rr *ResourceRegistry) *SkillRegistry {
	if rr == nil {
		panic("mcp: NewSkillRegistry requires a non-nil ResourceRegistry")
	}
	return &SkillRegistry{
		rr:      rr,
		skills:  []Skill{},
		owns:    make(map[string][]string),
		refs:    make(map[string]int),
		digests: make(map[string]string),
	}
}

// Register adds one skill and every file resource it names, computing each file's sha256
// digest from its bytes. Registering a skill URI that is already present replaces it and
// moves it to the end of the list, mirroring ResourceRegistry.Register.
//
// The whole batch — files dropped by a replacement and files added — moves through the
// resource registry under one lock and fires a single
// notifications/resources/list_changed, rather than one per file.
//
// It returns an error if def.URI does not end in "/SKILL.md", if def.Files omits def.URI or
// names a file outside the skill root, if the SKILL.md does not begin with YAML frontmatter
// or that frontmatter fails the Agent Skills naming rules, if def.Frontmatter is set and
// differs from the file's, if the last segment of the skill root does not equal the
// frontmatter name, or if the skill exceeds MaxSkillResources or MaxSkillBytes (see
// AllowOversizedSkills).
//
// A def with Dynamic set registers the entry alone: see SkillDef.Dynamic.
func (r *SkillRegistry) Register(def SkillDef) error {
	skill, resources, fns, err := buildSkill(def)
	if err != nil {
		return err
	}

	r.mu.Lock()

	if !r.oversized {
		if err := checkSkillLimits(skill); err != nil {
			r.mu.Unlock()
			return err
		}
	}

	// Validate before mutating: a file URI this skill publishes that another skill also
	// publishes must carry identical bytes, or skills/list would advertise a digest that
	// resources/read — last-writer-wins on the shared ResourceFunction — cannot reproduce.
	// This only arises for genuinely nested skills (buildSkill already rejects a file outside
	// the skill root, so unrelated skills can't collide); LoadFS re-reads the same file, so
	// its shared digests always agree and this never fires for it. A skill's own prior
	// incarnation is excluded — replacing a skill may legitimately change its own files.
	ownedBySelf := make(map[string]struct{}, len(r.owns[skill.URI]))
	for _, u := range r.owns[skill.URI] {
		ownedBySelf[u] = struct{}{}
	}
	for _, sr := range skill.Resources {
		if _, self := ownedBySelf[sr.URI]; self {
			continue
		}
		if existing, ok := r.digests[sr.URI]; ok && existing != sr.Digest {
			r.mu.Unlock()
			return fmt.Errorf(
				"skill %q: file %q is already published by another skill with a different digest (%s vs %s); shared files must carry identical bytes",
				skill.URI, sr.URI, existing, sr.Digest)
		}
	}

	var stale []string
	if r.removeSkillLocked(skill.URI) {
		stale = r.releaseLocked(skill.URI)
	}
	r.skills = append(r.skills, skill)
	owned := make([]string, 0, len(resources))
	for i, res := range resources {
		owned = append(owned, res.URI)
		r.refs[res.URI]++
		r.digests[res.URI] = skill.Resources[i].Digest
	}
	r.owns[skill.URI] = owned
	// A file the new incarnation still publishes must not be unregistered on the way in;
	// letting the add path see it as a replacement is what fires resources/updated for it.
	stale = withoutAny(stale, owned)
	// Register the file resources before releasing r.mu, so skills/list — which also needs
	// r.mu — cannot observe this skill until resources/read can already serve its files. The
	// lock order is always SkillRegistry.mu -> ResourceRegistry.mu (rr never calls back into
	// this registry), so holding both here cannot deadlock.
	r.rr.mutateBatch(stale, resources, fns)
	r.mu.Unlock()
	return nil
}

// Unregister removes the skill registered under uri along with the files it published,
// reporting whether one was found. Files that another skill still publishes — the nested
// skill case, where a descendant's files also belong to the enclosing skill — are left
// registered.
func (r *SkillRegistry) Unregister(uri string) bool {
	r.mu.Lock()
	if !r.removeSkillLocked(uri) {
		r.mu.Unlock()
		return false
	}
	stale := r.releaseLocked(uri)
	// Unregister the files before releasing r.mu, symmetric with Register: skills/list must
	// not observe the skill gone while its files are still readable, or vice versa.
	r.rr.mutateBatch(stale, nil, nil)
	r.mu.Unlock()
	return true
}

// List returns every registered skill in registration order.
func (r *SkillRegistry) List() []Skill {
	r.mu.RLock()
	defer r.mu.RUnlock()
	// Deliberately make() rather than a nil slice: this feeds skills/list, where an empty
	// catalog must serialize as [] and not null.
	out := make([]Skill, len(r.skills))
	copy(out, r.skills)
	return out
}

// Get returns the skill registered under uri. It does not consult the resolver; see
// SetResolver.
func (r *SkillRegistry) Get(uri string) (Skill, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, s := range r.skills {
		if s.URI == uri {
			return s, true
		}
	}
	return Skill{}, false
}

// Has reports whether any skill is registered. Note that the extension is declared whenever
// a SkillRegistry is configured, empty or not: an empty listing is legitimate (a
// resolver-backed catalog is unenumerable by design) and the specification says hosts MUST
// NOT read one as proof that a server has no skills.
func (r *SkillRegistry) Has() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.skills) > 0
}

// SetResolver installs the fallback consulted by skills/get when a URI is not registered.
// Passing nil removes it.
func (r *SkillRegistry) SetResolver(f SkillResolver) {
	r.mu.Lock()
	r.resolver = f
	r.mu.Unlock()
}

// AllowOversizedSkills lets Register publish a skill beyond MaxSkillResources or
// MaxSkillBytes. The specification says a server SHOULD NOT: a host is only required to
// accept a skill within both limits, so a larger one may load nowhere. Set this only for
// hosts you know take it. It affects later registrations, not skills already registered.
func (r *SkillRegistry) AllowOversizedSkills(allow bool) {
	r.mu.Lock()
	r.oversized = allow
	r.mu.Unlock()
}

// checkSkillLimits applies the specification's two per-skill limits to a manifest.
func checkSkillLimits(skill Skill) error {
	if n := len(skill.Resources); n > MaxSkillResources {
		return fmt.Errorf("skill %q: %d files exceeds the %d a host must accept (AllowOversizedSkills overrides)",
			skill.URI, n, MaxSkillResources)
	}
	var total int64
	for _, sr := range skill.Resources {
		total += sr.Size
	}
	if total > MaxSkillBytes {
		return fmt.Errorf("skill %q: %d bytes exceeds the %d a host must accept (AllowOversizedSkills overrides)",
			skill.URI, total, MaxSkillBytes)
	}
	return nil
}

// resolve runs the resolver, if one is installed. Callers must have already tried Get.
func (r *SkillRegistry) resolve(ctx context.Context, uri string) (Skill, bool, error) {
	r.mu.RLock()
	f := r.resolver
	r.mu.RUnlock()
	if f == nil {
		return Skill{}, false, nil
	}
	return f(ctx, uri)
}

// removeSkillLocked drops the entry for uri from the ordered list, reporting whether one
// existed. Callers must hold r.mu for writing. It does not touch refs — see releaseLocked.
func (r *SkillRegistry) removeSkillLocked(uri string) bool {
	for i, s := range r.skills {
		if s.URI == uri {
			r.skills = append(r.skills[:i], r.skills[i+1:]...)
			return true
		}
	}
	return false
}

// releaseLocked gives up uri's claim on the files it published and returns those no other
// skill still claims, which are the ones safe to unregister. Callers must hold r.mu for
// writing.
func (r *SkillRegistry) releaseLocked(uri string) []string {
	owned := r.owns[uri]
	delete(r.owns, uri)
	var stale []string
	for _, f := range owned {
		r.refs[f]--
		if r.refs[f] <= 0 {
			delete(r.refs, f)
			delete(r.digests, f)
			stale = append(stale, f)
		}
	}
	return stale
}

// withoutAny returns items minus everything in exclude.
func withoutAny(items, exclude []string) []string {
	if len(items) == 0 || len(exclude) == 0 {
		return items
	}
	drop := make(map[string]struct{}, len(exclude))
	for _, e := range exclude {
		drop[e] = struct{}{}
	}
	out := items[:0]
	for _, it := range items {
		if _, skip := drop[it]; !skip {
			out = append(out, it)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Building a skill from a definition
// ---------------------------------------------------------------------------

// skillNamePattern is the Agent Skills naming rule: lowercase alphanumerics in
// hyphen-separated groups, which forbids a leading or trailing hyphen and a "--" run
// without needing three more checks to say so.
var skillNamePattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

const (
	maxSkillNameLen          = 64
	maxSkillDescriptionLen   = 1024
	maxSkillCompatibilityLen = 500
)

// buildSkill validates a definition and derives everything the wire and the resource
// registry need from it: the frontmatter, the digests and sizes, the mime types, and one
// ResourceFunction per file.
func buildSkill(def SkillDef) (Skill, []Resource, []ResourceFunction, error) {
	fail := func(format string, args ...interface{}) (Skill, []Resource, []ResourceFunction, error) {
		return Skill{}, nil, nil, fmt.Errorf(format, args...)
	}

	root, ok := skillRoot(def.URI)
	if !ok {
		return fail("skill uri %q must end in %q", def.URI, "/"+skillFileName)
	}

	frontmatter := def.Frontmatter
	if def.Dynamic {
		if len(def.Files) > 0 {
			return fail("skill %q: a Dynamic skill takes no Files; serve them through the ResourceRegistry", def.URI)
		}
	} else {
		manifest, present := def.Files[def.URI]
		if !present {
			return fail("skill %q: Files must contain the SKILL.md uri itself", def.URI)
		}
		// The entry's frontmatter comes from the bytes a host will read, not from a
		// second copy that could drift: a host compares the two field by field and
		// refuses the skill on any difference.
		parsed, err := parseFrontmatter(manifest.Bytes)
		if err != nil {
			return fail("skill %q: %w", def.URI, err)
		}
		if def.Frontmatter != nil && !sameJSON(def.Frontmatter, parsed) {
			return fail("skill %q: Frontmatter differs from the frontmatter in the SKILL.md bytes; leave it nil to use the file's", def.URI)
		}
		frontmatter = parsed
	}

	name, err := validateSkillFrontmatter(frontmatter)
	if err != nil {
		return fail("skill %q: %w", def.URI, err)
	}

	// "The final segment of <skill-path> MUST equal the skill's name". Compared lexically
	// so it holds for skill://git-workflow/SKILL.md, where the name occupies the URI's
	// authority component, as well as for skill://acme/billing/refunds/SKILL.md.
	if seg := lastSegment(root); seg != name {
		return fail("skill %q: final path segment %q does not equal frontmatter name %q", def.URI, seg, name)
	}

	skill := Skill{URI: def.URI, Frontmatter: frontmatter, Dynamic: def.Dynamic, Meta: def.Meta}
	if def.Dynamic {
		return skill, nil, nil, nil
	}

	// SKILL.md first, then the rest lexically: deterministic output, and the entry point
	// leads the resources array the way the specification's examples show it.
	uris := make([]string, 0, len(def.Files))
	for u := range def.Files {
		if u == def.URI {
			continue
		}
		if !strings.HasPrefix(u, root+"/") {
			return fail("skill %q: file %q is not under the skill root %q", def.URI, u, root)
		}
		uris = append(uris, u)
	}
	sort.Strings(uris)
	uris = append([]string{def.URI}, uris...)

	description, _ := frontmatter["description"].(string)

	skill.Resources = make([]SkillResource, 0, len(uris))
	resources := make([]Resource, 0, len(uris))
	fns := make([]ResourceFunction, 0, len(uris))

	for _, u := range uris {
		content := def.Files[u]
		mimeType := content.MimeType
		if mimeType == "" {
			mimeType = skillMimeType(u, content.Bytes)
		}

		// One length and one hash of one byte slice: the manifest entry, the resource's
		// size and what resources/read serves cannot disagree.
		size := int64(len(content.Bytes))
		skill.Resources = append(skill.Resources, SkillResource{
			URI:    u,
			Digest: fmt.Sprintf("sha256:%x", sha256.Sum256(content.Bytes)),
			Size:   size,
		})

		res := Resource{
			URI:      u,
			Name:     displaySegment(strings.TrimPrefix(u, root+"/")),
			MimeType: mimeType,
			Size:     &size,
		}
		if u == def.URI {
			// A SKILL.md resource SHOULD take its name and description from the
			// frontmatter.
			res.Name = name
			res.Description = description
		}
		resources = append(resources, res)
		fns = append(fns, staticResourceFunction(content.Bytes, mimeType))
	}

	return skill, resources, fns, nil
}

// sameJSON reports whether a and b render to the same JSON value. It is how a supplied
// frontmatter map is compared with the parsed one: the wire carries JSON, so two maps that
// differ only in Go types (int versus float64, say) are the same frontmatter.
func sameJSON(a, b interface{}) bool {
	norm := func(v interface{}) (interface{}, bool) {
		raw, err := json.Marshal(v)
		if err != nil {
			return nil, false
		}
		var out interface{}
		if err := json.Unmarshal(raw, &out); err != nil {
			return nil, false
		}
		return out, true
	}
	na, okA := norm(a)
	nb, okB := norm(b)
	return okA && okB && reflect.DeepEqual(na, nb)
}

// skillDigestPattern is the only digest form the specification allows.
var skillDigestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// validateSkillEntry checks an entry a SkillResolver assembled by hand against the rules a
// host enforces on receipt, so a resolver bug surfaces here and not as a skill that
// silently fails to load. want is the URI the client asked for. Entries built by Register
// satisfy all of this by construction and are not re-checked.
func validateSkillEntry(skill Skill, want string) error {
	if skill.URI != want {
		return fmt.Errorf("resolver returned entry %q for %q", skill.URI, want)
	}
	root, ok := skillRoot(skill.URI)
	if !ok {
		return fmt.Errorf("skill uri %q must end in %q", skill.URI, "/"+skillFileName)
	}
	name, err := validateSkillFrontmatter(skill.Frontmatter)
	if err != nil {
		return fmt.Errorf("skill %q: %w", skill.URI, err)
	}
	if seg := lastSegment(root); seg != name {
		return fmt.Errorf("skill %q: final path segment %q does not equal frontmatter name %q", skill.URI, seg, name)
	}
	if skill.Dynamic {
		if len(skill.Resources) > 0 {
			return fmt.Errorf("skill %q: Dynamic and Resources are mutually exclusive", skill.URI)
		}
		return nil
	}
	if len(skill.Resources) == 0 {
		return fmt.Errorf("skill %q: Resources must list SKILL.md and every other file, or Dynamic must be set", skill.URI)
	}
	seen := make(map[string]struct{}, len(skill.Resources))
	for _, sr := range skill.Resources {
		if sr.URI != skill.URI && !strings.HasPrefix(sr.URI, root+"/") {
			return fmt.Errorf("skill %q: file %q is not under the skill root %q", skill.URI, sr.URI, root)
		}
		if _, dup := seen[sr.URI]; dup {
			return fmt.Errorf("skill %q: file %q is listed twice", skill.URI, sr.URI)
		}
		seen[sr.URI] = struct{}{}
		if !skillDigestPattern.MatchString(sr.Digest) {
			return fmt.Errorf("skill %q: file %q: digest %q is not sha256: plus 64 lowercase hex characters", skill.URI, sr.URI, sr.Digest)
		}
		if sr.Size < 0 {
			return fmt.Errorf("skill %q: file %q: negative size", skill.URI, sr.URI)
		}
	}
	if _, ok := seen[skill.URI]; !ok {
		return fmt.Errorf("skill %q: Resources must include the SKILL.md itself", skill.URI)
	}
	return nil
}

// staticResourceFunction serves a fixed byte slice. The payload is decided once, at
// registration: valid NUL-free UTF-8 goes out as text, anything else as a base64 blob.
// Either way the client digests exactly the bytes that produced the published digest.
func staticResourceFunction(b []byte, mimeType string) ResourceFunction {
	content := ResourceContentResult{MimeType: mimeType}
	if isTextual(b) {
		content.Text = string(b)
	} else {
		content.Blob = base64.StdEncoding.EncodeToString(b)
	}
	return func(context.Context) (ResourceContentResult, error) { return content, nil }
}

// validateSkillFrontmatter enforces the Agent Skills specification's rules for the two
// required keys (and the one optional key with a documented bound), and confirms the whole
// map can be rendered as JSON. Everything else — license, metadata, allowed-tools — passes
// through uninterpreted. allowed-tools in particular is a host-side consent matter that
// hosts MUST ignore for MCP-origin skills absent explicit per-skill approval, so this
// library must not act on it.
func validateSkillFrontmatter(fm map[string]interface{}) (string, error) {
	if len(fm) == 0 {
		return "", fmt.Errorf("frontmatter is required and must contain name and description")
	}

	name, ok := fm["name"].(string)
	if !ok {
		return "", fmt.Errorf("frontmatter name is required and must be a string")
	}
	if n := utf8.RuneCountInString(name); n < 1 || n > maxSkillNameLen {
		return "", fmt.Errorf("frontmatter name %q must be 1-%d characters, got %d", name, maxSkillNameLen, n)
	}
	if !skillNamePattern.MatchString(name) {
		return "", fmt.Errorf(
			"frontmatter name %q must match %s (lowercase alphanumerics and single interior hyphens)",
			name, skillNamePattern)
	}

	description, ok := fm["description"].(string)
	if !ok || description == "" {
		return "", fmt.Errorf("frontmatter description is required and must be a non-empty string")
	}
	if n := utf8.RuneCountInString(description); n > maxSkillDescriptionLen {
		return "", fmt.Errorf("frontmatter description must be at most %d characters, got %d", maxSkillDescriptionLen, n)
	}

	if raw, present := fm["compatibility"]; present {
		compat, ok := raw.(string)
		if !ok {
			return "", fmt.Errorf("frontmatter compatibility must be a string")
		}
		if n := utf8.RuneCountInString(compat); n > maxSkillCompatibilityLen {
			return "", fmt.Errorf("frontmatter compatibility must be at most %d characters, got %d", maxSkillCompatibilityLen, n)
		}
	}

	// skills/list carries the frontmatter verbatim as JSON, so a map that cannot be
	// rendered has to fail here rather than at serialization time, where the error would
	// surface as a broken response instead of a registration error.
	if _, err := json.Marshal(fm); err != nil {
		return "", fmt.Errorf("frontmatter cannot be rendered as JSON: %w", err)
	}

	return name, nil
}

// skillRoot splits a SKILL.md uri into the skill root, reporting whether the uri named a
// SKILL.md at all.
func skillRoot(uri string) (string, bool) {
	suffix := "/" + skillFileName
	if !strings.HasSuffix(uri, suffix) {
		return "", false
	}
	root := strings.TrimSuffix(uri, suffix)
	if root == "" || lastSegment(root) == "" {
		return "", false
	}
	return root, true
}

// lastSegment returns the text after the final "/", which for skill://git-workflow is
// "git-workflow" (the authority) and for skill://acme/billing/refunds is "refunds".
func lastSegment(s string) string {
	if i := strings.LastIndexByte(s, '/'); i >= 0 {
		return s[i+1:]
	}
	return s
}

// displaySegment renders a URI path fragment for a human-facing name field, undoing
// percent-encoding where it can and leaving the raw text alone where it cannot.
func displaySegment(s string) string {
	if unescaped, err := url.PathUnescape(s); err == nil {
		return unescaped
	}
	return s
}

// isTextual reports whether b can travel as a JSON string without loss. NUL is excluded
// deliberately: it is valid UTF-8 but a reliable sign of binary content.
func isTextual(b []byte) bool {
	if !utf8.Valid(b) {
		return false
	}
	for _, c := range b {
		if c == 0 {
			return false
		}
	}
	return true
}

// skillMimeTypes is a small, deliberately hand-rolled extension table. mime.TypeByExtension
// consults the host's mime.types database, so the same skill would publish different mime
// types on different machines; a skill's catalog must not depend on where the server runs.
var skillMimeTypes = map[string]string{
	".markdown": "text/markdown",
	".md":       "text/markdown",
	".txt":      "text/plain",
	".json":     "application/json",
	".yaml":     "application/yaml",
	".yml":      "application/yaml",
	".toml":     "application/toml",
	".csv":      "text/csv",
	".html":     "text/html",
	".htm":      "text/html",
	".css":      "text/css",
	".js":       "text/javascript",
	".py":       "text/x-python",
	".sh":       "text/x-shellscript",
	".go":       "text/x-go",
	".sql":      "application/sql",
	".xml":      "application/xml",
	".svg":      "image/svg+xml",
	".png":      "image/png",
	".jpg":      "image/jpeg",
	".jpeg":     "image/jpeg",
	".gif":      "image/gif",
	".webp":     "image/webp",
	".pdf":      "application/pdf",
}

func skillMimeType(uri string, b []byte) string {
	if m, ok := skillMimeTypes[strings.ToLower(path.Ext(uri))]; ok {
		return m
	}
	if isTextual(b) {
		return "text/plain"
	}
	return "application/octet-stream"
}

// ---------------------------------------------------------------------------
// Loading skills from a filesystem
// ---------------------------------------------------------------------------

// LoadFS walks fsys for SKILL.md files, parses and validates their frontmatter, computes
// sha256 digests over raw bytes, and registers each skill together with every file under
// its directory. uriPrefix is typically "skill://" and must end in "://" or "/"; a skill's
// URI is that prefix followed by its path within fsys, percent-encoded segment by segment.
//
// This is the point of the API: one call turns an embed.FS of skill directories into a
// compliant skills/list plus readable resources, with no digest bookkeeping to rot. It
// takes an fs.FS rather than an embed.FS so os.DirFS and testing/fstest work too. With the
// usual //go:embed all:content layout, hand it fs.Sub(embedded, "content") so the embed
// directory name does not end up in every URI.
//
// Walk rules:
//
//   - A directory is a skill if and only if it directly contains SKILL.md.
//   - A skill's files are every regular file at or below its directory. That
//     includes the files of any nested skill, which is also registered as its own ordinary
//     top-level entry — publication is flat and nesting is not marked.
//   - A directory with no SKILL.md that is not inside a skill is an organizational prefix
//     (acme/, acme/billing/) and is fine. A file in such a directory is an error: it would
//     belong to no skill and be unreachable.
//   - Entries whose base name begins with "." are skipped.
//
// Registration is per skill, so a filesystem holding three skills fires three
// notifications/resources/list_changed, not one per file.
func (r *SkillRegistry) LoadFS(fsys fs.FS, uriPrefix string) error {
	if !strings.HasSuffix(uriPrefix, "://") && !strings.HasSuffix(uriPrefix, "/") {
		return fmt.Errorf("uri prefix %q must end in %q or %q", uriPrefix, "://", "/")
	}

	var files []string
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == "." {
			return nil
		}
		if strings.HasPrefix(path.Base(p), ".") {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if !d.IsDir() && d.Type().IsRegular() {
			files = append(files, p)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("walking skill filesystem: %w", err)
	}
	sort.Strings(files)

	var roots []string
	for _, f := range files {
		if path.Base(f) != skillFileName {
			continue
		}
		dir := path.Dir(f)
		if dir == "." {
			return fmt.Errorf(
				"%s at the root of the filesystem: a skill must live in a directory named after its frontmatter name", f)
		}
		roots = append(roots, dir)
	}
	if len(roots) == 0 {
		return fmt.Errorf("no %s found in the filesystem", skillFileName)
	}

	owned := make(map[string][]string, len(roots))
	for _, f := range files {
		var owners int
		for _, root := range roots {
			if f == root+"/"+skillFileName || strings.HasPrefix(f, root+"/") {
				owned[root] = append(owned[root], f)
				owners++
			}
		}
		if owners == 0 {
			return fmt.Errorf("file %q belongs to no skill: it is not under any directory containing a %s", f, skillFileName)
		}
	}

	for _, root := range roots {
		def := SkillDef{
			URI:   uriPrefix + escapeURIPath(root+"/"+skillFileName),
			Files: make(map[string]SkillContent, len(owned[root])),
		}
		for _, f := range owned[root] {
			b, err := fs.ReadFile(fsys, f)
			if err != nil {
				return fmt.Errorf("reading %q: %w", f, err)
			}
			def.Files[uriPrefix+escapeURIPath(f)] = SkillContent{Bytes: b}
		}
		if err := r.Register(def); err != nil {
			return fmt.Errorf("%s/%s: %w", root, skillFileName, err)
		}
	}
	return nil
}

// escapeURIPath percent-encodes each segment of a filesystem path so it can be pasted into
// a URI. Ordinary skill and file names pass through untouched.
func escapeURIPath(p string) string {
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return strings.Join(segs, "/")
}

// parseFrontmatter extracts the leading "---"-delimited YAML block of a SKILL.md and
// decodes it into the map that goes on the wire verbatim.
//
// The fences are read strictly, the way the hosts that will re-parse this file read them:
// the first line is exactly "---", and the block ends at the next line that is "---"
// (trailing spaces or tabs allowed). A file a host cannot split is refused here.
func parseFrontmatter(b []byte) (map[string]interface{}, error) {
	s := strings.TrimPrefix(string(b), "\ufeff")
	var rest string
	switch {
	case strings.HasPrefix(s, "---\n"):
		rest = s[4:]
	case strings.HasPrefix(s, "---\r\n"):
		rest = s[5:]
	default:
		return nil, fmt.Errorf("missing YAML frontmatter: file does not begin with a --- line")
	}

	end := -1
	for offset := 0; offset < len(rest); {
		line := rest[offset:]
		if i := strings.IndexByte(line, '\n'); i >= 0 {
			line = line[:i]
		}
		if strings.TrimRight(line, " \t\r") == "---" && strings.HasPrefix(line, "---") {
			end = offset
			break
		}
		offset += len(line) + 1
	}
	if end < 0 {
		return nil, fmt.Errorf("unterminated YAML frontmatter: no closing --- line")
	}

	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(rest[:end]), &doc); err != nil {
		return nil, fmt.Errorf("parsing YAML frontmatter: %w", err)
	}
	if doc.Kind == 0 || len(doc.Content) == 0 {
		return nil, fmt.Errorf("YAML frontmatter is empty")
	}
	budget := maxFrontmatterNodes
	v, err := yamlJSONValue(doc.Content[0], 0, &budget)
	if err != nil {
		return nil, fmt.Errorf("parsing YAML frontmatter: %w", err)
	}
	fm, ok := v.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("YAML frontmatter must be a mapping")
	}
	if len(fm) == 0 {
		return nil, fmt.Errorf("YAML frontmatter is empty")
	}
	return fm, nil
}

// Bounds on walking a frontmatter document. Aliases can make a few bytes of YAML expand
// without limit, or refer to themselves.
const (
	maxFrontmatterDepth = 64
	maxFrontmatterNodes = 10000
)

// yamlJSONValue converts a YAML node to the JSON value a YAML 1.2 core-schema parser would
// produce for it, which is what the hosts comparing this entry with the file use.
//
// Decoding straight into interface{} would follow yaml.v3's own resolution instead, which
// keeps some YAML 1.1 habits: an unquoted 2026-11-25 becomes a time.Time (and then a
// different string in JSON), 014 is read as octal, and << merges maps. A host would see a
// frontmatter that differs from the entry and refuse the skill.
func yamlJSONValue(n *yaml.Node, depth int, budget *int) (interface{}, error) {
	if depth > maxFrontmatterDepth {
		return nil, fmt.Errorf("nested deeper than %d levels", maxFrontmatterDepth)
	}
	if *budget--; *budget < 0 {
		return nil, fmt.Errorf("more than %d values", maxFrontmatterNodes)
	}
	switch n.Kind {
	case yaml.AliasNode:
		return yamlJSONValue(n.Alias, depth+1, budget)
	case yaml.MappingNode:
		out := make(map[string]interface{}, len(n.Content)/2)
		for i := 0; i+1 < len(n.Content); i += 2 {
			k := n.Content[i]
			if k.Kind == yaml.AliasNode {
				k = k.Alias
			}
			if k.Kind != yaml.ScalarNode {
				return nil, fmt.Errorf("line %d: a mapping key must be a scalar", k.Line)
			}
			if _, dup := out[k.Value]; dup {
				return nil, fmt.Errorf("line %d: duplicate key %q", k.Line, k.Value)
			}
			v, err := yamlJSONValue(n.Content[i+1], depth+1, budget)
			if err != nil {
				return nil, err
			}
			out[k.Value] = v
		}
		return out, nil
	case yaml.SequenceNode:
		out := make([]interface{}, 0, len(n.Content))
		for _, c := range n.Content {
			v, err := yamlJSONValue(c, depth+1, budget)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		return out, nil
	case yaml.ScalarNode:
		return yamlScalar(n)
	default:
		return nil, fmt.Errorf("line %d: unsupported YAML node", n.Line)
	}
}

// The YAML 1.2 core schema's plain-scalar forms.
var (
	yamlDecimalPattern = regexp.MustCompile(`^[-+]?[0-9]+$`)
	yamlOctalPattern   = regexp.MustCompile(`^0o[0-7]+$`)
	yamlHexPattern     = regexp.MustCompile(`^0x[0-9a-fA-F]+$`)
	yamlFloatPattern   = regexp.MustCompile(`^[-+]?(\.[0-9]+|[0-9]+(\.[0-9]*)?)([eE][-+]?[0-9]+)?$`)
	yamlInfNaNPattern  = regexp.MustCompile(`^([-+]?\.(inf|Inf|INF)|\.(nan|NaN|NAN))$`)
)

// yamlScalar resolves one scalar by the YAML 1.2 core schema. Anything quoted, in block
// style or explicitly tagged !!str is a string; a plain scalar is null, a boolean or a
// number only in the forms that schema names, and a string otherwise.
func yamlScalar(n *yaml.Node) (interface{}, error) {
	if n.Style&(yaml.DoubleQuotedStyle|yaml.SingleQuotedStyle|yaml.LiteralStyle|yaml.FoldedStyle) != 0 || n.Tag == "!!str" && n.Style&yaml.TaggedStyle != 0 {
		return n.Value, nil
	}
	v := n.Value
	switch v {
	case "", "~", "null", "Null", "NULL":
		return nil, nil
	case "true", "True", "TRUE":
		return true, nil
	case "false", "False", "FALSE":
		return false, nil
	}
	switch {
	case yamlDecimalPattern.MatchString(v):
		return yamlInt(v, strings.TrimPrefix(v, "+"), 10), nil
	case yamlOctalPattern.MatchString(v):
		return yamlInt(v, v[2:], 8), nil
	case yamlHexPattern.MatchString(v):
		return yamlInt(v, v[2:], 16), nil
	case yamlFloatPattern.MatchString(v):
		f, err := strconv.ParseFloat(v, 64)
		if err != nil && !errors.Is(err, strconv.ErrRange) {
			return v, nil
		}
		if math.IsInf(f, 0) {
			return nil, fmt.Errorf("line %d: %s is out of range for JSON", n.Line, v)
		}
		return f, nil
	case yamlInfNaNPattern.MatchString(v):
		return nil, fmt.Errorf("line %d: %s cannot be rendered as JSON; quote it", n.Line, v)
	}
	return v, nil
}

// yamlInt parses digits in base. A value too large for int64 becomes a float64, the same
// number a JavaScript host ends up holding, and "-0" stays negative zero for the same
// reason.
func yamlInt(literal, digits string, base int) interface{} {
	i, err := strconv.ParseInt(digits, base, 64)
	if err != nil {
		f, _ := strconv.ParseFloat(digits, 64)
		if base != 10 {
			u, uerr := strconv.ParseUint(digits, base, 64)
			if uerr == nil {
				return float64(u)
			}
			return literal
		}
		return f
	}
	if i == 0 && strings.HasPrefix(literal, "-") {
		return math.Copysign(0, -1)
	}
	return i
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

// SkillsListResult is the result of skills/list.
type SkillsListResult struct {
	CacheableResult
	Skills     []Skill `json:"skills"`
	NextCursor string  `json:"nextCursor,omitempty"`
}

func (s *Server) handleSkillsList(ctx context.Context, params json.RawMessage) (Result, *transport.RPCError) {
	var p struct {
		Cursor string `json:"cursor"`
	}
	if len(params) > 0 {
		_ = json.Unmarshal(params, &p)
	}

	all := s.config.Skills.List()
	page, next, err := paginate(all, p.Cursor, defaultPageSize)
	if err != nil {
		return nil, invalidParamsErr("invalid cursor")
	}

	return &SkillsListResult{
		CacheableResult: NewCacheableResult(s.listTTLMs, s.cacheScope()),
		Skills:          page,
		NextCursor:      next,
	}, nil
}

// SkillsGetResult is the result of skills/get. The entry sits under a "skill" key rather
// than being the result itself.
//
// ttlMs and cacheScope are required here as on every cacheable result. ttlMs says how long
// a host may treat the entry as current before calling skills/get again, so it is the list
// TTL (an entry is a listing of one), not the resources/read TTL. Neither field is an
// integrity property: a host verifies every read against the manifest however fresh its
// copy of the entry is.
type SkillsGetResult struct {
	CacheableResult
	Skill Skill `json:"skill"`
}

func (s *Server) handleSkillsGet(ctx context.Context, params json.RawMessage) (Result, *transport.RPCError) {
	var p struct {
		URI string `json:"uri"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, invalidParamsErr("invalid skills/get params: %v", err)
	}
	if p.URI == "" {
		return nil, invalidParamsErr("skills/get requires a uri")
	}

	skill, ok := s.config.Skills.Get(p.URI)
	if !ok {
		// Only now the resolver: it exists for catalogs skills/list cannot enumerate, and
		// a registered hit must never pay for it.
		var err error
		skill, ok, err = s.config.Skills.resolve(ctx, p.URI)
		if err != nil {
			return nil, internalErr(err)
		}
		if ok {
			if err := validateSkillEntry(skill, p.URI); err != nil {
				return nil, internalErr(fmt.Errorf("skill resolver returned an invalid entry: %w", err))
			}
		}
	}
	if !ok {
		// -32602, the same code resources/read uses for an unknown resource. A URI that is
		// a perfectly good resource but not a skill lands here too: skill-ness is
		// established by this method, never by a URI's shape.
		return nil, invalidParamsErr("Unknown skill: %s", p.URI)
	}

	return &SkillsGetResult{
		CacheableResult: NewCacheableResult(s.listTTLMs, s.cacheScope()),
		Skill:           skill,
	}, nil
}

// ResourcesDirectoryReadResult is the result of resources/directory/read.
type ResourcesDirectoryReadResult struct {
	CacheableResult
	Resources  []Resource `json:"resources"`
	NextCursor string     `json:"nextCursor,omitempty"`
}

// handleResourcesDirectoryRead answers the extension's scoped directory listing. The method
// is gated behind the skills extension's directoryRead setting because that is where it is
// declared, but the method itself is scheme-agnostic and not skill-exclusive: it lists
// any directory in this server's resource namespace.
func (s *Server) handleResourcesDirectoryRead(ctx context.Context, params json.RawMessage) (Result, *transport.RPCError) {
	var p struct {
		URI    string `json:"uri"`
		Cursor string `json:"cursor"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, invalidParamsErr("invalid resources/directory/read params: %v", err)
	}
	if p.URI == "" {
		return nil, invalidParamsErr("resources/directory/read requires a uri")
	}

	children, ok := s.resourceRegistry.DirectoryChildren(p.URI)
	if !ok {
		return nil, invalidParamsErr("Not a directory resource: %s", p.URI)
	}

	page, next, err := paginate(children, p.Cursor, defaultPageSize)
	if err != nil {
		return nil, invalidParamsErr("invalid cursor")
	}

	return &ResourcesDirectoryReadResult{
		CacheableResult: NewCacheableResult(s.listTTLMs, s.cacheScope()),
		Resources:       page,
		NextCursor:      next,
	}, nil
}
