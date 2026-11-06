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
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/container/apiv1/containerpb"
	"golang.org/x/oauth2"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestRESTConfigFromClusterEndpointsAndValidation(t *testing.T) {
	ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "fake-token"})
	ca := []byte("-----BEGIN CERTIFICATE-----\nFAKE\n-----END CERTIFICATE-----\n")
	caB64 := base64.StdEncoding.EncodeToString(ca)

	t.Run("prefers DNS when AllowExternalTraffic nil or true", func(t *testing.T) {
		c := &containerpb.Cluster{
			Name:     "prod",
			Endpoint: "34.1.2.3",
			ControlPlaneEndpointsConfig: &containerpb.ControlPlaneEndpointsConfig{
				DnsEndpointConfig: &containerpb.ControlPlaneEndpointsConfig_DNSEndpointConfig{
					Endpoint: "gke-12345.us-central1.gke.goog",
				},
			},
		}
		cfg, usingDNS, err := RESTConfigFromCluster(c, ts)
		if err != nil {
			t.Fatalf("RESTConfigFromCluster: %v", err)
		}
		if !usingDNS || cfg.Host != "https://gke-12345.us-central1.gke.goog" || len(cfg.CAData) != 0 {
			t.Fatalf("unexpected DNS config: usingDNS=%v host=%q caLen=%d", usingDNS, cfg.Host, len(cfg.CAData))
		}
	})

	t.Run("falls back to IP endpoint when AllowExternalTraffic is false", func(t *testing.T) {
		allow := false
		c := &containerpb.Cluster{
			Name:     "private-dns",
			Endpoint: "2001:db8::1",
			MasterAuth: &containerpb.MasterAuth{
				ClusterCaCertificate: caB64,
			},
			ControlPlaneEndpointsConfig: &containerpb.ControlPlaneEndpointsConfig{
				DnsEndpointConfig: &containerpb.ControlPlaneEndpointsConfig_DNSEndpointConfig{
					Endpoint:             "gke-blocked.us-central1.gke.goog",
					AllowExternalTraffic: &allow,
				},
			},
		}
		cfg, usingDNS, err := RESTConfigFromCluster(c, ts)
		if err != nil {
			t.Fatalf("RESTConfigFromCluster: %v", err)
		}
		if usingDNS || cfg.Host != "https://[2001:db8::1]" || string(cfg.CAData) != string(ca) {
			t.Fatalf("unexpected IPv6 fallback: usingDNS=%v host=%q", usingDNS, cfg.Host)
		}
	})

	t.Run("rejects IP endpoint with empty CA certificate", func(t *testing.T) {
		c := &containerpb.Cluster{Name: "missing-ca", Endpoint: "10.0.0.2"}
		if _, _, err := RESTConfigFromCluster(c, ts); err == nil {
			t.Fatalf("expected error on missing CA certificate")
		}
	})

	t.Run("validates DialRESTConfig cluster coordinates", func(t *testing.T) {
		if _, _, err := DialRESTConfig(context.Background(), "", "us-central1", "c"); err == nil {
			t.Fatalf("expected error on empty project")
		}
		if _, _, err := DialRESTConfig(context.Background(), "p/bad", "us-central1", "c"); err == nil {
			t.Fatalf("expected error on slash in project")
		}
	})
}

func TestScrubSecretsFlagsHeadersURLsAndPreservesRefs(t *testing.T) {
	const rawSecret = "super-secret-db-password-999"
	const rawTLSKey = "MIIEvQIBADANBgkqhkiG9w0BAQEFAASCBKcwggSjAgEAAoIBAQC"

	// 1. SecretList & Secret with empty TypeMeta.Kind (wire/client-go shape)
	secList := &corev1.SecretList{
		Items: []corev1.Secret{{
			ObjectMeta: metav1.ObjectMeta{Namespace: "prod", Name: "tls-pair"},
			Type:       corev1.SecretTypeTLS,
			Data: map[string][]byte{
				"tls.key": []byte(rawTLSKey),
				"tls.crt": []byte("cert-bytes"),
			},
			StringData: map[string]string{
				"pin": rawSecret,
			},
		}},
	}
	scrubbedList, err := ScrubObject(secList)
	if err != nil {
		t.Fatal(err)
	}
	rawJSON, _ := json.Marshal(scrubbedList)
	if strings.Contains(string(rawJSON), rawSecret) || strings.Contains(string(rawJSON), rawTLSKey) || strings.Contains(string(rawJSON), "cert-bytes") {
		t.Fatalf("ScrubObject(SecretList) leaked secret payload: %s", string(rawJSON))
	}

	// Single Secret item with Kind == "" passed as map[string]any (unstructured item)
	var itemAsMap map[string]any
	itemBytes, _ := json.Marshal(secList.Items[0])
	_ = json.Unmarshal(itemBytes, &itemAsMap)
	scrubbedItem, err := ScrubObject(itemAsMap)
	if err != nil {
		t.Fatal(err)
	}
	rawItemJSON, _ := json.Marshal(scrubbedItem)
	if strings.Contains(string(rawItemJSON), rawTLSKey) || strings.Contains(string(rawItemJSON), rawSecret) {
		t.Fatalf("ScrubObject(untyped Secret map) leaked secret payload: %s", string(rawItemJSON))
	}

	// 2. Pod with CLI flags, URL userinfo, HTTP auth header, InitContainer failure, and Volume secretName
	list := &corev1.PodList{
		Items: []corev1.Pod{
			{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "prod",
					Name:      "checkout-init-fail",
					ManagedFields: []metav1.ManagedFieldsEntry{
						{Manager: "kubectl", FieldsV1: &metav1.FieldsV1{Raw: []byte(`{"f:spec":{},"pad":"` + strings.Repeat("x", 4096) + `"}`)}},
					},
					Annotations: map[string]string{
						"kubectl.kubernetes.io/last-applied-configuration": rawSecret,
						"dsn-hint": "postgres://app:" + rawSecret + "@10.0.0.8:5432/orders",
					},
				},
				Spec: corev1.PodSpec{
					Volumes: []corev1.Volume{{
						Name: "tls-vol",
						VolumeSource: corev1.VolumeSource{
							Secret: &corev1.SecretVolumeSource{SecretName: "orders-tls-cert"},
						},
					}},
					InitContainers: []corev1.Container{{
						Name:    "migrate",
						Image:   "us-docker.pkg.dev/demo/migrate:v1",
						Command: []string{"/migrate", "--db-password=" + rawSecret, "--token", rawSecret},
						Env:     []corev1.EnvVar{{Name: "MIGRATE_PASS", Value: rawSecret}},
					}},
					Containers: []corev1.Container{{
						Name:  "checkout",
						Image: "us-docker.pkg.dev/demo/checkout:v1",
						Args:  []string{"--dsn=postgres://admin:" + rawSecret + "@db:5432/app"},
						Env: []corev1.EnvVar{
							{Name: "DB_PASS", Value: rawSecret},
							{Name: "API_KEY", ValueFrom: &corev1.EnvVarSource{
								SecretKeyRef: &corev1.SecretKeySelector{
									LocalObjectReference: corev1.LocalObjectReference{Name: "api-creds"},
									Key:                  "token",
								},
							}},
						},
						LivenessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								HTTPGet: &corev1.HTTPGetAction{
									HTTPHeaders: []corev1.HTTPHeader{
										{Name: "Authorization", Value: "Bearer " + rawSecret},
									},
								},
							},
						},
					}},
				},
				Status: corev1.PodStatus{
					Phase: corev1.PodPending,
					InitContainerStatuses: []corev1.ContainerStatus{{
						Name:         "migrate",
						RestartCount: 4,
						State: corev1.ContainerState{
							Waiting: &corev1.ContainerStateWaiting{
								Reason:  "CrashLoopBackOff",
								Message: "dial postgres://app:" + rawSecret + "@10.0.0.8:5432 failed",
							},
						},
					}},
				},
			},
		},
	}

	abnormal := ScrubPodList(list, true)
	if len(abnormal) != 1 || abnormal[0].Restarts != 4 || len(abnormal[0].InitContainers) != 1 {
		t.Fatalf("expected abnormal init-failing pod with restarts=4 and init_containers, got %+v", abnormal)
	}
	summaryJSON, _ := json.Marshal(abnormal)
	if strings.Contains(string(summaryJSON), rawSecret) {
		t.Fatalf("ScrubPodList leaked secret in message: %s", string(summaryJSON))
	}

	scrubbedPodJSON, _ := json.Marshal(ScrubPod(&list.Items[0]))
	if strings.Contains(string(scrubbedPodJSON), rawSecret) {
		t.Fatalf("ScrubPod leaked secret in args/headers/annotations: %s", string(scrubbedPodJSON))
	}

	podObjMap, err := ScrubObject(&list.Items[0])
	if err != nil {
		t.Fatal(err)
	}
	podMapJSON, _ := json.Marshal(podObjMap)
	if strings.Contains(string(podMapJSON), rawSecret) || strings.Contains(string(podMapJSON), "managedFields") {
		t.Fatalf("ScrubObject(Pod) leaked secret or managedFields: %s", string(podMapJSON))
	}
	// Volume secret.secretName is a reference name, not secret material — must survive!
	if !strings.Contains(string(podMapJSON), "orders-tls-cert") {
		t.Fatalf("ScrubObject(Pod) falsely redacted volumes[].secret.secretName: %s", string(podMapJSON))
	}

	// 3. NetworkPolicy empty podSelector: {} must NOT be deleted!
	np := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Namespace: "prod", Name: "default-deny"},
		Spec:       networkingv1.NetworkPolicySpec{PodSelector: metav1.LabelSelector{}},
	}
	npMap, err := ScrubObject(np)
	if err != nil {
		t.Fatal(err)
	}
	specMap, _ := npMap["spec"].(map[string]any)
	if _, exists := specMap["podSelector"]; !exists {
		t.Fatalf("ScrubObject deleted empty spec.podSelector {}: %+v", npMap)
	}
}

func TestDrainClusterLogsPlainKlogAndJSON(t *testing.T) {
	d := NewDrainer()
	base := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)

	// Info lines containing "errors=0" must stay "info", not promote to "error"!
	for i := 0; i < 300; i++ {
		d.Add(
			fmt.Sprintf("checkout-%d", i%3),
			base.Add(time.Duration(i)*time.Second),
			fmt.Sprintf("2026-11-06T12:00:00Z level=info msg=healthcheck errors=0 latency=%dms", 5+(i%20)),
		)
	}
	// Compact JSON error logs (no spaces) must cluster into 1 error template!
	for i := 0; i < 50; i++ {
		d.Add(
			"checkout-1",
			base.Add(time.Duration(i)*time.Second),
			fmt.Sprintf(`{"time":"2026-11-06T12:01:%02dZ","level":"error","msg":"db pool exhausted","host":"10.8.4.%d","wait":"%dms"}`, i%60, i%10, 1500+i),
		)
	}
	// Kubernetes klog W-header lines must detect as "warn"!
	for i := 0; i < 20; i++ {
		d.Add(
			"kubelet",
			base,
			fmt.Sprintf("W1106 12:02:00.123456 1 reflector.go:424] watch ended duration=%ds", i+1),
		)
	}

	clusters := d.Clusters(10)
	if len(clusters) != 3 {
		t.Fatalf("expected 3 templates from 370 lines, got %d: %+v", len(clusters), clusters)
	}
	if clusters[0].Level != "error" || clusters[0].Count != 50 {
		t.Fatalf("expected JSON error cluster ranked #1 with count=50, got %+v", clusters[0])
	}
	if clusters[1].Level != "warn" || clusters[1].Count != 20 {
		t.Fatalf("expected klog warn cluster ranked #2 with count=20, got %+v", clusters[1])
	}
	if clusters[2].Level != "info" || clusters[2].Count != 300 || clusters[2].Sources != 3 {
		t.Fatalf("expected info cluster ranked #3 with count=300 sources=3, got %+v", clusters[2])
	}
}
