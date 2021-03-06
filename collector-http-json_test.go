package zipkintracer

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"sync"
	"testing"
	"time"
)

func TestJsonHttpCollector(t *testing.T) {
	t.Parallel()

	port := 18720
	server := newJSONHTTPServer(t, port)
	c, err := NewJSONHTTPCollector(fmt.Sprintf("http://localhost:%d/api/v1/spans", port),
		JSONHTTPBatchSize(1))
	if err != nil {
		t.Fatal(err)
	}

	var (
		serviceName  = "service"
		methodName   = "method"
		traceID      = uint64(17051370458307041793)
		spanID       = uint64(456)
		parentSpanID = uint64(0)
	)

	span := makeNewJSONSpan("1.2.3.4:1234", serviceName, methodName, traceID, spanID, parentSpanID, nil, true)
	if err := c.Collect(span); err != nil {
		t.Errorf("error during collection: %v", err)
	}

	if err = eventually(func() bool { return len(server.spans()) == 1 }, 1*time.Second); err != nil {
		t.Fatalf("never received a span %v", server.spans())
	}

	gotSpan := server.spans()[0]
	if want, have := methodName, gotSpan.Name; want != have {
		t.Errorf("want %q, have %q", want, have)
	}
	if want, have := fmt.Sprintf("%08x", traceID), gotSpan.TraceID; want != have {
		t.Errorf("want %s, have %s", want, have)
	}
	if want, have := fmt.Sprintf("%08x", spanID), gotSpan.ID; want != have {
		t.Errorf("want %s, have %s", want, have)
	}
	if want, have := "", gotSpan.ParentID; want != have {
		t.Errorf("want %s, have %s", want, have)
	}

}

func TestHighTraceIdJsonHttpCollector(t *testing.T) {
	t.Parallel()

	port := 18721
	server := newJSONHTTPServer(t, port)
	c, err := NewJSONHTTPCollector(fmt.Sprintf("http://localhost:%d/api/v1/spans", port),
		JSONHTTPBatchSize(1))
	if err != nil {
		t.Fatal(err)
	}

	var (
		serviceName  = "service"
		methodName   = "method"
		traceID      = uint64(17051370458307041793)
		highTraceID  = uint64(12313211111111111111)
		spanID       = uint64(456)
		parentSpanID = uint64(0)
	)

	span := makeNewJSONSpan("1.2.3.4:1234", serviceName, methodName, traceID, spanID, parentSpanID, &highTraceID, true)
	if err := c.Collect(span); err != nil {
		t.Errorf("error during collection: %v", err)
	}

	if err = eventually(func() bool { return len(server.spans()) == 1 }, 1*time.Second); err != nil {
		t.Fatalf("never received a span %v", server.spans())
	}

	gotSpan := server.spans()[0]
	if want, have := methodName, gotSpan.Name; want != have {
		t.Errorf("want %q, have %q", want, have)
	}
	if want, have := fmt.Sprintf("%08x", highTraceID) + fmt.Sprintf("%08x", traceID), gotSpan.TraceID; want != have {
		t.Errorf("want %s, have %s", want, have)
	}
	if want, have := fmt.Sprintf("%08x", spanID), gotSpan.ID; want != have {
		t.Errorf("want %s, have %s", want, have)
	}
	if want, have := "", gotSpan.ParentID; want != have {
		t.Errorf("want %s, have %s", want, have)
	}

}

type jsonHTTPServer struct {
	t            *testing.T
	zipkinSpans  []*CoreSpan
	zipkinHeader http.Header
	mutex        sync.RWMutex
}

func (s *jsonHTTPServer) spans() []*CoreSpan {
	s.mutex.RLock()
	defer s.mutex.RUnlock()
	return s.zipkinSpans
}

func newJSONHTTPServer(t *testing.T, port int) *jsonHTTPServer {
	server := &jsonHTTPServer{
		t:           t,
		zipkinSpans: make([]*CoreSpan, 0),
		mutex:       sync.RWMutex{},
	}

	handler := http.NewServeMux()

	handler.HandleFunc("/api/v1/spans", func(w http.ResponseWriter, r *http.Request) {
		contextType := r.Header.Get("Content-Type")
		if contextType != "application/json" {
			t.Fatalf("except Content-Type should be application/x-thrift, but is %s", contextType)
		}

		// clone headers from request
		headers := make(http.Header, len(r.Header))
		for k, vv := range r.Header {
			vv2 := make([]string, len(vv))
			copy(vv2, vv)
			headers[k] = vv2
		}

		body, err := ioutil.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		var spans []*CoreSpan
		if err := json.Unmarshal(body, &spans); err != nil {
			log.Fatal(err.Error())
		}

		server.mutex.Lock()
		defer server.mutex.Unlock()
		server.zipkinSpans = append(server.zipkinSpans, spans...)
		server.zipkinHeader = headers
	})

	handler.HandleFunc("/api/v1/sleep", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(serverSleep)
	})

	go func() {
		http.ListenAndServe(fmt.Sprintf(":%d", port), handler)
	}()

	return server
}

func makeNewJSONSpan(hostPort, serviceName, methodName string, traceID, spanID, parentSpanID uint64, highTraceID *uint64, debug bool) *CoreSpan {
	timestamp := time.Now().UnixNano() / 1e3
	span := &CoreSpan{
		Name:      methodName,
		TraceID:   fmt.Sprintf("%08x", traceID),
		ID:        fmt.Sprintf("%08x", spanID),
		Debug:     debug,
		Timestamp: timestamp,
	}
	if highTraceID != nil {
		span.TraceIDHigh = fmt.Sprintf("%08x", *highTraceID)
		span.TraceID = span.TraceIDHigh + span.TraceID
	}

	if parentSpanID > 0 {
		span.ParentID = fmt.Sprintf("%08x", parentSpanID)
	}

	return span
}
