# Security policy

## Reporting a vulnerability

Please report vulnerabilities privately through GitHub:
**[Report a vulnerability](https://github.com/SkillfulAgents/tabtender/security/advisories/new)**.
Do not open a public issue for a security problem.

Include what you found, how to reproduce it, and the version (`tabtenderd
version`) and platform. You will get an acknowledgement within a few working
days. Tabtender is in beta; fixes ship in the next release, and the advisory
is published with it.

## What Tabtender protects, and what it does not

Tabtender puts native browsers behind one authenticated endpoint. Reports are
most useful when they are judged against what it sets out to guarantee:

- Nothing is served without a credential: the API key, or — for one session's
  CDP endpoint only — that session's token.
- A session token is accepted nowhere but on its own session's CDP endpoint.
- Browsers expose no debugging port. Nothing on the machine reaches a
  session's browser except through the authenticated endpoint.
- Secrets (API key, session tokens, tunnel token) are never logged and never
  passed to helper processes.
- The management API is reachable only by the operating-system user the
  service runs as.

By design, it does **not** sandbox the browsers it runs. Whoever controls a
session over CDP controls a real browser running as the service's user, and
can read and write that user's files through it. Sessions are not a security
boundary between mutually untrusted parties. See the
[Security section of the README](README.md#security).
