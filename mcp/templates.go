package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/spirilis/generic-go-mcp/transport"
)

// ResourceTemplate describes a family of resources whose URIs share an RFC 6570 template —
// the answer to an unbounded keyspace that resources/list could never enumerate (every pod
// in a cluster, every row in a table). The server advertises the template through
// resources/templates/list, the client expands it locally, and reads land on the ordinary
// resources/read method carrying a concrete URI.
//
// The field set mirrors Resource, minus Size: a template describes a shape, and the size of
// something that has not been named yet is not a thing that exists.
type ResourceTemplate struct {
	URITemplate string                 `json:"uriTemplate"`
	Name        string                 `json:"name"`
	Title       string                 `json:"title,omitempty"`
	Description string                 `json:"description,omitempty"`
	MimeType    string                 `json:"mimeType,omitempty"`
	Annotations *Annotations           `json:"annotations,omitempty"`
	Icons       []Icon                 `json:"icons,omitempty"`
	Meta        map[string]interface{} `json:"_meta,omitempty"`
}

// ResourceReadRequest is what a ResourceTemplateFunction receives on resources/read: the
// concrete URI the client asked for, plus the variable bindings extracted from it.
type ResourceReadRequest struct {
	// URI is the concrete URI the client sent, exactly as received (still percent-encoded).
	URI string
	// Vars maps each template variable to its percent-decoded value. Every variable in the
	// template is present — the URI could not have matched otherwise.
	Vars map[string]string
}

// ResourceTemplateFunction produces the content of one concrete member of a template
// family. An error surfaces to the client as JSON-RPC -32603, except for ErrResourceNotFound
// (and anything wrapping it), which becomes -32602 — see that variable.
type ResourceTemplateFunction func(ctx context.Context, req *ResourceReadRequest) (ResourceContentResult, error)

// ErrResourceNotFound is returned by a ResourceFunction or ResourceTemplateFunction to say
// "the URI is well-formed and mine, but no such resource exists". resources/read answers it
// with -32602 ("Unknown resource"), the same code an unregistered URI gets, rather than
// -32603.
//
// This matters mostly for templates: a template claims a whole URI shape, so
// "kube://prod/pod/default/does-not-exist" reaches the handler rather than failing to match.
// Only the handler knows the pod is not there, and reporting that as -32603 would tell the
// client the server is broken when it is merely being asked about something absent. Wrap it
// with %w to add detail:
//
//	return mcp.ResourceContentResult{}, fmt.Errorf("pod %q: %w", name, mcp.ErrResourceNotFound)
//
// Returning empty content instead is never right: it is indistinguishable from a resource
// that legitimately has none.
var ErrResourceNotFound = errors.New("resource not found")

// compiledTemplate is a registered ResourceTemplate plus everything derived from it at
// registration time: the anchored matcher, the capture-group-ordered variable names, the
// read function, and an optional completion provider.
type compiledTemplate struct {
	tmpl ResourceTemplate
	fn   ResourceTemplateFunction
	comp CompletionProvider // nil unless SetTemplateCompleter was called
	re   *regexp.Regexp
	vars []string // variable names, in capture-group order
}

// varNamePattern is RFC 6570's varchar restricted to what this matcher supports: no "%"
// pct-encoded triplets and no "." separators, both of which are legal in the RFC but buy
// nothing here and complicate the error messages.
var varNamePattern = regexp.MustCompile(`^[A-Za-z0-9_]+$`)

// compileURITemplate translates an RFC 6570 URI template into an anchored regular
// expression plus the ordered list of variable names its capture groups correspond to.
//
// RFC 6570 defines expansion only — template to URI. Matching a concrete URI back to a set
// of variable bindings is not in the RFC and has to be built. This implementation covers
// exactly two forms, and rejects everything else at registration time rather than failing
// mysteriously at read time:
//
//	{var}   simple string expansion. Matches one path segment: one or more characters, never
//	        a "/". This is RFC 6570 Level 1, which is all many MCP clients implement.
//	{+var}  reserved expansion. Matches one or more characters including "/", so it can span
//	        path segments. Permitted only as the final expression in the template — a greedy
//	        multi-segment match followed by more pattern is a shape whose bindings are not
//	        obvious to a human reading the template, and ambiguity in a URI-to-handler
//	        mapping is not worth the expressiveness.
//
// Everything else — {#frag}, {?query}, {&param}, {/path}, {.ext}, {;param}, {a,b},
// {var:3}, {var*} — is a registration error naming the offending expression.
func compileURITemplate(s string) (*regexp.Regexp, []string, error) {
	var (
		pattern strings.Builder
		literal strings.Builder
		vars    []string
	)
	pattern.WriteString("^")

	flushLiteral := func() {
		if literal.Len() > 0 {
			pattern.WriteString(regexp.QuoteMeta(literal.String()))
			literal.Reset()
		}
	}

	for i := 0; i < len(s); {
		switch s[i] {
		case '}':
			return nil, nil, fmt.Errorf("uri template %q: unbalanced %q at offset %d", s, "}", i)
		case '{':
			end := strings.IndexByte(s[i:], '}')
			if end < 0 {
				return nil, nil, fmt.Errorf("uri template %q: unterminated expression starting at offset %d", s, i)
			}
			expr := s[i+1 : i+end]
			if strings.ContainsRune(expr, '{') {
				return nil, nil, fmt.Errorf("uri template %q: nested %q inside expression {%s}", s, "{", expr)
			}
			name, isReserved, err := parseTemplateExpression(s, expr)
			if err != nil {
				return nil, nil, err
			}
			for _, seen := range vars {
				if seen == name {
					return nil, nil, fmt.Errorf("uri template %q: duplicate variable %q", s, name)
				}
			}

			flushLiteral()
			if isReserved {
				pattern.WriteString(`(.+)`)
			} else {
				pattern.WriteString(`([^/]+)`)
			}
			vars = append(vars, name)
			i += end + 1

			// Terminal-only, checked against the whole remainder rather than just the next
			// expression: a trailing literal ("{+path}/meta") is as much a violation as a
			// trailing variable ("{+path}/{name}").
			if isReserved && i != len(s) {
				return nil, nil, fmt.Errorf(
					"uri template %q: {+%s} reserved expansion must be the final expression; %q follows it",
					s, name, s[i:])
			}
		default:
			literal.WriteByte(s[i])
			i++
		}
	}
	flushLiteral()
	pattern.WriteString("$")

	if len(vars) == 0 {
		return nil, nil, fmt.Errorf("uri template %q: no variable expressions; register it as a concrete Resource instead", s)
	}

	re, err := regexp.Compile(pattern.String())
	if err != nil {
		// Unreachable in practice: every dynamic span is either QuoteMeta'd or a literal
		// group. Reported rather than panicked because a registration error is already the
		// contract here.
		return nil, nil, fmt.Errorf("uri template %q: %w", s, err)
	}
	return re, vars, nil
}

// parseTemplateExpression validates the inside of one {...} and reports its variable name
// and whether it used reserved ("+") expansion.
func parseTemplateExpression(tmpl, expr string) (name string, reserved bool, err error) {
	if expr == "" {
		return "", false, fmt.Errorf("uri template %q: empty expression {}", tmpl)
	}

	switch expr[0] {
	case '+':
		reserved = true
		name = expr[1:]
	case '#', '.', '/', ';', '?', '&', '=', ',', '!', '@', '|':
		return "", false, fmt.Errorf(
			"uri template %q: operator %q in {%s} is not supported; only {var} and a terminal {+var} are",
			tmpl, string(expr[0]), expr)
	default:
		name = expr
	}

	// Modifiers and multi-variable lists get their own messages: they are the forms someone
	// is most likely to reach for, and "invalid variable name" would not explain why.
	switch {
	case strings.Contains(name, ","):
		return "", false, fmt.Errorf(
			"uri template %q: multi-variable expression {%s} is not supported; use one variable per expression", tmpl, expr)
	case strings.HasSuffix(name, "*"):
		return "", false, fmt.Errorf(
			"uri template %q: explode modifier in {%s} is not supported", tmpl, expr)
	case strings.Contains(name, ":"):
		return "", false, fmt.Errorf(
			"uri template %q: prefix modifier in {%s} is not supported", tmpl, expr)
	}

	if !varNamePattern.MatchString(name) {
		return "", false, fmt.Errorf(
			"uri template %q: invalid variable name %q in {%s}; must be one or more of [A-Za-z0-9_]", tmpl, name, expr)
	}
	return name, reserved, nil
}

// match reports whether uri belongs to this template, and if so the percent-decoded
// variable bindings. A captured group that is not valid percent-encoding is treated as no
// match rather than an error, so a malformed URI falls through to the next template (and
// ultimately to "Unknown resource") instead of masking a template that would have matched.
func (c *compiledTemplate) match(uri string) (map[string]string, bool) {
	groups := c.re.FindStringSubmatch(uri)
	if groups == nil {
		return nil, false
	}
	vars := make(map[string]string, len(c.vars))
	for i, name := range c.vars {
		// PathUnescape, not QueryUnescape: "+" is a literal plus in a path, not a space.
		decoded, err := url.PathUnescape(groups[i+1])
		if err != nil {
			return nil, false
		}
		vars[name] = decoded
	}
	return vars, true
}

// hasVar reports whether name is one of this template's variables.
func (c *compiledTemplate) hasVar(name string) bool {
	for _, v := range c.vars {
		if v == name {
			return true
		}
	}
	return false
}

// ResourcesTemplatesListResult is the result of resources/templates/list.
type ResourcesTemplatesListResult struct {
	CacheableResult
	ResourceTemplates []ResourceTemplate `json:"resourceTemplates"`
	NextCursor        string             `json:"nextCursor,omitempty"`
}

// handleResourcesTemplatesList implements resources/templates/list.
//
// There is no "templates supported" capability flag in this revision — capabilities.resources
// carries only listChanged/subscribe — so clients discover template support by calling this
// method and treating -32601 as "not supported". This server always answers, returning an
// empty array when nothing is registered, exactly as resources/list does on an empty
// registry.
func (s *Server) handleResourcesTemplatesList(ctx context.Context, params json.RawMessage) (Result, *transport.RPCError) {
	var p struct {
		Cursor string `json:"cursor"`
	}
	if len(params) > 0 {
		_ = json.Unmarshal(params, &p)
	}

	all := s.resourceRegistry.ListTemplates()
	page, next, err := paginate(all, p.Cursor, defaultPageSize)
	if err != nil {
		return nil, invalidParamsErr("invalid cursor")
	}

	return &ResourcesTemplatesListResult{
		CacheableResult:   NewCacheableResult(s.listTTLMs, s.cacheScope()),
		ResourceTemplates: page,
		NextCursor:        next,
	}, nil
}
