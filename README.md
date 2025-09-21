# HTML5 Game Scraper

This is a simple Go application that uses `chromedp` to scrape and download all resources (assets) of an HTML5 game from a given URL.

## How to Use

1.  **Ensure you have Go installed.**
2.  **Install dependencies:**
    ```sh
    go get github.com/chromedp/chromedp
    ```
3.  **Run the application:**
    ```sh
    go run main.go
    ```

All game resources will be downloaded into the `resources` directory, preserving the original folder structure from the server.
