module github.com/oliver-kriska/scribe

// Single source of truth for the Go version. 1.26.6 carried the last stdlib
// security fixes scribe's call graph reaches (GO-2026-6090 crypto/tls,
// GO-2026-5972 asn1); 1.26.7 and 1.26.8 add net/http, cgo, compiler and
// runtime bug fixes, and scribe is a cgo build. CI reads this via setup-go's
// `go-version-file: go.mod`; GOTOOLCHAIN=auto fetches it for local and
// release-container builds. Bump here and everything follows.
go 1.26.9

require (
	github.com/alecthomas/kong v1.16.1
	github.com/mattn/go-sqlite3 v1.14.52
	gopkg.in/yaml.v3 v3.0.1
)

require (
	github.com/JohannesKaufmann/html-to-markdown/v2 v2.5.2
	github.com/fsnotify/fsnotify v1.10.1
	github.com/ledongthuc/pdf v0.0.0-20260907135840-6c8c28e0e8a0
	golang.org/x/sync v0.24.0
)

require (
	github.com/JohannesKaufmann/dom v0.3.1 // indirect
	golang.org/x/net v0.60.0 // indirect
	golang.org/x/sys v0.48.0 // indirect
)
