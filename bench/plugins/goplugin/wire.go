package goplugin

import (
	"context"
	"encoding/json"
	"fmt"

	goplugin "github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"
	"google.golang.org/grpc/encoding"
)

// The wire contract, written as the .proto file it would be in production.
// protoc could not be run in the sandbox that built this prototype, so the
// two services are hand-written grpc.ServiceDesc values and the messages
// are JSON (the same shapes as the WASM ABI v0 of ADR 0011), carried by a
// gRPC codec registered under the content-subtype "angelia-json".
// go-plugin's own services (broker, controller, stdio, health) keep
// protobuf.
//
//	service Source {                      // served by the plugin
//	  rpc Configure(ConfigureRequest) returns (Empty);
//	  rpc Validate(Target) returns (ValidateResponse);
//	  rpc List(ListRequest) returns (ListResponse);
//	}
//	service Host {                        // served by the core, dialled back through the GRPCBroker
//	  rpc HTTP(HTTPRequest) returns (HTTPResponse);
//	}

// Handshake returns the handshake both sides check before any RPC. The
// cookie is not a security feature: it only stops a plugin binary from
// being run by hand.
func Handshake() goplugin.HandshakeConfig {
	return goplugin.HandshakeConfig{
		ProtocolVersion:  1,
		MagicCookieKey:   "ANGELIA_PLUGIN",
		MagicCookieValue: "source-v0",
	}
}

// PluginName is the key of the source plugin in the go-plugin PluginSet.
const PluginName = "source"

const (
	sourceService = "angelia.plugin.v0.Source"
	hostService   = "angelia.plugin.v0.Host"
	codecName     = "angelia-json"
)

// jsonCodec is the gRPC codec of the two Angelia services.
type jsonCodec struct{}

func (jsonCodec) Marshal(v any) ([]byte, error)      { return json.Marshal(v) }
func (jsonCodec) Unmarshal(data []byte, v any) error { return json.Unmarshal(data, v) }
func (jsonCodec) Name() string                       { return codecName }

func init() { //nolint:gochecknoinits // gRPC looks codecs up by content-subtype in its registry.
	encoding.RegisterCodec(jsonCodec{})
}

// callOpt selects the JSON codec for a client call.
var callOpt = grpc.CallContentSubtype(codecName)

// State mirrors source.FetchState.
type State struct {
	ETag         string `json:"etag,omitempty"`
	LastModified string `json:"last_modified,omitempty"`
	Cursor       string `json:"cursor,omitempty"`
}

// Target mirrors source.Target.
type Target struct {
	Kind        string            `json:"kind"`
	Coordinates map[string]string `json:"coordinates"`
}

// ConfigureRequest is sent once after the plugin starts.
type ConfigureRequest struct {
	Config   map[string]string `json:"config"`
	BrokerID uint32            `json:"broker_id"` // where the Host service listens
}

// Empty is an empty message.
type Empty struct{}

// ValidateResponse holds the reason a target is invalid, empty if valid.
type ValidateResponse struct {
	Reason string `json:"reason,omitempty"`
}

// ListRequest is one List call. CallID ties the plugin's HTTP requests to
// the call, for its request budget and policy violations.
type ListRequest struct {
	CallID string `json:"call_id"`
	Target Target `json:"target"`
	State  State  `json:"state"`
}

// Version mirrors source.Version; PublishedAt is RFC 3339.
type Version struct {
	Raw         string         `json:"raw"`
	PublishedAt string         `json:"published_at,omitempty"`
	Prerelease  *bool          `json:"prerelease,omitempty"`
	Yanked      bool           `json:"yanked"`
	URL         string         `json:"url,omitempty"`
	NotesURL    string         `json:"notes_url,omitempty"`
	Metadata    map[string]any `json:"metadata,omitempty"`
}

// ListResponse is the result of a List call.
type ListResponse struct {
	Versions    []Version `json:"versions"`
	State       State     `json:"state"`
	NotModified bool      `json:"not_modified"`
}

// Validators are the conditional-request validators of an HTTP request.
type Validators struct {
	ETag         string `json:"etag,omitempty"`
	LastModified string `json:"last_modified,omitempty"`
}

// HTTPRequest is the Host.HTTP request: the WASM http_request of ADR 0011
// plus the call id.
type HTTPRequest struct {
	CallID     string            `json:"call_id"`
	Method     string            `json:"method"`
	URL        string            `json:"url"`
	Headers    map[string]string `json:"headers"`
	Body       []byte            `json:"body,omitempty"`
	Validators *Validators       `json:"validators,omitempty"`
}

// HTTPError is a request the host refused or could not complete. The codes
// are those of ADR 0011.
type HTTPError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// HTTPResponse is the Host.HTTP response. Any HTTP status is a response.
type HTTPResponse struct {
	Status      int                 `json:"status"`
	Headers     map[string][]string `json:"headers,omitempty"`
	Body        []byte              `json:"body,omitempty"`
	Validators  Validators          `json:"validators"`
	NotModified bool                `json:"not_modified"`
	Error       *HTTPError          `json:"error,omitempty"`
}

// SourceServer is implemented by the plugin.
type SourceServer interface {
	Configure(ctx context.Context, req *ConfigureRequest) (*Empty, error)
	Validate(ctx context.Context, t *Target) (*ValidateResponse, error)
	List(ctx context.Context, req *ListRequest) (*ListResponse, error)
}

// hostServer is implemented by the core.
type hostServer interface {
	HTTP(ctx context.Context, req *HTTPRequest) (*HTTPResponse, error)
}

// unary builds a grpc.MethodDesc for a handler of type S, as protoc-gen-go-grpc
// would generate it.
func unary[S any, Req, Resp any](name, service string, fn func(S, context.Context, *Req) (*Resp, error)) grpc.MethodDesc {
	full := fmt.Sprintf("/%s/%s", service, name)
	return grpc.MethodDesc{
		MethodName: name,
		Handler: func(srv any, ctx context.Context, dec func(any) error, ic grpc.UnaryServerInterceptor) (any, error) {
			in := new(Req)
			if err := dec(in); err != nil {
				return nil, err
			}
			if ic == nil {
				return fn(srv.(S), ctx, in)
			}
			info := &grpc.UnaryServerInfo{Server: srv, FullMethod: full}
			return ic(ctx, in, info, func(ctx context.Context, req any) (any, error) {
				return fn(srv.(S), ctx, req.(*Req))
			})
		},
	}
}

var sourceDesc = grpc.ServiceDesc{
	ServiceName: sourceService,
	HandlerType: (*SourceServer)(nil),
	Methods: []grpc.MethodDesc{
		unary("Configure", sourceService, SourceServer.Configure),
		unary("Validate", sourceService, SourceServer.Validate),
		unary("List", sourceService, SourceServer.List),
	},
	Metadata: "angelia/plugin/v0 (hand-written, JSON codec)",
}

var hostDesc = grpc.ServiceDesc{
	ServiceName: hostService,
	HandlerType: (*hostServer)(nil),
	Methods:     []grpc.MethodDesc{unary("HTTP", hostService, hostServer.HTTP)},
	Metadata:    "angelia/plugin/v0 (hand-written, JSON codec)",
}

func invoke[Resp any](ctx context.Context, cc grpc.ClientConnInterface, service, method string, req any) (*Resp, error) {
	out := new(Resp)
	if err := cc.Invoke(ctx, "/"+service+"/"+method, req, out, callOpt); err != nil {
		return nil, err
	}
	return out, nil
}
