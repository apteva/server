package main

import (
	"context"
	"sync"
)

func parallelJobs(ctx context.Context, n, workers int, fn func(int)) {
	jobs := make(chan int)
	var wg sync.WaitGroup
	for i := 0; i < min(n, workers); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				if ctx.Err() == nil {
					fn(j)
				}
			}
		}()
	}
	for i := 0; i < n; i++ {
		select {
		case jobs <- i:
		case <-ctx.Done():
		}
	}
	close(jobs)
	wg.Wait()
}
