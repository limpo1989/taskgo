# taskgo
taskgo is a lightweight task pool in Go

## Install
`go get github.com/limpo1989/taskgo@latest`

## API Overview
```
type TaskExecutor[T any] interface {
	Exec(task T)
	Cancel(timeout time.Duration)
}

func NewTaskExecutor[T any](parent context.Context, concurrency int32, executor func(ctx context.Context, task T)) TaskExecutor[T]
func NewActionExecutor(parent context.Context, concurrency int32) TaskExecutor[func()]
```

## Example
```go
package main

import (
	"context"
	"sync"

	"github.com/limpo1989/taskgo"
)

func fib(n int) int {
	switch n {
	case 0, 1:
		return n
	default:
		return fib(n-1) + fib(n-2)
	}
}

func main() {
	ae := taskgo.NewActionExecutor(context.Background(), 16)

	const M = 10000
	const N = 20

	wg := &sync.WaitGroup{}
	wg.Add(M)

	for j := 0; j < M; j++ {
		ae.Exec(func() {
			fib(N)
			wg.Done()
		})
	}

	wg.Wait()
}

```