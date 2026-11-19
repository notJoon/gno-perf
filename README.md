# gno-perf

## Description

A CLI tool for tracking GnoVM performance. Collect metrics, compare commits and pull requests, and bisect performance regressions.

## Development

Use Go 1.25.9 and set `gno_core` in `gnoperf.toml` to your local Gno checkout.

```sh
go test ./...
go build -o bin/gnoperf ./cmd/gnoperf
./bin/gnoperf collect HEAD
```
