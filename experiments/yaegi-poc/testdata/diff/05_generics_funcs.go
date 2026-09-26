// Generic helper functions: Map/Filter/Reduce, constraints, type inference.
package main

import (
	"cmp"
	"fmt"
	"strconv"
)

func Map[T, U any](xs []T, f func(T) U) []U {
	out := make([]U, 0, len(xs))
	for _, x := range xs {
		out = append(out, f(x))
	}
	return out
}

func Filter[T any](xs []T, keep func(T) bool) []T {
	var out []T
	for _, x := range xs {
		if keep(x) {
			out = append(out, x)
		}
	}
	return out
}

func Reduce[T, A any](xs []T, init A, f func(A, T) A) A {
	acc := init
	for _, x := range xs {
		acc = f(acc, x)
	}
	return acc
}

func Max[T cmp.Ordered](xs ...T) T {
	m := xs[0]
	for _, x := range xs[1:] {
		if x > m {
			m = x
		}
	}
	return m
}

type Number interface {
	~int | ~int64 | ~float64
}

func Sum[T Number](xs []T) T {
	var s T
	for _, x := range xs {
		s += x
	}
	return s
}

type Celsius float64

func main() {
	xs := []int{1, 2, 3, 4, 5, 6}
	fmt.Println(Map(xs, strconv.Itoa))
	fmt.Println(Filter(xs, func(x int) bool { return x%2 == 0 }))
	fmt.Println(Reduce(xs, "", func(a string, x int) string { return a + strconv.Itoa(x) }))
	fmt.Println(Max(3, 9, 2), Max("pear", "apple"), Max(1.5, -2.0))
	fmt.Println(Sum(xs), Sum([]Celsius{20.5, 1.5}))
}
