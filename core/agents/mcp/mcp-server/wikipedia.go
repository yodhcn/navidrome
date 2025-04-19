package main

import (
	"context"
	"encoding/json"
	"fmt"
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
	pageTitle, err := extractPageTitleFromURL(wikipediaURL)
	if err != nil {
		return "", fmt.Errorf("could not extract title from Wikipedia URL %s: %w", wikipediaURL, err)
	}

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

	timeout := defaultWikipediaTimeout
	if deadline, ok := ctx.Deadline(); ok {
		timeout = time.Until(deadline)
	}

	statusCode, bodyBytes, err := fetcher.Fetch(ctx, "GET", reqURL, nil, timeout)
	if err != nil {
		return "", fmt.Errorf("failed to execute MediaWiki request for title '%s': %w", pageTitle, err)
	}

	if statusCode != http.StatusOK {
		return "", fmt.Errorf("MediaWiki query for '%s' failed with status %d: %s", pageTitle, statusCode, string(bodyBytes))
	}

	// Parse the response
	var result MediaWikiQueryResult
	if err := json.Unmarshal(bodyBytes, &result); err != nil {
		return "", fmt.Errorf("failed to decode MediaWiki response for '%s': %w", pageTitle, err)
	}

	// Extract the text - MediaWiki API returns pages keyed by page ID
	for _, page := range result.Query.Pages {
		if page.Extract != "" {
			// Often includes a newline at the end, trim it
			return strings.TrimSpace(page.Extract), nil
		}
	}

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
