/*
Copyright 2026 Google LLC

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package gkeutil

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// Redacted replaces masked credential values so structure survives
// without leaking secret material.
const Redacted = "[REDACTED]"

var (
	reURLUserinfo = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://[^/\s:@]*):([^@\s/]+)@`)
	reInlineFlag  = regexp.MustCompile(`(?i)(--?(?:[a-z0-9-]+-)?(?:password|passwd|pwd|passphrase|secret|token|api-key|apikey|client-secret|private-key|requirepass)=)(\S+)`)
	reBareFlag    = regexp.MustCompile(`(?i)^--?(?:[a-z0-9-]+-)?(?:password|passwd|pwd|passphrase|secret|token|api-key|apikey|client-secret|private-key|requirepass)$`)
	rePEMBlock    = regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?-----END [A-Z0-9 ]*PRIVATE KEY-----`)
)

// PodSummary is a compact, secret-safe projection of a Pod suitable for
// returning directly in kode-gopher's 256 KiB JSON result budget.
type PodSummary struct {
	Namespace      string            `json:"namespace"`
	Name           string            `json:"name"`
	Node           string            `json:"node,omitempty"`
	Phase          string            `json:"phase"`
	Ready          string            `json:"ready"`
	Restarts       int32             `json:"restarts"`
	Reason         string            `json:"reason,omitempty"`
	Message        string            `json:"message,omitempty"`
	InitContainers []ContainerReport `json:"init_containers,omitempty"`
	Containers     []ContainerReport `json:"containers,omitempty"`
}

// ContainerReport summarizes one container's image, state, and env references
// without ever exposing literal environment values.
type ContainerReport struct {
	Name       string   `json:"name"`
	Image      string   `json:"image"`
	Ready      bool     `json:"ready"`
	Restarts   int32    `json:"restarts"`
	State      string   `json:"state"`
	Message    string   `json:"message,omitempty"`
	EnvSources []string `json:"env_sources,omitempty"`
}

// ScrubPodList projects a PodList into compact, secret-safe PodSummary
// records. If onlyAbnormal is true, healthy Running+Ready pods (and
// Succeeded pods) with zero restarts are omitted.
func ScrubPodList(list *corev1.PodList, onlyAbnormal bool) []PodSummary {
	if list == nil {
		return nil
	}
	var out []PodSummary
	for i := range list.Items {
		s, abnormal := summarizePod(&list.Items[i])
		if onlyAbnormal && !abnormal {
			continue
		}
		out = append(out, s)
	}
	return out
}

func summarizePod(p *corev1.Pod) (PodSummary, bool) {
	initByName, initRestarts, initAbnormal := indexInitStatuses(p.Status.InitContainerStatuses)
	appByName, readyCount, appRestarts, appAbnormal := indexAppStatuses(p.Status.ContainerStatuses, p.Status.Phase)

	totalRestarts := initRestarts + appRestarts
	abnormal := p.Status.Phase != corev1.PodRunning && p.Status.Phase != corev1.PodSucceeded
	if initAbnormal || appAbnormal {
		abnormal = true
	}

	reason, msg := podHeadlineReason(p)
	if reason != "" {
		abnormal = true
	}

	return PodSummary{
		Namespace:      p.Namespace,
		Name:           p.Name,
		Node:           p.Spec.NodeName,
		Phase:          string(p.Status.Phase),
		Ready:          fmt.Sprintf("%d/%d", readyCount, len(p.Spec.Containers)),
		Restarts:       totalRestarts,
		Reason:         reason,
		Message:        truncateUTF8(maskInlineSecrets(msg), 200),
		InitContainers: buildContainerReports(p.Spec.InitContainers, initByName),
		Containers:     buildContainerReports(p.Spec.Containers, appByName),
	}, abnormal
}

func indexInitStatuses(statuses []corev1.ContainerStatus) (map[string]corev1.ContainerStatus, int32, bool) {
	byName := make(map[string]corev1.ContainerStatus, len(statuses))
	var restarts int32
	abnormal := false
	for _, cs := range statuses {
		byName[cs.Name] = cs
		restarts += cs.RestartCount
		if cs.RestartCount > 0 || cs.State.Waiting != nil || (cs.State.Terminated != nil && cs.State.Terminated.ExitCode != 0) {
			abnormal = true
		}
	}
	return byName, restarts, abnormal
}

func indexAppStatuses(statuses []corev1.ContainerStatus, phase corev1.PodPhase) (map[string]corev1.ContainerStatus, int, int32, bool) {
	byName := make(map[string]corev1.ContainerStatus, len(statuses))
	var ready int
	var restarts int32
	abnormal := false
	for _, cs := range statuses {
		byName[cs.Name] = cs
		if cs.Ready {
			ready++
		} else if phase != corev1.PodSucceeded {
			abnormal = true
		}
		restarts += cs.RestartCount
		if cs.RestartCount > 0 {
			abnormal = true
		}
	}
	return byName, ready, restarts, abnormal
}

func buildContainerReports(specs []corev1.Container, byName map[string]corev1.ContainerStatus) []ContainerReport {
	if len(specs) == 0 {
		return nil
	}
	out := make([]ContainerReport, 0, len(specs))
	for _, c := range specs {
		cs := byName[c.Name]
		st, stMsg := describeContainerState(cs)
		out = append(out, ContainerReport{
			Name:       c.Name,
			Image:      c.Image,
			Ready:      cs.Ready,
			Restarts:   cs.RestartCount,
			State:      st,
			Message:    truncateUTF8(maskInlineSecrets(stMsg), 200),
			EnvSources: summarizeEnvSources(c.Env),
		})
	}
	return out
}

func podHeadlineReason(p *corev1.Pod) (string, string) {
	for _, cs := range append(append([]corev1.ContainerStatus{}, p.Status.InitContainerStatuses...), p.Status.ContainerStatuses...) {
		if w := cs.State.Waiting; w != nil && w.Reason != "" && w.Reason != "PodInitializing" {
			return w.Reason, w.Message
		}
		if t := cs.State.Terminated; t != nil && t.ExitCode != 0 {
			return fmt.Sprintf("%s(exit=%d)", t.Reason, t.ExitCode), t.Message
		}
	}
	for _, cond := range p.Status.Conditions {
		if cond.Type == corev1.PodScheduled && cond.Status == corev1.ConditionFalse {
			return cond.Reason, cond.Message
		}
	}
	return p.Status.Reason, p.Status.Message
}

func describeContainerState(cs corev1.ContainerStatus) (string, string) {
	if cs.State.Waiting != nil {
		return "waiting:" + cs.State.Waiting.Reason, cs.State.Waiting.Message
	}
	if cs.State.Terminated != nil {
		return fmt.Sprintf("terminated:%s(exit=%d)", cs.State.Terminated.Reason, cs.State.Terminated.ExitCode), cs.State.Terminated.Message
	}
	if cs.State.Running != nil {
		if cs.LastTerminationState.Terminated != nil && cs.RestartCount > 0 {
			t := cs.LastTerminationState.Terminated
			return fmt.Sprintf("running(last=%s,exit=%d)", t.Reason, t.ExitCode), t.Message
		}
		return "running", ""
	}
	return "unknown", ""
}

func summarizeEnvSources(envs []corev1.EnvVar) []string {
	var out []string
	for _, e := range envs {
		switch {
		case e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil:
			r := e.ValueFrom.SecretKeyRef
			out = append(out, fmt.Sprintf("%s->secret:%s/%s", e.Name, r.Name, r.Key))
		case e.ValueFrom != nil && e.ValueFrom.ConfigMapKeyRef != nil:
			r := e.ValueFrom.ConfigMapKeyRef
			out = append(out, fmt.Sprintf("%s->configmap:%s/%s", e.Name, r.Name, r.Key))
		case e.Value != "":
			out = append(out, e.Name+"->literal("+Redacted+")")
		}
	}
	return out
}

// ScrubPod returns a deep copy of p with metadata.managedFields,
// applied-configuration annotations, literal container env values,
// credential CLI flags, and sensitive probe HTTP headers redacted.
func ScrubPod(p *corev1.Pod) *corev1.Pod {
	if p == nil {
		return nil
	}
	cp := p.DeepCopy()
	cp.ManagedFields = nil
	cp.Annotations = stripAppliedAnnotations(cp.Annotations)
	scrubContainers(cp.Spec.InitContainers)
	scrubContainers(cp.Spec.Containers)
	for i := range cp.Spec.EphemeralContainers {
		scrubEphemeralCommon(&cp.Spec.EphemeralContainers[i].EphemeralContainerCommon)
	}
	scrubPodStatus(&cp.Status)
	return cp
}

func scrubPodStatus(st *corev1.PodStatus) {
	st.Message = maskInlineSecrets(st.Message)
	for i := range st.Conditions {
		st.Conditions[i].Message = maskInlineSecrets(st.Conditions[i].Message)
	}
	scrubContainerStatuses(st.InitContainerStatuses)
	scrubContainerStatuses(st.ContainerStatuses)
	scrubContainerStatuses(st.EphemeralContainerStatuses)
}

func scrubContainerStatuses(cs []corev1.ContainerStatus) {
	for i := range cs {
		scrubContainerState(&cs[i].State)
		scrubContainerState(&cs[i].LastTerminationState)
	}
}

func scrubContainerState(s *corev1.ContainerState) {
	if s.Waiting != nil {
		s.Waiting.Message = maskInlineSecrets(s.Waiting.Message)
	}
	if s.Terminated != nil {
		s.Terminated.Message = maskInlineSecrets(s.Terminated.Message)
	}
}

func scrubContainers(cs []corev1.Container) {
	for i := range cs {
		c := &cs[i]
		for j := range c.Env {
			if c.Env[j].Value != "" {
				c.Env[j].Value = Redacted
			}
		}
		scrubArgStrings(c.Command)
		scrubArgStrings(c.Args)
		scrubProbe(c.LivenessProbe)
		scrubProbe(c.ReadinessProbe)
		scrubProbe(c.StartupProbe)
		scrubLifecycle(c.Lifecycle)
	}
}

func scrubEphemeralCommon(c *corev1.EphemeralContainerCommon) {
	for j := range c.Env {
		if c.Env[j].Value != "" {
			c.Env[j].Value = Redacted
		}
	}
	scrubArgStrings(c.Command)
	scrubArgStrings(c.Args)
	scrubProbe(c.LivenessProbe)
	scrubProbe(c.ReadinessProbe)
	scrubProbe(c.StartupProbe)
	scrubLifecycle(c.Lifecycle)
}

func scrubProbe(p *corev1.Probe) {
	if p != nil && p.HTTPGet != nil {
		scrubHTTPHeaders(p.HTTPGet.HTTPHeaders)
	}
}

func scrubLifecycle(lc *corev1.Lifecycle) {
	if lc == nil {
		return
	}
	if lc.PostStart != nil && lc.PostStart.HTTPGet != nil {
		scrubHTTPHeaders(lc.PostStart.HTTPGet.HTTPHeaders)
	}
	if lc.PreStop != nil && lc.PreStop.HTTPGet != nil {
		scrubHTTPHeaders(lc.PreStop.HTTPGet.HTTPHeaders)
	}
}

func scrubHTTPHeaders(hdrs []corev1.HTTPHeader) {
	for i := range hdrs {
		if isSensitiveHeader(hdrs[i].Name) && hdrs[i].Value != "" {
			hdrs[i].Value = Redacted
		} else {
			hdrs[i].Value = maskInlineSecrets(hdrs[i].Value)
		}
	}
}

func scrubArgStrings(args []string) {
	for i := 0; i < len(args); i++ {
		if i+1 < len(args) && reBareFlag.MatchString(args[i]) {
			args[i+1] = Redacted
			i++
			continue
		}
		args[i] = maskInlineSecrets(args[i])
	}
}

// ScrubNode returns a deep copy of n with metadata.managedFields,
// applied-configuration annotations, and status.images (10-40 KiB/node) removed.
func ScrubNode(n *corev1.Node) *corev1.Node {
	if n == nil {
		return nil
	}
	cp := n.DeepCopy()
	cp.ManagedFields = nil
	cp.Annotations = stripAppliedAnnotations(cp.Annotations)
	cp.Status.Images = nil
	cp.Status.VolumesInUse = nil
	cp.Status.VolumesAttached = nil
	return cp
}

// ScrubObject marshals any Kubernetes object (typed or unstructured, including
// Secret and SecretList even when TypeMeta.Kind is unset) into a sanitized
// map[string]any with managedFields, last-applied snapshots, Secret payloads,
// credential CLI flags, probe auth headers, URL userinfo, and literal env
// values stripped or redacted.
func ScrubObject(obj any) (map[string]any, error) {
	forceSecret, forceSecretList := detectTypedSecret(obj)
	raw, err := json.Marshal(obj)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	scrubMapInPlace(m, "", forceSecret, forceSecretList)
	return m, nil
}

func detectTypedSecret(obj any) (isSecret, isSecretList bool) {
	switch obj.(type) {
	case *corev1.Secret, corev1.Secret:
		return true, false
	case *corev1.SecretList, corev1.SecretList:
		return false, true
	default:
		return false, false
	}
}

func scrubMapInPlace(m map[string]any, parent string, forceSecret, forceSecretList bool) {
	isSec := forceSecret || isSecretObjectMap(m)
	isSecList := forceSecretList || mapKindEquals(m, "SecretList")
	if isSec {
		redactSecretField(m, "data")
		redactSecretField(m, "stringData")
	}
	pruneSystemFields(m, parent)

	for k, v := range m {
		if isSec && (k == "data" || k == "stringData") {
			continue
		}
		m[k] = scrubMapEntry(parent, k, v, isSecList && k == "items")
		if k == "annotations" {
			if sub, ok := m[k].(map[string]any); ok && len(sub) == 0 {
				delete(m, k)
			}
		}
	}
}

func scrubMapEntry(parent, key string, v any, itemsAreSecrets bool) any {
	switch val := v.(type) {
	case map[string]any:
		scrubMapInPlace(val, key, false, false)
		return val
	case []any:
		scrubSliceInPlace(key, val, itemsAreSecrets)
		return val
	case string:
		if val != "" && isValueCredentialKey(parent, key) {
			return Redacted
		}
		return maskInlineSecrets(val)
	default:
		return v
	}
}

func scrubSliceInPlace(key string, slice []any, itemsAreSecrets bool) {
	switch key {
	case "env":
		redactEnvSlice(slice)
	case "httpHeaders":
		redactHeaderSlice(slice)
	case "command", "args":
		redactArgSlice(slice)
		return
	}
	for i, elem := range slice {
		switch sub := elem.(type) {
		case map[string]any:
			scrubMapInPlace(sub, key, itemsAreSecrets, false)
		case string:
			slice[i] = maskInlineSecrets(sub)
		}
	}
}

func pruneSystemFields(m map[string]any, parent string) {
	switch parent {
	case "metadata":
		delete(m, "managedFields")
		delete(m, "selfLink")
	case "annotations":
		for k := range m {
			if isAppliedAnnotation(k) {
				delete(m, k)
			}
		}
	case "status":
		delete(m, "images")
	}
}

func isSecretObjectMap(m map[string]any) bool {
	if mapKindEquals(m, "Secret") {
		return true
	}
	if _, hasStringData := m["stringData"].(map[string]any); hasStringData {
		return true
	}
	t, _ := m["type"].(string)
	if t == "Opaque" || strings.HasPrefix(t, "kubernetes.io/") || strings.HasPrefix(t, "bootstrap.kubernetes.io/") || strings.HasPrefix(t, "helm.sh/") {
		return true
	}
	return false
}

func mapKindEquals(m map[string]any, want string) bool {
	k, _ := m["kind"].(string)
	return k == want
}

func redactSecretField(m map[string]any, field string) {
	sub, ok := m[field].(map[string]any)
	if !ok {
		return
	}
	for k := range sub {
		sub[k] = Redacted
	}
}

func redactEnvSlice(envs []any) {
	for _, item := range envs {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if val, ok := entry["value"].(string); ok && val != "" {
			entry["value"] = Redacted
		}
	}
}

func redactHeaderSlice(hdrs []any) {
	for _, item := range hdrs {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		name, _ := entry["name"].(string)
		val, ok := entry["value"].(string)
		if !ok || val == "" {
			continue
		}
		if isSensitiveHeader(name) {
			entry["value"] = Redacted
		} else {
			entry["value"] = maskInlineSecrets(val)
		}
	}
}

func redactArgSlice(args []any) {
	for i := 0; i < len(args); i++ {
		s, ok := args[i].(string)
		if !ok {
			continue
		}
		if i+1 < len(args) && reBareFlag.MatchString(s) {
			if _, nextStr := args[i+1].(string); nextStr {
				args[i+1] = Redacted
				i++
				continue
			}
		}
		args[i] = maskInlineSecrets(s)
	}
}

func maskInlineSecrets(s string) string {
	if s == "" {
		return s
	}
	s = rePEMBlock.ReplaceAllString(s, Redacted)
	s = reURLUserinfo.ReplaceAllString(s, "${1}:"+Redacted+"@")
	s = reInlineFlag.ReplaceAllString(s, "${1}"+Redacted)
	return s
}

func stripAppliedAnnotations(ann map[string]string) map[string]string {
	if len(ann) == 0 {
		return ann
	}
	out := make(map[string]string, len(ann))
	for k, v := range ann {
		if !isAppliedAnnotation(k) {
			out[k] = maskInlineSecrets(v)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func isAppliedAnnotation(k string) bool {
	lk := strings.ToLower(k)
	return lk == "kubectl.kubernetes.io/last-applied-configuration" ||
		lk == "kapp.k14s.io/original" ||
		strings.Contains(lk, "last-applied") ||
		strings.Contains(lk, "applied-config") ||
		strings.Contains(lk, "original-configuration")
}

func isSensitiveHeader(name string) bool {
	ln := strings.ToLower(strings.TrimSpace(name))
	switch ln {
	case "authorization", "proxy-authorization", "cookie", "set-cookie", "x-api-key", "x-auth-token":
		return true
	default:
		return isValueCredentialKey("httpHeaders", ln)
	}
}

// isValueCredentialKey matches keys whose string value holds credential
// material while excluding Kubernetes object-reference fields (secretName,
// secretRef, key, name, imagePullSecrets, serviceAccountName).
func isValueCredentialKey(parent, key string) bool {
	if parent == "secretKeyRef" || parent == "configMapKeyRef" {
		return false
	}
	lk := strings.ToLower(key)
	switch lk {
	case "secretname", "secretref", "name", "key", "serviceaccountname", "serviceaccount", "tokenreviews":
		return false
	}
	if strings.HasSuffix(lk, "secretname") || strings.HasSuffix(lk, "secretref") {
		return false
	}
	for _, needle := range []string{
		"password", "passwd", "passphrase", "secret", "token",
		"api_key", "apikey", "api-key",
		"private_key", "privatekey", "private-key",
		"client_key", "clientkey", "tls.key", ".dockerconfigjson",
	} {
		if strings.Contains(lk, needle) {
			return true
		}
	}
	return false
}
