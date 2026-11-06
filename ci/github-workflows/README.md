# GitHub Actions workflows

These files belong in `.github/workflows/`. They are kept here because the
token used to push this repository has no `workflow` scope.

To activate them, move both files to `.github/workflows/` (for example in the
GitHub web UI or with a token that has the `workflow` scope).

- `ci.yml`: gofmt, go vet, go test and a multi-arch image build without push.
- `release.yml`: on a `v*` tag, tests and pushes the image to
  `ghcr.io/steffenbarthel/external-dns-connbyte-webhook`.
