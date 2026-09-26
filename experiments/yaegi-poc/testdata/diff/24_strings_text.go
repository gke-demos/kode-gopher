// Text processing: strings.Builder in helpers, tabwriter tables, runes vs
// bytes, fmt padding — the "format a report" end of a snippet.
package main

import (
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"unicode/utf8"
)

type row struct {
	name   string
	status string
	age    int
}

func render(rows []row) string {
	var b strings.Builder
	for i, r := range rows {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%s(%s)", r.name, r.status)
	}
	return b.String()
}

func main() {
	rows := []row{{"web-1", "Running", 3}, {"db-primary", "Pending", 12}, {"cache", "Running", 1}}
	for i := 0; i < 2; i++ {
		fmt.Println(render(rows[i:]))
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tSTATUS\tAGE")
	for _, r := range rows {
		fmt.Fprintf(w, "%s\t%s\t%dd\n", r.name, r.status, r.age)
	}
	w.Flush()

	s := "héllo, 世界"
	fmt.Println(len(s), utf8.RuneCountInString(s), []rune(s)[1], string([]rune(s)[7:]))
	for i, r := range "aé" {
		fmt.Print(i, ":", string(r), " ")
	}
	fmt.Println()
	fmt.Printf("[%-8s][%8s][%08.3f]\n", "left", "right", 3.14159)
	fmt.Println(strings.Title("x"), strings.EqualFold("GKE", "gke"), strings.SplitN("a:b:c", ":", 2))
}
