package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestResolveUpstream(t *testing.T) {
	tests := []struct {
		name         string
		target       string
		host         string
		wantHost     string
		wantDockerIO bool
	}{
		{
			name:         "default docker hub",
			target:       "http://docker.example/v2/",
			host:         "docker.example",
			wantHost:     dockerHub,
			wantDockerIO: true,
		},
		{
			name:         "prefix route ghcr",
			target:       "http://ghcr.example/v2/",
			host:         "ghcr.example",
			wantHost:     "ghcr.io",
			wantDockerIO: false,
		},
		{
			name:         "ns overrides host",
			target:       "http://docker.example/v2/?ns=quay.io",
			host:         "docker.example",
			wantHost:     "quay.io",
			wantDockerIO: false,
		},
		{
			name:         "docker io ns maps to registry host",
			target:       "http://docker.example/v2/?ns=docker.io",
			host:         "docker.example",
			wantHost:     dockerHub,
			wantDockerIO: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tt.target, nil)
			req.Host = tt.host

			gotHost, gotDockerIO := resolveUpstream(req)
			if gotHost != tt.wantHost || gotDockerIO != tt.wantDockerIO {
				t.Fatalf("resolveUpstream() = (%q, %v), want (%q, %v)", gotHost, gotDockerIO, tt.wantHost, tt.wantDockerIO)
			}
		})
	}
}

func TestRewriteAuthHeader(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://ghcr.proxy.example/v2/", nil)
	req.Host = "ghcr.proxy.example"
	req.Header.Set("X-Forwarded-Proto", "https")

	got := rewriteAuthHeader(`Bearer realm="https://ghcr.io/token",service="ghcr.io"`, req, "ghcr.io")
	want := `Bearer realm="https://ghcr.proxy.example/_auth/ghcr.io/token",service="ghcr.io"`
	if got != want {
		t.Fatalf("rewriteAuthHeader() = %q, want %q", got, want)
	}
}

func TestCopySelectHeadersDropsHopByHopConnectionAndKeepsAccepts(t *testing.T) {
	src := http.Header{}
	src.Set("User-Agent", "docker/26")
	src.Add("Accept", "application/vnd.oci.image.index.v1+json")
	src.Add("Accept", "application/vnd.docker.distribution.manifest.v2+json")
	src.Set("Connection", "close")
	src.Set("Authorization", "Bearer secret")

	dst := http.Header{}
	copySelectHeaders(dst, src)

	if got := dst.Get("Connection"); got != "" {
		t.Fatalf("Connection header copied as %q", got)
	}
	if got := dst.Get("Authorization"); got != "" {
		t.Fatalf("Authorization should not be copied by copySelectHeaders, got %q", got)
	}
	if got := dst.Values("Accept"); len(got) != 2 {
		t.Fatalf("Accept headers = %#v, want two values", got)
	}
}

func TestFixEncodedLibrary(t *testing.T) {
	got := fixEncodedLibrary("/v2/nginx/manifests/latest?scope=repository%3Anginx%3Apull&service=registry.docker.io")
	want := "/v2/nginx/manifests/latest?scope=repository%3Alibrary%2Fnginx%3Apull&service=registry.docker.io"
	if got != want {
		t.Fatalf("fixEncodedLibrary() = %q, want %q", got, want)
	}
}

func TestHandleV2UsesGetForThirdPartyHeadPing(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Fatalf("upstream method = %s, want GET", r.Method)
		}
		if r.URL.Path != "/v2/" {
			t.Fatalf("upstream path = %s, want /v2/", r.URL.Path)
		}
		w.Header().Set("Www-Authenticate", `Bearer realm="`+upstreamRealm(r)+`",service="ghcr.io"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer upstream.Close()

	previousClient := registryClient
	registryClient = upstream.Client()
	defer func() {
		registryClient = previousClient
	}()

	hubHost := strings.TrimPrefix(upstream.URL, "https://")
	req := httptest.NewRequest(http.MethodHead, "https://ghcr.proxy.example/v2/", nil)
	req.Host = "ghcr.proxy.example"
	req.Header.Set("X-Forwarded-Proto", "https")
	rec := httptest.NewRecorder()

	handleV2(rec, req, hubHost, false)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
	if got := rec.Header().Get("Www-Authenticate"); !strings.Contains(got, "https://ghcr.proxy.example/_auth/") {
		t.Fatalf("Www-Authenticate was not rewritten through proxy: %q", got)
	}
}

func upstreamRealm(r *http.Request) string {
	return "https://" + r.Host + "/token"
}
