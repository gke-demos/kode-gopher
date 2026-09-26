// Uses k8s.io/api types by name, not just through the clientset.
//
// list_nodes_k8s.go only names kubernetes, clientcmd and metav1 — everything
// else is reached through method return values, which don't need symbols.
// This snippet names corev1/appsv1 types and resource.Quantity directly,
// which is what real snippets do, so it needs the k8s.io/api extracts.
//
//	KG_SYMBOL_PLUGINS=/tmp/kgsyms2 KUBECONFIG=... \
//	  /tmp/yaegi-poc-plugin testdata/k8s_api_types.go
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

func requests(pod corev1.Pod) (cpu, mem resource.Quantity) {
	for _, c := range pod.Spec.Containers {
		if q, ok := c.Resources.Requests[corev1.ResourceCPU]; ok {
			cpu.Add(q)
		}
		if q, ok := c.Resources.Requests[corev1.ResourceMemory]; ok {
			mem.Add(q)
		}
	}
	return cpu, mem
}

func ready(d appsv1.Deployment) string {
	return fmt.Sprintf("%d/%d", d.Status.ReadyReplicas, d.Status.Replicas)
}

func main() {
	cfg, err := clientcmd.BuildConfigFromFlags("", os.Getenv("KUBECONFIG"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "build config: %v\n", err)
		os.Exit(1)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "new clientset: %v\n", err)
		os.Exit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pods, err := cs.CoreV1().Pods("kube-system").List(ctx, metav1.ListOptions{})
	if err != nil {
		fmt.Fprintf(os.Stderr, "list pods: %v\n", err)
		os.Exit(1)
	}
	var totalCPU, totalMem resource.Quantity
	running := 0
	for _, p := range pods.Items {
		if p.Status.Phase == corev1.PodRunning {
			running++
		}
		c, m := requests(p)
		totalCPU.Add(c)
		totalMem.Add(m)
	}

	deps, err := cs.AppsV1().Deployments("kube-system").List(ctx, metav1.ListOptions{})
	if err != nil {
		fmt.Fprintf(os.Stderr, "list deployments: %v\n", err)
		os.Exit(1)
	}
	depReady := map[string]string{}
	for _, d := range deps.Items {
		depReady[d.Name] = ready(d)
	}

	b, _ := json.MarshalIndent(map[string]interface{}{
		"pods":        len(pods.Items),
		"running":     running,
		"cpuRequests": totalCPU.String(),
		"memRequests": totalMem.String(),
		"deployments": depReady,
	}, "", "  ")
	fmt.Println(string(b))
}
