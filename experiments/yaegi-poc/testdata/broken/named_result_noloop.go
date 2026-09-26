// Plain int named result, three straight-line calls (no loop), each assigned.
package main

import "fmt"

func f() (n int) {
	n++
	return
}

func main() {
	a := f()
	b := f()
	c := f()
	fmt.Println(a, b, c)
}
