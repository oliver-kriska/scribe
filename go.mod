module github.com/oliver-kriska/scribe

// Single source of truth for the Go version. 1.26.5 carries the stdlib
// security fix for GO-2026-5856 (Encrypted Client Hello privacy leak in
// crypto/tls), on top of GO-2026-5039 (net/textproto) and GO-2026-5037
// (crypto/x509). CI reads this via setup-go's `go-version-file: go.mod`;
// GOTOOLCHAIN=auto fetches it for local and release-container builds. Bump
// here and everything follows.
go 1.26.5

require (
	github.com/alecthomas/kong v1.16.0
	github.com/mattn/go-sqlite3 v1.14.47
	gopkg.in/yaml.v3 v3.0.1
)

require (
	github.com/JohannesKaufmann/html-to-markdown/v2 v2.5.2
	github.com/fsnotify/fsnotify v1.10.1
	github.com/ledongthuc/pdf v0.0.0-20250511090121-5959a4027728
	golang.org/x/sync v0.22.0
)

require (
	github.com/JohannesKaufmann/dom v0.3.1 // indirect
	golang.org/x/net v0.57.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
)
