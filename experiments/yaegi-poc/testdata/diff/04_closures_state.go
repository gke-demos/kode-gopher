// Closures holding state: generators, memoization, function-valued maps.
package main

import (
	"fmt"
	"strings"
)

func counter() func() int {
	c := 0
	return func() int { c++; return c }
}

func memo(f func(int) int) func(int) int {
	cache := map[int]int{}
	return func(n int) int {
		if v, ok := cache[n]; ok {
			return v
		}
		v := f(n)
		cache[n] = v
		return v
	}
}

func main() {
	a, b := counter(), counter()
	fmt.Println(a(), a(), b(), a())

	calls := 0
	sq := memo(func(n int) int { calls++; return n * n })
	for _, n := range []int{3, 4, 3, 3, 4} {
		fmt.Print(sq(n), " ")
	}
	fmt.Println("calls:", calls)

	ops := map[string]func(int, int) int{
		"add": func(x, y int) int { return x + y },
		"mul": func(x, y int) int { return x * y },
	}
	for _, k := range []string{"add", "mul"} {
		fmt.Println(k, ops[k](6, 7))
	}

	var fib func(int) int
	fib = func(n int) int {
		if n < 2 {
			return n
		}
		return fib(n-1) + fib(n-2)
	}
	fmt.Println(fib(20))

	up := strings.Map(func(r rune) rune {
		if r == 'a' {
			return 'A'
		}
		return r
	}, "banana")
	fmt.Println(up)
}
