**What this changes**

**Checklist**

- [ ] `gofmt -l .` prints nothing
- [ ] `go vet ./...` and `go vet -tags omp ./...` pass
- [ ] `go test ./...` passes
- [ ] `go build -tags omp .` still compiles
- [ ] New behaviour has a test that fails without the change
- [ ] Comments and user-facing strings are in English
