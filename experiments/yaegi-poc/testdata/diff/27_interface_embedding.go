// Interface composition, interface values holding pointers, nil interface vs
// typed nil, calling interface methods in loops through a helper.
package main

import (
	"fmt"
)

type Getter interface{ Get(k string) (string, bool) }
type Setter interface{ Set(k, v string) }
type Store interface {
	Getter
	Setter
}

type mapStore map[string]string

func (m mapStore) Get(k string) (string, bool) { v, ok := m[k]; return v, ok }
func (m mapStore) Set(k, v string)             { m[k] = v }

type loggingStore struct {
	Store
	log []string
}

func (l *loggingStore) Set(k, v string) {
	l.log = append(l.log, "set "+k)
	l.Store.Set(k, v)
}

type MyErr struct{}

func (*MyErr) Error() string { return "my" }

func mayFail(fail bool) error {
	var e *MyErr
	if fail {
		e = &MyErr{}
	}
	return e // typed nil when !fail: non-nil interface
}

func fill(s Setter, n int) {
	for i := 0; i < n; i++ {
		s.Set(fmt.Sprint("k", i), fmt.Sprint(i*i))
	}
}

func main() {
	ls := &loggingStore{Store: mapStore{}}
	fill(ls, 3)
	v, ok := ls.Get("k2")
	fmt.Println(v, ok, ls.log)
	var g Getter = ls
	_, ok = g.Get("nope")
	fmt.Println(ok)

	fmt.Println(mayFail(true) != nil, mayFail(false) != nil, mayFail(false) == (*MyErr)(nil))
	var e error
	fmt.Println(e == nil)
}
