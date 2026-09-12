package main

import "testing"

func TestGenusOf(t *testing.T) {
	if genus, ok := genusOf("Antigone canadensis"); !ok || genus != "Antigone" {
		t.Errorf("genusOf(binomial) = %q, %v; want Antigone, true", genus, ok)
	}
	for _, name := range []string{"Siren", "Camera", "", "Gracilinanus microtarsus guahybae"} {
		if _, ok := genusOf(name); ok {
			t.Errorf("genusOf(%q) accepted a non-binomial", name)
		}
	}
}

func TestSpeciesTaxonomy(t *testing.T) {
	crane := gbifMatch{Kingdom: "Animalia", MatchType: "EXACT", Rank: "SPECIES", Class: "Aves", Order: "Gruiformes", Family: "Gruidae"}
	tax, ok := speciesTaxonomy(crane)
	if !ok || tax.Family != "Gruidae" {
		t.Errorf("exact species match = %+v, %v; want the crane", tax, ok)
	}

	// GBIF has no Microtarsus eutilotus and falls back to the genus, the aphid.
	aphidGenus := gbifMatch{Kingdom: "Animalia", MatchType: "HIGHERRANK", Rank: "GENUS", Class: "Insecta", Family: "Aphididae"}
	if _, ok := speciesTaxonomy(aphidGenus); ok {
		t.Error("speciesTaxonomy accepted a HIGHERRANK fallback")
	}

	fuzzy := gbifMatch{Kingdom: "Animalia", MatchType: "FUZZY", Rank: "SPECIES", Class: "Aves"}
	if _, ok := speciesTaxonomy(fuzzy); ok {
		t.Error("speciesTaxonomy accepted a fuzzy match")
	}

	// Corypha africana is a lark here and a palm to GBIF; the lookup must fall
	// through to the legacy name Mirafra africana.
	palm := gbifMatch{Kingdom: "Plantae", MatchType: "EXACT", Rank: "SPECIES", Class: "Liliopsida", Family: "Arecaceae"}
	if _, ok := speciesTaxonomy(palm); ok {
		t.Error("speciesTaxonomy accepted a match outside Animalia")
	}
}

func TestGenusTaxonomyRejectsHomonym(t *testing.T) {
	// Microtarsus Shinji, 1929 (aphid) and Microtarsus Eyton, 1839 (bulbul).
	homonym := gbifMatch{
		Kingdom: "Animalia", MatchType: "EXACT", Rank: "GENUS", Class: "Insecta", Family: "Aphididae",
		Alternatives: []gbifMatch{{Kingdom: "Animalia", MatchType: "EXACT", Rank: "GENUS", Class: "Aves", Family: "Pycnonotidae"}},
	}
	tax, classes, ok := genusTaxonomy(homonym)
	if ok {
		t.Fatalf("genusTaxonomy resolved a homonym to %+v", tax)
	}
	if len(classes) != 2 || classes[0] != "Aves" || classes[1] != "Insecta" {
		t.Errorf("competing classes = %v; want [Aves Insecta]", classes)
	}
}

func TestGenusTaxonomyAcceptsUnambiguous(t *testing.T) {
	// A fuzzy near-miss in another class is noise, not a homonym.
	unambiguous := gbifMatch{
		Kingdom: "Animalia", MatchType: "EXACT", Rank: "GENUS", Class: "Aves", Family: "Gruidae",
		Alternatives: []gbifMatch{{Kingdom: "Animalia", MatchType: "FUZZY", Rank: "GENUS", Class: "Insecta"}},
	}
	tax, classes, ok := genusTaxonomy(unambiguous)
	if !ok || tax.Family != "Gruidae" || classes != nil {
		t.Errorf("genusTaxonomy = %+v, %v, %v; want the crane genus", tax, classes, ok)
	}
}

func TestSynonymsOf(t *testing.T) {
	got := synonymsOf(map[string]string{
		"Brachypodius melanocephalos": "Microtarsus melanocephalos",
		"Pycnonotus melanocephalos":   "Microtarsus melanocephalos",
		"Carduelis hornemanni":        "Acanthis hornemanni",
	})
	names := got["Microtarsus melanocephalos"]
	if len(names) != 2 || names[0] != "Brachypodius melanocephalos" || names[1] != "Pycnonotus melanocephalos" {
		t.Errorf("synonyms = %v; want both legacy names, sorted", names)
	}
}
