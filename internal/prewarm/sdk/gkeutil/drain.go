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
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	drainWildcard    = "<*>"
	drainTreeDepth   = 2
	drainMaxChildren = 64
	drainSimThresh   = 0.5
	drainMaxLenKey   = 40
	drainMaxSample   = 200
)

// LogCluster represents one merged Drain log template and its occurrence stats.
type LogCluster struct {
	Template  string `json:"template"`
	Count     int    `json:"count"`
	Sources   int    `json:"sources,omitempty"`
	Level     string `json:"level,omitempty"`
	FirstSeen string `json:"first_seen,omitempty"`
	LastSeen  string `json:"last_seen,omitempty"`
	Sample    string `json:"sample"`
}

// Drainer clusters streaming or multi-pod log lines into Drain-style <*>
// templates in O(N) time using a fixed-depth prefix parse tree.
type Drainer struct {
	roots map[int]*drainNode
	all   []*drainCluster
}

type drainNode struct {
	children map[string]*drainNode
	clusters []*drainCluster
}

type drainCluster struct {
	template []string
	count    int
	sources  map[string]struct{}
	first    time.Time
	last     time.Time
	sample   string
	level    int
}

// NewDrainer returns an empty Drain log template clusterer.
func NewDrainer() *Drainer {
	return &Drainer{roots: make(map[int]*drainNode)}
}

// ClusterLogs clusters raw log lines (newline-separated or pre-split) and
// returns up to maxTemplates clusters ordered by highest severity first,
// then descending count.
func ClusterLogs(lines []string, maxTemplates int) []LogCluster {
	d := NewDrainer()
	for _, raw := range lines {
		for _, line := range strings.Split(raw, "\n") {
			d.Add("", time.Time{}, line)
		}
	}
	return d.Clusters(maxTemplates)
}

// Add inserts one log line from optional source (e.g. pod name) and optional
// timestamp ts into the parse tree.
func (d *Drainer) Add(source string, ts time.Time, line string) {
	line = strings.TrimSpace(line)
	if line == "" {
		return
	}
	tokens := tokenizeAndMask(line)
	if len(tokens) == 0 {
		return
	}

	node := d.leafFor(tokens)
	best := findBestCluster(node.clusters, tokens)
	lvl := detectLevel(line)
	if best != nil {
		mergeTemplate(best.template, tokens)
		best.observe(source, ts, lvl)
		return
	}

	c := &drainCluster{
		template: append([]string(nil), tokens...),
		sources:  make(map[string]struct{}),
		sample:   truncateUTF8(maskInlineSecrets(line), drainMaxSample),
	}
	c.observe(source, ts, lvl)
	node.clusters = append(node.clusters, c)
	d.all = append(d.all, c)
}

// Clusters returns the merged templates ordered by highest severity first,
// then descending count. If limit > 0, at most limit templates are returned.
func (d *Drainer) Clusters(limit int) []LogCluster {
	sorted := make([]*drainCluster, len(d.all))
	copy(sorted, d.all)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].level != sorted[j].level {
			return sorted[i].level > sorted[j].level
		}
		return sorted[i].count > sorted[j].count
	})
	if limit > 0 && len(sorted) > limit {
		sorted = sorted[:limit]
	}
	out := make([]LogCluster, len(sorted))
	for i, c := range sorted {
		lc := LogCluster{
			Template: strings.Join(c.template, " "),
			Count:    c.count,
			Sources:  len(c.sources),
			Level:    levelName(c.level),
			Sample:   c.sample,
		}
		if !c.first.IsZero() {
			lc.FirstSeen = c.first.UTC().Format(time.RFC3339)
		}
		if !c.last.IsZero() {
			lc.LastSeen = c.last.UTC().Format(time.RFC3339)
		}
		out[i] = lc
	}
	return out
}

func (d *Drainer) leafFor(tokens []string) *drainNode {
	key := min(len(tokens), drainMaxLenKey)
	node := d.roots[key]
	if node == nil {
		node = &drainNode{}
		d.roots[key] = node
	}
	for i := 0; i < drainTreeDepth && i < len(tokens); i++ {
		tok := branchKey(tokens[i])
		if node.children == nil {
			node.children = make(map[string]*drainNode)
		}
		child, ok := node.children[tok]
		if !ok {
			if len(node.children) >= drainMaxChildren {
				tok = drainWildcard
				child = node.children[tok]
			}
			if child == nil {
				child = &drainNode{}
				node.children[tok] = child
			}
		}
		node = child
	}
	return node
}

func findBestCluster(candidates []*drainCluster, tokens []string) *drainCluster {
	var best *drainCluster
	bestSim := 0.0
	for _, c := range candidates {
		if len(c.template) != len(tokens) {
			continue
		}
		if sim := tokenSimilarity(c.template, tokens); sim >= drainSimThresh && sim > bestSim {
			best, bestSim = c, sim
		}
	}
	return best
}

func (c *drainCluster) observe(source string, ts time.Time, lvl int) {
	c.count++
	if source != "" {
		c.sources[source] = struct{}{}
	}
	if lvl > c.level {
		c.level = lvl
	}
	if !ts.IsZero() {
		if c.first.IsZero() || ts.Before(c.first) {
			c.first = ts
		}
		if c.last.IsZero() || ts.After(c.last) {
			c.last = ts
		}
	}
}

func branchKey(tok string) string {
	if strings.Contains(tok, drainWildcard) || strings.ContainsAny(tok, "0123456789") {
		return drainWildcard
	}
	return tok
}

func tokenSimilarity(tpl, tokens []string) float64 {
	same := 0
	for i := range tpl {
		if tpl[i] == drainWildcard || tpl[i] == tokens[i] {
			same++
		}
	}
	return float64(same) / float64(len(tpl))
}

func mergeTemplate(tpl, tokens []string) {
	for i := range tpl {
		if tpl[i] == drainWildcard || tpl[i] == tokens[i] {
			continue
		}
		if k1, _, ok1 := strings.Cut(tpl[i], "="); ok1 && k1 != "" {
			if k2, _, ok2 := strings.Cut(tokens[i], "="); ok2 && k1 == k2 {
				tpl[i] = k1 + "=" + drainWildcard
				continue
			}
		}
		tpl[i] = drainWildcard
	}
}

var (
	reNum           = regexp.MustCompile(`^[+-]?\d+([.,]\d+)*%?$`)
	reDur           = regexp.MustCompile(`^\d+(\.\d+)?(ns|us|µs|ms|s|m|h)$`)
	reSize          = regexp.MustCompile(`(?i)^\d+(\.\d+)?[kmgtp]i?b$`)
	reHex           = regexp.MustCompile(`^(0[xX][0-9a-fA-F]+|[0-9a-fA-F]{8,})$`)
	reUUID          = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	reIP            = regexp.MustCompile(`^\d{1,3}(\.\d{1,3}){3}(:\d+)?$`)
	reISO           = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}([T ]\d{2}:\d{2}.*)?$`)
	reWallClock     = regexp.MustCompile(`^\d{2}:\d{2}:\d{2}([.,]\d+)?Z?$`)
	reExplicitLevel = regexp.MustCompile(`(?i)(?:^|[\s,{])"?(?:level|lvl|severity)"?\s*[:=]\s*"?(fatal|panic|crit(?:ical)?|err(?:or)?|warn(?:ing)?|info|debug|trace)"?(?:[\s,"}]|$)`)
	reKlogPrefix    = regexp.MustCompile(`^([FEWI])\d{4}\s+\d{2}:\d{2}:\d{2}`)
	reWordLevel     = regexp.MustCompile(`(?i)\b(FATAL|PANIC|CRITICAL|ERROR|WARN(?:ING)?|INFO)\b`)
)

func tokenizeAndMask(text string) []string {
	if flat, ok := flattenJSONLog(text); ok {
		text = flat
	}
	fields := strings.Fields(text)
	for i, f := range fields {
		fields[i] = maskToken(f, 0)
	}
	return fields
}

// flattenJSONLog normalizes compact single-line JSON structured logs (slog,
// zap, zerolog) into sorted key=value tokens so JSON lines without spaces
// cluster cleanly on their keys.
func flattenJSONLog(text string) (string, bool) {
	if len(text) < 2 || text[0] != '{' || text[len(text)-1] != '}' {
		return "", false
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(text), &m); err != nil || len(m) == 0 {
		return "", false
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(k)
		b.WriteByte('=')
		switch v := m[k].(type) {
		case string:
			// Replace whitespace inside string values with underscores when
			// value is variable-free? No: keep words space-separated after
			// k=firstWord so multi-word messages retain their fixed words!
			words := strings.Fields(v)
			if len(words) == 0 {
				b.WriteString(`""`)
			} else {
				b.WriteString(words[0])
				for _, w := range words[1:] {
					b.WriteByte(' ')
					b.WriteString(w)
				}
			}
		default:
			raw, _ := json.Marshal(v)
			b.Write(raw)
		}
	}
	return b.String(), true
}

func maskToken(tok string, depth int) string {
	if depth < 2 {
		if k, v, ok := strings.Cut(tok, "="); ok && k != "" && v != "" {
			return k + "=" + maskToken(v, depth+1)
		}
	}
	pre, core, suf := trimWrappingPunct(tok)
	if core != "" && isVariableCore(core) {
		return pre + drainWildcard + suf
	}
	return tok
}

func trimWrappingPunct(tok string) (string, string, string) {
	start, end := 0, len(tok)
	for start < end && strings.ContainsRune(`([{<"'`+"`", rune(tok[start])) {
		start++
	}
	for end > start && strings.ContainsRune(`)]}>"'.,;:`+"`", rune(tok[end-1])) {
		end--
	}
	return tok[:start], tok[start:end], tok[end:]
}

func isVariableCore(core string) bool {
	if reNum.MatchString(core) || reDur.MatchString(core) || reSize.MatchString(core) {
		return true
	}
	if reHex.MatchString(core) && strings.ContainsAny(core, "0123456789") {
		return true
	}
	return reUUID.MatchString(core) || reIP.MatchString(core) || reISO.MatchString(core) || reWallClock.MatchString(core)
}

func detectLevel(line string) int {
	if m := reExplicitLevel.FindStringSubmatch(line); len(m) == 2 {
		return classifyLevelWord(m[1])
	}
	if m := reKlogPrefix.FindStringSubmatch(line); len(m) == 2 {
		switch m[1] {
		case "F", "E":
			return 3
		case "W":
			return 2
		case "I":
			return 1
		}
	}
	if m := reWordLevel.FindStringSubmatch(line); len(m) == 2 {
		return classifyLevelWord(m[1])
	}
	return 0
}

func classifyLevelWord(w string) int {
	switch strings.ToLower(w) {
	case "fatal", "panic", "crit", "critical", "err", "error":
		return 3
	case "warn", "warning":
		return 2
	case "info":
		return 1
	default:
		return 0
	}
}

func levelName(lvl int) string {
	switch lvl {
	case 3:
		return "error"
	case 2:
		return "warn"
	case 1:
		return "info"
	default:
		return ""
	}
}

func truncateUTF8(s string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	s = strings.ToValidUTF8(s, "\ufffd")
	if len(s) <= maxBytes {
		return s
	}
	cut := maxBytes
	for cut > 0 && (s[cut]&0xC0) == 0x80 {
		cut--
	}
	return s[:cut] + "..."
}
