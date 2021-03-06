package zipkintracer

import (
	"encoding/binary"
	"fmt"
	otext "github.com/opentracing/opentracing-go/ext"
	"github.com/opentracing/opentracing-go/log"
	"net"
	"strconv"
	"time"

	"github.com/openzipkin-contrib/zipkin-go-opentracing/flag"
	"github.com/openzipkin-contrib/zipkin-go-opentracing/thrift/gen-go/zipkincore"
)

var (
	// JSONSpanKindResource will be regarded as a SA annotation by Zipkin.
	JSONSpanKindResource = otext.SpanKindEnum("resource")
)

// JSONRecorder implements the SpanRecorder interface.
type JSONRecorder struct {
	collector    AgnosticCollector
	debug        bool
	endpoint     *zipkincore.Endpoint
	materializer func(logFields []log.Field) ([]byte, error)
}

// JSONRecorderOption allows for functional options.
type JSONRecorderOption func(r *JSONRecorder)

// JSONWithLogFmtMaterializer will convert OpenTracing Log fields to a LogFmt representation.
func JSONWithLogFmtMaterializer() JSONRecorderOption {
	return func(r *JSONRecorder) {
		r.materializer = MaterializeWithLogFmt
	}
}

// JSONWithJSONMaterializer will convert OpenTracing Log fields to a JSON representation.
func JSONWithJSONMaterializer() JSONRecorderOption {
	return func(r *JSONRecorder) {
		r.materializer = MaterializeWithJSON
	}
}

// JSONWithStrictMaterializer will only record event Log fields and discard the rest.
func JSONWithStrictMaterializer() JSONRecorderOption {
	return func(r *JSONRecorder) {
		r.materializer = StrictZipkinMaterializer
	}
}

// NewJSONRecorder creates a new Zipkin Recorder backed by the provided Collector.
//
// hostPort and serviceName allow you to set the default Zipkin endpoint
// information which will be added to the application's standard core
// annotations. hostPort will be resolved into an IPv4 and/or IPv6 address and
// Port number, serviceName will be used as the application's service
// identifier.
//
// If application does not listen for incoming requests or an endpoint Context
// does not involve network address and/or port these cases can be solved like
// this:
//  # port is not applicable:
//  NewRecorder(c, debug, "192.168.1.12:0", "ServiceA")
//
//  # network address and port are not applicable:
//  NewRecorder(c, debug, "0.0.0.0:0", "ServiceB")
func NewJSONRecorder(c AgnosticCollector, debug bool, hostPort, serviceName string, options ...JSONRecorderOption) SpanRecorder {
	r := &JSONRecorder{
		collector:    c,
		debug:        debug,
		endpoint:     makeEndpoint(hostPort, serviceName),
		materializer: MaterializeWithLogFmt,
	}
	for _, opts := range options {
		opts(r)
	}
	return r
}

// RecordSpan converts a RawSpan into the Zipkin representation of a span
// and records it to the underlying collector.
func (r *JSONRecorder) RecordSpan(sp RawSpan) {
	if !sp.Context.Sampled {
		return
	}
	span := &CoreSpan{
		Name:    sp.Operation,
		ID:      fmt.Sprintf("%08x", sp.Context.SpanID),
		TraceID: fmt.Sprintf("%08x", sp.Context.TraceID.Low),
		Debug:   r.debug || (sp.Context.Flags&flag.Debug == flag.Debug),
	}

	if sp.Context.TraceID.High > 0 {
		span.TraceIDHigh = fmt.Sprintf("%08x", sp.Context.TraceID.High)
		span.TraceID = span.TraceIDHigh + span.TraceID
	}

	if sp.Context.ParentSpanID != nil {
		span.ParentID = fmt.Sprintf("%08x", *sp.Context.ParentSpanID)
	}

	// only send timestamp and duration if this process owns the current span.
	if sp.Context.Owner {
		timestamp := sp.Start.UnixNano() / 1e3
		duration := sp.Duration.Nanoseconds() / 1e3
		// since we always time our spans we will round up to 1 microsecond if the
		// span took less.
		if duration == 0 {
			duration = 1
		}
		span.Timestamp = timestamp
		span.Duration = duration
	}

	if kind, ok := sp.Tags[string(otext.SpanKind)]; ok {
		switch kind {
		case otext.SpanKindRPCClient, otext.SpanKindRPCClientEnum:
			annotateCore(span, sp.Start, zipkincore.CLIENT_SEND, r.endpoint)
			annotateCore(span, sp.Start.Add(sp.Duration), zipkincore.CLIENT_RECV, r.endpoint)
		case otext.SpanKindRPCServer, otext.SpanKindRPCServerEnum:
			annotateCore(span, sp.Start, zipkincore.SERVER_RECV, r.endpoint)
			annotateCore(span, sp.Start.Add(sp.Duration), zipkincore.SERVER_SEND, r.endpoint)
		case SpanKindResource:
			serviceName, ok := sp.Tags[string(otext.PeerService)]
			if !ok {
				serviceName = r.endpoint.GetServiceName()
			}
			host, ok := sp.Tags[string(otext.PeerHostname)].(string)
			if !ok {
				if r.endpoint.GetIpv4() > 0 {
					ip := make([]byte, 4)
					binary.BigEndian.PutUint32(ip, uint32(r.endpoint.GetIpv4()))
					host = net.IP(ip).To4().String()
				} else {
					ip := r.endpoint.GetIpv6()
					host = net.IP(ip).String()
				}
			}
			var sPort string
			port, ok := sp.Tags[string(otext.PeerPort)]
			if !ok {
				sPort = strconv.FormatInt(int64(r.endpoint.GetPort()), 10)
			} else {
				sPort = strconv.FormatInt(int64(port.(uint16)), 10)
			}
			re := makeEndpoint(net.JoinHostPort(host, sPort), serviceName.(string))
			if re != nil {
				annotateBinaryCore(span, zipkincore.SERVER_ADDR, serviceName, re)
			} else {
				fmt.Printf("endpoint creation failed: host: %q port: %q", host, sPort)
			}
			annotateCore(span, sp.Start, zipkincore.CLIENT_SEND, r.endpoint)
			annotateCore(span, sp.Start.Add(sp.Duration), zipkincore.CLIENT_RECV, r.endpoint)
		default:
			annotateBinaryCore(span, zipkincore.LOCAL_COMPONENT, r.endpoint.GetServiceName(), r.endpoint)
		}
		delete(sp.Tags, string(otext.SpanKind))
	} else {
		annotateBinaryCore(span, zipkincore.LOCAL_COMPONENT, r.endpoint.GetServiceName(), r.endpoint)
	}

	for key, value := range sp.Tags {
		annotateBinaryCore(span, key, value, r.endpoint)
	}

	_ = r.collector.Collect(span)
}

// annotateCore annotates the span with the given value.
func annotateCore(span *CoreSpan, timestamp time.Time, value string, host *zipkincore.Endpoint) {
	if timestamp.IsZero() {
		timestamp = time.Now()
	}
	span.Annotations = append(span.Annotations, &CoreAnnotation{
		Timestamp: timestamp.UnixNano() / 1e3,
		Value:     value,
		Host:      &CoreEndpoint{ServiceName: host.ServiceName, Port: host.Port, Ipv4: fmt.Sprintf("%d", host.Ipv4), Ipv6: string(host.Ipv6)},
	})
}

// annotateBinaryCore annotates the span with the given value.
func annotateBinaryCore(span *CoreSpan, key string, value interface{}, host *zipkincore.Endpoint) {
	if b, ok := value.(bool); ok {
		if b {
			value = "true"
		} else {
			value = "false"
		}
	}
	span.BinaryAnnotations = append(span.BinaryAnnotations, &CoreBinaryAnnotation{
		Key:      key,
		Value:    fmt.Sprintf("%+v", value),
		Endpoint: CoreEndpoint{ServiceName: host.ServiceName, Port: host.Port, Ipv4: fmt.Sprintf("%d", host.Ipv4), Ipv6: string(host.Ipv6)},
	})
}
