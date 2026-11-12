package sse

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"net"
	"net/http"
	"net/http/httptrace"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/grafana/sobek"
	"go.k6.io/k6/js/common"
	"go.k6.io/k6/js/modules"
	httpModule "go.k6.io/k6/js/modules/k6/http"
	"go.k6.io/k6/js/promises"
	"go.k6.io/k6/metrics"
)

// AsyncClient represents an asynchronous SSE client
type AsyncClient struct {
	ctx        context.Context
	cancel     context.CancelFunc
	httpClient *http.Client
	url        string
	request    *http.Request
	response   *http.Response

	// Event handling
	eventChan chan Event
	doneChan  chan struct{}

	// State management
	mu       sync.RWMutex
	isOpen   bool
	isClosed bool
	lastErr  error

	// k6 integration
	vu             modules.VU
	tagsAndMeta    *metrics.TagsAndMeta
	samplesOutput  chan<- metrics.SampleContainer
	builtinMetrics *metrics.BuiltinMetrics
	sseMetrics     *sseMetrics
}

// OpenAsync opens an SSE connection asynchronously
// It returns the client immediately, while the connection happens in the background.
func (s *sse) OpenAsync(url string, paramsObj sobek.Value) (*AsyncClient, error) {
	state := s.vu.State()
	if state == nil {
		return nil, common.NewInitContextError("using sse in the init context is not supported")
	}

	// 1. Parse Options
	params, err := parseAsyncOptions(s.vu, url, paramsObj)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithCancel(s.vu.Context())

	// 2. Configure HTTP Client
	// Overriding the NextProtos to avoid talking http2 for SSE if needed
	var tlsConfig *tls.Config
	if state.TLSConfig != nil {
		tlsConfig = state.TLSConfig.Clone()
		tlsConfig.NextProtos = []string{"http/1.1"}
	}

	// Implementation of Connection Pooling via Transport configuration
	transport := &http.Transport{
		DialContext:         state.Dialer.DialContext,
		Proxy:               http.ProxyFromEnvironment,
		TLSClientConfig:     tlsConfig,
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     90 * time.Second,
		DisableKeepAlives: state.Options.NoConnectionReuse.ValueOrZero() ||
			state.Options.NoVUConnectionReuse.ValueOrZero(),
	}

	httpClient := &http.Client{
		Transport: transport,
	}

	if params.Jar != nil {
		httpClient.Jar = params.Jar
	}

	// 3. Initialize Client
	client := &AsyncClient{
		ctx:            ctx,
		cancel:         cancel,
		httpClient:     httpClient,
		url:            url,
		eventChan:      make(chan Event, 100), // Buffer slightly to handle bursts
		doneChan:       make(chan struct{}),
		isOpen:         false,
		isClosed:       false,
		vu:             s.vu,
		tagsAndMeta:    params.tagsAndMeta,
		samplesOutput:  state.Samples,
		builtinMetrics: state.BuiltinMetrics,
		sseMetrics:     s.metrics,
	}

	// Start Connection in Background with retry logic
	go client.connectWithRetry(params)

	return client, nil
}

func (c *AsyncClient) connectWithRetry(opts *asyncOptions) {
	defer close(c.doneChan)

	var bodyBytes []byte
	if opts.Body != "" {
		bodyBytes = []byte(opts.Body)
	}

	for attempt := 0; attempt <= opts.MaxRetries; attempt++ {
		// Check for context cancellation before starting an attempt
		select {
		case <-c.ctx.Done():
			return
		default:
		}

		// Exponential Backoff with Jitter logic
		if attempt > 0 {
			c.sleepForRetry(opts, attempt)
		}

		if success := c.doConnectAttempt(opts, attempt, bodyBytes); success {
			return
		}
	}
}

func (c *AsyncClient) sleepForRetry(opts *asyncOptions, attempt int) {
	backoffFactor := math.Pow(2, float64(attempt-1))
	delay := time.Duration(float64(opts.BaseDelay) * backoffFactor)
	if delay > opts.MaxDelay {
		delay = opts.MaxDelay
	}

	// Use crypto/rand for secure jitter calculation
	// Add 20% jitter
	maxJitter := int64(float64(delay) * 0.2)
	var jitter time.Duration
	if maxJitter > 0 {
		if n, err := rand.Int(rand.Reader, big.NewInt(maxJitter)); err == nil {
			jitter = time.Duration(n.Int64())
		}
	}

	select {
	case <-time.After(delay + jitter):
	case <-c.ctx.Done():
	}
}

// Re-create request for each attempt
func (c *AsyncClient) doConnectAttempt(opts *asyncOptions, attempt int, bodyBytes []byte) bool {
	req, err := http.NewRequestWithContext(c.ctx, opts.Method, c.url, bytes.NewReader(bodyBytes))
	if err != nil {
		c.setError(fmt.Errorf("request creation failed: %w", err))
		return true // Stop retrying
	}

	// Apply headers
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("Connection", "keep-alive")
	for k, v := range opts.Headers {
		req.Header.Set(k, v)
	}

	// Trace for IP tags
	trace := &httptrace.ClientTrace{
		GotConn: func(connInfo httptrace.GotConnInfo) {
			if c.vu.State().Options.SystemTags.Has(metrics.TagIP) {
				if ip, _, err2 := net.SplitHostPort(connInfo.Conn.RemoteAddr().String()); err2 == nil {
					c.tagsAndMeta.SetSystemTagOrMeta(metrics.TagIP, ip)
				}
			}
		},
	}
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), trace))
	c.request = req

	// Metrics and Request
	start := time.Now()
	//nolint:gosec // k6 is a load testing tool; the user intentionally specifies the target URL
	resp, err := c.httpClient.Do(req)
	end := time.Now()

	c.pushConnectionMetrics(start, end)

	if err != nil {
		if attempt == opts.MaxRetries {
			c.setError(fmt.Errorf("connection failed after %d retries: %w", attempt, err))
			return true // Exhausted retries
		}
		return false // Retry
	}

	// Update Status Tag
	if c.vu.State().Options.SystemTags.Has(metrics.TagStatus) {
		c.tagsAndMeta.SetSystemTagOrMeta(metrics.TagStatus, strconv.Itoa(resp.StatusCode))
	}

	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		if attempt == opts.MaxRetries {
			c.setError(fmt.Errorf("unexpected status code: %d", resp.StatusCode))
			return true // Exhausted retries
		}
		return false // Retry
	}

	c.handleSuccessfulConnect(resp, start)
	return true
}

func (c *AsyncClient) pushConnectionMetrics(start, end time.Time) {
	if c.samplesOutput != nil {
		metrics.PushIfNotDone(c.ctx, c.samplesOutput, metrics.ConnectedSamples{
			Samples: []metrics.Sample{
				{
					TimeSeries: metrics.TimeSeries{
						Metric: c.builtinMetrics.HTTPReqSending,
						Tags:   c.tagsAndMeta.Tags,
					},
					Time:     start,
					Metadata: c.tagsAndMeta.Metadata,
					Value:    metrics.D(end.Sub(start)),
				},
			},
			Tags: c.tagsAndMeta.Tags,
			Time: start,
		})
	}
}

func (c *AsyncClient) handleSuccessfulConnect(resp *http.Response, start time.Time) {
	contentType := resp.Header.Get("Content-Type")
	if !strings.Contains(strings.ToLower(contentType), "text/event-stream") {
		_ = resp.Body.Close()
		c.setError(fmt.Errorf("unexpected content-type: %s", contentType))
		return
	}

	c.mu.Lock()
	c.response = resp
	c.isOpen = true
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		c.isOpen = false
		if c.response != nil && c.response.Body != nil {
			// Drain body to allow connection reuse if possible
			_, _ = io.Copy(io.Discard, c.response.Body)
			_ = c.response.Body.Close()
		}
		c.mu.Unlock()

		// Push final request metrics
		finish := time.Now()
		if c.samplesOutput != nil {
			metrics.PushIfNotDone(c.ctx, c.samplesOutput, metrics.ConnectedSamples{
				Samples: []metrics.Sample{
					{
						TimeSeries: metrics.TimeSeries{Metric: c.builtinMetrics.HTTPReqs, Tags: c.tagsAndMeta.Tags},
						Time:       finish, Value: 1,
					},
					{
						TimeSeries: metrics.TimeSeries{Metric: c.builtinMetrics.HTTPReqDuration, Tags: c.tagsAndMeta.Tags},
						Time:       finish, Value: metrics.D(finish.Sub(start)),
					},
				},
				Tags: c.tagsAndMeta.Tags,
				Time: finish,
			})
		}
	}()

	c.readEvents()
}

// Wraps SSE in a channel, follow the SSE format described in:
// https://developer.mozilla.org/en-US/docs/Web/API/Server-sent_events/Using_server-sent_events
func (c *AsyncClient) readEvents() {
	if c.response == nil {
		return
	}

	reader := bufio.NewReader(c.response.Body)
	ev := Event{}
	var buf bytes.Buffer

	for {
		// Check for cancellation
		select {
		case <-c.ctx.Done():
			return
		default:
		}

		line, err := reader.ReadBytes('\n')
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, context.Canceled) {
				c.setError(fmt.Errorf("read error: %w", err))
			}
			return
		}

		if c.processLine(line, &ev, &buf) {
			return // Context done signaled inside processLine
		}
	}
}

func (c *AsyncClient) processLine(line []byte, ev *Event, buf *bytes.Buffer) bool {
	switch {
	// id of event
	case hasPrefix(line, "id: "):
		ev.ID = stripPrefix(line, 4)
	case hasPrefix(line, "id:"):
		ev.ID = stripPrefix(line, 3)

	// Comment
	case hasPrefix(line, ": "):
		ev.Comment = stripPrefix(line, 2)
	case hasPrefix(line, ":"):
		ev.Comment = stripPrefix(line, 1)

	// name of event
	case hasPrefix(line, "event: "):
		ev.Name = stripPrefix(line, 7)
	case hasPrefix(line, "event:"):
		ev.Name = stripPrefix(line, 6)

	// event data
	case hasPrefix(line, "data: "):
		buf.Write(line[6:])
	case hasPrefix(line, "data:"):
		buf.Write(line[5:])

	case hasPrefix(line, "retry:"):
		// ignore

	// end of event
	case isLineEnd(line):
		// Trailing newlines are removed.
		ev.Data = strings.TrimRightFunc(buf.String(), func(r rune) bool {
			return r == '\r' || r == '\n'
		})

		// Only emit if we have data or an event name (ignore keep-alives)
		if ev.Data != "" || ev.Name != "" {
			c.pushEventReceivedMetric()
			select {
			case c.eventChan <- *ev:
				buf.Reset()
				*ev = Event{}
			case <-c.ctx.Done():
				return true
			}
		} else {
			buf.Reset()
			*ev = Event{}
		}
	}
	return false
}

func (c *AsyncClient) pushEventReceivedMetric() {
	if c.samplesOutput != nil && c.sseMetrics != nil {
		metrics.PushIfNotDone(c.ctx, c.samplesOutput, metrics.Sample{
			TimeSeries: metrics.TimeSeries{
				Metric: c.sseMetrics.SSEEventReceived,
				Tags:   c.tagsAndMeta.Tags,
			},
			Time:     time.Now(),
			Metadata: c.tagsAndMeta.Metadata,
			Value:    1,
		})
	}
}

// PollEvents retrieves events that have been received (non-blocking)
func (c *AsyncClient) PollEvents() []Event {
	c.mu.RLock()
	if c.isClosed {
		c.mu.RUnlock()
		return []Event{}
	}
	c.mu.RUnlock()

	events := make([]Event, 0)
	for {
		select {
		case event := <-c.eventChan:
			events = append(events, event)
		default:
			return events
		}
	}
}

// WaitForEvent waits for an event matching the predicate (blocking)
func (c *AsyncClient) WaitForEvent(predicateFn sobek.Value, timeoutMs int) (Event, error) {
	c.mu.RLock()
	if c.isClosed {
		c.mu.RUnlock()
		return Event{}, fmt.Errorf("client is closed")
	}
	c.mu.RUnlock()

	// Create a nil channel by default. Reading from a nil channel blocks forever,
	// which effectively disables the timeout case in the select block below.
	var timeoutChan <-chan time.Time
	if timeoutMs > 0 {
		timer := time.NewTimer(time.Duration(timeoutMs) * time.Millisecond)
		defer timer.Stop()
		timeoutChan = timer.C
	}

	for {
		select {
		case event := <-c.eventChan:
			match, err := c.evaluatePredicate(predicateFn, event)
			if err != nil {
				return Event{}, err
			}
			if match {
				return event, nil
			}
		case <-timeoutChan:
			return Event{}, fmt.Errorf("timeout waiting for event")
		case <-c.doneChan:
			if err := c.GetError(); err != nil {
				return Event{}, err
			}
			return Event{}, fmt.Errorf("connection closed")
		case <-c.ctx.Done():
			return Event{}, fmt.Errorf("context cancelled")
		}
	}
}

// WaitForEventAsync returns a Promise that resolves when the event is received
func (c *AsyncClient) WaitForEventAsync(predicateFn sobek.Value, timeoutMs int) *sobek.Promise {
	promise, resolve, reject := promises.New(c.vu)

	callbackChan := make(chan func(func() error), 1)

	// Initial registration
	callbackChan <- c.vu.RegisterCallback()

	go func() {
		// Quick check before waiting
		c.mu.RLock()
		if c.isClosed {
			c.mu.RUnlock()
			cb := <-callbackChan
			cb(func() error {
				reject(fmt.Errorf("client is closed"))
				return nil
			})
			return
		}
		c.mu.RUnlock()

		// Create a nil channel by default. Reading from a nil channel blocks forever,
		// which effectively disables the timeout case in the select block below.
		var timeoutChan <-chan time.Time
		if timeoutMs > 0 {
			timer := time.NewTimer(time.Duration(timeoutMs) * time.Millisecond)
			defer timer.Stop()
			timeoutChan = timer.C
		}

		for {
			select {
			case event := <-c.eventChan:
				// Get the *current* callback handle
				cb := <-callbackChan

				// We use this bool to signal the Go loop to exit
				shouldExit := false

				cb(func() error {
					match, err := c.evaluatePredicate(predicateFn, event)
					if err != nil {
						reject(err)
						shouldExit = true
						return nil
					}

					if match {
						resolve(event)
						shouldExit = true
					} else {
						// If we didn't match, we need to listen for the next event.
						// We are on the JS thread, so we can register a NEW callback.
						callbackChan <- c.vu.RegisterCallback()
					}
					return nil
				})

				if shouldExit {
					return
				}

			case <-timeoutChan:
				cb := <-callbackChan
				cb(func() error {
					reject(fmt.Errorf("timeout waiting for event"))
					return nil
				})
				return

			case <-c.doneChan:
				cb := <-callbackChan
				cb(func() error {
					if err := c.GetError(); err != nil {
						reject(err)
					} else {
						reject(fmt.Errorf("connection closed"))
					}
					return nil
				})
				return

			case <-c.ctx.Done():
				cb := <-callbackChan
				cb(func() error {
					reject(fmt.Errorf("context cancelled"))
					return nil
				})
				return
			}
		}
	}()

	return promise
}

func (c *AsyncClient) evaluatePredicate(predicateFn sobek.Value, event Event) (bool, error) {
	if predicateFn == nil || sobek.IsUndefined(predicateFn) || sobek.IsNull(predicateFn) {
		return true, nil
	}

	rt := c.vu.Runtime()
	callable, isCallable := sobek.AssertFunction(predicateFn)
	if !isCallable {
		return false, fmt.Errorf("predicate is not a function")
	}

	result, err := callable(sobek.Undefined(), rt.ToValue(event))
	if err != nil {
		return false, fmt.Errorf("predicate error: %w", err)
	}

	return result.ToBoolean(), nil
}

// IsOpen returns true if the connection is currently open.
func (c *AsyncClient) IsOpen() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.isOpen
}

// GetError returns the last error encountered by the client.
func (c *AsyncClient) GetError() error {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.lastErr
}

// Close closes the SSE connection and cleans up background routines.
func (c *AsyncClient) Close() error {
	c.mu.Lock()
	if c.isClosed {
		c.mu.Unlock()
		return nil
	}
	c.isClosed = true
	c.mu.Unlock()

	c.cancel()

	select {
	case <-c.doneChan:
		return nil
	case <-time.After(5 * time.Second):
		return fmt.Errorf("timeout waiting for connection to close")
	}
}

// GetStatus returns the HTTP status code of the connection handshake.
func (c *AsyncClient) GetStatus() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.response != nil {
		return c.response.StatusCode
	}
	return 0
}

func (c *AsyncClient) setError(err error) {
	c.mu.Lock()
	if c.lastErr == nil {
		c.lastErr = err
	}
	c.mu.Unlock()
}

// Helpers for options parsing

type asyncOptions struct {
	Method      string
	Body        string
	Headers     map[string]string
	Timeout     time.Duration
	Jar         http.CookieJar
	tagsAndMeta *metrics.TagsAndMeta
	MaxRetries  int
	BaseDelay   time.Duration
	MaxDelay    time.Duration
}

func parseAsyncOptions(vu modules.VU, url string, paramsObj sobek.Value) (*asyncOptions, error) {
	state := vu.State()
	// Default options
	opts := &asyncOptions{
		Method:     "GET",
		Headers:    make(map[string]string),
		Timeout:    60 * time.Second,
		MaxRetries: 0,
		BaseDelay:  1000 * time.Millisecond,
		MaxDelay:   30000 * time.Millisecond,
	}

	// Capture initial tags
	tagsAndMeta := state.Tags.GetCurrentValues()
	tagsAndMeta.SetSystemTagOrMetaIfEnabled(state.Options.SystemTags, metrics.TagURL, url)
	opts.tagsAndMeta = &tagsAndMeta

	// Set default User-Agent
	opts.Headers["User-Agent"] = state.Options.UserAgent.String
	opts.Jar = state.CookieJar

	// If no params object, return defaults
	if paramsObj == nil || sobek.IsUndefined(paramsObj) || sobek.IsNull(paramsObj) {
		return opts, nil
	}

	rt := vu.Runtime()
	paramsMap := paramsObj.ToObject(rt)
	if paramsMap == nil {
		return opts, nil
	}

	applyBasicOptions(rt, opts, paramsMap)
	applyRetryOptions(opts, paramsMap)

	if err := applyTagsAndJarOptions(rt, opts, paramsMap); err != nil {
		return nil, err
	}

	return opts, nil
}

func applyBasicOptions(rt *sobek.Runtime, opts *asyncOptions, paramsMap *sobek.Object) {
	if val := paramsMap.Get("method"); val != nil && !sobek.IsUndefined(val) {
		opts.Method = strings.ToUpper(val.String())
	}
	if val := paramsMap.Get("body"); val != nil && !sobek.IsUndefined(val) {
		opts.Body = val.String()
	}
	if val := paramsMap.Get("headers"); val != nil && !sobek.IsUndefined(val) {
		if headersObj := val.ToObject(rt); headersObj != nil {
			for _, key := range headersObj.Keys() {
				opts.Headers[key] = headersObj.Get(key).String()
			}
		}
	}
	if val := paramsMap.Get("timeout"); val != nil && !sobek.IsUndefined(val) {
		if t := val.ToInteger(); t > 0 {
			opts.Timeout = time.Duration(t) * time.Millisecond
		} else if tStr := val.ToString().String(); tStr != "" {
			if d, err := time.ParseDuration(tStr); err == nil {
				opts.Timeout = d
			}
		}
	}
}

func applyRetryOptions(opts *asyncOptions, paramsMap *sobek.Object) {
	if val := paramsMap.Get("maxRetries"); val != nil && !sobek.IsUndefined(val) {
		opts.MaxRetries = int(val.ToInteger())
	}
	if val := paramsMap.Get("baseDelay"); val != nil && !sobek.IsUndefined(val) {
		if t := val.ToInteger(); t > 0 {
			opts.BaseDelay = time.Duration(t) * time.Millisecond
		}
	}
	if val := paramsMap.Get("maxDelay"); val != nil && !sobek.IsUndefined(val) {
		if t := val.ToInteger(); t > 0 {
			opts.MaxDelay = time.Duration(t) * time.Millisecond
		}
	}
}

func applyTagsAndJarOptions(rt *sobek.Runtime, opts *asyncOptions, paramsMap *sobek.Object) error {
	if tags := paramsMap.Get("tags"); tags != nil && !sobek.IsUndefined(tags) {
		if err := common.ApplyCustomUserTags(rt, opts.tagsAndMeta, tags); err != nil {
			return fmt.Errorf("invalid metric tags: %w", err)
		}
	}
	if jarValue := paramsMap.Get("jar"); jarValue != nil && !sobek.IsUndefined(jarValue) {
		// Try to unwrap k6 http.CookieJar
		if exported := jarValue.Export(); exported != nil {
			if k6Jar, ok := exported.(*httpModule.CookieJar); ok {
				opts.Jar = k6Jar.Jar
			} else if stdJar, ok := exported.(http.CookieJar); ok {
				opts.Jar = stdJar
			}
		}
	}
	return nil
}
