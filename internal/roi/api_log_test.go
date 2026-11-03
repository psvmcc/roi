package roi

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAPIRequestLogRedactsSecrets(t *testing.T) {
	a := testApp(t)
	for range 55 {
		r := httptest.NewRequest("GET", "/greader/reader/api/0/user-info?Passwd=secret-query", strings.NewReader("secret-body"))
		r.Header.Set("Authorization", "secret-header")
		r.Header.Set("User-Agent", "Reeder/test")
		w := httptest.NewRecorder()
		a.APIHandler().ServeHTTP(w, r)
	}
	events, err := a.securityHistory()
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatal(len(events))
	}
	if a.metrics.GetOrCreateCounter("roi_security_events_suppressed_total").Get() != 54 {
		t.Fatal("repeated denials were not counted")
	}
	b, e := json.Marshal(events)
	if e != nil {
		t.Fatal(e)
	}
	if strings.Contains(string(b), "secret") {
		t.Fatal("API log leaked secrets")
	}
	for _, event := range events {
		if event.Status != 401 || event.Client != "Reeder/test" {
			t.Fatal(event)
		}
	}
}
