# Working in the mono-jig sandbox

The mono-jig broker, on the user's Mac, holds the credentials for the
services below and enforces their rules. Its refusals come from
"mono-jig broker" and say why. Don't log in, and don't ask for tokens.
Other hosts go through a domain filter; if one is blocked, ask the user to
allow it rather than working around it.
