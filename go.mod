module github.com/apteva/server

go 1.25.1

require (
	golang.org/x/crypto v0.49.0
	modernc.org/sqlite v1.47.0
)

require gopkg.in/yaml.v3 v3.0.1 // indirect

require (
	github.com/apteva/app-sdk v0.1.0
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/gorilla/websocket v1.5.3
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	golang.org/x/sys v0.42.0 // indirect
	modernc.org/libc v1.70.0 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.11.0 // indirect
)

// Local sibling — the monorepo carries app-sdk one directory up.
// build-local.sh runs `go build` inside server/ so ../app-sdk
// resolves naturally; the Docker build copies app-sdk next to server/
// before `go build` for the same reason. Drop this replace once we
// cut a tagged SDK release that includes the new `kind: static`
// runtime variant + UIApp.MountPath.
replace github.com/apteva/app-sdk => ../app-sdk
