// Command fetch-gbif refreshes the class/order/family of every species in
// data/metadata.json from the GBIF Backbone (CC0).
//
// Lookups use the full binomial, because a genus name is only unique within a
// kingdom: Antigone is a crane and a venus clam, Microtarsus a bulbul and an
// aphid. What the backbone cannot resolve is left alone and reported in
// data/validation/gbif_ambiguous.json.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tphakala/openfauna/internal/dataset"
)

// kingdom constrains every lookup: nothing in a bioacoustics dictionary is a
// plant, fungus or bacterium, only a name that collides with one.
const kingdom = "Animalia"

const (
	metadataPath  = "data/metadata.json"
	aliasesPath   = "data/aliases.json"
	ambiguousPath = "data/validation/gbif_ambiguous.json"
)

// gbifMatch is one /species/match hit. The verbose form fills Alternatives,
// which is where a homonym becomes visible.
type gbifMatch struct {
	Rank         string      `json:"rank"`
	MatchType    string      `json:"matchType"`
	Kingdom      string      `json:"kingdom"`
	Class        string      `json:"class"`
	Order        string      `json:"order"`
	Family       string      `json:"family"`
	Alternatives []gbifMatch `json:"alternatives,omitempty"`
}

func (m gbifMatch) taxonomy() dataset.Taxonomy {
	return dataset.Taxonomy{Class: m.Class, Order: m.Order, Family: m.Family}
}

// genusOf splits a "Genus species" key. Keys that are not binomials are sound
// labels rather than taxa ("Camera", "Siren"), and matching the bare word
// returns whatever genus shares it.
func genusOf(name string) (string, bool) {
	genus, epithet, found := strings.Cut(name, " ")
	if !found || genus == "" || epithet == "" || strings.Contains(epithet, " ") {
		return "", false
	}
	return genus, true
}

// speciesTaxonomy accepts a hit on the binomial itself. MatchType HIGHERRANK
// means GBIF knew the genus but not the species and fell back to it, which is
// the guess this tool exists to avoid.
func speciesTaxonomy(m gbifMatch) (dataset.Taxonomy, bool) {
	if m.Kingdom != kingdom || m.MatchType != "EXACT" {
		return dataset.Taxonomy{}, false
	}
	if m.Rank != "SPECIES" && m.Rank != "SUBSPECIES" {
		return dataset.Taxonomy{}, false
	}
	return m.taxonomy(), true
}

// genusTaxonomy is the last resort, for a binomial the backbone does not carry.
// Equally exact hits that disagree on the class are a homonym, and are reported
// rather than picked between.
func genusTaxonomy(m gbifMatch) (dataset.Taxonomy, []string, bool) {
	if m.Kingdom != kingdom || m.MatchType != "EXACT" || m.Rank != "GENUS" {
		return dataset.Taxonomy{}, nil, false
	}
	classes := []string{m.Class}
	for _, alt := range m.Alternatives {
		if alt.Kingdom == kingdom && alt.MatchType == "EXACT" && alt.Rank == "GENUS" &&
			alt.Class != "" && alt.Class != m.Class {
			classes = append(classes, alt.Class)
		}
	}
	if len(classes) > 1 {
		sort.Strings(classes)
		return dataset.Taxonomy{}, classes, false
	}
	return m.taxonomy(), nil, true
}

// synonymsOf inverts the alias map. OpenFauna can be ahead of the backbone,
// which carries Brachypodius melanocephalos but not the canonical Microtarsus
// melanocephalos, so a legacy name is a second way to reach the right taxon.
func synonymsOf(aliases map[string]string) map[string][]string {
	byCanonical := make(map[string][]string, len(aliases))
	for legacy, canonical := range aliases {
		byCanonical[canonical] = append(byCanonical[canonical], legacy)
	}
	for _, names := range byCanonical {
		sort.Strings(names)
	}
	return byCanonical
}

type fetcher struct {
	client *http.Client
	mu     sync.Mutex
	cache  map[string]gbifMatch
}

func (f *fetcher) match(name string, verbose bool) (gbifMatch, error) {
	apiURL := "https://api.gbif.org/v1/species/match?strict=true&kingdom=" + kingdom +
		"&name=" + url.QueryEscape(name)
	if verbose {
		apiURL += "&verbose=true"
	}

	f.mu.Lock()
	cached, ok := f.cache[apiURL]
	f.mu.Unlock()
	if ok {
		return cached, nil
	}

	resp, err := f.client.Get(apiURL)
	if err != nil {
		return gbifMatch{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return gbifMatch{}, fmt.Errorf("status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return gbifMatch{}, err
	}
	var m gbifMatch
	if err := json.Unmarshal(body, &m); err != nil {
		return gbifMatch{}, err
	}

	f.mu.Lock()
	f.cache[apiURL] = m
	f.mu.Unlock()
	return m, nil
}

type resolution struct {
	taxonomy dataset.Taxonomy
	resolved bool
	classes  []string // the competing classes, when a homonym genus was refused
}

// resolve walks the binomial, the species' legacy names, then the genus.
func (f *fetcher) resolve(species string, synonyms []string) resolution {
	for _, name := range append([]string{species}, synonyms...) {
		m, err := f.match(name, false)
		if err != nil {
			log.Printf("%s: match failed: %v", name, err)
			continue
		}
		if tax, ok := speciesTaxonomy(m); ok {
			return resolution{taxonomy: tax, resolved: true}
		}
	}

	genus, ok := genusOf(species)
	if !ok {
		return resolution{}
	}
	m, err := f.match(genus, true)
	if err != nil {
		log.Printf("%s: genus match failed: %v", genus, err)
		return resolution{}
	}
	tax, classes, ok := genusTaxonomy(m)
	return resolution{taxonomy: tax, resolved: ok, classes: classes}
}

func main() {
	concurrency := flag.Int("concurrency", 10, "concurrent GBIF requests")
	flag.Parse()

	metadata, err := dataset.LoadMetadata(metadataPath)
	if err != nil {
		log.Fatalf("Failed to load metadata: %v", err)
	}
	aliases, err := loadAliases(aliasesPath)
	if err != nil {
		log.Fatalf("Failed to load aliases: %v", err)
	}
	synonyms := synonymsOf(aliases)

	var species, skipped []string
	for name := range metadata {
		if _, ok := genusOf(name); ok {
			species = append(species, name)
		} else {
			skipped = append(skipped, name)
		}
	}
	sort.Strings(species)
	sort.Strings(skipped)
	log.Printf("Matching %d binomials against the GBIF Backbone", len(species))
	if len(skipped) > 0 {
		log.Printf("Skipping %d keys that are not binomials: %s", len(skipped), strings.Join(skipped, ", "))
	}

	f := &fetcher{
		client: &http.Client{Timeout: 30 * time.Second},
		cache:  map[string]gbifMatch{},
	}
	results := make([]resolution, len(species))

	var wg sync.WaitGroup
	var done atomic.Int64
	sem := make(chan struct{}, *concurrency)
	for i, name := range species {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			results[i] = f.resolve(name, synonyms[name])
			if n := done.Add(1); n%500 == 0 {
				log.Printf("Matched %d/%d...", n, len(species))
			}
		}()
	}
	wg.Wait()

	ambiguous := map[string][]string{}
	changed, unresolved := 0, 0
	for i, name := range species {
		rec := metadata[name]
		rec.Taxonomy.FamilyCommon = "" // Always clear proprietary eBird family-common names
		if res := results[i]; res.resolved {
			if rec.Taxonomy != res.taxonomy {
				changed++
			}
			rec.Taxonomy = res.taxonomy
		} else {
			unresolved++
			if len(res.classes) > 0 {
				ambiguous[name] = res.classes
			}
		}
		metadata[name] = rec
	}
	log.Printf("Resolved %d species (%d taxonomy changes), %d unresolved, %d left to a homonym genus",
		len(species)-unresolved, changed, unresolved, len(ambiguous))

	if err := writeJSON(metadataPath, metadata); err != nil {
		log.Fatalf("Failed to write %s: %v", metadataPath, err)
	}
	report := map[string]any{
		"_comment": "Species whose binomial is absent from the GBIF Backbone and whose genus is a homonym " +
			"spanning more than one class. GBIF cannot disambiguate these, so their taxonomy is left " +
			"untouched and each needs a hand-checked entry. Written by cmd/fetch-gbif.",
		"species": ambiguous,
	}
	if err := writeJSON(ambiguousPath, report); err != nil {
		log.Fatalf("Failed to write %s: %v", ambiguousPath, err)
	}
	log.Println("Done!")
}

func loadAliases(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var aliases map[string]string
	if err := json.Unmarshal(data, &aliases); err != nil {
		return nil, err
	}
	return aliases, nil
}

func writeJSON(path string, v any) error {
	buf, err := dataset.MarshalJSON(v)
	if err != nil {
		return err
	}
	return os.WriteFile(path, buf, 0o644)
}
