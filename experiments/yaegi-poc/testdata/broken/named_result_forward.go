// `return f(n)` where n is the caller's named result: the callee's result slot
// aliases n, which is also the argument. Guards the fix for named_result_*.go
// against zeroing the slot before the argument has been read.
package main

import "fmt"

func double(x int) (r int) {
	r += x * 2
	return
}

func g(start int) (n int) {
	n = start
	return double(n)
}

func main() {
	for i := 1; i <= 3; i++ {
		fmt.Printf("g(%d) = %d\n", i, g(i))
	}
}
