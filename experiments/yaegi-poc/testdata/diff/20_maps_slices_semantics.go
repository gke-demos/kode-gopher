// Value vs reference semantics: map of structs, slice aliasing on append,
// arrays copied by value, struct comparison, struct map keys.
package main

import (
	"fmt"
	"sort"
)

type Node struct {
	Name  string
	Ready bool
}

type key struct {
	zone string
	n    int
}

func mark(nodes []Node) {
	for i := range nodes {
		nodes[i].Ready = true
	}
}

func markCopy(nodes []Node) {
	for _, n := range nodes {
		n.Ready = true
	}
}

func main() {
	ns := []Node{{"a", false}, {"b", false}}
	markCopy(ns)
	fmt.Println(ns)
	mark(ns)
	fmt.Println(ns)

	m := map[string]*Node{"x": {"x", false}}
	m["x"].Ready = true
	fmt.Println(*m["x"])

	a := make([]int, 3, 10)
	b := append(a, 4)
	a = append(a, 99)
	fmt.Println(b[3], len(a), len(b))

	arr := [3]int{1, 2, 3}
	arr2 := arr
	arr2[0] = 100
	fmt.Println(arr, arr2, arr == [3]int{1, 2, 3})

	fmt.Println(Node{"a", true} == Node{"a", true})

	counts := map[key]int{}
	for _, z := range []string{"a", "b", "a"} {
		counts[key{z, 1}]++
	}
	ks := make([]string, 0)
	for k, v := range counts {
		ks = append(ks, fmt.Sprintf("%s=%d", k.zone, v))
	}
	sort.Strings(ks)
	fmt.Println(ks)

	grid := make([][]int, 3)
	for i := range grid {
		grid[i] = make([]int, 3)
		grid[i][i] = 1
	}
	fmt.Println(grid)
}
