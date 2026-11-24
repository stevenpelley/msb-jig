
## npm

- npm and pnpm reach {{.Hosts}} through the broker, which holds the
  user's token: installs of private packages work with no token in the
  sandbox. Don't add one to `.npmrc`.
- Read-only: `install`, `view`, `outdated` and `audit` work; `publish`,
  `unpublish`, `deprecate`, `dist-tag`, `access`, `owner`, `login` and
  `token` are refused.
