package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"
)

// maxPromptChars keeps the prompt small enough for a laptop-sized model.
const maxPromptChars = 12000

type Manager struct {
	kube         *Kube
	llm          *LLM
	store        *Store
	interval     time.Duration
	resolveAfter int // healthy polls in a row before an incident is resolved
	ignoreNS     map[string]bool
	webhook      string
	uiURL        string
	keepIPs      bool
	queue        chan string
}

// Run polls the cluster until ctx is done. One worker explains incidents one
// at a time, because a local model is happiest with one request at a time.
func (m *Manager) Run(ctx context.Context) {
	go m.explainWorker(ctx)

	// Pick up anything left unexplained from before a restart.
	for _, inc := range m.store.List() {
		if inc.Status == "open" && inc.ExplainState == "pending" {
			m.enqueue(inc.ID)
		}
	}

	t := time.NewTicker(m.interval)
	defer t.Stop()
	for {
		if err := m.poll(ctx); err != nil {
			log.Printf("poll: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (m *Manager) enqueue(id string) {
	select {
	case m.queue <- id:
	default:
		log.Printf("explain queue full, %s will be retried on restart", id)
	}
}

func (m *Manager) poll(ctx context.Context) error {
	pods, err := m.kube.ListPods(ctx)
	if err != nil {
		return err
	}
	now := time.Now()
	seen := map[string]Problem{}
	alive := map[string]bool{} // namespace/pod
	for _, p := range pods {
		if m.ignoreNS[p.Metadata.Namespace] {
			continue
		}
		alive[p.Metadata.Namespace+"/"+p.Metadata.Name] = true
		for _, pr := range Detect(p, now) {
			seen[pr.Key] = pr
		}
	}

	var toExplain, toNotify []string
	m.store.Do(func() {
		for key, pr := range seen {
			inc := m.store.OpenByKey(key)
			if inc == nil {
				inc = m.store.NewIncident(pr)
				inc.Log("Opened: %s. %s", pr.Reason, pr.Message)
				log.Printf("%s opened: %s %s", inc.ID, key, pr.Reason)
				toExplain = append(toExplain, inc.ID)
				continue
			}
			inc.LastSeen = now.UTC()
			inc.CleanPolls = 0
			if pr.Restarts != inc.Restarts {
				inc.Restarts = pr.Restarts
			}
			if pr.Reason != inc.Reason {
				inc.Log("Reason changed: %s -> %s", inc.Reason, pr.Reason)
				inc.Reason, inc.Category, inc.Message = pr.Reason, Category(pr.Reason), pr.Message
			}
		}
		for _, inc := range m.store.all {
			if inc.Status != "open" {
				continue
			}
			if _, still := seen[inc.Key]; still {
				continue
			}
			podKey := inc.Namespace + "/" + inc.Pod
			switch {
			case !alive[podKey]:
				m.resolve(inc, "pod no longer exists")
			default:
				inc.CleanPolls++
				if inc.CleanPolls >= m.resolveAfter {
					m.resolve(inc, fmt.Sprintf("healthy for %d checks in a row", inc.CleanPolls))
				}
			}
			if inc.Status == "resolved" && inc.Notified {
				toNotify = append(toNotify, inc.ID)
			}
		}
	})
	for _, id := range toExplain {
		m.enqueue(id)
	}
	for _, id := range toNotify {
		m.notify(m.store.Get(id))
	}
	return nil
}

func (m *Manager) resolve(inc *Incident, why string) {
	t := time.Now().UTC()
	inc.Status = "resolved"
	inc.Resolved = &t
	inc.Log("Resolved: %s", why)
	log.Printf("%s resolved: %s", inc.ID, why)
}

func (m *Manager) explainWorker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case id := <-m.queue:
			m.explain(ctx, id)
		}
	}
}

func (m *Manager) explain(ctx context.Context, id string) {
	inc := m.store.Get(id)
	if inc == nil {
		return
	}
	m.store.Do(func() { m.store.all[id].ExplainState = "running" })

	pr := Problem{Namespace: inc.Namespace, Pod: inc.Pod, Container: inc.Container, Reason: inc.Reason}
	raw, err := Evidence(ctx, m.kube, pr)
	if err != nil {
		m.fail(id, "collecting evidence: "+err.Error())
		return
	}
	clean, counts := Redact(raw, m.keepIPs)
	clean = Trim(clean, maxPromptChars)
	source := fmt.Sprintf("Kubernetes pod %s/%s, container %s, reported as %s", inc.Namespace, inc.Pod, inc.Container, inc.Reason)

	ex, err := m.llm.Explain(ctx, source, clean)
	if err != nil {
		m.store.Do(func() {
			i := m.store.all[id]
			i.SentText, i.Redactions = clean, counts
		})
		m.fail(id, err.Error())
		return
	}
	var notifyNow bool
	m.store.Do(func() {
		i := m.store.all[id]
		i.Explanation, i.SentText, i.Redactions = ex, clean, counts
		i.ExplainState, i.ExplainError = "done", ""
		i.Log("Explained by %s in %.1fs (confidence: %s). Redacted: %s", ex.Model, ex.Seconds, ex.Confidence, CountsString(counts))
		if i.Status == "open" && !i.Notified {
			i.Notified = true
			notifyNow = true
		}
	})
	log.Printf("%s explained in %.1fs", id, ex.Seconds)
	if notifyNow {
		m.notify(m.store.Get(id))
	}
}

func (m *Manager) fail(id, msg string) {
	m.store.Do(func() {
		i := m.store.all[id]
		i.ExplainState, i.ExplainError = "failed", msg
		i.Log("Explanation failed: %s", msg)
	})
	log.Printf("%s explain failed: %s", id, msg)
}

// notify posts a Slack-compatible message ({"text": ...}) to WEBHOOK_URL.
// Teams and Mattermost incoming webhooks accept the same shape.
func (m *Manager) notify(inc *Incident) {
	if m.webhook == "" || inc == nil {
		return
	}
	var b strings.Builder
	link := ""
	if m.uiURL != "" {
		link = " " + strings.TrimRight(m.uiURL, "/") + "/incidents/" + inc.ID
	}
	if inc.Status == "resolved" {
		fmt.Fprintf(&b, "✅ %s resolved: %s/%s (%s)%s", inc.ID, inc.Namespace, inc.Pod, inc.Reason, link)
	} else {
		fmt.Fprintf(&b, "🔥 %s %s/%s: %s%s", inc.ID, inc.Namespace, inc.Pod, inc.Reason, link)
		if inc.Explanation != nil {
			fmt.Fprintf(&b, "\n%s\nLikely cause: %s", inc.Explanation.Summary, inc.Explanation.LikelyCause)
			if len(inc.Explanation.NextSteps) > 0 {
				fmt.Fprintf(&b, "\nFirst check: %s", inc.Explanation.NextSteps[0])
			}
		}
	}
	body, _ := json.Marshal(map[string]string{"text": b.String()})
	resp, err := http.Post(m.webhook, "application/json", bytes.NewReader(body))
	if err != nil {
		log.Printf("notify %s: %v", inc.ID, err)
		return
	}
	resp.Body.Close()
}
