// Package ui serves the embedded read-only cluster dashboard.
package ui

import (
	_ "embed"
	"net/http"
)

//go:embed index.html
var indexHTML []byte

// Handler serves the embedded dashboard page. It is an unauthenticated app
// shell — every data fetch the page makes hits the auth'd API with a bearer
// token kept in the browser.
func Handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(indexHTML)
	}
}
