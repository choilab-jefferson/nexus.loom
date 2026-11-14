# Contributing

Issues are welcome. Pull requests are not merged on GitHub: development happens in a
private GitLab repository, and this repository receives one squashed commit per release.

## Reporting a bug

1. Check the open and closed [issues](https://github.com/choilab-jefferson/nexus.loom/issues).
2. Open a new issue: loom version (`loom version`), Go version and OS, the smallest example
   that reproduces it, and the output or panic trace.
3. Remove API keys, tokens, private paths and any patient data from what you paste.

## Feature requests and questions

Open an issue and describe the use case. A patch is welcome as a diff attached to the issue; a
maintainer may port it to the private repository, with credit.

## Security problems

Do not file a public issue: see [SECURITY.md](SECURITY.md).

## Running the tests

    make test          # CGO_ENABLED=1 go test -v -race ./...
    make test-nocgo    # fallback without a C toolchain

## License

By contributing an idea or patch you agree it may be used under the project's MIT license.
