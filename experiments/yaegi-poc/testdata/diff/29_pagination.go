// The paging-loop shape GCP/k8s snippets use: a fake client returning pages
// with a continue token, results aggregated by a helper with named results.
package main

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

type Instance struct {
	Name   string
	Zone   string
	Status string
	CPUs   int
}

type page struct {
	items []Instance
	next  string
}

var pages = map[string]page{
	"":   {[]Instance{{"a", "us-a", "RUNNING", 2}, {"b", "us-b", "STOPPED", 4}}, "p2"},
	"p2": {[]Instance{{"c", "us-a", "RUNNING", 8}}, "p3"},
	"p3": {[]Instance{{"d", "eu-a", "RUNNING", 1}}, ""},
}

var ErrDone = errors.New("done")

type iterator struct {
	token string
	buf   []Instance
	done  bool
}

func (it *iterator) Next() (Instance, error) {
	for len(it.buf) == 0 {
		if it.done {
			return Instance{}, ErrDone
		}
		p := pages[it.token]
		it.buf, it.token = p.items, p.next
		it.done = p.next == ""
	}
	v := it.buf[0]
	it.buf = it.buf[1:]
	return v, nil
}

func summarize(zone string, xs []Instance) (running, cpus int, names []string) {
	for _, x := range xs {
		if x.Zone != zone {
			continue
		}
		names = append(names, x.Name)
		if x.Status == "RUNNING" {
			running++
			cpus += x.CPUs
		}
	}
	return
}

func main() {
	it := &iterator{}
	var all []Instance
	for {
		inst, err := it.Next()
		if errors.Is(err, ErrDone) {
			break
		}
		all = append(all, inst)
	}
	fmt.Println(len(all))

	zones := map[string]bool{}
	for _, x := range all {
		zones[x.Zone] = true
	}
	var zs []string
	for z := range zones {
		zs = append(zs, z)
	}
	sort.Strings(zs)
	for _, z := range zs {
		r, c, n := summarize(z, all)
		fmt.Printf("%s running=%d cpus=%d [%s]\n", z, r, c, strings.Join(n, ","))
	}
}
