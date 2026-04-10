# Security policy

## Supported versions

Rein is in alpha. Only the latest release on the `main` branch is supported while we stabilise the API.

## Reporting a vulnerability

Please do not open a public issue for security reports.

Email `hello@archilea.com` with:

- a description of the vulnerability
- steps to reproduce
- the version or commit affected
- any proof-of-concept code (optional)

We aim to acknowledge within 48 hours and provide a fix or mitigation within 14 days for confirmed vulnerabilities. We will credit reporters in the release notes unless you prefer to remain anonymous.

## Out of scope

- Denial of service from unauthenticated traffic to admin endpoints. Admin endpoints must never be exposed without authentication.
- Third-party dependencies. Please report those to the upstream project.
