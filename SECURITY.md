# Security policy

## Supported versions

Rein is in alpha. Only the latest release on the `main` branch is supported while we stabilise the API.

## Reporting a vulnerability

Please do not open a public issue for security reports.

Email `security@archilea.com` with:

- a description of the vulnerability
- steps to reproduce
- the version or commit affected
- any proof-of-concept code (optional)

We aim to acknowledge within 48 hours and provide a fix or mitigation within 14 days for confirmed vulnerabilities. We will credit reporters in the release notes unless you prefer to remain anonymous.

## Out of scope

- Denial of service from unauthenticated traffic to admin endpoints. Admin endpoints must never be exposed without authentication.
- Third-party dependencies. Please report those to the upstream project.

## Image signing

Every tagged release is signed with [cosign](https://docs.sigstore.dev/cosign/overview/) using a committed public key at `cosign.pub` in the repo root. Signing happens inside the GitHub Actions release pipeline (`.github/workflows/release.yml`) only after tests, govulncheck, and hadolint pass. The private key is held exclusively in the `COSIGN_PRIVATE_KEY` GitHub Actions secret, password-protected with `COSIGN_PASSWORD`, and is never exposed to pull requests, forks, or non-tag workflow runs.

Images are signed by canonical digest once per release, so every tag (`0.1.1`, `0.1`, `latest`) that resolves to the same digest passes the same verification. See the [Verifying the image](./README.md#verifying-the-image) section of the README for the `cosign verify` command.

### Key rotation

If the signing key is ever compromised, or on a scheduled rotation:

1. Generate a new keypair with `cosign generate-key-pair` (interactively set a new password).
2. Update the `COSIGN_PRIVATE_KEY` and `COSIGN_PASSWORD` GitHub Actions secrets with the new values.
3. Commit the new `cosign.pub` to `main` in a single commit with a rotation note.
4. Cut a new release tag. All releases from that tag forward are signed with the new key.
5. Historical releases remain verifiable by pinning the verify command to the tag that matches the key in effect at release time (`--key https://raw.githubusercontent.com/archilea/rein/<tag>/cosign.pub`).
6. If the rotation is due to suspected compromise, yank and re-tag the affected releases, and publish an advisory referencing the commit that rotated the key.
