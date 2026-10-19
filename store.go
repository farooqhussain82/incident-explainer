package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

type TimelineEntry struct {
	At   time.Time `json:"at"`
	Text string    `json:"text"`
}

type Incident struct {
	ID         string     `json:"id"`
	Key        string     `json:"key"`
	Namespace  string     `json:"namespace"`
	Pod        string     `json:"pod"`
	Container  string     `json:"container"`
	Reason     string     `json:"reason"`
	Category   string     `json:"category"`
	Message    string     `json:"message"`
	Restarts   int        `json:"restarts"`
	Status     string     `json:"status"` // open | resolved
	Opened     time.Time  `json:"opened"`
	LastSeen   time.Time  `json:"last_seen"`
	Resolved   *time.Time `json:"resolved,omitempty"`
	CleanPolls int        `json:"-"`

	ExplainState string         `json:"explain_state"` // pending | running | done | failed
	ExplainError string         `json:"explain_error,omitempty"`
	Explanation  *Explanation   `json:"explanation,omitempty"`
	SentText     string         `json:"sent_text,omitempty"` // exactly what the model saw (redacted)
	Redactions   map[string]int `json:"redactions,omitempty"`
	Notified     bool           `json:"notified"`

	Timeline []TimelineEntry `json:"timeline"`
}

func (i *Incident) Log(format string, a ...any) {
	i.Timeline = append(i.Timeline, TimelineEntry{At: time.Now().UTC(), Text: fmt.Sprintf(format, a...)})
}

// Store keeps incidents in memory and in one JSON file. Small on purpose:
// a cluster that has thousands of open incidents has bigger problems.
type Store struct {
	mu   sync.Mutex
	path string
	next int
	all  map[string]*Incident
}

func OpenStore(path string) (*Store, error) {
	s := &Store{path: path, next: 1, all: map[string]*Incident{}}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	var list []*Incident
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	for _, inc := range list {
		s.all[inc.ID] = inc
		var n int
		if _, err := fmt.Sscanf(inc.ID, "INC-%d", &n); err == nil && n >= s.next {
			s.next = n + 1
		}
		// An explanation that was running when we restarted will never finish.
		if inc.ExplainState == "running" || inc.ExplainState == "pending" {
			inc.ExplainState = "pending"
		}
	}
	return s, nil
}

// Do runs fn with the lock held, then saves.
func (s *Store) Do(fn func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn()
	if err := s.saveLocked(); err != nil {
		fmt.Fprintln(os.Stderr, "save:", err)
	}
}

func (s *Store) saveLocked() error {
	if s.path == "" {
		return nil
	}
	data, err := json.MarshalIndent(s.listLocked(), "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(filepath.Dir(s.path), ".incidents.tmp")
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func (s *Store) NewIncident(p Problem) *Incident {
	inc := &Incident{
		ID: fmt.Sprintf("INC-%04d", s.next), Key: p.Key,
		Namespace: p.Namespace, Pod: p.Pod, Container: p.Container,
		Reason: p.Reason, Category: Category(p.Reason), Message: p.Message, Restarts: p.Restarts,
		Status: "open", Opened: time.Now().UTC(), LastSeen: time.Now().UTC(), ExplainState: "pending",
	}
	s.next++
	s.all[inc.ID] = inc
	return inc
}

func (s *Store) OpenByKey(key string) *Incident {
	for _, inc := range s.all {
		if inc.Status == "open" && inc.Key == key {
			return inc
		}
	}
	return nil
}

func (s *Store) Get(id string) *Incident {
	s.mu.Lock()
	defer s.mu.Unlock()
	if inc, ok := s.all[id]; ok {
		c := *inc
		return &c
	}
	return nil
}

// List returns copies, open incidents first, newest first.
func (s *Store) List() []Incident {
	s.mu.Lock()
	defer s.mu.Unlock()
	src := s.listLocked()
	out := make([]Incident, len(src))
	for i, inc := range src {
		out[i] = *inc
	}
	return out
}

func (s *Store) listLocked() []*Incident {
	out := make([]*Incident, 0, len(s.all))
	for _, inc := range s.all {
		out = append(out, inc)
	}
	sort.Slice(out, func(i, j int) bool {
		if (out[i].Status == "open") != (out[j].Status == "open") {
			return out[i].Status == "open"
		}
		return out[i].Opened.After(out[j].Opened)
	})
	return out
}
