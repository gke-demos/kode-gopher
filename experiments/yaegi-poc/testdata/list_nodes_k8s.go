// Lists cluster nodes through the k8s.io/client-go typed clientset.
//
// The point of this snippet is that client-go's typed clientset is the
// package yaegi's own extractor could never produce symbols for — 12 hours
// at 10.9 GB without finishing. With the export-data extractor it takes
// ~2 s, and this is the proof the resulting symbols actually work.
//
//	KG_SYMBOL_PLUGINS=/tmp/kgsyms KUBECONFIG=... \
//	  /tmp/yaegi-poc-plugin testdata/list_nodes_k8s.go
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

func main() {
	kubeconfig := os.Getenv("KUBECONFIG")
	if kubeconfig == "" {
		kubeconfig = os.Getenv("HOME") + "/.kube/config"
	}

	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		fmt.Fprintf(os.Stderr, "build config: %v\n", err)
		os.Exit(1)
	}

	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "new clientset: %v\n", err)
		os.Exit(1)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	nodes, err := clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		fmt.Fprintf(os.Stderr, "list nodes: %v\n", err)
		os.Exit(1)
	}

	out := []map[string]interface{}{}
	for _, n := range nodes.Items {
		ready := "Unknown"
		for _, c := range n.Status.Conditions {
			if string(c.Type) == "Ready" {
				ready = string(c.Status)
			}
		}
		out = append(out, map[string]interface{}{
			"name":    n.Name,
			"ready":   ready,
			"version": n.Status.NodeInfo.KubeletVersion,
		})
	}

	result := map[string]interface{}{
		"nodes": out,
		"count": len(out),
	}
	b, _ := json.MarshalIndent(result, "", "  ")
	fmt.Println(string(b))
}
