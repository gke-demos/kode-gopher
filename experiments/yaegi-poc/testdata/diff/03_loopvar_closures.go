// Go 1.22+ per-iteration loop variables: closures and goroutines capturing
// the loop variable each see their own copy.
package main

import (
	"fmt"
	"sort"
	"sync"
)

func main() {
	var fns []func() int
	for i := 0; i < 3; i++ {
		fns = append(fns, func() int { return i })
	}
	for _, f := range fns {
		fmt.Print(f(), " ")
	}
	fmt.Println()

	var ptrs []*string
	for _, s := range []string{"a", "b", "c"} {
		ptrs = append(ptrs, &s)
	}
	for _, p := range ptrs {
		fmt.Print(*p, " ")
	}
	fmt.Println()

	var mu sync.Mutex
	var wg sync.WaitGroup
	var got []int
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			mu.Lock()
			got = append(got, i)
			mu.Unlock()
		}()
	}
	wg.Wait()
	sort.Ints(got)
	fmt.Println(got)
}
