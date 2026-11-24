
## AWS

The AWS MCP Server is at `$MCP_GATEWAY_URL` ({{.URL}}). The
broker signs each call on the Mac with the user's AWS profile
`{{.Profile}}`. There are no AWS credentials or CLI logins in the
sandbox, and none are needed.

- Call it from the shell with `mcpcli`, never as native MCP tools. Send
  results to a file and read what you need with `jq`, `head` or `grep`:

      mcpcli tools [substring]              # tool names
      mcpcli describe <tool>                # description and input schema
      mcpcli call <tool> '<json-args>' >out.json
      mcpcli py '<python>' | @file.py       # aws___run_script, in AWS's sandbox
      mcpcli logs <minutes> '<query>' --session <id> --group <log-group>

- `aws___run_script` is the tool that reaches AWS. Its Python can't import
  boto3: it calls `await call_boto3(service_name=..., operation_name=...,
  params={...})` and ends with `result = ...` then `result`. Read
  `mcpcli describe aws___run_script` before the first script.
- CloudWatch Logs Insights queries cost money per GB scanned. Run them only
  with `mcpcli logs`, which estimates first and enforces spend budgets
  (`mcpcli spend --session <id>`), with your session's id; raise a budget
  only when the user says so.
- `curl` works too, without a session, for one-off calls:
  `curl -s $MCP_GATEWAY_URL -H 'Content-Type: application/json' -d '{"jsonrpc":"2.0","id":1,"method":"tools/list"}'`
- An access denied from AWS is the role's: report it, don't look for
  another way in.
{{- if .Others}}
- Other profiles: {{.Others}}. Pick one per call with the tools'
  `aws_profile` argument (`mcpcli py --profile <p>`).
{{- end}}
