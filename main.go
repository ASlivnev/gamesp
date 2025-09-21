package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
)

const (
	downloadDir    = "resources"
	requestTimeout = 2 * time.Minute
)

func main() {
	// Handler for the root path to serve the HTML interface
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "index.html")
	})

	// Handler for the /scrape endpoint to start the scraping process
	http.HandleFunc("/scrape", scrapeHandler)

	// Handler to serve the downloaded resources
	http.Handle("/resources/", http.StripPrefix("/resources/", http.FileServer(http.Dir(downloadDir))))

	port := "8088"
	log.Printf("Starting server on http://localhost:%s", port)
	if err := http.ListenAndServe(":"+port, nil); err != nil {
		log.Fatalf("Failed to start server: %v", err)
	}
}

func scrapeHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Invalid request method", http.StatusMethodNotAllowed)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "Failed to parse form", http.StatusBadRequest)
		return
	}

	gameURL := r.FormValue("gameURL")
	if gameURL == "" {
		http.Error(w, "gameURL is required", http.StatusBadRequest)
		return
	}

	log.Printf("Received scrape request for URL: %s", gameURL)

	// Run scraping in a separate goroutine to not block the HTTP response
	go scrapeResources(gameURL)

	// Respond to the user immediately
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "Scraping started for %s. Check the console for progress.", gameURL)
}

func scrapeResources(gameURL string) {
	log.Println("Cleaning up resources directory...")
	if err := os.RemoveAll(downloadDir); err != nil {
		log.Printf("Failed to clean up download directory: %v", err)
	}
	if err := os.MkdirAll(downloadDir, os.ModePerm); err != nil {
		log.Printf("Failed to create download directory: %v", err)
		return
	}

	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.Flag("headless", false),
		chromedp.Flag("lang", "ru-RU"),
		chromedp.UserAgent(`Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36`),
	)
	allocCtx, cancel := chromedp.NewExecAllocator(context.Background(), opts...)
	defer cancel()

	ctx, cancel := chromedp.NewContext(allocCtx, chromedp.WithLogf(log.Printf))
	defer cancel()
	ctx, cancel = context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	resourceURLs := make(map[string]bool)
	var mu sync.Mutex

	chromedp.ListenTarget(ctx, func(ev interface{}) {
		switch ev := ev.(type) {
		case *network.EventRequestWillBeSent:
			mu.Lock()
			if _, exists := resourceURLs[ev.Request.URL]; !exists {
				resourceURLs[ev.Request.URL] = true
				log.Printf("Discovered resource: %s", ev.Request.URL)
			}
			mu.Unlock()
		}
	})

	log.Printf("Navigating to %s", gameURL)
	if err := chromedp.Run(ctx, network.Enable(), chromedp.Navigate(gameURL), chromedp.Sleep(45*time.Second)); err != nil {
		log.Printf("Failed to navigate and load page: %v", err)
		return
	}
	log.Println("Navigation and sleep completed.")

	var htmlContent string
	if err := chromedp.Run(ctx, chromedp.OuterHTML("html", &htmlContent)); err != nil {
		log.Printf("Failed to get HTML content: %v", err)
		return
	}

	parsedGameURL, err := url.Parse(gameURL)
	if err != nil {
		log.Printf("Failed to parse gameURL: %v", err)
		return
	}
	// Create the directory structure based on the URL
	gameResDir := filepath.Join(downloadDir, parsedGameURL.Host, parsedGameURL.Path)
	// Remove any trailing slash and ensure we don't double up on index.html
	gameResDir = strings.TrimSuffix(gameResDir, "/")
	gameResDir = strings.TrimSuffix(gameResDir, "/index.html")

	indexPath := filepath.Join(gameResDir, "index.html")
	if err := os.MkdirAll(filepath.Dir(indexPath), os.ModePerm); err != nil {
		log.Printf("Failed to create directory for index.html: %v", err)
		return
	}
	if err := os.WriteFile(indexPath, []byte(htmlContent), 0644); err != nil {
		log.Printf("Failed to write index.html: %v", err)
		return
	}
	log.Printf("Successfully saved index.html to %s", indexPath)

	log.Printf("Discovered %d unique resources. Starting download...", len(resourceURLs))

	var wg sync.WaitGroup
	for resURL := range resourceURLs {
		// The main HTML is already saved, so we skip it in the download loop.
		if resURL == gameURL {
			continue
		}
		wg.Add(1)
		go func(u string) {
			defer wg.Done()
			if err := downloadResource(u, downloadDir); err != nil {
				log.Printf("Failed to download %s: %v", u, err)
			}
		}(resURL)
	}

	wg.Wait()
	log.Println("All resources downloaded successfully.")
}

func downloadResource(rawURL, baseDir string) error {
	if strings.HasPrefix(rawURL, "data:") {
		return nil
	}

	// Skip blob URLs as they can't be downloaded via HTTP
	if strings.HasPrefix(rawURL, "blob:") {
		log.Printf("Skipping blob URL: %s", rawURL)
		return nil
	}

	parsedURL, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("could not parse URL %s: %w", rawURL, err)
	}

	// Skip URLs with empty paths (like root domains)
	if parsedURL.Path == "" || parsedURL.Path == "/" {
		parsedURL.Path = "/index.html"
	}

	filePath := filepath.Join(baseDir, parsedURL.Host, parsedURL.Path)

	// If the path from the URL ends in a slash, it's a directory; append index.html.
	if strings.HasSuffix(parsedURL.Path, "/") {
		filePath = filepath.Join(filePath, "index.html")
	}

	// Ensure the parent directory for the file exists.
	if err := os.MkdirAll(filepath.Dir(filePath), os.ModePerm); err != nil {
		return fmt.Errorf("could not create directory for %s: %w", filePath, err)
	}

	// Create the file.
	log.Printf("Creating file: %s", filePath)
	out, err := os.Create(filePath)
	if err != nil {
		// Check if a directory with this name already exists
		if info, statErr := os.Stat(filePath); statErr == nil && info.IsDir() {
			log.Printf("ERROR: %s is a directory, not a file! This should not happen.", filePath)
			return fmt.Errorf("cannot create file %s: path exists as directory", filePath)
		}
		return fmt.Errorf("could not create file %s: %w", filePath, err)
	}
	defer out.Close()

	resp, err := http.Get(rawURL)
	if err != nil {
		return fmt.Errorf("http.Get failed for %s: %w", rawURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("bad status for %s: %s", rawURL, resp.Status)
	}

	_, err = io.Copy(out, resp.Body)
	if err != nil {
		return fmt.Errorf("io.Copy failed for %s: %w", rawURL, err)
	}

	log.Printf("Successfully downloaded %s to %s", rawURL, filePath)
	return nil
}
