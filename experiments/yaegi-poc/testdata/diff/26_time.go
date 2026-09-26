// time with fixed instants: parsing RFC3339, durations, formatting,
// truncation, sorting by time — the "how old is this resource" pattern.
package main

import (
	"fmt"
	"sort"
	"time"
)

type Res struct {
	Name    string
	Created time.Time
}

func age(now, t time.Time) string {
	d := now.Sub(t)
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

func main() {
	now, _ := time.Parse(time.RFC3339, "2026-09-24T12:00:00Z")
	var rs []Res
	for _, s := range []string{"2026-09-24T11:15:00Z", "2026-09-01T00:00:00Z", "2026-09-23T09:30:00Z"} {
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			panic(err)
		}
		rs = append(rs, Res{s[:10], t})
	}
	sort.Slice(rs, func(i, j int) bool { return rs[i].Created.Before(rs[j].Created) })
	for _, r := range rs {
		fmt.Println(r.Name, age(now, r.Created), r.Created.Format("Jan 2 15:04"), r.Created.Weekday())
	}
	d := 90*time.Minute + 30*time.Second
	fmt.Println(d, d.Round(time.Hour), d.Truncate(time.Minute), time.Duration(1500)*time.Millisecond)
	fmt.Println(now.Add(-36*time.Hour).Format(time.DateOnly), now.YearDay(), now.Unix())
	_, err := time.Parse(time.RFC3339, "not a time")
	fmt.Println(err != nil)
}
