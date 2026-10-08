# Contributing

Use Go 1.26.2 or newer and golangci-lint 2.11.4. Windows release resources
also require go-winres (`make deps-dev`).

```sh
make build
go vet ./...
go test -race ./...
make lint
```

The default suite uses temporary fixtures and local listeners. It needs no
model downloads, cloud credentials or lab hosts. Format changed Go files with
`gofmt`, add tests for changed behavior, and keep changes focused.

Submit a pull request explaining the problem, change and checks run.
Contributions are licensed under the project's [Apache 2.0 license](LICENSE).
Report vulnerabilities through [SECURITY.md](SECURITY.md).
