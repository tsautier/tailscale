// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

//go:build !plan9

package apiproxy

import (
	"net/http"
)

// switchingTransport is an http.RoundTripper that chooses which transport to
// use based on the presence of a Tailscale identity in the request context.
// The authTransport should attach the proxy's own auth headers to requests,
// which will make the impersonation headers attached earlier in the request
// lifecycle effective. The plainTransport should leave auth headers unchanged.
type switchingTransport struct {
	authTransport  http.RoundTripper
	plainTransport http.RoundTripper
}

func (t *switchingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	reqData := requestDataKey.Value(r.Context())
	if reqData.impersonate {
		return t.authTransport.RoundTrip(r)
	}

	return t.plainTransport.RoundTrip(r)
}
