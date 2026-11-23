// Package cliproxy starts and stops an optional embedded CLIProxyAPI service.
// CLIProxyAPI owns upstream API/OAuth credentials and dynamic plugins.
// The router discovers models and translates requests through the proxy HTTP API.
// Without config_path, CLIProxyAPI runs as an external service.
// Dynamic plugin loading in embedded mode requires a CGO-enabled build.
package cliproxy
