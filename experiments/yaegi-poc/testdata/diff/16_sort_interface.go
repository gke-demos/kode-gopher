// Interpreted types satisfying compiled stdlib interfaces that call back into
// them: sort.Interface, heap.Interface.
package main

import (
	"container/heap"
	"fmt"
	"sort"
)

type byLen []string

func (b byLen) Len() int           { return len(b) }
func (b byLen) Less(i, j int) bool { return len(b[i]) < len(b[j]) }
func (b byLen) Swap(i, j int)      { b[i], b[j] = b[j], b[i] }

type item struct {
	name string
	prio int
}

type pq []*item

func (q pq) Len() int            { return len(q) }
func (q pq) Less(i, j int) bool  { return q[i].prio > q[j].prio }
func (q pq) Swap(i, j int)       { q[i], q[j] = q[j], q[i] }
func (q *pq) Push(x any)         { *q = append(*q, x.(*item)) }
func (q *pq) Pop() any {
	old := *q
	n := len(old)
	it := old[n-1]
	*q = old[:n-1]
	return it
}

func main() {
	words := byLen{"kubernetes", "go", "gke", "yaegi"}
	sort.Sort(words)
	fmt.Println(words)
	sort.Stable(sort.Reverse(words))
	fmt.Println(words)

	q := &pq{}
	heap.Init(q)
	for i, n := range []string{"low", "high", "mid"} {
		heap.Push(q, &item{n, []int{1, 9, 5}[i]})
	}
	for q.Len() > 0 {
		fmt.Print(heap.Pop(q).(*item).name, " ")
	}
	fmt.Println()
}
