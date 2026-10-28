package trace

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// recordingCollector answers every request with status and body, and remembers
// the path and raw query it was sent to.
type recordingCollector struct {
	*httptest.Server
	mu           sync.Mutex
	path, query  string
	requestCount int
}

func newRecordingCollector(t *testing.T, status int, body string) *recordingCollector {
	t.Helper()
	c := &recordingCollector{}
	c.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		c.path, c.query = r.URL.Path, r.URL.RawQuery
		c.requestCount++
		c.mu.Unlock()
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(c.Close)
	return c
}

func (c *recordingCollector) seen() (path, query string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.path, c.query
}

// A query string on the endpoint is part of the address, not of the path. The
// traces path used to be appended to the whole string, which posted to the
// endpoint's own path with the suffix at the end of the query, and a real
// collector answered 404.
func TestExporterKeepsTheQueryOfTheEndpoint(t *testing.T) {
	for _, tc := range []struct{ name, suffix, wantPath string }{
		{"query on the origin", "?token=abc", "/v1/traces"},
		{"query after a slash", "/?token=abc", "/v1/traces"},
		{"query after a prefix", "/otel?token=abc", "/otel/v1/traces"},
		{"query after a prefix and a slash", "/otel/?token=abc", "/otel/v1/traces"},
		{"query after the full path", "/v1/traces?token=abc", "/v1/traces"},
		{"query after a prefix and the full path", "/otel/v1/traces?token=abc&x=1", "/otel/v1/traces"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			collector := newRecordingCollector(t, http.StatusOK, "")
			if _, err := (Exporter{Endpoint: collector.URL + tc.suffix}).Export(context.Background(), sampleTrace()); err != nil {
				t.Fatalf("export: %v", err)
			}
			path, query := collector.seen()
			wantQuery := tc.suffix[strings.Index(tc.suffix, "?")+1:]
			if path != tc.wantPath || query != wantQuery {
				t.Fatalf("the collector saw path %q and query %q, want path %q and query %q", path, query, tc.wantPath, wantQuery)
			}
		})
	}
}

// The endpoint of a hosted collector routinely carries a token, in the userinfo
// or the query, and header values are credentials too. Both callers print what
// the exporter returns, so nothing it returns may hold either: not the delivery
// report, not any of its errors, not the collector's own words echoed back.
func TestExporterNeverReturnsTheEndpointCredentials(t *testing.T) {
	const password, token, headerValue = "endpoint-password-not-a-secret", "query-token-not-a-secret", "header-value-not-a-secret"
	secrets := []string{password, token, headerValue}
	headers := map[string]string{"X-Api-Key": headerValue}
	withCredentials := func(url string) string {
		return strings.Replace(url, "http://", "http://collector:"+password+"@", 1) + "/?token=" + token
	}
	check := func(t *testing.T, what string, texts ...string) {
		t.Helper()
		for _, text := range texts {
			for _, secret := range secrets {
				if strings.Contains(text, secret) {
					t.Errorf("%s carries %q: %s", what, secret, text)
				}
			}
		}
	}

	t.Run("delivered", func(t *testing.T) {
		collector := newRecordingCollector(t, http.StatusOK, "")
		delivery, err := Exporter{Endpoint: withCredentials(collector.URL), Headers: headers}.Export(context.Background(), sampleTrace())
		if err != nil || !delivery.Delivered {
			t.Fatalf("delivery=%+v err=%v", delivery, err)
		}
		check(t, "the delivery endpoint", delivery.Endpoint)
		if want := collector.URL + "/v1/traces"; delivery.Endpoint != want {
			t.Errorf("endpoint = %q, want the address alone, %q", delivery.Endpoint, want)
		}
	})
	t.Run("rejected", func(t *testing.T) {
		// A collector that echoes the credentials back in its refusal.
		collector := newRecordingCollector(t, http.StatusForbidden, "denied for "+headerValue+" at "+token)
		delivery, err := Exporter{Endpoint: withCredentials(collector.URL), Headers: headers}.Export(context.Background(), sampleTrace())
		if err == nil {
			t.Fatal("a 403 was reported as success")
		}
		check(t, "the rejection error", err.Error())
		check(t, "the delivery endpoint", delivery.Endpoint)
		check(t, "the delivery response", delivery.Response)
		if !strings.Contains(err.Error(), "HTTP 403") {
			t.Errorf("the error lost the status: %v", err)
		}
	})
	t.Run("unreachable", func(t *testing.T) {
		collector := newRecordingCollector(t, http.StatusOK, "")
		endpoint := withCredentials(collector.URL)
		collector.Close()
		_, err := Exporter{Endpoint: endpoint, Headers: headers}.Export(context.Background(), sampleTrace())
		if err == nil {
			t.Fatal("a closed collector was reported as success")
		}
		check(t, "the transport error", err.Error())
		if !strings.Contains(err.Error(), "127.0.0.1") {
			t.Errorf("the error lost the host it was about: %v", err)
		}
	})
	t.Run("cancelled keeps its cause", func(t *testing.T) {
		collector := newRecordingCollector(t, http.StatusOK, "")
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := Exporter{Endpoint: withCredentials(collector.URL), Headers: headers}.Export(ctx, sampleTrace())
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want one that unwraps to context.Canceled", err)
		}
		check(t, "the cancellation error", err.Error())
	})
	// url.Parse quotes what it rejected, so a malformed endpoint is the case where
	// the standard library's own text holds the credential.
	for name, endpoint := range map[string]string{
		"bad escape":       "http://collector:pa%ss-" + password + "@127.0.0.1:4318/?token=" + token,
		"trailing newline": "http://collector:" + password + "@127.0.0.1:4318/?token=" + token + "\n",
		"leading space":    " http://collector:" + password + "@127.0.0.1:4318/?token=" + token,
		"bad port":         "http://collector:" + password + "@127.0.0.1:notaport/?token=" + token,
		"slash":            "http://collector:" + password + "/more@127.0.0.1:4318/?token=" + token,
	} {
		t.Run("malformed/"+name, func(t *testing.T) {
			_, err := Exporter{Endpoint: endpoint, Headers: headers}.Export(context.Background(), sampleTrace())
			if err == nil {
				t.Fatal("a malformed endpoint was accepted")
			}
			check(t, "the error", err.Error())
			for _, piece := range []string{"pa%ss", "/more"} {
				if strings.Contains(err.Error(), piece) {
					t.Errorf("the error carries %q: %v", piece, err)
				}
			}
			if !strings.Contains(err.Error(), "not a valid URL") {
				t.Errorf("the error does not say the endpoint is the problem: %v", err)
			}
		})
	}
}
