module github.com/apteva/server

go 1.25.1

require (
	github.com/dop251/goja v0.0.0-20250630131328-58d95d85e994
	golang.org/x/crypto v0.49.0
	modernc.org/sqlite v1.50.0
)

require gopkg.in/yaml.v3 v3.0.1

require github.com/decred/dcrd/dcrec/secp256k1/v4 v4.4.1

require (
	github.com/dlclark/regexp2 v1.11.4 // indirect
	github.com/go-sourcemap/sourcemap v2.1.3+incompatible // indirect
	github.com/google/pprof v0.0.0-20250317173921-a4b03ec1a45e // indirect
	golang.org/x/net v0.51.0 // indirect
	golang.org/x/text v0.35.0 // indirect
)

require (
	github.com/apteva/app-sdk v0.44.0
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/gorilla/websocket v1.5.3
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	golang.org/x/sys v0.42.0 // indirect
	modernc.org/libc v1.72.0 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.11.0 // indirect
)

// Local sibling — the monorepo carries app-sdk one directory up.
// build-local.sh runs `go build` inside server/ so ../app-sdk
// resolves naturally; the Docker build copies app-sdk next to server/
// before `go build` for the same reason. The standalone GitHub release
// workflow drops this replacement and builds against the pinned SDK tag.
replace github.com/apteva/app-sdk => ../app-sdk
