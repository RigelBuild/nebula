// Package fakeadmin provides scripted HTTP and clock fakes for the Garage tools' tests.
package fakeadmin

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"time"
)

// Request is one recorded HTTP request. Body is the decoded JSON body, or nil when there is none.
type Request struct {
	Method        string
	URL           string
	Body          any
	Authorization string
}

// Reply is a scripted response. Status 0 means 200. Body nil means an empty response body.
type Reply struct {
	Status int
	Body   any
}

// Doer answers each request from a script. It is an http.RoundTripper, so Client wraps it in an http.Client.
type Doer struct {
	mu       sync.Mutex
	respond  func(Request) (Reply, error)
	requests []Request
}

// NewDoer returns a Doer that answers each request with respond. An error from respond becomes a transport error.
func NewDoer(respond func(Request) (Reply, error)) *Doer {
	return &Doer{respond: respond}
}

// Client returns an http.Client whose transport is d. It is what the code under test receives.
func (d *Doer) Client() *http.Client {
	return &http.Client{Transport: d}
}

// RoundTrip records the request, asks the script for a reply, and returns it as an HTTP response.
func (d *Doer) RoundTrip(req *http.Request) (*http.Response, error) {
	request := Request{Method: req.Method, URL: req.URL.String(), Authorization: req.Header.Get("Authorization")}
	if req.Body != nil {
		raw, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &request.Body); err != nil {
				return nil, err
			}
		}
	}
	d.mu.Lock()
	d.requests = append(d.requests, request)
	d.mu.Unlock()

	reply, err := d.respond(request)
	if err != nil {
		return nil, err
	}
	status := reply.Status
	if status == 0 {
		status = http.StatusOK
	}
	var text []byte
	if reply.Body != nil {
		encoded, err := json.Marshal(reply.Body)
		if err != nil {
			return nil, err
		}
		text = encoded
	}
	return &http.Response{
		StatusCode: status,
		Status:     http.StatusText(status),
		Header:     make(http.Header),
		Body:       io.NopCloser(bytes.NewReader(text)),
		Request:    req,
	}, nil
}

// Requests returns a copy of every request recorded so far, in arrival order.
func (d *Doer) Requests() []Request {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]Request(nil), d.requests...)
}

// Clock is a fake clock. Sleep advances it and records the duration.
type Clock struct {
	mu     sync.Mutex
	now    time.Time
	sleeps []time.Duration
}

// NewClock starts a fake clock at start.
func NewClock(start time.Time) *Clock {
	return &Clock{now: start}
}

// Now returns the fake time.
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Sleep records d and advances the fake time by d.
func (c *Clock) Sleep(_ context.Context, d time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sleeps = append(c.sleeps, d)
	c.now = c.now.Add(d)
	return nil
}

// Sleeps returns a copy of every recorded sleep duration.
func (c *Clock) Sleeps() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Duration(nil), c.sleeps...)
}
