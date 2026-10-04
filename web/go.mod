// Marks web/ as its own (empty) Go module so `go ./...` and golangci-lint
// never descend into web/node_modules, which can contain stray .go files.
module github.com/vincent/tarea/web

go 1.22
