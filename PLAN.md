Please reference files in the folder ../genome-vcf-mcp for the original Golang project inspiring this one.
Full path is `/home/spirilis/src/genome-vcf-mcp` for that.  Ask permission to Read files (only read) from there.

I want to extract the Golang MCP and OAuth/GitHub OAuth-connector implementation from the aforementioned `genome-vcf-mcp`
project and spin it out into its own Golang project for a generic MCP framework.

This project should provide an MCP server, accessible over stdio OR SSE, if SSE we're using GitHub OAuth authentication,
and it takes a config.yaml to configure this.

This should produce a reference MCP server with 2 functions:

date(timezone) - Provide the date for the given timezone specification e.g. `US/Eastern`, `Europe/Amsterdam`
fortune() - Execute the local machine's `fortune` CLI program and present its output as a response to this function call.

Produce a dockerfile in `deploy/docker/Dockerfile` and a generic helm chart in `deploy/docker/go-mcp-framework` that runs
the MCP in kubernetes in SSE mode, with both Ingress (and tls, with Cert-Manager) and Gateway API (only providing the
HTTPRoute resource, not the Gateway, and support the use of Cert-Manager there as well for TLS).
