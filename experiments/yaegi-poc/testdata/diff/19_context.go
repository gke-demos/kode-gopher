// context: cancellation propagated to goroutines, values, deadline errors.
package main

import (
	"context"
	"errors"
	"fmt"
	"time"
)

type key string

func poll(ctx context.Context, out chan<- string) {
	for i := 0; ; i++ {
		select {
		case <-ctx.Done():
			out <- fmt.Sprintf("stopped: %v", ctx.Err())
			return
		case <-time.After(time.Millisecond):
		}
	}
}

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	out := make(chan string)
	go poll(ctx, out)
	time.Sleep(5 * time.Millisecond)
	cancel()
	fmt.Println(<-out)

	tctx, tcancel := context.WithTimeout(context.Background(), 2*time.Millisecond)
	defer tcancel()
	<-tctx.Done()
	fmt.Println(errors.Is(tctx.Err(), context.DeadlineExceeded))

	vctx := context.WithValue(context.Background(), key("project"), "gke-demos")
	fmt.Println(vctx.Value(key("project")), vctx.Value(key("missing")))
}
