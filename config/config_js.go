//go:build js

package config

import (
	"context"
	"errors"
	"fmt"
	"net"

	"github.com/libp2p/go-libp2p/core/connmgr"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/pnet"
	"github.com/libp2p/go-libp2p/core/sec"
	"github.com/libp2p/go-libp2p/core/transport"
	"github.com/libp2p/go-libp2p/p2p/net/swarm"
	tptu "github.com/libp2p/go-libp2p/p2p/net/upgrader"
	circuitv2 "github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/client"
	"github.com/libp2p/go-libp2p/p2p/security/insecure"
	"github.com/libp2p/go-libp2p/p2p/transport/quicreuse"
	"github.com/libp2p/go-libp2p/p2p/transport/tcpreuse"
	"github.com/quic-go/quic-go"
	"go.uber.org/fx"
	"go.uber.org/fx/fxevent"
)

func (cfg *Config) addTransports() ([]fx.Option, error) {
	fxopts := []fx.Option{
		fx.WithLogger(func() fxevent.Logger {
			return &fxevent.SlogLogger{
				Logger: log.With("system", "fx"),
			}
		}),
		fx.Provide(fx.Annotate(tptu.New, fx.ParamTags(`name:"security"`))),
		fx.Supply(cfg.Muxers),
		fx.Provide(func() connmgr.ConnectionGater { return cfg.ConnectionGater }),
		fx.Provide(func() pnet.PSK { return cfg.PSK }),
		fx.Provide(func() network.ResourceManager { return cfg.ResourceManager }),
		fx.Provide(func(upgrader transport.Upgrader) *tcpreuse.ConnMgr {
			if !cfg.ShareTCPListener {
				return nil
			}
			return tcpreuse.NewConnMgr(tcpreuse.EnvReuseportVal, upgrader)
		}),
		fx.Provide(func(cm *quicreuse.ConnManager, sw *swarm.Swarm) func(network string, laddr *net.UDPAddr) (net.PacketConn, error) {
			return func(network string, laddr *net.UDPAddr) (net.PacketConn, error) {
				return net.ListenUDP(network, laddr)
			}
		}),
	}
	fxopts = append(fxopts, cfg.Transports...)
	if cfg.Insecure {
		fxopts = append(fxopts,
			fx.Provide(
				fx.Annotate(
					func(id peer.ID, priv crypto.PrivKey) []sec.SecureTransport {
						return []sec.SecureTransport{insecure.NewWithIdentity(insecure.ID, id, priv)}
					},
					fx.ResultTags(`name:"security"`),
				),
			),
		)
	} else {
		// fx groups are unordered, but we need to preserve the order of the security transports
		// First of all, we construct the security transports that are needed,
		// and save them to a group call security_unordered.
		for _, s := range cfg.SecurityTransports {
			fxName := fmt.Sprintf(`name:"security_%s"`, s.ID)
			fxopts = append(fxopts, fx.Supply(fx.Annotate(s.ID, fx.ResultTags(fxName))))
			fxopts = append(fxopts,
				fx.Provide(fx.Annotate(
					s.Constructor,
					fx.ParamTags(fxName),
					fx.As(new(sec.SecureTransport)),
					fx.ResultTags(`group:"security_unordered"`),
				)),
			)
		}
		// Then we consume the group security_unordered, and order them by the user's preference.
		fxopts = append(fxopts, fx.Provide(
			fx.Annotate(
				func(secs []sec.SecureTransport) ([]sec.SecureTransport, error) {
					if len(secs) != len(cfg.SecurityTransports) {
						return nil, errors.New("inconsistent length for security transports")
					}
					t := make([]sec.SecureTransport, 0, len(secs))
					for _, s := range cfg.SecurityTransports {
						for _, st := range secs {
							if s.ID != st.ID() {
								continue
							}
							t = append(t, st)
						}
					}
					return t, nil
				},
				fx.ParamTags(`group:"security_unordered"`),
				fx.ResultTags(`name:"security"`),
			)))
	}

	fxopts = append(fxopts, fx.Provide(PrivKeyToStatelessResetKey))
	fxopts = append(fxopts, fx.Provide(PrivKeyToTokenGeneratorKey))
	if cfg.QUICReuse != nil {
		fxopts = append(fxopts, cfg.QUICReuse...)
	} else {
		fxopts = append(fxopts,
			fx.Provide(func(key quic.StatelessResetKey, tokenGenerator quic.TokenGeneratorKey, rcmgr network.ResourceManager, lifecycle fx.Lifecycle) (*quicreuse.ConnManager, error) {
				opts := []quicreuse.Option{
					quicreuse.ConnContext(func(ctx context.Context, clientInfo *quic.ClientInfo) (context.Context, error) {
						// even if creating the quic maddr fails, let the rcmgr decide what to do with the connection
						addr, err := quicreuse.ToQuicMultiaddr(clientInfo.RemoteAddr, quic.Version1)
						if err != nil {
							addr = nil
						}
						scope, err := rcmgr.OpenConnection(network.DirInbound, false, addr)
						if err != nil {
							return ctx, err
						}
						ctx = network.WithConnManagementScope(ctx, scope)
						context.AfterFunc(ctx, func() {
							scope.Done()
						})
						return ctx, nil
					}),
					quicreuse.VerifySourceAddress(func(addr net.Addr) bool {
						return rcmgr.VerifySourceAddress(addr)
					}),
				}
				if !cfg.DisableMetrics {
					opts = append(opts, quicreuse.EnableMetrics(cfg.PrometheusRegisterer))
				}
				cm, err := quicreuse.NewConnManager(key, tokenGenerator, opts...)
				if err != nil {
					return nil, err
				}
				lifecycle.Append(fx.StopHook(cm.Close))
				return cm, nil
			}),
		)
	}

	fxopts = append(fxopts, fx.Invoke(
		fx.Annotate(
			func(swrm *swarm.Swarm, tpts []transport.Transport) error {
				for _, t := range tpts {
					if err := swrm.AddTransport(t); err != nil {
						return err
					}
				}
				return nil
			},
			fx.ParamTags("", `group:"transport"`),
		)),
	)
	if cfg.Relay {
		fxopts = append(fxopts, fx.Invoke(circuitv2.AddTransport))
	}
	return fxopts, nil
}
