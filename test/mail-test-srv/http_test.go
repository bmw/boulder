package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func reqAndRecorder(t testing.TB, method, relativeUrl string, body io.Reader) (*httptest.ResponseRecorder, *http.Request) {
	endURL := fmt.Sprintf("http://localhost:9381%s", relativeUrl)
	r, err := http.NewRequest(method, endURL, body)
	if err != nil {
		t.Fatalf("could not construct request: %v", err)
	}
	return httptest.NewRecorder(), r
}

func TestHTTPClear(t *testing.T) {
	w, r := reqAndRecorder(t, "POST", "/clear", nil)
	allReceivedMail = []rcvdMail{rcvdMail{}}
	httpClear(w, r)
	if w.Code != 200 {
		t.Errorf("expected 200, got %d", w.Code)
	}
	if len(allReceivedMail) != 0 {
		t.Error("/clear failed to clear mail buffer")
	}

	w, r = reqAndRecorder(t, "GET", "/clear", nil)
	allReceivedMail = []rcvdMail{rcvdMail{}}
	httpClear(w, r)
	if w.Code != 405 {
		t.Errorf("expected 405, got %d", w.Code)
	}
	if len(allReceivedMail) != 1 {
		t.Error("GET /clear cleared the mail buffer")
	}
}

func TestHTTPCount(t *testing.T) {
	allReceivedMail = []rcvdMail{
		rcvdMail{From: "a", To: "b"},
		rcvdMail{From: "a", To: "b"},
		rcvdMail{From: "a", To: "c"},
		rcvdMail{From: "c", To: "a"},
		rcvdMail{From: "c", To: "b"},
	}

	tests := []struct {
		URL   string
		Count int
	}{
		{URL: "/count", Count: 5},
		{URL: "/count?from=a", Count: 3},
		{URL: "/count?from=c", Count: 2},
		{URL: "/count?to=b", Count: 3},
		{URL: "/count?from=a&to=b", Count: 2},
	}

	var buf bytes.Buffer
	for i, test := range tests {
		w, r := reqAndRecorder(t, "GET", test.URL, nil)
		buf.Reset()
		w.Body = &buf

		httpCount(w, r)
		if w.Code != 200 {
			t.Errorf("%d: expected 200, got %d", i, w.Code)
		}
		n, err := strconv.Atoi(strings.TrimSpace(buf.String()))
		if err != nil {
			t.Errorf("%d: expected a number, got '%s'", i, buf.String())
		} else if n != test.Count {
			t.Errorf("%d: expected %d, got %d", i, test.Count, n)
		}
	}
}
