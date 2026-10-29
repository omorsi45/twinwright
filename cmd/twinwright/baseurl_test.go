package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A base URL that does not parse used to start the run and fail on the first
// model call with url.Parse's own error, which quotes the whole string. The CLI
// printed it and the ledger stored it. It is now refused before a run exists, so
// nothing is written, and the message names the address without its credentials.
func TestMalformedBaseURLNeverLeaksFromTheCLI(t *testing.T) {
	const query, userinfo = "query-key-token", "userinfo-pw-token"
	t.Setenv("OPENAI_API_KEY", "cli-baseurl-key-not-a-secret")
	cases := map[string]string{
		"bad escape":       "http://user:pa%ss-" + userinfo + "@127.0.0.1:1/v1?key=" + query,
		"trailing newline": "http://user:" + userinfo + "@127.0.0.1:1/v1?key=" + query + "\n",
		"leading space":    " http://user:" + userinfo + "@127.0.0.1:1/v1?key=" + query,
		"bad port":         "http://user:" + userinfo + "@127.0.0.1:notaport/v1?key=" + query,
		"slash":            "http://user:" + userinfo + "/more@127.0.0.1:1/v1?key=" + query,
	}
	for name, base := range cases {
		for _, viaEnv := range []bool{false, true} {
			label := name + "/flag"
			if viaEnv {
				label = name + "/OPENAI_BASE_URL"
			}
			t.Run(label, func(t *testing.T) {
				dir := t.TempDir()
				manifest, db := buildBillingManifest(t, dir), filepath.Join(dir, "world.db")
				args := []string{"run", "duplicate-charge", "--agent", "openai-compatible", "--model", "local", "--manifest", manifest, "--db", db}
				t.Setenv("OPENAI_BASE_URL", "")
				if viaEnv {
					t.Setenv("OPENAI_BASE_URL", base)
				} else {
					args = append(args, "--base-url", base)
				}
				var out bytes.Buffer
				err := runCLI(args, &out)
				if err == nil {
					t.Fatalf("a malformed base URL was accepted:\n%s", out.String())
				}
				for _, secret := range []string{userinfo, query, "pa%ss", "/more"} {
					if strings.Contains(err.Error(), secret) || strings.Contains(out.String(), secret) {
						t.Errorf("%q leaked:\nerror: %v\noutput: %s", secret, err, out.String())
					}
				}
				if !strings.Contains(err.Error(), "--base-url") || !strings.Contains(err.Error(), "invalid URL") {
					t.Errorf("the error does not name the flag and the problem: %v", err)
				}
				if _, statErr := os.Stat(db); !os.IsNotExist(statErr) {
					t.Errorf("a refused base URL still created the database: %v", statErr)
				}
			})
		}
	}
}
