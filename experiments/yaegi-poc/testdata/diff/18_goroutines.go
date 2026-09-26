// Concurrency: worker pool over channels, results collected and sorted,
// select with default, sync.Once, buffered channel semantics.
package main

import (
	"fmt"
	"sort"
	"sync"
)

func worker(id int, jobs <-chan int, results chan<- [2]int, wg *sync.WaitGroup) {
	defer wg.Done()
	for j := range jobs {
		results <- [2]int{j, j * j}
	}
}

func main() {
	jobs := make(chan int, 10)
	results := make(chan [2]int, 10)
	var wg sync.WaitGroup
	for w := 1; w <= 3; w++ {
		wg.Add(1)
		go worker(w, jobs, results, &wg)
	}
	for i := 1; i <= 9; i++ {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	close(results)

	var got [][2]int
	for r := range results {
		got = append(got, r)
	}
	sort.Slice(got, func(i, j int) bool { return got[i][0] < got[j][0] })
	fmt.Println(got)

	ch := make(chan string, 1)
	select {
	case v := <-ch:
		fmt.Println("got", v)
	default:
		fmt.Println("empty")
	}
	ch <- "x"
	select {
	case v := <-ch:
		fmt.Println("got", v)
	default:
		fmt.Println("empty")
	}

	var once sync.Once
	n := 0
	for i := 0; i < 3; i++ {
		once.Do(func() { n++ })
	}
	fmt.Println("once:", n)

	v, ok := <-results
	fmt.Println(v, ok)
}
