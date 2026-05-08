package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestExtractClientIP(t *testing.T) {
	cases := []struct {
		name    string
		headers map[string]string
		wantIP  string
		wantOK  bool
	}{
		{
			name:    "no headers fails closed",
			headers: nil,
			wantIP:  "",
			wantOK:  false,
		},
		{
			name:    "x-real-ip wins when present",
			headers: map[string]string{"X-Real-IP": "203.0.113.7"},
			wantIP:  "203.0.113.7",
			wantOK:  true,
		},
		{
			name: "x-real-ip wins over x-forwarded-for",
			headers: map[string]string{
				"X-Real-IP":       "203.0.113.7",
				"X-Forwarded-For": "10.0.0.1, 198.51.100.2",
			},
			wantIP: "203.0.113.7",
			wantOK: true,
		},
		{
			name:    "x-real-ip is whitespace-trimmed",
			headers: map[string]string{"X-Real-IP": "  203.0.113.7  "},
			wantIP:  "203.0.113.7",
			wantOK:  true,
		},
		{
			name: "x-real-ip unparseable falls through to xff",
			headers: map[string]string{
				"X-Real-IP":       "not-an-ip",
				"X-Forwarded-For": "10.0.0.1, 198.51.100.2",
			},
			wantIP: "198.51.100.2",
			wantOK: true,
		},
		{
			name:    "rightmost xff entry wins",
			headers: map[string]string{"X-Forwarded-For": "1.1.1.1, 2.2.2.2, 3.3.3.3"},
			wantIP:  "3.3.3.3",
			wantOK:  true,
		},
		{
			name:    "single xff entry parses",
			headers: map[string]string{"X-Forwarded-For": "203.0.113.7"},
			wantIP:  "203.0.113.7",
			wantOK:  true,
		},
		{
			name:    "ipv6 in x-real-ip",
			headers: map[string]string{"X-Real-IP": "2001:db8::1"},
			wantIP:  "2001:db8::1",
			wantOK:  true,
		},
		{
			name:    "ipv4-mapped ipv6 normalizes via Unmap",
			headers: map[string]string{"X-Real-IP": "::ffff:203.0.113.7"},
			wantIP:  "203.0.113.7",
			wantOK:  true,
		},
		{
			name:    "rightmost xff trailing-comma is skipped",
			headers: map[string]string{"X-Forwarded-For": "1.1.1.1, 2.2.2.2,"},
			wantIP:  "2.2.2.2",
			wantOK:  true,
		},
		{
			name:    "rightmost xff garbage is skipped, then parseable wins",
			headers: map[string]string{"X-Forwarded-For": "1.1.1.1, 2.2.2.2, junk"},
			wantIP:  "2.2.2.2",
			wantOK:  true,
		},
		{
			name:    "all xff entries unparseable fails closed",
			headers: map[string]string{"X-Forwarded-For": "junk, also-junk"},
			wantIP:  "",
			wantOK:  false,
		},
		{
			name:    "empty x-real-ip and empty xff fails closed",
			headers: map[string]string{"X-Real-IP": "", "X-Forwarded-For": ""},
			wantIP:  "",
			wantOK:  false,
		},
		{
			name:    "x-real-ip with port is rejected (ParseAddr does not accept ports)",
			headers: map[string]string{"X-Real-IP": "203.0.113.7:443"},
			wantIP:  "",
			wantOK:  false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/check", http.NoBody)
			for k, v := range tc.headers {
				r.Header.Set(k, v)
			}

			addr, ok := extractClientIP(r)
			require.Equal(t, tc.wantOK, ok)
			if tc.wantOK {
				require.Equal(t, tc.wantIP, addr.String())
			}
		})
	}
}
