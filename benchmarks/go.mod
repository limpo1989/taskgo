module github.com/limpo1989/taskgo/benchmarks

go 1.18

require (
	github.com/limpo1989/taskgo v0.0.0
	github.com/lxzan/concurrency v1.4.3
)

require github.com/lxzan/dao v1.1.12 // indirect

// Use the in-tree library rather than a published version, so the benchmarks
// always run against the local source.
replace github.com/limpo1989/taskgo => ../
