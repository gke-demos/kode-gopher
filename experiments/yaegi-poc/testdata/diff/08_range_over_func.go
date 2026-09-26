// Go 1.23 range-over-func iterators, user-defined and stdlib.
package main

import (
	"fmt"
	"iter"
	"maps"
	"slices"
)

func Countdown(n int) iter.Seq[int] {
	return func(yield func(int) bool) {
		for i := n; i > 0; i-- {
			if !yield(i) {
				return
			}
		}
	}
}

func Enumerate[T any](xs []T) iter.Seq2[int, T] {
	return func(yield func(int, T) bool) {
		for i, x := range xs {
			if !yield(i, x) {
				return
			}
		}
	}
}

func main() {
	for i := range Countdown(5) {
		if i == 2 {
			break
		}
		fmt.Print(i, " ")
	}
	fmt.Println()

	for i, s := range Enumerate([]string{"x", "y"}) {
		fmt.Println(i, s)
	}

	m := map[string]int{"b": 2, "a": 1}
	for _, k := range slices.Sorted(maps.Keys(m)) {
		fmt.Println(k, m[k])
	}
	fmt.Println(slices.Collect(Countdown(3)))
}
