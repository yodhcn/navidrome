package main

import (
	"context"
	"encoding/json" // Reusing ErrNotFound from wikidata.go (implicitly via main)
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const dbpediaEndpoint = "https://dbpedia.org/sparql"

// Default timeout for DBpedia requests
const defaultDbpediaTimeout = 20 * time.Second

// Can potentially reuse SparqlResult, SparqlBindings, SparqlValue from wikidata.go
// if the structure is identical. Assuming it is for now.

// GetArtistBioFromDBpedia queries DBpedia for an artist's abstract using their name.
func GetArtistBioFromDBpedia(fetcher Fetcher, ctx context.Context, name string) (string, error) {
	log.Printf("[MCP] Debug: GetArtistBioFromDBpedia called for name: %s", name)
	if name == "" {
		log.Printf("[MCP] Error: GetArtistBioFromDBpedia requires a name.")
		return "", fmt.Errorf("name is required to query DBpedia by name")
	}

	// Escape name for SPARQL query literal
	escapedName := strings.ReplaceAll(name, "\"", "\\\"")

	// SPARQL query using DBpedia ontology (dbo)
	// Prefixes are recommended but can be omitted if endpoint resolves them.
	// Searching case-insensitively on the label.
	// Filtering for dbo:MusicalArtist or dbo:Band.
	// Selecting the English abstract.
	sparqlQuery := fmt.Sprintf(`
PREFIX dbo: <http://dbpedia.org/ontology/>
PREFIX rdfs: <http://www.w3.org/2000/01/rdf-schema#>
PREFIX rdf: <http://www.w3.org/1999/02/22-rdf-syntax-ns#>

SELECT DISTINCT ?abstract WHERE {
  ?artist rdfs:label ?nameLabel .
  FILTER(LCASE(STR(?nameLabel)) = LCASE("%s") && LANG(?nameLabel) = "en")
  
  # Ensure it's a musical artist or band
  { ?artist rdf:type dbo:MusicalArtist } UNION { ?artist rdf:type dbo:Band }
  
  ?artist dbo:abstract ?abstract .
  FILTER(LANG(?abstract) = "en")
} LIMIT 1`, escapedName)

	// Prepare and execute HTTP request
	queryValues := url.Values{}
	queryValues.Set("query", sparqlQuery)
	queryValues.Set("format", "application/sparql-results+json") // DBpedia standard format

	reqURL := fmt.Sprintf("%s?%s", dbpediaEndpoint, queryValues.Encode())
	log.Printf("[MCP] Debug: DBpedia Bio Request URL: %s", reqURL)

	timeout := defaultDbpediaTimeout
	if deadline, ok := ctx.Deadline(); ok {
		timeout = time.Until(deadline)
	}
	log.Printf("[MCP] Debug: Fetching from DBpedia with timeout: %v", timeout)

	statusCode, bodyBytes, err := fetcher.Fetch(ctx, "GET", reqURL, nil, timeout)
	if err != nil {
		log.Printf("[MCP] Error: Fetcher failed for DBpedia bio request (name: '%s'): %v", name, err)
		return "", fmt.Errorf("failed to execute DBpedia request: %w", err)
	}

	if statusCode != http.StatusOK {
		log.Printf("[MCP] Error: DBpedia bio query failed for name '%s' with status %d: %s", name, statusCode, string(bodyBytes))
		return "", fmt.Errorf("DBpedia query failed with status %d: %s", statusCode, string(bodyBytes))
	}
	log.Printf("[MCP] Debug: DBpedia bio query successful (status %d), %d bytes received.", statusCode, len(bodyBytes))

	var result SparqlResult
	if err := json.Unmarshal(bodyBytes, &result); err != nil {
		// Try reading the raw body for debugging if JSON parsing fails
		// (Seek back to the beginning might be needed if already read for error)
		// For simplicity, just return the parsing error now.
		log.Printf("[MCP] Error: Failed to decode DBpedia bio response for name '%s': %v", name, err)
		return "", fmt.Errorf("failed to decode DBpedia response: %w", err)
	}

	// Extract the abstract
	if len(result.Results.Bindings) > 0 {
		if abstractVal, ok := result.Results.Bindings[0]["abstract"]; ok {
			log.Printf("[MCP] Debug: Found DBpedia abstract for '%s'.", name)
			return abstractVal.Value, nil
		}
	}

	// Use the shared ErrNotFound
	log.Printf("[MCP] Warn: No abstract found on DBpedia for name '%s'.", name)
	return "", ErrNotFound
}

// GetArtistWikipediaURLFromDBpedia queries DBpedia for an artist's Wikipedia URL using their name.
func GetArtistWikipediaURLFromDBpedia(fetcher Fetcher, ctx context.Context, name string) (string, error) {
	log.Printf("[MCP] Debug: GetArtistWikipediaURLFromDBpedia called for name: %s", name)
	if name == "" {
		log.Printf("[MCP] Error: GetArtistWikipediaURLFromDBpedia requires a name.")
		return "", fmt.Errorf("name is required to query DBpedia by name for URL")
	}

	escapedName := strings.ReplaceAll(name, "\"", "\\\"")

	// SPARQL query using foaf:isPrimaryTopicOf
	sparqlQuery := fmt.Sprintf(`
PREFIX dbo: <http://dbpedia.org/ontology/>
PREFIX rdfs: <http://www.w3.org/2000/01/rdf-schema#>
PREFIX rdf: <http://www.w3.org/1999/02/22-rdf-syntax-ns#>
PREFIX foaf: <http://xmlns.com/foaf/0.1/>

SELECT DISTINCT ?wikiPage WHERE {
  ?artist rdfs:label ?nameLabel .
  FILTER(LCASE(STR(?nameLabel)) = LCASE("%s") && LANG(?nameLabel) = "en")

  { ?artist rdf:type dbo:MusicalArtist } UNION { ?artist rdf:type dbo:Band }

  ?artist foaf:isPrimaryTopicOf ?wikiPage .
  # Ensure it links to the English Wikipedia
  FILTER(STRSTARTS(STR(?wikiPage), "https://en.wikipedia.org/"))
} LIMIT 1`, escapedName)

	// Prepare and execute HTTP request (similar structure to bio query)
	queryValues := url.Values{}
	queryValues.Set("query", sparqlQuery)
	queryValues.Set("format", "application/sparql-results+json")

	reqURL := fmt.Sprintf("%s?%s", dbpediaEndpoint, queryValues.Encode())
	log.Printf("[MCP] Debug: DBpedia URL Request URL: %s", reqURL)

	timeout := defaultDbpediaTimeout
	if deadline, ok := ctx.Deadline(); ok {
		timeout = time.Until(deadline)
	}
	log.Printf("[MCP] Debug: Fetching DBpedia URL with timeout: %v", timeout)

	statusCode, bodyBytes, err := fetcher.Fetch(ctx, "GET", reqURL, nil, timeout)
	if err != nil {
		log.Printf("[MCP] Error: Fetcher failed for DBpedia URL request (name: '%s'): %v", name, err)
		return "", fmt.Errorf("failed to execute DBpedia URL request: %w", err)
	}

	if statusCode != http.StatusOK {
		log.Printf("[MCP] Error: DBpedia URL query failed for name '%s' with status %d: %s", name, statusCode, string(bodyBytes))
		return "", fmt.Errorf("DBpedia URL query failed with status %d: %s", statusCode, string(bodyBytes))
	}
	log.Printf("[MCP] Debug: DBpedia URL query successful (status %d), %d bytes received.", statusCode, len(bodyBytes))

	var result SparqlResult
	if err := json.Unmarshal(bodyBytes, &result); err != nil {
		log.Printf("[MCP] Error: Failed to decode DBpedia URL response for name '%s': %v", name, err)
		return "", fmt.Errorf("failed to decode DBpedia URL response: %w", err)
	}

	// Extract the URL
	if len(result.Results.Bindings) > 0 {
		if pageVal, ok := result.Results.Bindings[0]["wikiPage"]; ok {
			log.Printf("[MCP] Debug: Found DBpedia Wikipedia URL for '%s': %s", name, pageVal.Value)
			return pageVal.Value, nil
		}
	}

	log.Printf("[MCP] Warn: No Wikipedia URL found on DBpedia for name '%s'.", name)
	return "", ErrNotFound
}
