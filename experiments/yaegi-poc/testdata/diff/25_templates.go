// text/template executing against interpreted structs and methods —
// reflection from compiled code over interpreted types.
package main

import (
	"os"
	"strings"
	"text/template"
)

type Bucket struct {
	Name     string
	Location string
	Objects  int
}

func (b Bucket) Big() bool { return b.Objects > 100 }

type Report struct {
	Project string
	Buckets []Bucket
}

const tmpl = `Project: {{.Project}}
{{range $i, $b := .Buckets}}{{$i}}. {{$b.Name | upper}} ({{$b.Location}}){{if $b.Big}} BIG{{end}}
{{end}}Total: {{len .Buckets}}
`

func main() {
	t := template.Must(template.New("r").Funcs(template.FuncMap{"upper": strings.ToUpper}).Parse(tmpl))
	r := Report{"gke-demos", []Bucket{{"logs", "US", 500}, {"assets", "EU", 3}}}
	if err := t.Execute(os.Stdout, r); err != nil {
		panic(err)
	}
}
