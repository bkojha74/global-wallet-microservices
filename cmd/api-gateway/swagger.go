package main

import (
	_ "embed"
	"net/http"
)

//go:embed openapi.json
var openAPISpecJSON []byte

const swaggerUIHTML = `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <meta name="viewport" content="width=device-width, initial-scale=1.0">
  <title>Global Wallet Microservices API - Interactive Swagger UI</title>
  <link rel="stylesheet" type="text/css" href="https://cdn.jsdelivr.net/npm/swagger-ui-dist@5/swagger-ui.css" />
  <link rel="icon" type="image/png" href="https://cdn.jsdelivr.net/npm/swagger-ui-dist@5/favicon-32x32.png" sizes="32x32" />
  <link rel="icon" type="image/png" href="https://cdn.jsdelivr.net/npm/swagger-ui-dist@5/favicon-16x16.png" sizes="16x16" />
  <style>
    html { box-sizing: border-box; overflow-y: scroll; }
    *, *:before, *:after { box-sizing: inherit; }
    body {
      margin: 0;
      background: #fafafa;
      font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, Helvetica, Arial, sans-serif;
    }
    .topbar {
      background-color: #0f172a !important;
      padding: 10px 0;
      border-bottom: 2px solid #3b82f6;
    }
    .topbar-wrapper {
      max-width: 1460px;
      margin: 0 auto;
      padding: 0 20px;
      display: flex;
      align-items: center;
      justify-content: space-between;
    }
    .topbar-brand {
      display: flex;
      align-items: center;
      text-decoration: none;
      color: #ffffff;
      font-size: 1.2rem;
      font-weight: 700;
      letter-spacing: -0.02em;
    }
    .topbar-brand svg {
      margin-right: 10px;
      color: #60a5fa;
    }
    .topbar-brand span {
      color: #93c5fd;
      font-weight: 500;
      font-size: 0.95rem;
      margin-left: 8px;
    }
    .swagger-ui .info {
      margin: 28px 0;
    }
    .swagger-ui .info .title {
      font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif;
      font-weight: 800;
      color: #0f172a;
    }
    .swagger-ui .scheme-container {
      background: #f1f5f9;
      box-shadow: none;
      border-radius: 8px;
      padding: 15px;
      margin: 20px 0;
    }
  </style>
</head>
<body>
  <div class="topbar">
    <div class="topbar-wrapper">
      <a class="topbar-brand" href="/swagger/">
        <svg xmlns="http://www.w3.org/2000/svg" width="26" height="26" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
          <path d="M21 12V7H5a2 2 0 0 1 0-4h14v4"/>
          <path d="M3 5v14a2 2 0 0 0 2 2h16v-5"/>
          <path d="M18 12a2 2 0 0 0 0 4h4v-4Z"/>
        </svg>
        Global Wallet <span>API Documentation</span>
      </a>
      <div>
        <a href="/api-docs/openapi.json" target="_blank" style="color: #cbd5e1; font-size: 0.85rem; text-decoration: none; background: #1e293b; padding: 6px 12px; border-radius: 6px; border: 1px solid #334155;">Raw OpenAPI Spec (JSON)</a>
      </div>
    </div>
  </div>
  <div id="swagger-ui"></div>
  <script src="https://cdn.jsdelivr.net/npm/swagger-ui-dist@5/swagger-ui-bundle.js" charset="UTF-8"></script>
  <script src="https://cdn.jsdelivr.net/npm/swagger-ui-dist@5/swagger-ui-standalone-preset.js" charset="UTF-8"></script>
  <script>
    window.onload = function() {
      window.ui = SwaggerUIBundle({
        url: "/api-docs/openapi.json",
        dom_id: '#swagger-ui',
        deepLinking: true,
        presets: [
          SwaggerUIBundle.presets.apis,
          SwaggerUIStandalonePreset
        ],
        plugins: [
          SwaggerUIBundle.plugins.DownloadUrl
        ],
        layout: "StandaloneLayout",
        persistAuthorization: true,
        displayRequestDuration: true,
        docExpansion: "list",
        filter: true,
        defaultModelsExpandDepth: 1,
        defaultModelExpandDepth: 1
      });
    };
  </script>
</body>
</html>`

// handleOpenAPISpec serves the raw OpenAPI 3.0.3 JSON document.
func handleOpenAPISpec(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodGet {
		_, _ = w.Write(openAPISpecJSON)
	}
}

// handleSwaggerUI serves the embedded Swagger UI interactive HTML documentation.
func handleSwaggerUI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodGet {
		_, _ = w.Write([]byte(swaggerUIHTML))
	}
}

// handleSwaggerRedirect redirects requests from /swagger or /docs to /swagger/.
func handleSwaggerRedirect(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/swagger/", http.StatusMovedPermanently)
}
