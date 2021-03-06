// copy from https://github.com/grpc-ecosystem/grpc-gateway/tree/master/runtime

package context

import (
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/textproto"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/grpclog"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// MetadataHeaderPrefix is the http prefix that represents custom metadata
// parameters to or from a gRPC call.
const MetadataHeaderPrefix = "Grpc-Metadata-"

// MetadataPrefix is prepended to permanent HTTP header keys (as specified
// by the IANA) when added to the gRPC context.
const MetadataPrefix = "pbgo-grpc-"

// MetadataTrailerPrefix is prepended to gRPC metadata as it is converted to
// HTTP headers in a response handled by pbgo-grpc
const MetadataTrailerPrefix = "Grpc-Trailer-"

const metadataGrpcTimeout = "Grpc-Timeout"
const metadataHeaderBinarySuffix = "-Bin"

const xForwardedFor = "X-Forwarded-For"
const xForwardedHost = "X-Forwarded-Host"

var (
	// DefaultContextTimeout is used for gRPC call context.WithTimeout whenever a Grpc-Timeout inbound
	// header isn't present. If the value is 0 the sent `context` will not have a timeout.
	DefaultContextTimeout = 0 * time.Second
)

func decodeBinHeader(v string) ([]byte, error) {
	if len(v)%4 == 0 {
		// Input was padded, or padding was not necessary.
		return base64.StdEncoding.DecodeString(v)
	}
	return base64.RawStdEncoding.DecodeString(v)
}

// HeaderMatcherFunc checks whether a header key should be forwarded to/from gRPC context.
type HeaderMatcherFunc func(string) (string, bool)

// DefaultHeaderMatcher is used to pass http request headers to/from gRPC context. This adds permanent HTTP header
// keys (as specified by the IANA) to gRPC context with pbgo-grpc- prefix. HTTP headers that start with
// 'Grpc-Metadata-' are mapped to gRPC metadata after removing prefix 'Grpc-Metadata-'.
func DefaultHeaderMatcher(key string) (string, bool) {
	key = textproto.CanonicalMIMEHeaderKey(key)
	if isPermanentHTTPHeader(key) {
		return MetadataPrefix + key, true
	} else if strings.HasPrefix(key, MetadataHeaderPrefix) {
		return key[len(MetadataHeaderPrefix):], true
	}
	return "", false
}

// AnnotateIncomingContext adds context information such as metadata from the request.
//
// At a minimum, the RemoteAddr is included in the fashion of "X-Forwarded-For",
// except that the forwarded destination is not another HTTP service but rather
// a gRPC service.
func AnnotateIncomingContext(
	ctx context.Context, req *http.Request,
	incomingHeaderMatcher HeaderMatcherFunc,
	metadataAnnotators ...func(context.Context, *http.Request) metadata.MD,
) (context.Context, error) {
	md, err := annotateContext(ctx, req, incomingHeaderMatcher, metadataAnnotators...)
	if err != nil {
		return nil, err
	}
	if len(md) == 0 {
		return ctx, nil
	}
	return metadata.NewIncomingContext(ctx, md), nil
}

// AnnotateOutgoingContext adds context information such as metadata from the request.
//
// At a minimum, the RemoteAddr is included in the fashion of "X-Forwarded-For",
// except that the forwarded destination is not another HTTP service but rather
// a gRPC service.
func AnnotateOutgoingContext(
	ctx context.Context, req *http.Request,
	incomingHeaderMatcher HeaderMatcherFunc,
	metadataAnnotators ...func(context.Context, *http.Request) metadata.MD,
) (context.Context, error) {
	md, err := annotateContext(ctx, req, incomingHeaderMatcher, metadataAnnotators...)
	if err != nil {
		return nil, err
	}
	if len(md) == 0 {
		return ctx, nil
	}
	return metadata.NewOutgoingContext(ctx, md), nil
}

func annotateContext(
	ctx context.Context, req *http.Request,
	fnHeaderMatcher HeaderMatcherFunc,
	metadataAnnotators ...func(context.Context, *http.Request) metadata.MD,
) (metadata.MD, error) {
	var pairs []string
	timeout := DefaultContextTimeout
	if tm := req.Header.Get(metadataGrpcTimeout); tm != "" {
		var err error
		timeout, err = timeoutDecode(tm)
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "invalid grpc-timeout: %s", tm)
		}
	}

	if fnHeaderMatcher == nil {
		fnHeaderMatcher = DefaultHeaderMatcher
	}

	for key, vals := range req.Header {
		for _, val := range vals {
			key = textproto.CanonicalMIMEHeaderKey(key)
			// For backwards-compatibility, pass through 'authorization' header with no prefix.
			if key == "Authorization" {
				pairs = append(pairs, "authorization", val)
			}
			if h, ok := fnHeaderMatcher(key); ok {
				// Handles "-bin" metadata in grpc, since grpc will do another base64
				// encode before sending to server, we need to decode it first.
				if strings.HasSuffix(key, metadataHeaderBinarySuffix) {
					b, err := decodeBinHeader(val)
					if err != nil {
						return nil, status.Errorf(codes.InvalidArgument, "invalid binary header %s: %s", key, err)
					}

					val = string(b)
				}
				pairs = append(pairs, h, val)
			}
		}
	}
	if host := req.Header.Get(xForwardedHost); host != "" {
		pairs = append(pairs, strings.ToLower(xForwardedHost), host)
	} else if req.Host != "" {
		pairs = append(pairs, strings.ToLower(xForwardedHost), req.Host)
	}

	if addr := req.RemoteAddr; addr != "" {
		if remoteIP, _, err := net.SplitHostPort(addr); err == nil {
			if fwd := req.Header.Get(xForwardedFor); fwd == "" {
				pairs = append(pairs, strings.ToLower(xForwardedFor), remoteIP)
			} else {
				pairs = append(pairs, strings.ToLower(xForwardedFor), fmt.Sprintf("%s, %s", fwd, remoteIP))
			}
		} else {
			grpclog.Infof("invalid remote addr: %s", addr)
		}
	}

	if timeout != 0 {
		ctx, _ = context.WithTimeout(ctx, timeout)
	}
	if len(pairs) == 0 {
		return nil, nil
	}
	md := metadata.Pairs(pairs...)
	for _, mda := range metadataAnnotators {
		md = metadata.Join(md, mda(ctx, req))
	}

	return md, nil
}

// ServerMetadata consists of metadata sent from gRPC server.
type ServerMetadata struct {
	HeaderMD  metadata.MD
	TrailerMD metadata.MD
}

type serverMetadataKey struct{}

// NewServerMetadataContext creates a new context with ServerMetadata
func NewServerMetadataContext(ctx context.Context, md ServerMetadata) context.Context {
	return context.WithValue(ctx, serverMetadataKey{}, md)
}

// ServerMetadataFromContext returns the ServerMetadata in ctx
func ServerMetadataFromContext(ctx context.Context) (md ServerMetadata, ok bool) {
	md, ok = ctx.Value(serverMetadataKey{}).(ServerMetadata)
	return
}

func timeoutDecode(s string) (time.Duration, error) {
	size := len(s)
	if size < 2 {
		return 0, fmt.Errorf("timeout string is too short: %q", s)
	}
	d, ok := timeoutUnitToDuration(s[size-1])
	if !ok {
		return 0, fmt.Errorf("timeout unit is not recognized: %q", s)
	}
	t, err := strconv.ParseInt(s[:size-1], 10, 64)
	if err != nil {
		return 0, err
	}
	return d * time.Duration(t), nil
}

func timeoutUnitToDuration(u uint8) (d time.Duration, ok bool) {
	switch u {
	case 'H':
		return time.Hour, true
	case 'M':
		return time.Minute, true
	case 'S':
		return time.Second, true
	case 'm':
		return time.Millisecond, true
	case 'u':
		return time.Microsecond, true
	case 'n':
		return time.Nanosecond, true
	default:
	}
	return
}

// isPermanentHTTPHeader checks whether hdr belongs to the list of
// permenant request headers maintained by IANA.
// http://www.iana.org/assignments/message-headers/message-headers.xml
func isPermanentHTTPHeader(hdr string) bool {
	switch hdr {
	case
		"Accept",
		"Accept-Charset",
		"Accept-Language",
		"Accept-Ranges",
		"Authorization",
		"Cache-Control",
		"Content-Type",
		"Cookie",
		"Date",
		"Expect",
		"From",
		"Host",
		"If-Match",
		"If-Modified-Since",
		"If-None-Match",
		"If-Schedule-Tag-Match",
		"If-Unmodified-Since",
		"Max-Forwards",
		"Origin",
		"Pragma",
		"Referer",
		"User-Agent",
		"Via",
		"Warning":
		return true
	}
	return false
}
