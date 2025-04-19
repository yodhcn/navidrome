package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"time"
)

const wikidataEndpoint = "https://query.wikidata.org/sparql"

// ErrNotFound indicates a specific item (like an artist or URL) was not found on Wikidata.
var ErrNotFound = errors.New("item not found on Wikidata")

// Wikidata SPARQL query result structures
type SparqlResult struct {
	Results SparqlBindings `json:"results"`
}

type SparqlBindings struct {
	Bindings []map[string]SparqlValue `json:"bindings"`
}

type SparqlValue struct {
	Type  string `json:"type"`
	Value string `json:"value"`
	Lang  string `json:"xml:lang,omitempty"` // Handle language tags like "en"
}

// GetArtistBioFromWikidata queries Wikidata for an artist's description using their MBID.
func GetArtistBioFromWikidata(client *http.Client, mbid string) (string, error) {
	if mbid == "" {
		return "", fmt.Errorf("MBID is required to query Wikidata")
	}

	// SPARQL query to find the English description for an entity with a specific MusicBrainz ID
	sparqlQuery := fmt.Sprintf(`
SELECT ?artistDescription WHERE {
  ?artist wdt:P434 "%s" . # P434 is the property for MusicBrainz artist ID
  OPTIONAL { 
    ?artist schema:description ?artistDescription .
    FILTER(LANG(?artistDescription) = "en")
  }
  SERVICE wikibase:label { bd:serviceParam wikibase:language "en". }
}
LIMIT 1`, mbid)

	// Prepare the HTTP request
	queryValues := url.Values{}
	queryValues.Set("query", sparqlQuery)
	queryValues.Set("format", "json")

	reqURL := fmt.Sprintf("%s?%s", wikidataEndpoint, queryValues.Encode())

	req, err := http.NewRequest("GET", reqURL, nil)
	if err != nil {
		return "", fmt.Errorf("failed to create Wikidata request: %w", err)
	}
	req.Header.Set("Accept", "application/sparql-results+json")
	req.Header.Set("User-Agent", "MCPGoServerExample/0.1 (https://example.com/contact)") // Good practice to identify your client

	// Execute the request
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to execute Wikidata request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// Attempt to read body for more error info, but don't fail if it doesn't work
		bodyBytes, readErr := io.ReadAll(resp.Body)
		errorMsg := "Could not read error body"
		if readErr == nil {
			errorMsg = string(bodyBytes)
		}
		return "", fmt.Errorf("Wikidata query failed with status %d: %s", resp.StatusCode, errorMsg)
	}

	// Parse the response
	var result SparqlResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("failed to decode Wikidata response: %w", err)
	}

	// Extract the description
	if len(result.Results.Bindings) > 0 {
		if descriptionVal, ok := result.Results.Bindings[0]["artistDescription"]; ok {
			return descriptionVal.Value, nil
		}
	}

	return "", fmt.Errorf("no English description found on Wikidata for MBID %s", mbid)
}

// GetArtistWikipediaURL queries Wikidata for an artist's English Wikipedia page URL using MBID.
// It tries searching by MBID first, then falls back to searching by name.
func GetArtistWikipediaURL(fetcher Fetcher, ctx context.Context, mbid string) (string, error) {
	// 1. Try finding by MBID
	if mbid == "" {
		return "", fmt.Errorf("MBID is required to find Wikipedia URL on Wikidata")
	} else {
		// SPARQL query to find the enwiki URL for an entity with a specific MusicBrainz ID
		sparqlQuery := fmt.Sprintf(`
SELECT ?article WHERE {
  ?artist wdt:P434 "%s" . # P434 is MusicBrainz artist ID
  ?article schema:about ?artist ;
           schema:isPartOf <https://en.wikipedia.org/> .
}
LIMIT 1`, mbid)

		foundURL, err := executeWikidataURLQuery(fetcher, ctx, sparqlQuery)
		if err == nil && foundURL != "" {
			return foundURL, nil // Found via MBID
		}
		fmt.Fprintf(os.Stderr, "Wikidata URL lookup via MBID %s failed: %v\n", mbid, err)
		return "", fmt.Errorf("Wikidata URL lookup via MBID failed: %w", err)
	}

	// Should not be reached if MBID is provided
	return "", fmt.Errorf("internal error: reached end of GetArtistWikipediaURL unexpectedly")
}

// executeWikidataURLQuery is a helper to run SPARQL and extract the first bound URL for '?article'.
func executeWikidataURLQuery(fetcher Fetcher, ctx context.Context, sparqlQuery string) (string, error) {
	queryValues := url.Values{}
	queryValues.Set("query", sparqlQuery)
	queryValues.Set("format", "json")

	reqURL := fmt.Sprintf("%s?%s", wikidataEndpoint, queryValues.Encode())

	// Directly use the fetcher
	// Note: Headers (Accept, User-Agent) are now handled by the Fetcher implementation
	// The WASM fetcher currently doesn't support setting them via the host func interface.
	// Timeout is handled via context for native, and passed to host func for WASM.
	// Let's use a default timeout here if not provided via context (e.g., 15s)
	// TODO: Consider making timeout configurable or passed down
	timeout := 15 * time.Second
	if deadline, ok := ctx.Deadline(); ok {
		timeout = time.Until(deadline)
	}

	statusCode, bodyBytes, err := fetcher.Fetch(ctx, "GET", reqURL, nil, timeout)
	if err != nil {
		return "", fmt.Errorf("failed to execute Wikidata request: %w", err)
	}

	// Check status code. Fetcher interface implies body might be returned even on error.
	if statusCode != http.StatusOK {
		return "", fmt.Errorf("Wikidata query failed with status %d: %s", statusCode, string(bodyBytes))
	}

	var result SparqlResult
	if err := json.Unmarshal(bodyBytes, &result); err != nil { // Use Unmarshal for byte slice
		return "", fmt.Errorf("failed to decode Wikidata response: %w", err)
	}

	if len(result.Results.Bindings) > 0 {
		if articleVal, ok := result.Results.Bindings[0]["article"]; ok {
			return articleVal.Value, nil
		}
	}

	return "", ErrNotFound
}
