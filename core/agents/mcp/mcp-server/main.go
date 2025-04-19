package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"

	mcp_golang "github.com/metoro-io/mcp-golang"
	"github.com/metoro-io/mcp-golang/transport/stdio"
)

type Content struct {
	Title       string  `json:"title" jsonschema:"required,description=The title to submit"`
	Description *string `json:"description" jsonschema:"description=The description to submit"`
}
type MyFunctionsArguments struct {
	Submitter string  `json:"submitter" jsonschema:"required,description=The name of the thing calling this tool (openai, google, claude, etc)"`
	Content   Content `json:"content" jsonschema:"required,description=The content of the message"`
}

type ArtistBiography struct {
	ID   string `json:"id" jsonschema:"required,description=The id of the artist"`
	Name string `json:"name" jsonschema:"required,description=The name of the artist"`
	MBID string `json:"mbid" jsonschema:"description=The mbid of the artist"`
}

type ArtistURLArgs struct {
	ID   string `json:"id" jsonschema:"required,description=The id of the artist"`
	Name string `json:"name" jsonschema:"required,description=The name of the artist"`
	MBID string `json:"mbid" jsonschema:"description=The mbid of the artist"`
}

func main() {
	done := make(chan struct{})

	// Create the appropriate fetcher (native or WASM based on build tags)
	// The context passed here is just background; specific calls might use request-scoped contexts.
	fetcher := NewFetcher()

	// --- Command Line Flag Handling ---
	nameFlag := flag.String("name", "", "Artist name to query directly")
	mbidFlag := flag.String("mbid", "", "Artist MBID to query directly")
	flag.Parse()

	if *nameFlag != "" || *mbidFlag != "" {
		fmt.Println("--- Running Tools Directly ---")

		// Call getArtistBiography
		fmt.Printf("Calling get_artist_biography (Name: '%s', MBID: '%s')...\n", *nameFlag, *mbidFlag)
		if *mbidFlag == "" && *nameFlag == "" {
			fmt.Println("  Error: --mbid or --name is required for get_artist_biography")
		} else {
			// Use context.Background for CLI calls
			bio, bioErr := getArtistBiography(fetcher, context.Background(), "cli", *nameFlag, *mbidFlag)
			if bioErr != nil {
				fmt.Printf("  Error: %v\n", bioErr)
			} else {
				fmt.Printf("  Result: %s\n", bio)
			}
		}

		// Call getArtistURL
		fmt.Printf("Calling get_artist_url (Name: '%s', MBID: '%s')...\n", *nameFlag, *mbidFlag)
		if *mbidFlag == "" && *nameFlag == "" {
			fmt.Println("  Error: --mbid or --name is required for get_artist_url")
		} else {
			urlResult, urlErr := getArtistURL(fetcher, context.Background(), "cli", *nameFlag, *mbidFlag)
			if urlErr != nil {
				fmt.Printf("  Error: %v\n", urlErr)
			} else {
				fmt.Printf("  Result: %s\n", urlResult)
			}
		}

		fmt.Println("-----------------------------")
		return // Exit after direct execution
	}
	// --- End Command Line Flag Handling ---

	server := mcp_golang.NewServer(stdio.NewStdioServerTransport())
	err := server.RegisterTool("hello", "Say hello to a person", func(arguments MyFunctionsArguments) (*mcp_golang.ToolResponse, error) {
		return mcp_golang.NewToolResponse(mcp_golang.NewTextContent(fmt.Sprintf("Hello, %server!", arguments.Submitter))), nil
	})
	if err != nil {
		panic(err)
	}

	err = server.RegisterTool("get_artist_biography", "Get the biography of an artist", func(arguments ArtistBiography) (*mcp_golang.ToolResponse, error) {
		bio, err := getArtistBiography(fetcher, context.Background(), arguments.ID, arguments.Name, arguments.MBID)
		if err != nil {
			return nil, err
		}
		return mcp_golang.NewToolResponse(mcp_golang.NewTextContent(bio)), nil
	})
	if err != nil {
		panic(err)
	}

	err = server.RegisterTool("get_artist_url", "Get the artist's specific Wikipedia URL via MBID, or a search URL using name as fallback", func(arguments ArtistURLArgs) (*mcp_golang.ToolResponse, error) {
		urlResult, err := getArtistURL(fetcher, context.Background(), arguments.ID, arguments.Name, arguments.MBID)
		if err != nil {
			return nil, err
		}
		return mcp_golang.NewToolResponse(mcp_golang.NewTextContent(urlResult)), nil
	})
	if err != nil {
		panic(err)
	}

	err = server.RegisterPrompt("prompt_test", "This is a test prompt", func(arguments Content) (*mcp_golang.PromptResponse, error) {
		return mcp_golang.NewPromptResponse("description", mcp_golang.NewPromptMessage(mcp_golang.NewTextContent(fmt.Sprintf("Hello, %server!", arguments.Title)), mcp_golang.RoleUser)), nil
	})
	if err != nil {
		panic(err)
	}

	err = server.RegisterResource("test://resource", "resource_test", "This is a test resource", "application/json", func() (*mcp_golang.ResourceResponse, error) {
		return mcp_golang.NewResourceResponse(mcp_golang.NewTextEmbeddedResource("test://resource", "This is a test resource", "application/json")), nil
	})
	if err != nil {
		panic(err)
	}

	err = server.RegisterResource("file://app_logs", "app_logs", "The app logs", "text/plain", func() (*mcp_golang.ResourceResponse, error) {
		return mcp_golang.NewResourceResponse(mcp_golang.NewTextEmbeddedResource("file://app_logs", "This is a test resource", "text/plain")), nil
	})
	if err != nil {
		panic(err)
	}

	err = server.Serve()
	if err != nil {
		panic(err)
	}

	<-done
}

func getArtistBiography(fetcher Fetcher, ctx context.Context, id, name, mbid string) (string, error) {
	if mbid == "" {
		fmt.Fprintf(os.Stderr, "MBID not provided, attempting DBpedia lookup by name: %s\n", name)
	} else {
		// 1. Attempt Wikidata MBID lookup first
		wikiURL, err := GetArtistWikipediaURL(fetcher, ctx, mbid)
		if err == nil {
			// 1a. Found Wikidata URL, now fetch from Wikipedia API
			bio, errBio := GetBioFromWikipediaAPI(fetcher, ctx, wikiURL)
			if errBio == nil {
				return bio, nil // Success via Wikidata/Wikipedia!
			} else {
				// Failed to get bio even though URL was found
				fmt.Fprintf(os.Stderr, "Found Wikipedia URL (%s) via MBID %s, but failed to fetch bio: %v\n", wikiURL, mbid, errBio)
				// Fall through to try DBpedia by name as a last resort?
				// Let's fall through for now.
			}
		} else if !errors.Is(err, ErrNotFound) {
			// Wikidata lookup failed for a reason other than not found (e.g., network)
			fmt.Fprintf(os.Stderr, "Wikidata URL lookup failed for MBID %s (non-NotFound error): %v\n", mbid, err)
			// Don't proceed to DBpedia name lookup if Wikidata had a technical failure
			return "", fmt.Errorf("Wikidata lookup failed: %w", err)
		} else {
			// Wikidata lookup returned ErrNotFound for MBID
			fmt.Fprintf(os.Stderr, "MBID %s not found on Wikidata, attempting DBpedia lookup by name: %s\n", mbid, name)
		}
	}

	// 2. Attempt DBpedia lookup by name (if MBID was missing or failed with ErrNotFound)
	if name == "" {
		return "", fmt.Errorf("cannot find artist: MBID lookup failed or MBID not provided, and no name provided for DBpedia fallback")
	}
	dbpediaBio, errDb := GetArtistBioFromDBpedia(fetcher, ctx, name)
	if errDb == nil {
		return dbpediaBio, nil // Success via DBpedia!
	}

	// 3. If both Wikidata (MBID) and DBpedia (Name) failed
	if errors.Is(errDb, ErrNotFound) {
		return "", fmt.Errorf("artist '%s' (MBID: %s) not found via Wikidata MBID or DBpedia Name lookup", name, mbid)
	}

	// Return DBpedia's error if it wasn't ErrNotFound
	return "", fmt.Errorf("DBpedia lookup failed for name '%s': %w", name, errDb)
}

// getArtistURL attempts to find the specific Wikipedia URL using MBID (via Wikidata),
// then by Name (via DBpedia), falling back to a search URL using name.
func getArtistURL(fetcher Fetcher, ctx context.Context, id, name, mbid string) (string, error) {
	if mbid == "" {
		fmt.Fprintf(os.Stderr, "getArtistURL: MBID not provided, attempting DBpedia lookup by name: %s\n", name)
	} else {
		// Try to get the specific URL from Wikidata using MBID
		wikiURL, err := GetArtistWikipediaURL(fetcher, ctx, mbid)
		if err == nil && wikiURL != "" {
			return wikiURL, nil // Found specific URL via MBID
		}
		// Log error if Wikidata lookup failed for reasons other than not found
		if err != nil && !errors.Is(err, ErrNotFound) {
			fmt.Fprintf(os.Stderr, "getArtistURL: Wikidata URL lookup failed for MBID %s (non-NotFound error): %v\n", mbid, err)
			// Fall through to try DBpedia if name is available
		} else if errors.Is(err, ErrNotFound) {
			fmt.Fprintf(os.Stderr, "getArtistURL: MBID %s not found on Wikidata, attempting DBpedia lookup by name: %s\n", mbid, name)
		}
	}

	// Fallback 1: Try DBpedia lookup by name
	if name != "" {
		dbpediaWikiURL, errDb := GetArtistWikipediaURLFromDBpedia(fetcher, ctx, name)
		if errDb == nil && dbpediaWikiURL != "" {
			return dbpediaWikiURL, nil // Found specific URL via DBpedia Name lookup
		}
		// Log error if DBpedia lookup failed for reasons other than not found
		if errDb != nil && !errors.Is(errDb, ErrNotFound) {
			fmt.Fprintf(os.Stderr, "getArtistURL: DBpedia URL lookup failed for name '%s' (non-NotFound error): %v\n", name, errDb)
			// Fall through to search URL fallback
		} else if errors.Is(errDb, ErrNotFound) {
			fmt.Fprintf(os.Stderr, "getArtistURL: Name '%s' not found on DBpedia, attempting search fallback\n", name)
		}
	}

	// Fallback 2: Generate a search URL if name is provided
	if name != "" {
		searchURL := fmt.Sprintf("https://en.wikipedia.org/w/index.php?search=%s", url.QueryEscape(name))
		fmt.Fprintf(os.Stderr, "getArtistURL: Falling back to search URL: %s\n", searchURL)
		return searchURL, nil
	}

	// Final error: MBID lookup failed (or no MBID given) AND no name provided for fallback
	return "", fmt.Errorf("cannot generate Wikipedia URL: Wikidata/DBpedia lookups failed and no artist name provided for search fallback")
}
