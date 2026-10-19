package main

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// A rule masks one kind of sensitive value. If keepPrefix is set, the first
// capture group (a key name or URL scheme) is kept so the model still knows
// what was there, e.g. "DB_PASSWORD=[REDACTED-SECRET]".
type rule struct {
	label      string
	re         *regexp.Regexp
	keepPrefix bool
	suffix     string
}

var rules = []rule{
	{label: "PRIVATE-KEY", re: regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?-----END [A-Z ]*PRIVATE KEY-----`)},
	{label: "AWS-KEY", re: regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`)},
	{label: "JWT", re: regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`)},
	{label: "BEARER", re: regexp.MustCompile(`(?i)(bearer\s+)[A-Za-z0-9._~+/=-]{12,}`), keepPrefix: true},
	{label: "URL-CREDS", re: regexp.MustCompile(`(?i)\b([a-z][a-z0-9+.-]*://)[^\s:/@]+:[^\s@/]+@`), keepPrefix: true, suffix: "@"},
	// Values starting with "<" are kubectl placeholders like "<set to the key ...>", not secrets.
	{label: "SECRET", re: regexp.MustCompile(`(?i)\b([A-Z0-9_]*(?:password|passwd|secret|token|api[_-]?key|access[_-]?key)[A-Z0-9_]*["']?\s*[:=]\s*["']?)([^\s"',;<]{4,})`), keepPrefix: true},
	{label: "EMAIL", re: regexp.MustCompile(`\b[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}\b`)},
}

var ipRe = regexp.MustCompile(`\b(?:(?:25[0-5]|2[0-4]\d|1?\d?\d)\.){3}(?:25[0-5]|2[0-4]\d|1?\d?\d)\b`)

// Redact masks secrets and personal data before anything reaches the model.
// Single host IPs are masked; CIDR ranges and 0.0.0.0 are kept, because
// "open to 0.0.0.0/0" is often the whole point of a finding.
func Redact(text string, keepIPs bool) (string, map[string]int) {
	counts := map[string]int{}
	for _, r := range rules {
		r := r
		text = r.re.ReplaceAllStringFunc(text, func(m string) string {
			counts[r.label]++
			prefix := ""
			if r.keepPrefix {
				if sub := r.re.FindStringSubmatch(m); len(sub) > 1 {
					prefix = sub[1]
				}
			}
			return prefix + "[REDACTED-" + r.label + "]" + r.suffix
		})
	}
	if !keepIPs {
		text = redactIPs(text, counts)
	}
	return text, counts
}

func redactIPs(text string, counts map[string]int) string {
	var b strings.Builder
	last := 0
	for _, loc := range ipRe.FindAllStringIndex(text, -1) {
		start, end := loc[0], loc[1]
		ip := text[start:end]
		if ip == "0.0.0.0" || (end < len(text) && text[end] == '/') {
			continue
		}
		b.WriteString(text[last:start])
		b.WriteString("[REDACTED-IP]")
		counts["IP"]++
		last = end
	}
	b.WriteString(text[last:])
	return b.String()
}

// CountsString renders redaction counts as "2 ip, 1 secret".
func CountsString(counts map[string]int) string {
	if len(counts) == 0 {
		return "nothing"
	}
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%d %s", counts[k], strings.ToLower(k)))
	}
	return strings.Join(parts, ", ")
}

var signalRe = regexp.MustCompile(`(?i)(error|fail|fatal|panic|exception|traceback|killed|oom|denied|refused|timeout|timed out|back-?off|unhealthy|cannot|can't|unable|not found|no such|exit code|will be destroyed|must be replaced|forces replacement|warning)`)

// Trim fits text into the prompt budget. Lines that look like errors (and two
// lines around them) are kept first, then as much of the tail as fits.
func Trim(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	lines := strings.Split(text, "\n")
	keep := map[int]bool{}
	for i, l := range lines {
		if signalRe.MatchString(l) {
			for j := max(0, i-2); j < min(len(lines), i+3); j++ {
				keep[j] = true
			}
		}
	}
	idx := make([]int, 0, len(keep))
	for i := range keep {
		idx = append(idx, i)
	}
	sort.Ints(idx)
	picked := make([]string, 0, len(idx))
	for _, i := range idx {
		picked = append(picked, lines[i])
	}
	head := strings.Join(picked, "\n")
	if len(head) > limit/2 {
		head = head[:limit/2]
	}
	tailLen := limit - len(head) - 100
	tail := text[len(text)-tailLen:]
	return head + "\n\n[... trimmed, last part of the input follows ...]\n\n" + tail
}
