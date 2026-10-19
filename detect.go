package main

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Problem is one unhealthy container (or pod) seen in a single poll.
type Problem struct {
	Key       string // namespace/pod/container: one incident per container
	Namespace string
	Pod       string
	Container string
	Reason    string
	Message   string
	Restarts  int
}

// Waiting reasons that always mean something is wrong.
var badWaiting = map[string]bool{
	"CrashLoopBackOff":           true,
	"ImagePullBackOff":           true,
	"ErrImagePull":               true,
	"InvalidImageName":           true,
	"CreateContainerConfigError": true,
	"CreateContainerError":       true,
	"RunContainerError":          true,
}

// A container that crashed this recently still counts as broken, even if it is
// "Running" again right now. This stops crash loops from flapping open/closed.
const recentCrash = 10 * time.Minute

// notReadyAfter is how long a running container may stay unready before it
// becomes an incident (slow starters are normal).
const notReadyAfter = 3 * time.Minute

// Detect returns the problems for one pod. now is passed in for tests.
func Detect(p Pod, now time.Time) []Problem {
	var out []Problem
	add := func(container, reason, msg string, restarts int) {
		out = append(out, Problem{
			Key:       p.Metadata.Namespace + "/" + p.Metadata.Name + "/" + container,
			Namespace: p.Metadata.Namespace, Pod: p.Metadata.Name, Container: container,
			Reason: reason, Message: msg, Restarts: restarts,
		})
	}

	if p.Status.Phase == "Pending" {
		for _, c := range p.Status.Conditions {
			if c.Type == "PodScheduled" && c.Status == "False" && c.Reason == "Unschedulable" {
				add("-", "Unschedulable", c.Message, 0)
				return out
			}
		}
	}

	for _, cs := range p.Status.ContainerStatuses {
		last := cs.LastState.Terminated
		recentlyCrashed := last != nil && last.ExitCode != 0 && now.Sub(last.FinishedAt) < recentCrash

		switch {
		// OOMKilled shows up as CrashLoopBackOff + lastState OOMKilled. Name the real cause.
		case last != nil && last.Reason == "OOMKilled" && recentlyCrashed:
			add(cs.Name, "OOMKilled", fmt.Sprintf("killed for using too much memory (exit %d)", last.ExitCode), cs.RestartCount)
		case cs.State.Waiting != nil && badWaiting[cs.State.Waiting.Reason]:
			reason := cs.State.Waiting.Reason
			if reason == "ErrImagePull" {
				reason = "ImagePullBackOff" // same incident, avoid flapping between the two
			}
			add(cs.Name, reason, cs.State.Waiting.Message, cs.RestartCount)
		case cs.State.Terminated != nil && cs.State.Terminated.ExitCode != 0 && p.Spec.RestartPolicy == "Never":
			t := cs.State.Terminated
			add(cs.Name, "Failed", fmt.Sprintf("%s (exit %d)", t.Reason, t.ExitCode), cs.RestartCount)
		case recentlyCrashed:
			add(cs.Name, "CrashLoopBackOff", fmt.Sprintf("last run ended with %s (exit %d)", last.Reason, last.ExitCode), cs.RestartCount)
		case cs.State.Running != nil && !cs.Ready && now.Sub(cs.State.Running.StartedAt) > notReadyAfter:
			add(cs.Name, "NotReady", "running but failing its readiness check", cs.RestartCount)
		}
	}
	return out
}

// Category is a coarse label for the UI, from the Kubernetes reason.
func Category(reason string) string {
	switch reason {
	case "CrashLoopBackOff", "Failed":
		return "crash-loop"
	case "ImagePullBackOff", "InvalidImageName":
		return "image-pull"
	case "OOMKilled":
		return "out-of-memory"
	case "CreateContainerConfigError", "CreateContainerError", "RunContainerError":
		return "config-or-secret"
	case "Unschedulable":
		return "scheduling"
	case "NotReady":
		return "probe-failure"
	}
	return "unknown"
}

// Evidence builds a describe-like text for the model: pod facts, container
// specs (env var NAMES only, never values), events and logs.
func Evidence(ctx context.Context, k *Kube, pr Problem) (string, error) {
	p, err := k.GetPod(ctx, pr.Namespace, pr.Pod)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "## Pod %s/%s\n", p.Metadata.Namespace, p.Metadata.Name)
	fmt.Fprintf(&b, "Phase: %s  Node: %s  Age: %s\n", p.Status.Phase, p.Spec.NodeName,
		time.Since(p.Metadata.CreationTimestamp).Round(time.Second))
	for _, o := range p.Metadata.OwnerReferences {
		fmt.Fprintf(&b, "Owned by: %s/%s\n", o.Kind, o.Name)
	}
	if p.Status.Reason != "" || p.Status.Message != "" {
		fmt.Fprintf(&b, "Pod status: %s %s\n", p.Status.Reason, p.Status.Message)
	}
	for _, c := range p.Status.Conditions {
		if c.Status != "True" {
			fmt.Fprintf(&b, "Condition %s=%s %s %s\n", c.Type, c.Status, c.Reason, c.Message)
		}
	}

	for _, c := range p.Spec.Containers {
		fmt.Fprintf(&b, "\n## Container %s\nImage: %s\n", c.Name, c.Image)
		if len(c.Command)+len(c.Args) > 0 {
			fmt.Fprintf(&b, "Command: %s\n", strings.Join(append(c.Command, c.Args...), " "))
		}
		if len(c.Resources.Limits)+len(c.Resources.Requests) > 0 {
			fmt.Fprintf(&b, "Limits: %v  Requests: %v\n", c.Resources.Limits, c.Resources.Requests)
		}
		for _, e := range c.Env {
			switch {
			case e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil:
				fmt.Fprintf(&b, "Env var %s comes from Secret %q key %q\n", e.Name, e.ValueFrom.SecretKeyRef.Name, e.ValueFrom.SecretKeyRef.Key)
			case e.ValueFrom != nil && e.ValueFrom.ConfigMapKeyRef != nil:
				fmt.Fprintf(&b, "Env var %s comes from ConfigMap %q key %q\n", e.Name, e.ValueFrom.ConfigMapKeyRef.Name, e.ValueFrom.ConfigMapKeyRef.Key)
			default:
				fmt.Fprintf(&b, "Env var %s is set (value not shown)\n", e.Name)
			}
		}
		if len(c.LivenessProbe) > 0 {
			fmt.Fprintf(&b, "Liveness probe: %s\n", c.LivenessProbe)
		}
		if len(c.ReadinessProbe) > 0 {
			fmt.Fprintf(&b, "Readiness probe: %s\n", c.ReadinessProbe)
		}
	}

	restarts := 0
	for _, cs := range p.Status.ContainerStatuses {
		restarts += cs.RestartCount
		fmt.Fprintf(&b, "\n## Status of container %s\nReady: %v  Restarts: %d\n", cs.Name, cs.Ready, cs.RestartCount)
		writeState(&b, "State", cs.State)
		writeState(&b, "Last state", cs.LastState)
	}

	b.WriteString("\n## Events\n")
	evs := k.Events(ctx, pr.Namespace, pr.Pod)
	if len(evs) == 0 {
		b.WriteString("(none)\n")
	}
	for _, e := range evs {
		fmt.Fprintf(&b, "%s %s x%d: %s\n", e.Type, e.Reason, max(e.Count, 1), e.Message)
	}

	if pr.Container != "-" {
		if restarts > 0 {
			fmt.Fprintf(&b, "\n## Logs from the previous (crashed) run of %s\n%s\n", pr.Container,
				k.Logs(ctx, pr.Namespace, pr.Pod, pr.Container, true, 200))
		}
		fmt.Fprintf(&b, "\n## Current logs of %s\n%s\n", pr.Container,
			k.Logs(ctx, pr.Namespace, pr.Pod, pr.Container, false, 200))
	}
	return b.String(), nil
}

func writeState(b *strings.Builder, label string, s ContainerState) {
	switch {
	case s.Waiting != nil:
		fmt.Fprintf(b, "%s: Waiting (%s) %s\n", label, s.Waiting.Reason, s.Waiting.Message)
	case s.Running != nil:
		fmt.Fprintf(b, "%s: Running since %s\n", label, s.Running.StartedAt.Format(time.RFC3339))
	case s.Terminated != nil:
		fmt.Fprintf(b, "%s: Terminated (%s) exit code %d %s\n", label, s.Terminated.Reason, s.Terminated.ExitCode, s.Terminated.Message)
	}
}
