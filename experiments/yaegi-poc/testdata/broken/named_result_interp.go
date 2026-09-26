// Named result of an interpreted struct type, mutated via a pointer-receiver method.
package main

import "fmt"

type acc struct{ n int }

func (a *acc) add(v int) { a.n += v }

func f() (a acc) {
	a.add(1)
	return
}

func main() {
	for i := 0; i < 3; i++ {
		fmt.Printf("call %d: %d\n", i, f().n)
	}
}
