// json.Unmarshal into interpreted structs and into map[string]any with type
// assertions — the shape of parsing an API response.
package main

import (
	"encoding/json"
	"fmt"
	"sort"
)

type Item struct {
	Name   string            `json:"name"`
	Size   int64             `json:"size,string"`
	Labels map[string]string `json:"labels"`
	Parts  []struct {
		ID int `json:"id"`
	} `json:"parts"`
}

type Resp struct {
	Items []Item `json:"items"`
	Next  *string `json:"nextPageToken"`
}

const payload = `{"items":[{"name":"a","size":"42","labels":{"k":"v"},"parts":[{"id":1},{"id":2}]},
{"name":"b","size":"7"}],"nextPageToken":null}`

func main() {
	var r Resp
	if err := json.Unmarshal([]byte(payload), &r); err != nil {
		fmt.Println("err:", err)
		return
	}
	for _, it := range r.Items {
		fmt.Println(it.Name, it.Size, it.Labels, len(it.Parts))
	}
	fmt.Println(r.Next == nil)

	var generic map[string]any
	json.Unmarshal([]byte(payload), &generic)
	items := generic["items"].([]any)
	first := items[0].(map[string]any)
	keys := make([]string, 0, len(first))
	for k := range first {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	fmt.Println(keys, first["size"].(string), len(first["parts"].([]any)))

	var bad Item
	fmt.Println(json.Unmarshal([]byte(`{"size": 5}`), &bad) != nil)
}
