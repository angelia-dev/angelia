package goplugin

import (
	"context"
	"errors"
	"sync"

	goplugin "github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"
)

// Plugin is what a source plugin binary implements. Serve wraps it in the
// gRPC services.
type Plugin interface {
	// Configure receives the plugin config map, once, before any call.
	Configure(cfg map[string]string) error
	// Validate returns why t is invalid, or "" if it is valid. No network.
	Validate(t Target) string
	// List lists the versions of req.Target. Every HTTP request goes
	// through host, which is bound to this call.
	List(ctx context.Context, host *Host, req *ListRequest) (*ListResponse, error)
}

// Host is the core's HTTP service as a plugin sees it during one call.
type Host struct {
	callID string
	cc     grpc.ClientConnInterface
}

// HTTP sends req through the core (allowed_hosts, netguard, budgets).
func (h *Host) HTTP(ctx context.Context, req HTTPRequest) (*HTTPResponse, error) {
	req.CallID = h.callID
	return invoke[HTTPResponse](ctx, h.cc, hostService, "HTTP", &req)
}

// Serve runs p as a go-plugin plugin process. It blocks until the core
// kills the process; call it from main.
func Serve(p Plugin) {
	goplugin.Serve(&goplugin.ServeConfig{
		HandshakeConfig: Handshake(),
		Plugins:         goplugin.PluginSet{PluginName: &sourcePlugin{impl: p}},
		GRPCServer:      goplugin.DefaultGRPCServer,
	})
}

// sourcePlugin is the go-plugin glue on both sides: GRPCServer runs in the
// plugin, GRPCClient in the core.
type sourcePlugin struct {
	goplugin.NetRPCUnsupportedPlugin
	impl Plugin
}

func (p *sourcePlugin) GRPCServer(b *goplugin.GRPCBroker, s *grpc.Server) error {
	s.RegisterService(&sourceDesc, &pluginServer{impl: p.impl, broker: b})
	return nil
}

func (p *sourcePlugin) GRPCClient(_ context.Context, b *goplugin.GRPCBroker, cc *grpc.ClientConn) (any, error) {
	return &remote{cc: cc, broker: b}, nil
}

// remote is the core's handle on a running plugin.
type remote struct {
	cc     *grpc.ClientConn
	broker *goplugin.GRPCBroker
}

// pluginServer adapts a Plugin to SourceServer.
type pluginServer struct {
	impl   Plugin
	broker *goplugin.GRPCBroker

	mu   sync.Mutex
	host *grpc.ClientConn
}

var errNotConfigured = errors.New("goplugin: plugin not configured")

func (s *pluginServer) Configure(_ context.Context, req *ConfigureRequest) (*Empty, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.host != nil {
		return nil, errors.New("goplugin: already configured")
	}
	if err := s.impl.Configure(req.Config); err != nil {
		return nil, err
	}
	cc, err := s.broker.Dial(req.BrokerID)
	if err != nil {
		return nil, err
	}
	s.host = cc
	return &Empty{}, nil
}

func (s *pluginServer) Validate(_ context.Context, t *Target) (*ValidateResponse, error) {
	return &ValidateResponse{Reason: s.impl.Validate(*t)}, nil
}

func (s *pluginServer) List(ctx context.Context, req *ListRequest) (*ListResponse, error) {
	s.mu.Lock()
	cc := s.host
	s.mu.Unlock()
	if cc == nil {
		return nil, errNotConfigured
	}
	return s.impl.List(ctx, &Host{callID: req.CallID, cc: cc}, req)
}
