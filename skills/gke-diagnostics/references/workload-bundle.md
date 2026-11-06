# Workload Bundle, Dependency Edges & Drain Log Clustering

Adapted from `go-steer/k8s-lookout` (`pkg/checks/bundle`, `pkg/kube/transform.go`,
and `pkg/checks/logs/drain.go`) using the prewarmed
`github.com/gke-demos/kode-gopher/sdk/gkeutil` helpers (`tidied: false`).

## When to Use

Use when investigating a specific broken Deployment/StatefulSet (`CrashLoopBackOff`,
`ImagePullBackOff`, `CreateContainerConfigError`, readiness failures, or noisy 5xx logs)
so you fetch **rollout health + abnormal pods + broken ConfigMap/Secret/EndpointSlice
edges + clustered log templates** in a single `execute_go_code` call.

## Complete `run(ctx)` Pattern

Replace `project`, `location`, `cluster`, `namespace`, and `deployName` (or read
them from env / `/app/persist/cluster-scan.json`). Literal env and Secret values
are never copied into the result — only `EnvVar.Name` and `ValueFrom` key names
are inspected.

```go
package snippet

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/gke-demos/kode-gopher/sdk/gkeutil"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
)

type BundleReport struct {
	Cluster        *gkeutil.ClusterInfo  `json:"cluster"`
	Workload       string                `json:"workload"`
	Replicas       string                `json:"replicas"`
	PodAnomalies   []gkeutil.PodSummary  `json:"pod_anomalies,omitempty"`
	BrokenEdges    []string              `json:"broken_edges,omitempty"`
	ReadyEndpoints int                   `json:"ready_endpoints"`
	LogTemplates   []gkeutil.LogCluster  `json:"log_templates,omitempty"`
}

func run(ctx context.Context) (any, error) {
	const (
		project    = "my-project"
		location   = "us-central1"
		cluster    = "prod-cluster"
		namespace  = "prod"
		deployName = "checkout"
	)

	cs, info, err := gkeutil.DialClientset(ctx, project, location, cluster)
	if err != nil {
		return nil, err
	}

	dep, err := cs.AppsV1().Deployments(namespace).Get(ctx, deployName, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}

	sel := labels.Set(dep.Spec.Selector.MatchLabels).AsSelector().String()
	pods, err := cs.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: sel})
	if err != nil {
		return nil, err
	}

	report := BundleReport{
		Cluster:      info,
		Workload:     fmt.Sprintf("Deployment/%s/%s", namespace, deployName),
		Replicas:     fmt.Sprintf("%d/%d ready (%d unavailable)", dep.Status.ReadyReplicas, dep.Status.Replicas, dep.Status.UnavailableReplicas),
		PodAnomalies: gkeutil.ScrubPodList(pods, true),
		BrokenEdges:  checkConfigEdges(ctx, cs, namespace, &dep.Spec.Template.Spec),
	}

	// Check Service -> EndpointSlice readiness matching the workload's pods.
	if slices, err := cs.DiscoveryV1().EndpointSlices(namespace).List(ctx, metav1.ListOptions{}); err == nil {
		podNames := make(map[string]bool, len(pods.Items))
		for i := range pods.Items {
			podNames[pods.Items[i].Name] = true
		}
		for _, es := range slices.Items {
			for _, ep := range es.Endpoints {
				if ep.TargetRef != nil && podNames[ep.TargetRef.Name] && (ep.Conditions.Ready == nil || *ep.Conditions.Ready) {
					report.ReadyEndpoints++
				}
			}
		}
	}

	// Drain-cluster tail logs across up to 3 pods, attributing pod source count.
	drainer := gkeutil.NewDrainer()
	tail := int64(250)
	for i := 0; i < len(pods.Items) && i < 3; i++ {
		podName := pods.Items[i].Name
		raw, err := cs.CoreV1().Pods(namespace).GetLogs(podName, &corev1.PodLogOptions{TailLines: &tail}).DoRaw(ctx)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(raw), "\n") {
			drainer.Add(podName, time.Time{}, line)
		}
	}
	report.LogTemplates = drainer.Clusters(15)
	return report, nil
}

func checkConfigEdges(ctx context.Context, cs *kubernetes.Clientset, ns string, spec *corev1.PodSpec) []string {
	var broken []string
	cmCache := map[string]*corev1.ConfigMap{}
	secKeys := map[string]map[string]bool{}

	allContainers := append(append([]corev1.Container{}, spec.InitContainers...), spec.Containers...)
	for _, c := range allContainers {
		for _, env := range c.Env {
			if env.ValueFrom == nil {
				continue
			}
			if ref := env.ValueFrom.ConfigMapKeyRef; ref != nil {
				cm, seen := cmCache[ref.Name]
				if !seen {
					got, err := cs.CoreV1().ConfigMaps(ns).Get(ctx, ref.Name, metav1.GetOptions{})
					if apierrors.IsNotFound(err) {
						broken = append(broken, fmt.Sprintf("container %s env %s: missing ConfigMap %q", c.Name, env.Name, ref.Name))
						cmCache[ref.Name] = nil
						continue
					}
					cm = got
					cmCache[ref.Name] = cm
				}
				if cm != nil {
					if _, has := cm.Data[ref.Key]; !has {
						broken = append(broken, fmt.Sprintf("container %s env %s: key %q missing in ConfigMap %q", c.Name, env.Name, ref.Key, ref.Name))
					}
				}
			}
			if ref := env.ValueFrom.SecretKeyRef; ref != nil {
				keys, seen := secKeys[ref.Name]
				if !seen {
					got, err := cs.CoreV1().Secrets(ns).Get(ctx, ref.Name, metav1.GetOptions{})
					if apierrors.IsNotFound(err) {
						broken = append(broken, fmt.Sprintf("container %s env %s: missing Secret %q", c.Name, env.Name, ref.Name))
						secKeys[ref.Name] = nil
						continue
					}
					if got != nil {
						keys = make(map[string]bool, len(got.Data))
						for k := range got.Data {
							keys[k] = true
						}
						secKeys[ref.Name] = keys
					}
				}
				if keys != nil && !keys[ref.Key] {
					broken = append(broken, fmt.Sprintf("container %s env %s: key %q missing in Secret %q", c.Name, env.Name, ref.Key, ref.Name))
				}
			}
		}
	}
	return broken
}
```
