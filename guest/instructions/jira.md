
## Jira

Jira Cloud ({{.Site}}) is at `$JIRA_URL` ({{.URL}}), read-only, as the
user {{.Email}}. The broker adds the user's token on the Mac. There's
none in the sandbox, and none is needed: send no `Authorization`.

- Use `curl` against the REST API, v3 (`/rest/api/3/…`) or Jira
  Software's (`/rest/agile/1.0/…`). Send results to a file and read what
  you need with `jq`:

      curl -s "$JIRA_URL/rest/api/3/issue/KEY-1?fields=summary,status" >issue.json
      curl -s "$JIRA_URL/rest/api/3/search/jql" -H 'Content-Type: application/json' \
        -d '{"jql":"project = KEY ORDER BY updated DESC","fields":["summary","status"],"maxResults":20}' >search.json

- Search is `POST /rest/api/3/search/jql`; the old `/search` is gone.
  Descriptions and comments are in Atlassian Document Format (JSON).
- Reads only: GETs, and the POSTs that read (searches, bulk fetches, JQL
  and expression checks). Creating, editing, transitioning, commenting
  and deleting are refused.
- A link in a response that points at api.atlassian.com/ex/jira/… isn't
  reachable from the sandbox: call the same `/rest/…` path under
  `$JIRA_URL` instead.
  Issue links for the user are https://{{.Site}}/browse/KEY-1.
- A refusal from the broker, or a 401 or 403 from Jira (the token's
  scopes), is the user's policy: report it, don't look for another way
  in.
