
## GitHub

- Use `git` and `gh` as usual: the broker handles {{.Host}}, and `gh`
  is already configured.
- Push only to branches under `agent/{{.User}}/`, e.g. `agent/{{.User}}/fix-timeout`.
  Pushes to any other branch, and push options, are refused. Then open a
  pull request: `gh pr create --head agent/{{.User}}/<branch> --base <target>`.
- Only pull requests from `agent/{{.User}}/` branches can be edited, closed or
  commented on; for a stack, target the branch below.
- Never merge, approve or set auto-merge: a human does that.
{{if .Owners}}- Your credential reaches only the repositories of {{.Owners}}. Other
  owners' public repositories can be cloned and read (REST, `gh api
  repos/…`) anonymously, but not written, and GraphQL (most `gh`
  commands) works only on {{.Owners}}.
{{end}}- Run `gh` non-interactively: pass `--title` and `--body` (or
  `--fill`), and `-R <owner>/<repo>` when outside a clone.
- CI: `gh pr checks`, `gh run view --log-failed`. For anything else,
  `gh api` (REST) or `gh api graphql`. Writing files, refs or releases
  through the API is refused: push instead.
