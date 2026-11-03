# Contributing

Contributions are welcome through pull requests.

Before submitting a pull request:

1. Read and accept [CLA.md](CLA.md).
2. Keep changes focused and preserve the existing public API conventions.
3. Run `gofmt -w` on changed Go files.
4. Run `go vet ./...` and `go test -race ./...`.
5. Update documentation and tests for behavior changes.

The CLA Assistant workflow checks CLA acceptance on pull requests. Corporate
contributors must ensure that the submitting contributor is authorized to bind
the relevant entity. Repository administrators must configure the
`CLA_ASSISTANT_TOKEN` secret for the workflow to record signatures.
