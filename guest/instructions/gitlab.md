
## GitLab

- Use `git` and `glab` as usual: the broker handles {{.Host}}, and
  `glab` is already configured.
- Push only to branches under `agent/{{.User}}/`, e.g. `agent/{{.User}}/fix-timeout`.
  Pushes to any other branch, and push options (`git push -o`), are refused.
- Only merge requests from `agent/{{.User}}/` branches can be opened, edited
  or commented on.
- Never merge, approve or set auto-merge: a human does that.
- Run `glab` non-interactively: pass `--yes`, titles and descriptions as
  flags, and `-R <group>/<project>` when outside a clone.
- For anything `glab` has no command for, use `glab api` (REST, or
  `glab api graphql` for queries). Save large responses to a file and read
  what you need with `jq`, `head` or `grep`.
