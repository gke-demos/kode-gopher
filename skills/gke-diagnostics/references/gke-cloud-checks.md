# GKE Cloud Capacity, Stockouts & IP Space Recipes

Adapted from `go-steer/k8s-lookout` (`pkg/cloud/gke/capacity.go`, `stockout.go`,
and `ipspace.go`). All clients (`cloud.google.com/go/logging/logadmin`,
`cloud.google.com/go/container/apiv1`, `cloud.google.com/go/compute/apiv1`) are
prewarmed in the `kode-gopher` sandbox image (`tidied: false`).

## Why Distinguish These Three Signals?

When GKE pods sit `Pending` with `FailedScaleUp` / `Unschedulable`, the remedy
depends completely on the underlying cloud cause:
- **`GCE_STOCKOUT` (`ZONE_RESOURCE_POOL_EXHAUSTED`)**: Zone lacks hardware for that
  machine family -> add a fallback compute class / machine series or another zone.
- **`GCE_QUOTA_EXCEEDED`**: Regional CPU/GPU/SSD quota hit -> request quota increase
  or scale down idle pools.
- **`IP_SPACE_EXHAUSTED`**: Subnet secondary Pod CIDR full -> add secondary CIDR via
  multi-pod-networking or shrink `--max-pods-per-node`.

---

## Recipe 1: Cluster Autoscaler Visibility Decisions + GCE Stockout Machine-Type Join

Queries two Cloud Logging streams in parallel:
1. `container.googleapis.com%2Fcluster-autoscaler-visibility` — structured CA
   `noScaleUp` and `resultInfo` error events (never scrape unstructured text logs).
2. `cloudaudit.googleapis.com%2Factivity` — joins `ZONE_RESOURCE_POOL_EXHAUSTED`
   completion records with `operation.first=true` start records by `operation.id`
   so you recover `request.machineType` (which GCE omits on completion entries!).

```go
package snippet

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"cloud.google.com/go/logging"
	"cloud.google.com/go/logging/logadmin"
	"golang.org/x/sync/errgroup"
	"google.golang.org/api/iterator"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

type CADecision struct {
	Time      string `json:"time"`
	Decision  string `json:"decision"`
	NodeGroup string `json:"node_group,omitempty"`
	Reason    string `json:"reason"`
	Message   string `json:"message"`
}

type StockoutEvent struct {
	Time        string `json:"time"`
	Zone        string `json:"zone"`
	MachineType string `json:"machine_type"`
	Message     string `json:"message"`
}

var caErrorReasons = map[string]string{
	"scale.up.error.out.of.resources":   "GCE_STOCKOUT",
	"scale.up.error.quota.exceeded":     "GCE_QUOTA_EXCEEDED",
	"scale.up.error.ip.space.exhausted": "IP_SPACE_EXHAUSTED",
}

func run(ctx context.Context) (any, error) {
	const (
		project  = "my-project"
		location = "us-central1"
		cluster  = "prod-cluster"
		lookback = 2 * time.Hour
	)

	admin, err := logadmin.NewClient(ctx, project)
	if err != nil {
		return nil, err
	}
	defer admin.Close()

	end := time.Now().UTC()
	start := end.Add(-lookback)

	var (
		decisions []CADecision
		stockouts []StockoutEvent
	)
	g, gctx := errgroup.WithContext(ctx)

	g.Go(func() error {
		var err error
		decisions, err = queryCAVisibility(gctx, admin, project, location, cluster, start, end)
		return err
	})
	g.Go(func() error {
		var err error
		stockouts, err = queryGCEStockouts(gctx, admin, project, start, end)
		return err
	})
	if err := g.Wait(); err != nil {
		return nil, err
	}

	return map[string]any{
		"window_start": start.Format(time.RFC3339),
		"window_end":   end.Format(time.RFC3339),
		"ca_decisions": decisions,
		"stockouts":    stockouts,
	}, nil
}

func queryCAVisibility(ctx context.Context, admin *logadmin.Client, project, location, cluster string, start, end time.Time) ([]CADecision, error) {
	filter := fmt.Sprintf(
		`logName="projects/%s/logs/container.googleapis.com%%2Fcluster-autoscaler-visibility" AND resource.type="k8s_cluster" AND resource.labels.cluster_name=%q AND resource.labels.location=%q AND timestamp>=%q AND timestamp<%q`,
		project, cluster, location, start.Format(time.RFC3339), end.Format(time.RFC3339),
	)
	it := admin.Entries(ctx, logadmin.Filter(filter), logadmin.NewestFirst())
	var out []CADecision
	for count := 0; count < 300; count++ {
		entry, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return nil, err
		}
		raw, ok := entryJSON(entry)
		if !ok {
			continue
		}
		out = append(out, parseCAEntry(entry.Timestamp, raw)...)
	}
	return out, nil
}

func queryGCEStockouts(ctx context.Context, admin *logadmin.Client, project string, start, end time.Time) ([]StockoutEvent, error) {
	filter := fmt.Sprintf(
		`logName="projects/%s/logs/cloudaudit.googleapis.com%%2Factivity" AND resource.type="gce_instance" AND protoPayload.methodName:"compute.instances.insert" AND (protoPayload.status.message:"ZONE_RESOURCE_POOL_EXHAUSTED" OR operation.first=true) AND timestamp>=%q AND timestamp<%q`,
		project, start.Format(time.RFC3339), end.Format(time.RFC3339),
	)
	type pending struct {
		ev   StockoutEvent
		opID string
	}
	var pend []pending
	opMachine := map[string]string{}

	it := admin.Entries(ctx, logadmin.Filter(filter), logadmin.NewestFirst())
	for count := 0; count < 500; count++ {
		entry, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return nil, err
		}
		raw, ok := entryJSON(entry)
		if !ok {
			continue
		}
		var payload struct {
			Status  struct{ Message string `json:"message"` }     `json:"status"`
			Request struct{ MachineType string `json:"machineType"` } `json:"request"`
		}
		if json.Unmarshal(raw, &payload) != nil {
			continue
		}
		opID := ""
		if entry.Operation != nil {
			opID = entry.Operation.Id
		}
		mt := tailSegment(payload.Request.MachineType)
		if opID != "" && mt != "" {
			opMachine[opID] = mt
		}
		if strings.Contains(payload.Status.Message, "ZONE_RESOURCE_POOL_EXHAUSTED") {
			zone := ""
			if entry.Resource != nil {
				zone = entry.Resource.Labels["zone"]
			}
			pend = append(pend, pending{
				opID: opID,
				ev: StockoutEvent{
					Time:        entry.Timestamp.UTC().Format(time.RFC3339),
					Zone:        zone,
					MachineType: mt,
					Message:     payload.Status.Message,
				},
			})
		}
	}
	out := make([]StockoutEvent, len(pend))
	for i, p := range pend {
		if p.ev.MachineType == "" && p.opID != "" {
			p.ev.MachineType = opMachine[p.opID]
		}
		out[i] = p.ev
	}
	return out, nil
}

func entryJSON(e *logging.Entry) ([]byte, bool) {
	switch p := e.Payload.(type) {
	case proto.Message:
		b, err := protojson.Marshal(p)
		return b, err == nil
	case map[string]any:
		b, err := json.Marshal(p)
		return b, err == nil
	default:
		return nil, false
	}
}

func parseCAEntry(ts time.Time, raw []byte) []CADecision {
	var p struct {
		Decision *struct {
			NoScaleUp *struct {
				SkippedMigs []struct {
					Mig    struct{ Name string `json:"name"` } `json:"mig"`
					Reason struct {
						MessageID  string   `json:"messageId"`
						Parameters []string `json:"parameters"`
					} `json:"reason"`
				} `json:"skippedMigs"`
				UnhandledPodGroups []struct {
					RejectedMigs []struct {
						Mig    struct{ Name string `json:"name"` } `json:"mig"`
						Reason struct {
							MessageID  string   `json:"messageId"`
							Parameters []string `json:"parameters"`
						} `json:"reason"`
					} `json:"rejectedMigs"`
				} `json:"unhandledPodGroups"`
			} `json:"noScaleUp"`
		} `json:"decision"`
		ResultInfo *struct {
			Results []struct {
				ErrorMsg *struct {
					MessageID  string   `json:"messageId"`
					Parameters []string `json:"parameters"`
				} `json:"errorMsg"`
			} `json:"results"`
		} `json:"resultInfo"`
	}
	if json.Unmarshal(raw, &p) != nil {
		return nil
	}
	stamp := ts.UTC().Format(time.RFC3339)
	var out []CADecision
	if p.Decision != nil && p.Decision.NoScaleUp != nil {
		for _, sk := range p.Decision.NoScaleUp.SkippedMigs {
			out = append(out, CADecision{
				Time: stamp, Decision: "noScaleUp", NodeGroup: sk.Mig.Name,
				Reason: sk.Reason.MessageID, Message: strings.Join(sk.Reason.Parameters, ", "),
			})
		}
		for _, g := range p.Decision.NoScaleUp.UnhandledPodGroups {
			for _, rej := range g.RejectedMigs {
				out = append(out, CADecision{
					Time: stamp, Decision: "noScaleUp", NodeGroup: rej.Mig.Name,
					Reason: rej.Reason.MessageID, Message: strings.Join(rej.Reason.Parameters, ", "),
				})
			}
		}
	}
	if p.ResultInfo != nil {
		for _, res := range p.ResultInfo.Results {
			if res.ErrorMsg == nil {
				continue
			}
			reason := caErrorReasons[res.ErrorMsg.MessageID]
			if reason == "" {
				reason = res.ErrorMsg.MessageID
			}
			out = append(out, CADecision{
				Time: stamp, Decision: "scaleUpError", Reason: reason,
				Message: fmt.Sprintf("%s (%s)", res.ErrorMsg.MessageID, strings.Join(res.ErrorMsg.Parameters, ", ")),
			})
		}
	}
	return out
}

func tailSegment(s string) string {
	if idx := strings.LastIndexByte(s, '/'); idx >= 0 {
		return s[idx+1:]
	}
	return s
}
```

---

## Recipe 2: Secondary Pod CIDR Utilization Check

When `IP_SPACE_EXHAUSTED` fires (or nodes fail pod CIDR allocation), inspect
`Cluster.IpAllocationPolicy.ClusterSecondaryRangeName` via `container/apiv1`,
fetch the VPC subnetwork via `compute/apiv1` (`SubnetworksClient.Get`), compute
total IPs in the secondary CIDR (`1 << (32 - prefixLen)`), and compare against
`len(nodes) * maxPodsPerNode` (each GKE node carves a `/24` = 256 IPs at the
default 110 max-pods-per-node!). Often a `/21` secondary range (2,048 IPs) is
100% exhausted at **8 nodes**, even when only 80 pods are running!
