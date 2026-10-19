package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"
)

// Kube is a tiny read-only Kubernetes API client. It only ever reads pods,
// pod logs and events. It never reads Secrets, and its RBAC role forbids it.
type Kube struct {
	base  string
	token string
	http  *http.Client
}

const saDir = "/var/run/secrets/kubernetes.io/serviceaccount"

// NewKube uses the in-cluster service account, or KUBE_API (for example
// http://127.0.0.1:8001 from `kubectl proxy`) when running on a laptop.
func NewKube() (*Kube, error) {
	if api := os.Getenv("KUBE_API"); api != "" {
		return &Kube{base: strings.TrimRight(api, "/"), http: &http.Client{Timeout: 30 * time.Second}}, nil
	}
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" {
		return nil, fmt.Errorf("not running in a cluster: set KUBE_API (e.g. run `kubectl proxy` and use http://127.0.0.1:8001)")
	}
	token, err := os.ReadFile(saDir + "/token")
	if err != nil {
		return nil, err
	}
	ca, err := os.ReadFile(saDir + "/ca.crt")
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(ca)
	return &Kube{
		base:  "https://" + host + ":" + port,
		token: strings.TrimSpace(string(token)),
		http: &http.Client{
			Timeout:   30 * time.Second,
			Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}},
		},
	}, nil
}

func (k *Kube) get(ctx context.Context, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, k.base+path, nil)
	if err != nil {
		return nil, err
	}
	if k.token != "" {
		req.Header.Set("Authorization", "Bearer "+k.token)
	}
	resp, err := k.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		return body, fmt.Errorf("GET %s: %s: %s", path, resp.Status, strings.TrimSpace(string(body)))
	}
	return body, nil
}

// --- the small subset of the Pod and Event API we need ---

type Pod struct {
	Metadata struct {
		Name              string    `json:"name"`
		Namespace         string    `json:"namespace"`
		UID               string    `json:"uid"`
		CreationTimestamp time.Time `json:"creationTimestamp"`
		OwnerReferences   []struct {
			Kind string `json:"kind"`
			Name string `json:"name"`
		} `json:"ownerReferences"`
	} `json:"metadata"`
	Spec struct {
		NodeName      string      `json:"nodeName"`
		RestartPolicy string      `json:"restartPolicy"`
		Containers    []Container `json:"containers"`
	} `json:"spec"`
	Status struct {
		Phase      string `json:"phase"`
		Reason     string `json:"reason"`
		Message    string `json:"message"`
		Conditions []struct {
			Type    string `json:"type"`
			Status  string `json:"status"`
			Reason  string `json:"reason"`
			Message string `json:"message"`
		} `json:"conditions"`
		ContainerStatuses []ContainerStatus `json:"containerStatuses"`
	} `json:"status"`
}

type Container struct {
	Name      string   `json:"name"`
	Image     string   `json:"image"`
	Command   []string `json:"command"`
	Args      []string `json:"args"`
	Resources struct {
		Limits   map[string]string `json:"limits"`
		Requests map[string]string `json:"requests"`
	} `json:"resources"`
	Env []struct {
		Name      string `json:"name"`
		ValueFrom *struct {
			SecretKeyRef *struct {
				Name string `json:"name"`
				Key  string `json:"key"`
			} `json:"secretKeyRef"`
			ConfigMapKeyRef *struct {
				Name string `json:"name"`
				Key  string `json:"key"`
			} `json:"configMapKeyRef"`
		} `json:"valueFrom"`
	} `json:"env"`
	LivenessProbe  json.RawMessage `json:"livenessProbe"`
	ReadinessProbe json.RawMessage `json:"readinessProbe"`
}

type ContainerState struct {
	Waiting *struct {
		Reason  string `json:"reason"`
		Message string `json:"message"`
	} `json:"waiting"`
	Running *struct {
		StartedAt time.Time `json:"startedAt"`
	} `json:"running"`
	Terminated *struct {
		Reason     string    `json:"reason"`
		ExitCode   int       `json:"exitCode"`
		Message    string    `json:"message"`
		FinishedAt time.Time `json:"finishedAt"`
	} `json:"terminated"`
}

type ContainerStatus struct {
	Name         string         `json:"name"`
	Ready        bool           `json:"ready"`
	RestartCount int            `json:"restartCount"`
	Image        string         `json:"image"`
	State        ContainerState `json:"state"`
	LastState    ContainerState `json:"lastState"`
}

type Event struct {
	Type          string    `json:"type"`
	Reason        string    `json:"reason"`
	Message       string    `json:"message"`
	Count         int       `json:"count"`
	LastTimestamp time.Time `json:"lastTimestamp"`
	EventTime     time.Time `json:"eventTime"`
}

func (k *Kube) ListPods(ctx context.Context) ([]Pod, error) {
	body, err := k.get(ctx, "/api/v1/pods")
	if err != nil {
		return nil, err
	}
	var list struct {
		Items []Pod `json:"items"`
	}
	return list.Items, json.Unmarshal(body, &list)
}

func (k *Kube) GetPod(ctx context.Context, ns, name string) (*Pod, error) {
	body, err := k.get(ctx, "/api/v1/namespaces/"+url.PathEscape(ns)+"/pods/"+url.PathEscape(name))
	if err != nil {
		return nil, err
	}
	var p Pod
	return &p, json.Unmarshal(body, &p)
}

func (k *Kube) Logs(ctx context.Context, ns, pod, container string, previous bool, tail int) string {
	q := url.Values{"container": {container}, "tailLines": {fmt.Sprint(tail)}}
	if previous {
		q.Set("previous", "true")
	}
	body, err := k.get(ctx, "/api/v1/namespaces/"+url.PathEscape(ns)+"/pods/"+url.PathEscape(pod)+"/log?"+q.Encode())
	if err != nil {
		return "(no logs: " + firstLine(err.Error()) + ")"
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		return "(empty)"
	}
	return string(body)
}

func (k *Kube) Events(ctx context.Context, ns, name string) []Event {
	q := url.Values{"fieldSelector": {"involvedObject.name=" + name}}
	body, err := k.get(ctx, "/api/v1/namespaces/"+url.PathEscape(ns)+"/events?"+q.Encode())
	if err != nil {
		return nil
	}
	var list struct {
		Items []Event `json:"items"`
	}
	if json.Unmarshal(body, &list) != nil {
		return nil
	}
	sort.Slice(list.Items, func(i, j int) bool { return eventTime(list.Items[i]).Before(eventTime(list.Items[j])) })
	return list.Items
}

func eventTime(e Event) time.Time {
	if !e.LastTimestamp.IsZero() {
		return e.LastTimestamp
	}
	return e.EventTime
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
