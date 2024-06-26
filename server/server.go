package server

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	go_cookiecutterv1 "github.com/shanipribadi/go-cookiecutter/gen/shanipribadi/go-cookiecutter/v1"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"

	"golang.org/x/sync/errgroup"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"

	"github.com/cloudflare/certinel/fswatcher"
	"github.com/rs/zerolog"
)

type Server struct {
	config *ServerConfig
	deps   *ServerDependencies
	log    zerolog.Logger
}

type ServerConfig struct {
	ListenAddress    string
	TlsListenAddress string
	TlsPrivateKey    string
	TlsCertificate   string
}

type ServerDependencies struct {
	Logger              zerolog.Logger
	CookieCutterService *CookieCutterService
}

type CookieCutterService struct {
	go_cookiecutterv1.UnimplementedCookieCutterServiceServer
}

func New(cfg *ServerConfig, deps *ServerDependencies) *Server {
	return &Server{
		config: cfg,
		deps:   deps,
		log:    deps.Logger,
	}
}

func (s *Server) Start(ctx context.Context) error {
	g, ctx := errgroup.WithContext(ctx)

	opts := []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}

	cc, err := grpc.DialContext(ctx, s.config.ListenAddress, opts...)
	if err != nil {
		return err
	}
	defer cc.Close()

	mux := runtime.NewServeMux(
		runtime.WithHealthzEndpoint(grpc_health_v1.NewHealthClient(cc)),
	)

	healthSrv := health.NewServer()
	grpcSrv := grpc.NewServer()
	grpc_health_v1.RegisterHealthServer(grpcSrv, healthSrv)

	/// XXX: your service registration
	err = go_cookiecutterv1.RegisterCookieCutterServiceHandler(ctx, mux, cc)
	if err != nil {
		return err
	}
	go_cookiecutterv1.RegisterCookieCutterServiceServer(grpcSrv, s.deps.CookieCutterService)

	reflection.Register(grpcSrv)

	lc := net.ListenConfig{}
	listener, err := lc.Listen(ctx, "tcp", s.config.ListenAddress)
	if err != nil {
		return err
	}

	h2cSrv := h2c.NewHandler(&grpcRouter{grpc: grpcSrv, http: mux}, &http2.Server{})
	handler := http.MaxBytesHandler(h2cSrv, 10e6)

	srv := &http.Server{
		Handler:        handler,
		ReadTimeout:    10 * time.Second,
		WriteTimeout:   10 * time.Second,
		MaxHeaderBytes: 1 << 20,
	}

	if s.config.TlsPrivateKey != "" && s.config.TlsCertificate != "" {
		certinel, err := fswatcher.New(s.config.TlsCertificate, s.config.TlsPrivateKey)
		if err != nil {
			return err
		}

		g.Go(func() error {
			return certinel.Start(ctx)
		})

		tlsCfg := &tls.Config{
			GetCertificate: certinel.GetCertificate,
			NextProtos:     []string{"h2", "http/1.1"},
		}

		tlsListener, err := lc.Listen(ctx, "tcp", s.config.TlsListenAddress)
		if err != nil {
			return err
		}
		tlsListener = tls.NewListener(tlsListener, tlsCfg)
		// Start HTTP server (and proxy calls to gRPC server endpoint)
		g.Go(func() error {
			return srv.Serve(tlsListener)
		})
	}

	// Start HTTP server (and proxy calls to gRPC server endpoint)
	g.Go(func() error {
		return srv.Serve(listener)
	})

	g.Go(func() error {
		<-ctx.Done()
		healthSrv.Shutdown()
		time.Sleep(time.Second)
		ctxShutdown, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		err := srv.Shutdown(ctxShutdown)
		return err
	})

	return g.Wait()
}

type grpcRouter struct {
	grpc *grpc.Server
	http http.Handler
}

func (gr *grpcRouter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.ProtoMajor == 2 && strings.HasPrefix(
		r.Header.Get("Content-Type"), "application/grpc") {
		gr.grpc.ServeHTTP(w, r)
	} else {
		gr.http.ServeHTTP(w, r)
	}
}
