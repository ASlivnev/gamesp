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
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
)

const (
	downloadDir    = "resources"
	requestTimeout = 4 * time.Minute // Increased timeout for complex games
)

func main() {
	// Handler for the root path to serve the HTML interface
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "index.html")
	})

	// Handler for the /scrape endpoint (new advanced scraper)
	http.HandleFunc("/scrape", scrapeHandler)

	// Handler for the /scrape-simple endpoint (old simple scraper)
	http.HandleFunc("/scrape-simple", scrapeSimpleHandler)

	// Handler for the /clear endpoint to clear resources directory
	http.HandleFunc("/clear", clearResourcesHandler)

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
	fmt.Fprintf(w, "Advanced scraping started for %s. Check the console for progress.", gameURL)
}

// scrapeSimpleHandler handles the simple scraping requests
func scrapeSimpleHandler(w http.ResponseWriter, r *http.Request) {
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

	log.Printf("Received simple scrape request for URL: %s", gameURL)

	// Run simple scraping in a separate goroutine
	go scrapeResourcesSimple(gameURL)

	// Respond to the user immediately
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "Simple scraping started for %s. Check the console for progress.", gameURL)
}

// clearResourcesHandler handles the clear resources requests
func clearResourcesHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Invalid request method", http.StatusMethodNotAllowed)
		return
	}

	log.Println("[CLEAR] Received request to clear resources directory")

	// Clear resources in a separate goroutine
	go func() {
		log.Println("[CLEAR] Starting cleanup of resources directory...")
		if err := forceCleanupDirectory(downloadDir); err != nil {
			log.Printf("[CLEAR] Failed to clear resources directory: %v", err)
		} else {
			log.Println("[CLEAR] Successfully cleared resources directory")
		}
	}()

	// Respond to the user immediately
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "Resources directory clearing started. Check the console for progress.")
}

func scrapeResources(gameURL string) {
	log.Println("Cleaning up resources directory...")

	// Force cleanup with detailed logging
	if err := forceCleanupDirectory(downloadDir); err != nil {
		log.Printf("Failed to clean up download directory: %v", err)
		// Try to continue anyway
	}

	if err := os.MkdirAll(downloadDir, os.ModePerm); err != nil {
		log.Printf("Failed to create download directory: %v", err)
		return
	}
	log.Printf("Successfully cleaned and recreated %s directory", downloadDir)

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
	var interceptedURLs []interface{}
	var mu sync.Mutex

	chromedp.ListenTarget(ctx, func(ev interface{}) {
		switch ev := ev.(type) {
		case *network.EventRequestWillBeSent:
			mu.Lock()
			if _, exists := resourceURLs[ev.Request.URL]; !exists {
				resourceURLs[ev.Request.URL] = true
				log.Printf("Discovered resource (RequestWillBeSent): %s", ev.Request.URL)
			}
			mu.Unlock()
		case *network.EventResponseReceived:
			mu.Lock()
			if _, exists := resourceURLs[ev.Response.URL]; !exists {
				resourceURLs[ev.Response.URL] = true
				log.Printf("Discovered resource (ResponseReceived): %s", ev.Response.URL)
			}
			mu.Unlock()
		case *network.EventLoadingFinished:
			// This event doesn't have URL, but we can use it to know when loading is complete
			log.Printf("Resource loading finished: %s", ev.RequestID)
		}
	})

	log.Printf("Navigating to %s", gameURL)
	if err := chromedp.Run(ctx,
		network.Enable(),
		chromedp.Navigate(gameURL),
		chromedp.Sleep(10*time.Second), // Initial wait
		// Inject JavaScript to intercept fetch and XMLHttpRequest
		chromedp.Evaluate(injectNetworkInterceptor(), nil),
		chromedp.Sleep(20*time.Second), // Wait after injection
		// Try to interact with the page to trigger more resource loading
		chromedp.Click("body", chromedp.ByQuery), // Click on the page
		chromedp.Sleep(15*time.Second),           // Wait after interaction
		// Try pressing some keys that might trigger game loading
		chromedp.KeyEvent(` `), // Space key
		chromedp.Sleep(10*time.Second),
		chromedp.KeyEvent(`Enter`),     // Enter key
		chromedp.Sleep(30*time.Second), // Final wait
		// Get intercepted URLs from JavaScript
		chromedp.Evaluate(`window.interceptedURLs || []`, &interceptedURLs),
	); err != nil {
		log.Printf("Failed to navigate and interact with page: %v", err)
		return
	}
	log.Println("Navigation, interaction and extended wait completed.")

	// Add intercepted URLs to our resource list
	mu.Lock()
	for _, interceptedURL := range interceptedURLs {
		if urlStr, ok := interceptedURL.(string); ok {
			if _, exists := resourceURLs[urlStr]; !exists {
				resourceURLs[urlStr] = true
				log.Printf("Discovered resource (JS interception): %s", urlStr)
			}
		}
	}
	mu.Unlock()

	// Wait a bit more to catch any late-loading resources
	log.Println("Waiting additional time for late-loading resources...")
	time.Sleep(15 * time.Second)

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

	// Parse HTML content to find additional resources
	additionalResources := parseHTMLForResources(htmlContent, gameURL)
	mu.Lock()
	for _, resURL := range additionalResources {
		if _, exists := resourceURLs[resURL]; !exists {
			resourceURLs[resURL] = true
			log.Printf("Discovered resource (HTML parsing): %s", resURL)
		}
	}
	mu.Unlock()

	// Try to discover common game resources by brute force
	log.Println("Attempting to discover common game resources...")
	commonResources := discoverCommonResources(gameURL)
	mu.Lock()
	for _, resURL := range commonResources {
		if _, exists := resourceURLs[resURL]; !exists {
			resourceURLs[resURL] = true
			log.Printf("Discovered resource (brute force): %s", resURL)
		}
	}
	mu.Unlock()

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

// parseHTMLForResources extracts resource URLs from HTML content
func parseHTMLForResources(htmlContent, baseURL string) []string {
	var resources []string
	baseURLParsed, err := url.Parse(baseURL)
	if err != nil {
		return resources
	}

	// Regular expressions to find various resource types
	patterns := []string{
		`<script[^>]*src=["']([^"']+)["']`, // JavaScript files
		`<link[^>]*href=["']([^"']+)["']`,  // CSS and other linked resources
		`<img[^>]*src=["']([^"']+)["']`,    // Images
		`<source[^>]*src=["']([^"']+)["']`, // Video/audio sources
		`<iframe[^>]*src=["']([^"']+)["']`, // Iframes
		`url\(["']?([^"')]+)["']?\)`,       // CSS url() references
		`["']([^"']*\.(js|css|png|jpg|jpeg|gif|svg|webp|ico|woff|woff2|ttf|eot|mp3|wav|ogg|mp4|webm|json|xml|wasm))["']`, // File extensions
	}

	for _, pattern := range patterns {
		re := regexp.MustCompile(pattern)
		matches := re.FindAllStringSubmatch(htmlContent, -1)
		for _, match := range matches {
			if len(match) > 1 {
				resourceURL := match[1]
				// Convert relative URLs to absolute
				if absoluteURL := resolveURL(baseURLParsed, resourceURL); absoluteURL != "" {
					resources = append(resources, absoluteURL)
				}
			}
		}
	}

	return resources
}

// resolveURL converts relative URLs to absolute URLs
func resolveURL(base *url.URL, href string) string {
	// Skip data URLs, blob URLs, and external protocols
	if strings.HasPrefix(href, "data:") || strings.HasPrefix(href, "blob:") ||
		strings.HasPrefix(href, "http://") || strings.HasPrefix(href, "https://") ||
		strings.HasPrefix(href, "//") || strings.HasPrefix(href, "mailto:") ||
		strings.HasPrefix(href, "tel:") || strings.HasPrefix(href, "javascript:") {
		if strings.HasPrefix(href, "http") {
			return href // Return absolute HTTP URLs
		}
		return "" // Skip other protocols
	}

	// Parse the href
	parsed, err := url.Parse(href)
	if err != nil {
		return ""
	}

	// Resolve relative to base
	absolute := base.ResolveReference(parsed)
	return absolute.String()
}

// injectNetworkInterceptor returns JavaScript code to intercept network requests
func injectNetworkInterceptor() string {
	return `
		// Create array to store intercepted URLs
		window.interceptedURLs = window.interceptedURLs || [];
		
		// Intercept fetch requests
		const originalFetch = window.fetch;
		window.fetch = function(...args) {
			const url = args[0];
			if (typeof url === 'string') {
				window.interceptedURLs.push(url);
				console.log('Intercepted fetch:', url);
			} else if (url && url.url) {
				window.interceptedURLs.push(url.url);
				console.log('Intercepted fetch:', url.url);
			}
			return originalFetch.apply(this, args);
		};
		
		// Intercept XMLHttpRequest
		const originalXHROpen = XMLHttpRequest.prototype.open;
		XMLHttpRequest.prototype.open = function(method, url) {
			if (typeof url === 'string') {
				window.interceptedURLs.push(url);
				console.log('Intercepted XHR:', url);
			}
			return originalXHROpen.apply(this, arguments);
		};
		
		// Intercept dynamic script loading
		const originalCreateElement = document.createElement;
		document.createElement = function(tagName) {
			const element = originalCreateElement.call(this, tagName);
			if (tagName.toLowerCase() === 'script') {
				const originalSrcSetter = Object.getOwnPropertyDescriptor(HTMLScriptElement.prototype, 'src').set;
				Object.defineProperty(element, 'src', {
					set: function(value) {
						if (value) {
							window.interceptedURLs.push(value);
							console.log('Intercepted dynamic script:', value);
						}
						return originalSrcSetter.call(this, value);
					},
					get: function() {
						return this.getAttribute('src');
					}
				});
			}
			return element;
		};
		
		console.log('Network interceptor injected successfully');
	`
}

// discoverCommonResources attempts to find common game resources by trying standard paths
func discoverCommonResources(baseURL string) []string {
	var resources []string
	baseURLParsed, err := url.Parse(baseURL)
	if err != nil {
		return resources
	}

	// Remove index.html from the path to get the base directory
	basePath := strings.TrimSuffix(baseURLParsed.Path, "/index.html")
	basePath = strings.TrimSuffix(basePath, "/")

	// Common file patterns for web games (especially Construct 3)
	commonFiles := []string{
		// Construct 3 common files
		"/c3main.js",
		"/data.json",
		"/box2d.wasm.js",
		"/c3runtime.js",
		"/offlineClient.js",
		"/register-sw.js",
		"/sw.js",
		"/workermain.js",
		"/scripts/c3main.js",
		"/scripts/data.json",
		"/scripts/box2d.wasm.js",
		"/scripts/c3runtime.js",
		"/scripts/offlineClient.js",
		"/scripts/register-sw.js",
		"/scripts/workermain.js",
		// Common image directories
		"/images/",
		"/img/",
		"/assets/",
		"/media/",
		// Common audio directories
		"/sounds/",
		"/audio/",
		"/music/",
		// Other common files
		"/manifest.json",
		"/appmanifest.json",
		"/favicon.ico",
		"/icon-16.png",
		"/icon-32.png",
		"/icon-64.png",
		"/icon-128.png",
		"/icon-256.png",
		"/icon-512.png",
		"/loading-logo.png",
	}

	// Try each common file
	for _, file := range commonFiles {
		fullURL := fmt.Sprintf("%s://%s%s%s", baseURLParsed.Scheme, baseURLParsed.Host, basePath, file)
		resources = append(resources, fullURL)
	}

	// Try to discover image files by common naming patterns
	imageExtensions := []string{".png", ".jpg", ".jpeg", ".gif", ".svg", ".webp"}
	imageNames := []string{
		"sprite", "background", "bg", "player", "enemy", "tile", "icon", "logo",
		"button", "ui", "menu", "game", "level", "character", "item", "weapon",
		"particle", "effect", "explosion", "coin", "gem", "star", "heart",
	}

	for _, name := range imageNames {
		for _, ext := range imageExtensions {
			for i := 0; i < 10; i++ { // Try numbered variants
				if i == 0 {
					// Try without number
					fullURL := fmt.Sprintf("%s://%s%s/images/%s%s", baseURLParsed.Scheme, baseURLParsed.Host, basePath, name, ext)
					resources = append(resources, fullURL)
				} else {
					// Try with number
					fullURL := fmt.Sprintf("%s://%s%s/images/%s%d%s", baseURLParsed.Scheme, baseURLParsed.Host, basePath, name, i, ext)
					resources = append(resources, fullURL)
				}
			}
		}
	}

	// Try common audio files
	audioExtensions := []string{".mp3", ".wav", ".ogg", ".m4a", ".webm"}
	audioNames := []string{
		"music", "sound", "sfx", "bgm", "background", "click", "jump", "shoot",
		"explosion", "pickup", "coin", "powerup", "victory", "defeat", "menu",
	}

	for _, name := range audioNames {
		for _, ext := range audioExtensions {
			for i := 0; i < 5; i++ { // Try numbered variants
				if i == 0 {
					fullURL := fmt.Sprintf("%s://%s%s/sounds/%s%s", baseURLParsed.Scheme, baseURLParsed.Host, basePath, name, ext)
					resources = append(resources, fullURL)
					fullURL2 := fmt.Sprintf("%s://%s%s/audio/%s%s", baseURLParsed.Scheme, baseURLParsed.Host, basePath, name, ext)
					resources = append(resources, fullURL2)
					fullURL3 := fmt.Sprintf("%s://%s%s/media/%s%s", baseURLParsed.Scheme, baseURLParsed.Host, basePath, name, ext)
					resources = append(resources, fullURL3)
				} else {
					fullURL := fmt.Sprintf("%s://%s%s/sounds/%s%d%s", baseURLParsed.Scheme, baseURLParsed.Host, basePath, name, i, ext)
					resources = append(resources, fullURL)
				}
			}
		}
	}

	return resources
}

// forceCleanupDirectory forcefully removes a directory with detailed logging
func forceCleanupDirectory(dir string) error {
	// Check if directory exists
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		log.Printf("Directory %s does not exist, nothing to clean", dir)
		return nil
	}

	log.Printf("Attempting to remove directory: %s", dir)

	// First attempt: normal removal
	err := os.RemoveAll(dir)
	if err == nil {
		log.Printf("Successfully removed directory: %s", dir)
		return nil
	}

	log.Printf("First removal attempt failed: %v", err)
	log.Printf("Attempting to remove files individually...")

	// Second attempt: remove files individually
	err = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			log.Printf("Error walking path %s: %v", path, err)
			return nil // Continue walking
		}

		if !info.IsDir() {
			log.Printf("Removing file: %s", path)
			if removeErr := os.Remove(path); removeErr != nil {
				log.Printf("Failed to remove file %s: %v", path, removeErr)
				// Try to change permissions and remove again
				if chmodErr := os.Chmod(path, 0777); chmodErr == nil {
					if retryErr := os.Remove(path); retryErr != nil {
						log.Printf("Failed to remove file %s even after chmod: %v", path, retryErr)
					} else {
						log.Printf("Successfully removed file %s after chmod", path)
					}
				}
			} else {
				log.Printf("Successfully removed file: %s", path)
			}
		}
		return nil
	})

	if err != nil {
		log.Printf("Error during individual file removal: %v", err)
	}

	// Third attempt: remove empty directories
	log.Printf("Removing empty directories...")
	err = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // Continue
		}
		if info.IsDir() && path != dir {
			if removeErr := os.Remove(path); removeErr != nil {
				log.Printf("Could not remove directory %s: %v", path, removeErr)
			} else {
				log.Printf("Removed directory: %s", path)
			}
		}
		return nil
	})

	// Final attempt: remove the root directory
	log.Printf("Final attempt to remove root directory: %s", dir)
	if finalErr := os.Remove(dir); finalErr != nil {
		log.Printf("Could not remove root directory %s: %v", dir, finalErr)
		return finalErr
	}

	log.Printf("Successfully removed directory: %s", dir)
	return nil
}

// scrapeResourcesSimple is the old simple scraper function
func scrapeResourcesSimple(gameURL string) {
	log.Println("[SIMPLE] Cleaning up resources directory...")
	if err := os.RemoveAll(downloadDir); err != nil {
		log.Printf("[SIMPLE] Failed to clean up download directory: %v", err)
	}
	if err := os.MkdirAll(downloadDir, os.ModePerm); err != nil {
		log.Printf("[SIMPLE] Failed to create download directory: %v", err)
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
	ctx, cancel = context.WithTimeout(ctx, 2*time.Minute) // Original timeout
	defer cancel()

	resourceURLs := make(map[string]bool)
	var mu sync.Mutex

	chromedp.ListenTarget(ctx, func(ev interface{}) {
		switch ev := ev.(type) {
		case *network.EventRequestWillBeSent:
			mu.Lock()
			if _, exists := resourceURLs[ev.Request.URL]; !exists {
				resourceURLs[ev.Request.URL] = true
				log.Printf("[SIMPLE] Discovered resource: %s", ev.Request.URL)
			}
			mu.Unlock()
		}
	})

	log.Printf("[SIMPLE] Navigating to %s", gameURL)
	if err := chromedp.Run(ctx, network.Enable(), chromedp.Navigate(gameURL), chromedp.Sleep(45*time.Second)); err != nil {
		log.Printf("[SIMPLE] Failed to navigate and load page: %v", err)
		return
	}
	log.Println("[SIMPLE] Navigation and sleep completed.")

	var htmlContent string
	if err := chromedp.Run(ctx, chromedp.OuterHTML("html", &htmlContent)); err != nil {
		log.Printf("[SIMPLE] Failed to get HTML content: %v", err)
		return
	}

	parsedGameURL, err := url.Parse(gameURL)
	if err != nil {
		log.Printf("[SIMPLE] Failed to parse gameURL: %v", err)
		return
	}
	// Create the directory structure based on the URL
	gameResDir := filepath.Join(downloadDir, parsedGameURL.Host, parsedGameURL.Path)
	// Remove any trailing slash and ensure we don't double up on index.html
	gameResDir = strings.TrimSuffix(gameResDir, "/")
	gameResDir = strings.TrimSuffix(gameResDir, "/index.html")

	indexPath := filepath.Join(gameResDir, "index.html")
	if err := os.MkdirAll(filepath.Dir(indexPath), os.ModePerm); err != nil {
		log.Printf("[SIMPLE] Failed to create directory for index.html: %v", err)
		return
	}
	if err := os.WriteFile(indexPath, []byte(htmlContent), 0644); err != nil {
		log.Printf("[SIMPLE] Failed to write index.html: %v", err)
		return
	}
	log.Printf("[SIMPLE] Successfully saved index.html to %s", indexPath)

	log.Printf("[SIMPLE] Discovered %d unique resources. Starting download...", len(resourceURLs))

	var wg sync.WaitGroup
	for resURL := range resourceURLs {
		// The main HTML is already saved, so we skip it in the download loop.
		if resURL == gameURL {
			continue
		}
		wg.Add(1)
		go func(u string) {
			defer wg.Done()
			if err := downloadResourceSimple(u, downloadDir); err != nil {
				log.Printf("[SIMPLE] Failed to download %s: %v", u, err)
			}
		}(resURL)
	}

	wg.Wait()
	log.Println("[SIMPLE] All resources downloaded successfully.")
}

// downloadResourceSimple is the old simple download function
func downloadResourceSimple(rawURL, baseDir string) error {
	if strings.HasPrefix(rawURL, "data:") {
		return nil
	}

	// Skip blob URLs as they can't be downloaded via HTTP
	if strings.HasPrefix(rawURL, "blob:") {
		log.Printf("[SIMPLE] Skipping blob URL: %s", rawURL)
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
	log.Printf("[SIMPLE] Creating file: %s", filePath)
	out, err := os.Create(filePath)
	if err != nil {
		// Check if a directory with this name already exists
		if info, statErr := os.Stat(filePath); statErr == nil && info.IsDir() {
			log.Printf("[SIMPLE] ERROR: %s is a directory, not a file! This should not happen.", filePath)
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

	log.Printf("[SIMPLE] Successfully downloaded %s to %s", rawURL, filePath)
	return nil
}
