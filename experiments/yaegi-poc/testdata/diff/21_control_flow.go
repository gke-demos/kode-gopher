// Control flow: labeled break/continue, switch fallthrough and no-condition
// switch, goto, shadowing in if/else chains.
package main

import "fmt"

func classify(n int) string {
	switch {
	case n < 0:
		return "neg"
	case n == 0:
		return "zero"
	case n%2 == 0:
		return "even"
	}
	return "odd"
}

func main() {
outer:
	for i := 0; i < 3; i++ {
		for j := 0; j < 3; j++ {
			if j == 2 {
				continue outer
			}
			if i == 2 {
				break outer
			}
			fmt.Print(i, j, " ")
		}
	}
	fmt.Println()

	switch x := 2; x {
	case 1:
		fmt.Println("one")
	case 2:
		fmt.Println("two")
		fallthrough
	case 3:
		fmt.Println("three (fallthrough)")
	default:
		fmt.Println("default")
	}

	for _, n := range []int{-1, 0, 4, 7} {
		fmt.Print(classify(n), " ")
	}
	fmt.Println()

	i := 0
loop:
	if i < 3 {
		i++
		goto loop
	}
	fmt.Println("goto:", i)

	x := 1
	if x := 2; x > 1 {
		fmt.Println("inner", x)
	} else if y := x * 2; y > 0 {
		fmt.Println("never", y)
	}
	fmt.Println("outer", x)
}
