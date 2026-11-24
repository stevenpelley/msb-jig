# Enforcing the harness: what we have to trust

The harness in `design.md` is elective: a developer can still run an agent outside it, with their own credentials. These notes ask what could make the harness *required*, and what each enforcement point makes us trust. Written 2026-11-22 from reasoning, not experiments. Items marked *unverified* haven't been checked against a source or tested.

## Threats

| | Threat | Who stops it |
|---|---|---|
| T1 | An agent inside the harness goes past its rules (prompt injection, bad judgement) | Sandbox and broker. This is what the mono-jig design already does. |
| T2 | A user runs an agent **outside** the harness with their own credentials, because it's easier | Needs a chokepoint the user can't route around (below) |
| T3 | A user hands a credential (a PAT, a pasted token, a file holding one) to an agent **inside** the harness | Out of scope, like T4. It's deliberate, and we don't try to stop it (decided 2026-11-22). |
| T4 | A deliberate insider writes a script with a PAT | Out of scope. It isn't an agent problem; existing insider controls and audit cover it. |

**The realistic goal for T2:** getting around the harness takes a deliberate, visible act, such as breaking policy or tampering with signed software, instead of happening by accident or for convenience. On a Mac where the user is an admin, that's the strongest guarantee any design can give.

## Where the chokepoint can go

Every agent needs three things: a model, a place to run, and provider credentials. Enforcement means controlling one of them so the user can't supply it themselves.

- **Provider credentials: no.** The user legitimately holds them and should keep using them. The provider can't tell a human's request from an agent's when both use the same token.
- **The place to run: no.** It's the user's Mac. They can run anything.
- **The model: yes.** It's the only ingredient the org already controls and that users don't own personally, at least not a corporate one. **Put enforcement on model access, not on the providers.** As a bonus, it needs nothing from the providers.

## The chain of trust

```
model gateway ──accepts only──▶ short-lived credential minted for an attested broker
broker        ──injects it only into──▶ sandboxes it launched (approved image + policy)
sandbox       ──egress only via──▶ broker (model + provider proxies) and the domain filter
```

| Component | Must provide | Trust basis |
|---|---|---|
| **Model gateway** (org-run) | Refuse any request without a broker-minted, short-lived credential; record which broker, device and sandbox image it came from | It's ours |
| **Device (Mac + MDM)** | A non-exportable Secure Enclave key, usable only by the org-signed broker binary; attestation that the device is managed and SIP is on (Apple Managed Device Attestation; which fields it covers is *unverified*) | Apple and MDM. This is the root of trust. Everything above it is only as strong as "the user didn't disable SIP or tamper with a signed binary". |
| **Broker** (ours) | Prove possession of the key to get gateway credentials; run only inside sandboxes the harness launched (the CLI starts the VM with `investigator-net` as its only network); report the image digest and policy hash; add the model credential to the sandbox's outbound traffic, never expose it inside | Code signing plus MDM-managed config |
| **Sandbox runtime** | Keep the agent away from host credentials and the model credential; allow egress only to the broker and the domain filter. Filtered hosts get anonymous access only, so the model credential must never be reachable through them | Measured, versioned runtime. Relied on for confinement, not identity. |
| **Agent software** | Nothing for enforcement. Any client can be pointed at the gateway, which is why the gateway checks the broker, not the agent. | — |
| **IDE connection** (VS Code or Kiro over SSH, added 2026-11-22) | Pass nothing from the Mac into the sandbox: no SSH agent forwarding, no git credential bridging, no editor sign-in tokens | Settings we ship. A user could change them, but only deliberately. |
| **Provider** | Nothing | — |

**What the chain doesn't try to stop.** A user can paste a token into the sandbox, or run an agent with a personal model subscription. Both are deliberate acts, and the harness doesn't try to block them (decided 2026-11-22): a developer with admin rights on their own Mac can always get around it. The goal is that the harness is the easy path, and that getting around it takes intent. Without credentials of its own, an agent reaching a provider host directly gets only anonymous access.

### What's left open

- **A personal model subscription or a local model, used with corporate credentials.** Corporate SASE can block model APIs except through the gateway, but in the end it's a policy question. That's acceptable: it's a deliberate act.
- **Agents whose vendor runs the backend.** Kiro authenticates to AWS's service, not to a gateway of ours, so we can't add attestation checks there. The options are routing its backend traffic through a central egress proxy and IP-restricting the vendor or IdP side, or relying on the vendor's enterprise controls. **Research this first:** it decides whether "require the harness" is possible with the editor work provides.
  - **Lead (2026-11-22):** Kiro's service APIs can be reached through an AWS PrivateLink interface endpoint in our VPC (`com.amazonaws.us-east-1.q`, which with private DNS also serves `runtime.`, `management.` and `telemetry.us-east-1.kiro.dev`). Kiro's docs also mention AWS data perimeters. If our Kiro profile could be made to *require* that endpoint (for example an `aws:SourceVpce` condition), then a central egress that only attested brokers can use becomes the org-controlled model path. Whether such a condition is possible is *unverified*; the docs only describe reaching Kiro privately, not restricting it.
  - **Since Kiro 1.5 the agent runs inside the sandbox** when the IDE connects over SSH (`design.md`). The model traffic to gate therefore leaves from the sandbox, through the broker, which is the shape the chain above needs. The IDE connection is a new component we rely on not to pass credentials into the sandbox.
- **Copying the session token out of a sandbox.** Short lifetimes plus binding the token to the broker's key (DPoP-style) make a copied token useless.

## Network-level enforcement

Route all provider access through a proxy and have providers accept only the proxy's IPs. The catch: **human** traffic would have to go through it too. GitHub and Datadog apply IP allowlists to the whole org, not per credential. That breaks "the user keeps their credentials for other uses", or turns the proxy into everyone's corporate egress, where it can't tell agents from humans.

- **AWS is the exception and already works.** IAM conditions (`aws:SourceIp`, `aws:SourceVpce`, session tags) apply per role, so the MCP-specific roles can be limited to a central proxy. It still doesn't stop an unharnessed agent from using the human's own roles. Model gating remains the real enforcement; network binding only strengthens it.
- **GitHub's human side can be narrowed today:** block classic PATs, require approval for fine-grained PATs, enforce SSO. "Create a PAT and hand it to an agent" becomes an approval-gated, audited act.

## Policy hooks at the provider

Provider-side Cedar, AuthZEN or wasm hooks are the most powerful option and the least likely. The provider would put a customer's endpoint on the path of every request: added latency, its availability tied to ours, request internals exposed. It also competes with their own permissions products. GitHub Enterprise Server's pre-receive hooks are the nearest precedent, and GitHub Enterprise Cloud doesn't offer them. Keep these hooks in **our** proxy, where the design already puts them.

## Small provider features worth asking for

Ranked by how likely a provider is to agree:

1. **Per-credential sender binding:** "this token is valid only from these CIDRs", or only with this mTLS or DPoP key (RFC 8705, RFC 9449). It's a standard, it only adds security, and it doesn't threaten their product. With it, agent credentials work only through a central proxy, so the provider enforces the path even if a broker on someone's Mac is tampered with. **The single most valuable ask.**
2. **A delegation marker on tokens and in audit logs:** "issued to app X, acting for user Y" (the `act` claim from RFC 8693), with permissions and rulesets able to condition on it, e.g. "tokens via app X can't merge". GitHub App user-to-server tokens already go halfway: their permissions are the intersection of the user's and the app's, and actions are logged "via app". What's missing is conditioning finer than repository level. AWS has a close analogue already: `sts:SourceIdentity` and session tags persist through role chaining and show up in CloudTrail.
3. **Org policy restricting which clients may hold API tokens,** so only approved OAuth or GitHub Apps can. GitHub's OAuth app access restrictions come close, and paired with a PAT ban they approach "the broker's app is the only API path". The cost lands on humans: the `gh` CLI's own OAuth app would also need approval, or would have to go through the broker.

## Summary

- **Trusted:** the org's model gateway, device attestation via MDM, our broker, the sandbox's confinement.
- **Not trusted:** the user's diligence, the agent software, the provider.
- **Needed from providers:** nothing for the main path. The one ask worth making is per-credential sender binding.

## Open questions

- Can Kiro, or whichever agent the team uses, be forced through a model path the org controls (our gateway, an egress proxy, or vendor or IdP IP restrictions)?
- Which fields does Apple Managed Device Attestation cover on macOS (SIP, Secure Boot), and can it attest a Secure Enclave key bound to a specific signed binary?
- Can corporate SASE block direct model APIs on work Macs without breaking Kiro?
- Does any of GitHub, Datadog or Wiz support per-token IP or sender binding today? Can a GitHub App's own IP allowlist restrict that app's tokens, rather than only being added to the org's allowlist?
- How much does blocking classic PATs and requiring approval for fine-grained ones cost the team's current workflows?

## Sources

- [RFC 8693: OAuth 2.0 Token Exchange](https://www.rfc-editor.org/rfc/rfc8693)
- [RFC 8705: OAuth 2.0 Mutual-TLS Client Authentication and Certificate-Bound Access Tokens](https://www.rfc-editor.org/rfc/rfc8705)
- [RFC 9449: OAuth 2.0 Demonstrating Proof of Possession (DPoP)](https://www.rfc-editor.org/rfc/rfc9449)
- [OpenID AuthZEN Authorization API](https://openid.net/wg/authzen/)
