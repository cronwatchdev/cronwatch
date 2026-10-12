// Examples of CronWatch in Go programs, a module of its own so the SQLite
// driver they use is never a requirement of the cronwatch module. Never
// tagged or imported.
module cronwatch.dev/go/examples

go 1.26.0

require (
	cronwatch.dev/go v0.0.0
	modernc.org/sqlite v1.59.0
)

require (
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/mattn/go-isatty v0.0.24 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	golang.org/x/mod v0.40.0 // indirect
	golang.org/x/sys v0.49.0 // indirect
	golang.org/x/tools v0.49.0 // indirect
	modernc.org/libc v1.75.7 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.12.1 // indirect
)

replace cronwatch.dev/go => ../

// Pin unused transitive tooling in this unpublished module to patched releases.
replace (
	github.com/yuin/goldmark => github.com/yuin/goldmark v1.7.17
	golang.org/x/crypto => golang.org/x/crypto v0.58.0
	golang.org/x/net => golang.org/x/net v0.60.0
)
