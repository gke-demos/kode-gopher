// Go 1.21+ stdlib: slices, maps, min/max/clear builtins, range over int.
package main

import (
	"fmt"
	"maps"
	"slices"
	"strings"
)

type user struct {
	name string
	age  int
}

func main() {
	xs := []int{5, 2, 8, 1}
	slices.Sort(xs)
	fmt.Println(xs, slices.Contains(xs, 8), slices.Index(xs, 5))
	fmt.Println(min(3, 1, 2), max(3, 1, 2))

	us := []user{{"bo", 30}, {"al", 25}, {"cy", 30}}
	slices.SortStableFunc(us, func(a, b user) int { return b.age - a.age })
	fmt.Println(us)

	m := map[string]int{"z": 26, "a": 1, "m": 13}
	keys := slices.Sorted(maps.Keys(m))
	fmt.Println(keys)

	for i := range 3 {
		fmt.Print(i, " ")
	}
	fmt.Println()

	clear(m)
	fmt.Println(len(m))

	ys := []int{1, 2, 3}
	slices.Reverse(ys)
	fmt.Println(strings.Fields(" a  b c "), ys, len(slices.Compact([]int{1, 1, 2, 2, 3})))
}
