# Contributing to Rein

Thanks for considering a contribution. Rein is built in the open and we welcome issues, pull requests, and ideas.

## Getting started

```bash
git clone https://github.com/archilea/rein.git
cd rein
make run
```

Rein requires Go 1.25 or newer (this is also the lowest version that still receives upstream security patches).

## Ways to contribute

- **Bug reports**. Use the bug report issue template. Include minimal reproduction steps and the Rein version.
- **Feature requests**. Use the feature request issue template. Describe the problem before the solution.
- **Pull requests**. Small focused PRs are the fastest to review.
- **Docs**. Typo fixes and clarifications in the docs folder are always welcome.

## Development flow

1. Fork the repository and create a branch from `main`.
2. Make your change. Add tests if it is a code change.
3. Run `make lint test` locally.
4. Open a pull request using the PR template.
5. A maintainer will review within a few days. We aim to respond within 72 hours on weekdays.

## Coding conventions

- Standard `gofmt` and `goimports` formatting.
- `golangci-lint run` must pass before merge.
- Package names are lowercase single words.
- Public APIs have GoDoc comments.
- No new third-party dependencies without discussion in an issue first.

## Commit messages

Rein uses [Conventional Commits](https://www.conventionalcommits.org). Examples:

- `feat(proxy): add anthropic streaming support`
- `fix(meter): correct input token rounding for gpt-4o`
- `docs(readme): clarify quickstart`

## License

By contributing you agree that your contributions will be licensed under the MIT License.
