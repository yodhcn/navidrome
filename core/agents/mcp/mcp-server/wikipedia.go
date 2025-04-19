package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const mediaWikiAPIEndpoint = "https://en.wikipedia.org/w/api.php"

// Structures for parsing MediaWiki API response (query extracts)
type MediaWikiQueryResult struct {
	Query MediaWikiQuery `json:"query"`
}

type MediaWikiQuery struct {
	Pages map[string]MediaWikiPage `json:"pages"`
}

type MediaWikiPage struct {
	PageID  int    `json:"pageid"`
	Ns      int    `json:"ns"`
	Title   string `json:"title"`
	Extract string `json:"extract"`
}

// Default timeout for Wikipedia API requests
const defaultWikipediaTimeout = 15 * time.Second

// GetBioFromWikipediaAPI fetches the introductory text of a Wikipedia page.
func GetBioFromWikipediaAPI(fetcher Fetcher, ctx context.Context, wikipediaURL string) (string, error) {
	log.Printf("[MCP] Debug: GetBioFromWikipediaAPI called for URL: %s", wikipediaURL)
	pageTitle, err := extractPageTitleFromURL(wikipediaURL)
	if err != nil {
		log.Printf("[MCP] Error: Could not extract title from Wikipedia URL '%s': %v", wikipediaURL, err)
		return "", fmt.Errorf("could not extract title from Wikipedia URL %s: %w", wikipediaURL, err)
	}
	log.Printf("[MCP] Debug: Extracted Wikipedia page title: %s", pageTitle)

	// Prepare API request parameters
	apiParams := url.Values{}
	apiParams.Set("action", "query")
	apiParams.Set("format", "json")
	apiParams.Set("prop", "extracts")    // Request page extracts
	apiParams.Set("exintro", "true")     // Get only the intro section
	apiParams.Set("explaintext", "true") // Get plain text instead of HTML
	apiParams.Set("titles", pageTitle)   // Specify the page title
	apiParams.Set("redirects", "1")      // Follow redirects

	reqURL := fmt.Sprintf("%s?%s", mediaWikiAPIEndpoint, apiParams.Encode())
	log.Printf("[MCP] Debug: MediaWiki API Request URL: %s", reqURL)

	timeout := defaultWikipediaTimeout
	if deadline, ok := ctx.Deadline(); ok {
		timeout = time.Until(deadline)
	}
	log.Printf("[MCP] Debug: Fetching from MediaWiki with timeout: %v", timeout)

	statusCode, bodyBytes, err := fetcher.Fetch(ctx, "GET", reqURL, nil, timeout)
	if err != nil {
		log.Printf("[MCP] Error: Fetcher failed for MediaWiki request (title: '%s'): %v", pageTitle, err)
		return "", fmt.Errorf("failed to execute MediaWiki request for title '%s': %w", pageTitle, err)
	}

	if statusCode != http.StatusOK {
		log.Printf("[MCP] Error: MediaWiki query for '%s' failed with status %d: %s", pageTitle, statusCode, string(bodyBytes))
		return "", fmt.Errorf("MediaWiki query for '%s' failed with status %d: %s", pageTitle, statusCode, string(bodyBytes))
	}
	log.Printf("[MCP] Debug: MediaWiki query successful (status %d), %d bytes received.", statusCode, len(bodyBytes))

	// Parse the response
	var result MediaWikiQueryResult
	if err := json.Unmarshal(bodyBytes, &result); err != nil {
		log.Printf("[MCP] Error: Failed to decode MediaWiki response for '%s': %v", pageTitle, err)
		return "", fmt.Errorf("failed to decode MediaWiki response for '%s': %w", pageTitle, err)
	}

	// Extract the text - MediaWiki API returns pages keyed by page ID
	for pageID, page := range result.Query.Pages {
		log.Printf("[MCP] Debug: Processing MediaWiki page ID: %s, Title: %s", pageID, page.Title)
		if page.Extract != "" {
			// Often includes a newline at the end, trim it
			log.Printf("[MCP] Debug: Found extract for '%s'. Length: %d", pageTitle, len(page.Extract))
			return strings.TrimSpace(page.Extract), nil
		}
	}

	log.Printf("[MCP] Warn: No extract found in MediaWiki response for title '%s'", pageTitle)
	return "", fmt.Errorf("no extract found in MediaWiki response for title '%s' (page might not exist or be empty)", pageTitle)
}

// extractPageTitleFromURL attempts to get the page title from a standard Wikipedia URL.
// Example: https://en.wikipedia.org/wiki/The_Beatles -> The_Beatles
func extractPageTitleFromURL(wikiURL string) (string, error) {
	parsedURL, err := url.Parse(wikiURL)
	if err != nil {
		return "", err
	}
	if parsedURL.Host != "en.wikipedia.org" {
		return "", fmt.Errorf("URL host is not en.wikipedia.org: %s", parsedURL.Host)
	}
	pathParts := strings.Split(strings.TrimPrefix(parsedURL.Path, "/"), "/")
	if len(pathParts) < 2 || pathParts[0] != "wiki" {
		return "", fmt.Errorf("URL path does not match /wiki/<title> format: %s", parsedURL.Path)
	}
	title := pathParts[1]
	if title == "" {
		return "", fmt.Errorf("extracted title is empty")
	}
	// URL Decode the title (e.g., %27 -> ')
	decodedTitle, err := url.PathUnescape(title)
	if err != nil {
		return "", fmt.Errorf("failed to decode title '%s': %w", title, err)
	}
	return decodedTitle, nil
}
