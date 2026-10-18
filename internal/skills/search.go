package skills

import (
	"fmt"
	"math"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"unicode"
)

// Filter narrows a listing or a search. Kind is a job kind (mapped as in KindsFor) or a catalog kind;
// Repo a repository path or folder name (entries whose projects include it); Status one status. With
// no status, deprecated and draft entries are left out.
type Filter struct {
	Kind   string
	Repo   string
	Status string
}

// Validate rejects an unknown status.
func (f Filter) Validate() error {
	if f.Status != "" && !slices.Contains(Statuses, f.Status) {
		return fmt.Errorf("status %q must be one of %s", f.Status, strings.Join(Statuses, "|"))
	}
	return nil
}

func (f Filter) match(e Entry, search bool) bool {
	switch {
	case f.Status == "" && (e.Status == StatusDeprecated || e.Status == StatusDraft):
		return false
	case f.Status != "" && e.Status != f.Status:
		return false
	case search && e.Status == StatusDeprecated:
		return false // a search never returns a deprecated entry
	}
	if f.Kind != "" {
		kinds := append(KindsFor(f.Kind), strings.ToLower(f.Kind))
		if !slices.ContainsFunc(e.Kinds, func(k string) bool { return slices.Contains(kinds, k) }) {
			return false
		}
	}
	if f.Repo != "" && !slices.Contains(e.Projects, filepath.Base(f.Repo)) {
		return false
	}
	return true
}

// List returns the entries the filter matches, sorted by name.
func (c *Catalog) List(f Filter) []Entry {
	var out []Entry
	if c == nil {
		return out
	}
	for _, e := range c.Entries {
		if f.match(e, false) {
			out = append(out, e)
		}
	}
	return out
}

// Hit is one search result.
type Hit struct {
	Entry
	Score float64 `json:"score"`
}

// SearchLimit is how many hits Search returns at most.
const SearchLimit = 10

// maxBodyBytes is how much of a SKILL.md body the search scores.
const maxBodyBytes = 4 << 10

// Field weights of the search: the name and the description count more than the body.
const (
	weightName  = 3.0
	weightDesc  = 2.0
	weightMeta  = 1.5 // kinds and projects
	weightBody  = 1.0
	weightAffix = 0.5 // a document term that starts with the query term (word forms: ревью / ревьюер)
	bm25K1      = 1.2
	bm25B       = 0.75
)

// Tokenize lowercases s and splits it into runs of letters and digits (any script, so Cyrillic and
// Latin alike).
func Tokenize(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
}

type doc struct {
	tf  map[string]float64 // weighted term frequency
	len float64
}

func makeDoc(e Entry) doc {
	d := doc{tf: map[string]float64{}}
	add := func(text string, w float64) {
		for _, t := range Tokenize(text) {
			d.tf[t] += w
			d.len += w
		}
	}
	add(strings.NewReplacer("-", " ", "_", " ", ".", " ").Replace(e.Name), weightName)
	add(e.descFull, weightDesc)
	if e.descFull == "" {
		add(e.Description, weightDesc)
	}
	add(strings.Join(e.Kinds, " ")+" "+strings.Join(e.Projects, " "), weightMeta)
	body := e.body
	if len(body) > maxBodyBytes {
		body = body[:maxBodyBytes]
	}
	add(body, weightBody)
	return d
}

// tfOf is the weighted frequency of query term q in d: exact matches, plus affix matches (a document
// term that starts with q, for q of 4 runes or more) at weightAffix.
func (d doc) tfOf(q string) float64 {
	f := d.tf[q]
	if len([]rune(q)) >= 4 {
		for t, n := range d.tf {
			if t != q && strings.HasPrefix(t, q) {
				f += n * weightAffix
			}
		}
	}
	return f
}

// Search scores the entries the filter matches against the query (BM25 over weighted fields: name,
// front matter description, kinds and projects, the first 4 KB of the SKILL.md body) and returns the
// best limit hits (SearchLimit when limit ≤ 0), best first. Deterministic and local: no model call.
func (c *Catalog) Search(query string, f Filter, limit int) []Hit {
	if limit <= 0 {
		limit = SearchLimit
	}
	terms := Tokenize(query)
	slices.Sort(terms)
	terms = slices.Compact(terms)
	var cands []Entry
	if c != nil {
		for _, e := range c.Entries {
			if f.match(e, true) {
				cands = append(cands, e)
			}
		}
	}
	if len(terms) == 0 || len(cands) == 0 {
		return []Hit{}
	}
	docs := make([]doc, len(cands))
	avg := 0.0
	for i, e := range cands {
		docs[i] = makeDoc(e)
		avg += docs[i].len
	}
	avg /= float64(len(docs))
	if avg == 0 {
		avg = 1
	}
	n := float64(len(docs))
	hits := []Hit{}
	for i, d := range docs {
		score := 0.0
		for _, q := range terms {
			tf := d.tfOf(q)
			if tf == 0 {
				continue
			}
			df := 0.0
			for _, o := range docs {
				if o.tfOf(q) > 0 {
					df++
				}
			}
			idf := math.Log(1 + (n-df+0.5)/(df+0.5))
			score += idf * tf * (bm25K1 + 1) / (tf + bm25K1*(1-bm25B+bm25B*d.len/avg))
		}
		if score > 0 {
			hits = append(hits, Hit{Entry: cands[i], Score: math.Round(score*1000) / 1000})
		}
	}
	sort.SliceStable(hits, func(a, b int) bool {
		if hits[a].Score != hits[b].Score {
			return hits[a].Score > hits[b].Score
		}
		return hits[a].Name < hits[b].Name
	})
	if len(hits) > limit {
		hits = hits[:limit]
	}
	return hits
}
