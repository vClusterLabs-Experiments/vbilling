package destinations

import (
	"fmt"
	"net/http"
	"strings"
	"time"
)

// HTTPError is a non-2xx response from a backend API.
type HTTPError struct {
	Method string
	URL    string
	Status int
	Body   string
}

func (e *HTTPError) Error() string {
	body := strings.TrimSpace(e.Body)
	if len(body) > 512 {
		body = body[:512] + "..."
	}
	return fmt.Sprintf("%s %s: HTTP %d: %s", e.Method, e.URL, e.Status, body)
}

// StatusError classifies a non-2xx response. Payload rejections (400, 413,
// 422) are permanent so the dispatcher can isolate the bad event. Auth
// failures, missing endpoints, rate limits and server errors are retried:
// they are configuration or availability problems, and dead-lettering
// every event because of an expired API key would lose revenue.
func StatusError(method, url string, status int, body []byte) error {
	err := &HTTPError{Method: method, URL: url, Status: status, Body: string(body)}
	switch status {
	case http.StatusBadRequest, http.StatusRequestEntityTooLarge, http.StatusUnprocessableEntity:
		return Permanent(err)
	default:
		return err
	}
}

// NewHTTPClient returns the client adapters share.
func NewHTTPClient() *http.Client {
	return &http.Client{Timeout: 30 * time.Second}
}
