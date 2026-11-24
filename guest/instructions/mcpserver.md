
## {{.Title}}

{{.Title}}'s hosted MCP server is at `${{.Env}}` ({{.URL}}). The broker
adds the user's credential on the Mac. There's none in the sandbox, and
none is needed.

- Call it from the shell with `mcpcli`, never as native MCP tools, with
  `MCP_GATEWAY_URL` set to it. Send results to a file and read what you
  need with `jq`, `head` or `grep`:

      MCP_GATEWAY_URL=${{.Env}} mcpcli tools [substring]
      MCP_GATEWAY_URL=${{.Env}} mcpcli describe <tool>
      MCP_GATEWAY_URL=${{.Env}} mcpcli call <tool> '<json-args>' >out.json

- Only these tools are served, all of them reads: {{.Tools}}. Others are
  refused, and so are URL parameters.
- A refusal from the broker, or an access denied from {{.Title}}, is the
  user's policy: report it, don't look for another way in.
