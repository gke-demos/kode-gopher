// Generic types: a stack with methods, a generic pair, a set built on a map.
package main

import (
	"fmt"
	"sort"
)

type Stack[T any] struct{ items []T }

func (s *Stack[T]) Push(v T) { s.items = append(s.items, v) }

func (s *Stack[T]) Pop() (T, bool) {
	var zero T
	if len(s.items) == 0 {
		return zero, false
	}
	v := s.items[len(s.items)-1]
	s.items = s.items[:len(s.items)-1]
	return v, true
}

func (s *Stack[T]) Len() int { return len(s.items) }

type Pair[K comparable, V any] struct {
	Key K
	Val V
}

type Set[T comparable] map[T]struct{}

func (s Set[T]) Add(v T)           { s[v] = struct{}{} }
func (s Set[T]) Has(v T) bool      { _, ok := s[v]; return ok }

func main() {
	var s Stack[string]
	for _, w := range []string{"a", "b", "c"} {
		s.Push(w)
	}
	for s.Len() > 0 {
		v, _ := s.Pop()
		fmt.Print(v)
	}
	_, ok := s.Pop()
	fmt.Println(" empty-pop-ok:", ok)

	ps := []Pair[string, int]{{"b", 2}, {"a", 1}, {"c", 3}}
	sort.Slice(ps, func(i, j int) bool { return ps[i].Key < ps[j].Key })
	fmt.Printf("%v\n", ps)

	set := Set[int]{}
	for _, n := range []int{1, 2, 2, 3} {
		set.Add(n)
	}
	fmt.Println(len(set), set.Has(2), set.Has(9))
}
