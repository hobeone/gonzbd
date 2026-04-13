# sabnzbd-go

A Go reimplementation of [SABnzbd](https://sabnzbd.org), the automated Usenet
binary newsreader.

This project tracks the functional behavior of the upstream Python SABnzbd
release while providing a single-binary, dependency-light Go implementation.
The reference Python source lives at `../sabnzbd/`. The functional
specification driving this rewrite is at
[`../sabnzbd/sabnzbd_spec.md`](../sabnzbd/sabnzbd_spec.md) and the phased
implementation plan is at
[`../sabnzbd/golang_implementation.md`](../sabnzbd/golang_implementation.md).

## Status

Early development. See `golang_implementation.md` for the current phase and
step.

## Requirements

- Go 1.22 or later
- (For full lint runs) [`golangci-lint`](https://golangci-lint.run/) v2.0+

## Build

```bash
go build ./cmd/sabnzbd
```

To produce a versioned build:

```bash
go build -ldflags "-X main.Version=$(git describe --tags --always --dirty)" ./cmd/sabnzbd
```

## Run

```bash
./sabnzbd --version    # print version and exit
./sabnzbd              # start the daemon (no-op at this stage)
```

## Test

```bash
go test ./...                  # run all unit tests
go test -race ./...            # run unit tests with the race detector
go test -run TestFoo ./internal/nzb/   # run a single test
go test -bench=. ./internal/decoder/   # run benchmarks for one package
go test -tags=integration ./test/...   # run integration tests (require build tag)
```

## Lint and static analysis

```bash
go vet ./...
golangci-lint run ./...
```

These checks must pass before each commit. See `CLAUDE.md` for the full
quality-gate policy.

## Repository layout

```
cmd/sabnzbd/        Main binary entry point
internal/           Internal packages (added per implementation steps)
test/integration/   Integration tests gated by //go:build integration
test/fixtures/      Test fixtures (NZB files, sample configs, etc.)
```

No `Makefile` is provided; standard `go` tooling is the supported build
interface.

## License

GPL-2.0 or later, matching upstream SABnzbd.
