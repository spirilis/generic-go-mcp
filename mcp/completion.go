package mcp

import (
	"context"
	"encoding/json"

	"github.com/spirilis/generic-go-mcp/transport"
)

// maxCompletionValues is the ceiling the specification places on one completion/complete
// response. A provider returning more is truncated and told so via hasMore.
const maxCompletionValues = 100

// CompletionRequest is one completion/complete call against a resource template variable.
type CompletionRequest struct {
	// URITemplate is the template being completed — the "uri" of the request's
	// ref/resource, which is the uriTemplate string exactly as registered.
	URITemplate string
	// Argument is the name of the template variable the client is completing.
	Argument string
	// Value is the partial input typed so far. It may be empty, which means "everything you
	// have" (still subject to the 100-value cap).
	Value string
	// Context carries template variables the client has already resolved, from the
	// request's context.arguments. Use it to constrain later variables — completing a pod
	// name is a different query once the namespace is known.
	Context map[string]string
}

// CompletionResult is what a CompletionProvider returns.
type CompletionResult struct {
	// Values are the candidate completions. The server truncates this to 100 before it
	// reaches the wire.
	Values []string
	// Total, if set, is the true number of matches, which may exceed len(Values).
	Total *int
	// HasMore says more matches exist than Values carries. The server also sets this on its
	// own if it had to truncate.
	HasMore bool
}

// CompletionProvider resolves candidate values for one variable of a resource template.
//
// Completion is entirely optional and entirely the embedder's business: templates describe
// the shape of a keyspace, and only the thing behind the template knows what is actually in
// it. A server that registers no provider does not advertise the completions capability and
// answers completion/complete with -32601, which is exactly what clients probe for.
//
// Back an implementation with a prefix index or a paged upstream query. Never materialize
// the keyspace — the reason templates exist is that it does not fit.
type CompletionProvider interface {
	Complete(ctx context.Context, req *CompletionRequest) (CompletionResult, error)
}

// CompletionFunc adapts a plain function to CompletionProvider, in the http.HandlerFunc
// idiom.
type CompletionFunc func(ctx context.Context, req *CompletionRequest) (CompletionResult, error)

// Complete implements CompletionProvider.
func (f CompletionFunc) Complete(ctx context.Context, req *CompletionRequest) (CompletionResult, error) {
	return f(ctx, req)
}

// CompletionValues is the "completion" object of a completion/complete result.
type CompletionValues struct {
	Values  []string `json:"values"`
	Total   *int     `json:"total,omitempty"`
	HasMore bool     `json:"hasMore,omitempty"`
}

// CompletionCompleteResult is the result of completion/complete.
//
// It embeds BaseResult rather than CacheableResult: completion/complete is not among the
// operations the specification requires to carry caching hints (see CacheableResult's doc
// comment), and a completion list is the last thing a client should be caching anyway.
type CompletionCompleteResult struct {
	BaseResult
	Completion CompletionValues `json:"completion"`
}

type completionRefParams struct {
	Ref struct {
		Type string `json:"type"`
		URI  string `json:"uri"`
		Name string `json:"name"`
	} `json:"ref"`
	Argument struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	} `json:"argument"`
	Context struct {
		Arguments map[string]string `json:"arguments"`
	} `json:"context"`
}

// handleCompletionComplete implements completion/complete for resource template variables.
//
// This is only reached when at least one registered template has a completion provider —
// otherwise the method is absent from the router entirely and the client gets -32601. See
// ResourceRegistry.SetTemplateCompleter.
func (s *Server) handleCompletionComplete(ctx context.Context, params json.RawMessage) (Result, *transport.RPCError) {
	var p completionRefParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, invalidParamsErr("invalid completion/complete params: %v", err)
	}

	// ref/prompt is the other reference type in the spec; prompts are not implemented by
	// this library at all, so there is nothing honest to complete against.
	if p.Ref.Type != "ref/resource" {
		return nil, invalidParamsErr("unsupported completion ref type %q: only \"ref/resource\" is supported", p.Ref.Type)
	}
	if p.Argument.Name == "" {
		return nil, invalidParamsErr("completion/complete requires argument.name")
	}

	tc, ok := s.resourceRegistry.templateCompletion(p.Ref.URI)
	if !ok {
		return nil, invalidParamsErr("Unknown resource template: %s", p.Ref.URI)
	}
	if tc.provider == nil {
		return nil, invalidParamsErr("resource template %s does not support completion", p.Ref.URI)
	}
	if !tc.hasVar(p.Argument.Name) {
		return nil, invalidParamsErr("resource template %s has no variable %q", p.Ref.URI, p.Argument.Name)
	}

	res, err := tc.provider.Complete(ctx, &CompletionRequest{
		URITemplate: p.Ref.URI,
		Argument:    p.Argument.Name,
		Value:       p.Argument.Value,
		Context:     p.Context.Arguments,
	})
	if err != nil {
		return nil, internalErr(err)
	}

	values := res.Values
	hasMore := res.HasMore
	if len(values) > maxCompletionValues {
		values = values[:maxCompletionValues]
		hasMore = true
	}
	if values == nil {
		// "values" is required on the wire; an absent provider result is an empty list, not
		// a null.
		values = []string{}
	}

	return &CompletionCompleteResult{
		Completion: CompletionValues{
			Values:  values,
			Total:   res.Total,
			HasMore: hasMore,
		},
	}, nil
}
