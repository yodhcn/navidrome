package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
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
	log.Println("[MCP] Starting mcp-server...")
	done := make(chan struct{})

	// Create the appropriate fetcher (native or WASM based on build tags)
	log.Printf("[MCP] Debug: Creating fetcher...")
	fetcher := NewFetcher()
	log.Printf("[MCP] Debug: Fetcher created successfully.")

	// --- Command Line Flag Handling ---
	nameFlag := flag.String("name", "", "Artist name to query directly")
	mbidFlag := flag.String("mbid", "", "Artist MBID to query directly")
	flag.Parse()

	if *nameFlag != "" || *mbidFlag != "" {
		log.Printf("[MCP] Debug: Running tools directly via CLI flags (Name: '%s', MBID: '%s')", *nameFlag, *mbidFlag)
		fmt.Println("--- Running Tools Directly ---")

		// Call getArtistBiography
		fmt.Printf("Calling get_artist_biography (Name: '%s', MBID: '%s')...\n", *nameFlag, *mbidFlag)
		if *mbidFlag == "" && *nameFlag == "" {
			fmt.Println("  Error: --mbid or --name is required for get_artist_biography")
		} else {
			// Use context.Background for CLI calls
			log.Printf("[MCP] Debug: CLI calling getArtistBiography...")
			bio, bioErr := getArtistBiography(fetcher, context.Background(), "cli", *nameFlag, *mbidFlag)
			if bioErr != nil {
				fmt.Printf("  Error: %v\n", bioErr)
				log.Printf("[MCP] Error: CLI getArtistBiography failed: %v", bioErr)
			} else {
				fmt.Printf("  Result: %s\n", bio)
				log.Printf("[MCP] Debug: CLI getArtistBiography succeeded.")
			}
		}

		// Call getArtistURL
		fmt.Printf("Calling get_artist_url (Name: '%s', MBID: '%s')...\n", *nameFlag, *mbidFlag)
		if *mbidFlag == "" && *nameFlag == "" {
			fmt.Println("  Error: --mbid or --name is required for get_artist_url")
		} else {
			log.Printf("[MCP] Debug: CLI calling getArtistURL...")
			urlResult, urlErr := getArtistURL(fetcher, context.Background(), "cli", *nameFlag, *mbidFlag)
			if urlErr != nil {
				fmt.Printf("  Error: %v\n", urlErr)
				log.Printf("[MCP] Error: CLI getArtistURL failed: %v", urlErr)
			} else {
				fmt.Printf("  Result: %s\n", urlResult)
				log.Printf("[MCP] Debug: CLI getArtistURL succeeded.")
			}
		}

		fmt.Println("-----------------------------")
		log.Printf("[MCP] Debug: CLI execution finished.")
		return // Exit after direct execution
	}
	// --- End Command Line Flag Handling ---

	log.Printf("[MCP] Debug: Initializing MCP server...")
	server := mcp_golang.NewServer(stdio.NewStdioServerTransport())

	log.Printf("[MCP] Debug: Registering tool 'hello'...")
	err := server.RegisterTool("hello", "Say hello to a person", func(arguments MyFunctionsArguments) (*mcp_golang.ToolResponse, error) {
		log.Printf("[MCP] Debug: Tool 'hello' called with args: %+v", arguments)
		return mcp_golang.NewToolResponse(mcp_golang.NewTextContent(fmt.Sprintf("Hello, %server!", arguments.Submitter))), nil
	})
	if err != nil {
		log.Fatalf("[MCP] Fatal: Failed to register tool 'hello': %v", err)
	}

	log.Printf("[MCP] Debug: Registering tool 'get_artist_biography'...")
	err = server.RegisterTool("get_artist_biography", "Get the biography of an artist", func(arguments ArtistBiography) (*mcp_golang.ToolResponse, error) {
		log.Printf("[MCP] Debug: Tool 'get_artist_biography' called with args: %+v", arguments)
		// Using background context in handlers as request context isn't passed through MCP library currently
		bio, err := getArtistBiography(fetcher, context.Background(), arguments.ID, arguments.Name, arguments.MBID)
		if err != nil {
			log.Printf("[MCP] Error: getArtistBiography handler failed: %v", err)
			return nil, fmt.Errorf("handler returned an error: %w", err) // Return structured error
		}
		log.Printf("[MCP] Debug: Tool 'get_artist_biography' succeeded.")
		return mcp_golang.NewToolResponse(mcp_golang.NewTextContent(bio)), nil
	})
	if err != nil {
		log.Fatalf("[MCP] Fatal: Failed to register tool 'get_artist_biography': %v", err)
	}

	log.Printf("[MCP] Debug: Registering tool 'get_artist_url'...")
	err = server.RegisterTool("get_artist_url", "Get the artist's specific Wikipedia URL via MBID, or a search URL using name as fallback", func(arguments ArtistURLArgs) (*mcp_golang.ToolResponse, error) {
		log.Printf("[MCP] Debug: Tool 'get_artist_url' called with args: %+v", arguments)
		urlResult, err := getArtistURL(fetcher, context.Background(), arguments.ID, arguments.Name, arguments.MBID)
		if err != nil {
			log.Printf("[MCP] Error: getArtistURL handler failed: %v", err)
			return nil, fmt.Errorf("handler returned an error: %w", err)
		}
		log.Printf("[MCP] Debug: Tool 'get_artist_url' succeeded.")
		return mcp_golang.NewToolResponse(mcp_golang.NewTextContent(urlResult)), nil
	})
	if err != nil {
		log.Fatalf("[MCP] Fatal: Failed to register tool 'get_artist_url': %v", err)
	}

	log.Printf("[MCP] Debug: Registering prompt 'prompt_test'...")
	err = server.RegisterPrompt("prompt_test", "This is a test prompt", func(arguments Content) (*mcp_golang.PromptResponse, error) {
		log.Printf("[MCP] Debug: Prompt 'prompt_test' called with args: %+v", arguments)
		return mcp_golang.NewPromptResponse("description", mcp_golang.NewPromptMessage(mcp_golang.NewTextContent(fmt.Sprintf("Hello, %server!", arguments.Title)), mcp_golang.RoleUser)), nil
	})
	if err != nil {
		log.Fatalf("[MCP] Fatal: Failed to register prompt 'prompt_test': %v", err)
	}

	log.Printf("[MCP] Debug: Registering resource 'test://resource'...")
	err = server.RegisterResource("test://resource", "resource_test", "This is a test resource", "application/json", func() (*mcp_golang.ResourceResponse, error) {
		log.Printf("[MCP] Debug: Resource 'test://resource' called")
		return mcp_golang.NewResourceResponse(mcp_golang.NewTextEmbeddedResource("test://resource", "This is a test resource", "application/json")), nil
	})
	if err != nil {
		log.Fatalf("[MCP] Fatal: Failed to register resource 'test://resource': %v", err)
	}

	log.Printf("[MCP] Debug: Registering resource 'file://app_logs'...")
	err = server.RegisterResource("file://app_logs", "app_logs", "The app logs", "text/plain", func() (*mcp_golang.ResourceResponse, error) {
		log.Printf("[MCP] Debug: Resource 'file://app_logs' called")
		return mcp_golang.NewResourceResponse(mcp_golang.NewTextEmbeddedResource("file://app_logs", "This is a test resource", "text/plain")), nil
	})
	if err != nil {
		log.Fatalf("[MCP] Fatal: Failed to register resource 'file://app_logs': %v", err)
	}

	log.Println("[MCP] MCP server initialized and starting to serve...")
	err = server.Serve()
	if err != nil {
		log.Fatalf("[MCP] Fatal: Server exited with error: %v", err)
	}

	log.Println("[MCP] Server exited cleanly.")
	<-done // Keep running until interrupted (though server.Serve() is blocking)
}

func getArtistBiography(fetcher Fetcher, ctx context.Context, id, name, mbid string) (string, error) {
	log.Printf("[MCP] Debug: getArtistBiography called (id: %s, name: %s, mbid: %s)", id, name, mbid)
	if mbid == "" {
		fmt.Fprintf(os.Stderr, "MBID not provided, attempting DBpedia lookup by name: %s\n", name)
		log.Printf("[MCP] Debug: MBID not provided, attempting DBpedia lookup by name: %s", name)
	} else {
		// 1. Attempt Wikidata MBID lookup first
		log.Printf("[MCP] Debug: Attempting Wikidata URL lookup for MBID: %s", mbid)
		wikiURL, err := GetArtistWikipediaURL(fetcher, ctx, mbid)
		if err == nil {
			// 1a. Found Wikidata URL, now fetch from Wikipedia API
			log.Printf("[MCP] Debug: Found Wikidata URL '%s', fetching bio from Wikipedia API...", wikiURL)
			bio, errBio := GetBioFromWikipediaAPI(fetcher, ctx, wikiURL)
			if errBio == nil {
				log.Printf("[MCP] Debug: Successfully fetched bio from Wikipedia API for '%s'.", name)
				return bio, nil // Success via Wikidata/Wikipedia!
			} else {
				// Failed to get bio even though URL was found
				log.Printf("[MCP] Error: Found Wikipedia URL (%s) via MBID %s, but failed to fetch bio: %v", wikiURL, mbid, errBio)
				fmt.Fprintf(os.Stderr, "Found Wikipedia URL (%s) via MBID %s, but failed to fetch bio: %v\n", wikiURL, mbid, errBio)
				// Fall through to try DBpedia by name as a last resort?
				// Let's fall through for now.
			}
		} else if !errors.Is(err, ErrNotFound) {
			// Wikidata lookup failed for a reason other than not found (e.g., network)
			log.Printf("[MCP] Error: Wikidata URL lookup failed for MBID %s (non-NotFound error): %v", mbid, err)
			fmt.Fprintf(os.Stderr, "Wikidata URL lookup failed for MBID %s (non-NotFound error): %v\n", mbid, err)
			// Don't proceed to DBpedia name lookup if Wikidata had a technical failure
			return "", fmt.Errorf("Wikidata lookup failed: %w", err)
		} else {
			// Wikidata lookup returned ErrNotFound for MBID
			log.Printf("[MCP] Debug: MBID %s not found on Wikidata, attempting DBpedia lookup by name: %s", mbid, name)
			fmt.Fprintf(os.Stderr, "MBID %s not found on Wikidata, attempting DBpedia lookup by name: %s\n", mbid, name)
		}
	}

	// 2. Attempt DBpedia lookup by name (if MBID was missing or failed with ErrNotFound)
	if name == "" {
		log.Printf("[MCP] Error: Cannot find artist bio: MBID lookup failed/missing, and no name provided.")
		return "", fmt.Errorf("cannot find artist: MBID lookup failed or MBID not provided, and no name provided for DBpedia fallback")
	}
	log.Printf("[MCP] Debug: Attempting DBpedia bio lookup by name: %s", name)
	dbpediaBio, errDb := GetArtistBioFromDBpedia(fetcher, ctx, name)
	if errDb == nil {
		log.Printf("[MCP] Debug: Successfully fetched bio from DBpedia for '%s'.", name)
		return dbpediaBio, nil // Success via DBpedia!
	}

	// 3. If both Wikidata (MBID) and DBpedia (Name) failed
	if errors.Is(errDb, ErrNotFound) {
		log.Printf("[MCP] Error: Artist '%s' (MBID: %s) not found via Wikidata or DBpedia name lookup.", name, mbid)
		return "", fmt.Errorf("artist '%s' (MBID: %s) not found via Wikidata MBID or DBpedia Name lookup", name, mbid)
	}

	// Return DBpedia's error if it wasn't ErrNotFound
	log.Printf("[MCP] Error: DBpedia lookup failed for name '%s': %v", name, errDb)
	return "", fmt.Errorf("DBpedia lookup failed for name '%s': %w", name, errDb)
}

// getArtistURL attempts to find the specific Wikipedia URL using MBID (via Wikidata),
// then by Name (via DBpedia), falling back to a search URL using name.
func getArtistURL(fetcher Fetcher, ctx context.Context, id, name, mbid string) (string, error) {
	log.Printf("[MCP] Debug: getArtistURL called (id: %s, name: %s, mbid: %s)", id, name, mbid)
	if mbid == "" {
		fmt.Fprintf(os.Stderr, "getArtistURL: MBID not provided, attempting DBpedia lookup by name: %s\n", name)
		log.Printf("[MCP] Debug: getArtistURL: MBID not provided, attempting DBpedia lookup by name: %s", name)
	} else {
		// Try to get the specific URL from Wikidata using MBID
		log.Printf("[MCP] Debug: getArtistURL: Attempting Wikidata URL lookup for MBID: %s", mbid)
		wikiURL, err := GetArtistWikipediaURL(fetcher, ctx, mbid)
		if err == nil && wikiURL != "" {
			log.Printf("[MCP] Debug: getArtistURL: Found specific URL '%s' via Wikidata MBID.", wikiURL)
			return wikiURL, nil // Found specific URL via MBID
		}
		// Log error if Wikidata lookup failed for reasons other than not found
		if err != nil && !errors.Is(err, ErrNotFound) {
			log.Printf("[MCP] Error: getArtistURL: Wikidata URL lookup failed for MBID %s (non-NotFound error): %v", mbid, err)
			fmt.Fprintf(os.Stderr, "getArtistURL: Wikidata URL lookup failed for MBID %s (non-NotFound error): %v\n", mbid, err)
			// Fall through to try DBpedia if name is available
		} else if errors.Is(err, ErrNotFound) {
			log.Printf("[MCP] Debug: getArtistURL: MBID %s not found on Wikidata, attempting DBpedia lookup by name: %s", mbid, name)
			fmt.Fprintf(os.Stderr, "getArtistURL: MBID %s not found on Wikidata, attempting DBpedia lookup by name: %s\n", mbid, name)
		}
	}

	// Fallback 1: Try DBpedia lookup by name
	if name != "" {
		log.Printf("[MCP] Debug: getArtistURL: Attempting DBpedia URL lookup by name: %s", name)
		dbpediaWikiURL, errDb := GetArtistWikipediaURLFromDBpedia(fetcher, ctx, name)
		if errDb == nil && dbpediaWikiURL != "" {
			log.Printf("[MCP] Debug: getArtistURL: Found specific URL '%s' via DBpedia Name lookup.", dbpediaWikiURL)
			return dbpediaWikiURL, nil // Found specific URL via DBpedia Name lookup
		}
		// Log error if DBpedia lookup failed for reasons other than not found
		if errDb != nil && !errors.Is(errDb, ErrNotFound) {
			log.Printf("[MCP] Error: getArtistURL: DBpedia URL lookup failed for name '%s' (non-NotFound error): %v", name, errDb)
			fmt.Fprintf(os.Stderr, "getArtistURL: DBpedia URL lookup failed for name '%s' (non-NotFound error): %v\n", name, errDb)
			// Fall through to search URL fallback
		} else if errors.Is(errDb, ErrNotFound) {
			log.Printf("[MCP] Debug: getArtistURL: Name '%s' not found on DBpedia, attempting search fallback", name)
			fmt.Fprintf(os.Stderr, "getArtistURL: Name '%s' not found on DBpedia, attempting search fallback\n", name)
		}
	}

	// Fallback 2: Generate a search URL if name is provided
	if name != "" {
		searchURL := fmt.Sprintf("https://en.wikipedia.org/w/index.php?search=%s", url.QueryEscape(name))
		log.Printf("[MCP] Debug: getArtistURL: Falling back to search URL: %s", searchURL)
		fmt.Fprintf(os.Stderr, "getArtistURL: Falling back to search URL: %s\n", searchURL)
		return searchURL, nil
	}

	// Final error: MBID lookup failed (or no MBID given) AND no name provided for fallback
	log.Printf("[MCP] Error: getArtistURL: Cannot generate Wikipedia URL: Lookups failed and no name provided.")
	return "", fmt.Errorf("cannot generate Wikipedia URL: Wikidata/DBpedia lookups failed and no artist name provided for search fallback")
}
