// Named results: accumulate in a loop-called helper, forward via return f(n),
// multiple named results, and early return.
package main

import "fmt"

type stats struct{ sum, n int }

func tally(xs []int) (s stats) {
	for _, x := range xs {
		s.sum += x
		s.n++
	}
	return
}

func divmod(a, b int) (q, r int) {
	q = a / b
	r = a % b
	return
}

func double(x int) (r int) {
	r += 2 * x
	return
}

func forward(x int) (n int) {
	n = x
	return double(n)
}

func find(xs []string, want string) (idx int, ok bool) {
	for i, x := range xs {
		if x == want {
			return i, true
		}
	}
	return
}

func main() {
	for _, xs := range [][]int{{1, 2, 3}, {10}, {}} {
		fmt.Printf("%+v\n", tally(xs))
	}
	for i := 1; i <= 3; i++ {
		q, r := divmod(10, i)
		fmt.Println(q, r, forward(i))
	}
	for _, w := range []string{"b", "z", "a"} {
		fmt.Println(find([]string{"a", "b"}, w))
	}
}
