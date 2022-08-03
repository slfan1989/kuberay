package main

import (
	"context"
	"flag"
	"math"
	"net"
	"net/http"
	"path"
	"strings"

	assetfs "github.com/elazarl/go-bindata-assetfs"
	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"
	"google.golang.org/protobuf/encoding/protojson"
	"k8s.io/klog/v2"

	grpc_middleware "github.com/grpc-ecosystem/go-grpc-middleware"
	grpc_prometheus "github.com/grpc-ecosystem/go-grpc-prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/ray-project/kuberay/apiserver/pkg/interceptor"
	"github.com/ray-project/kuberay/apiserver/pkg/manager"
	"github.com/ray-project/kuberay/apiserver/pkg/server"
	"github.com/ray-project/kuberay/apiserver/pkg/swagger"
	api "github.com/ray-project/kuberay/proto/go_client"
)

var (
	rpcPortFlag        = flag.String("rpcPortFlag", ":8887", "RPC Port")
	httpPortFlag       = flag.String("httpPortFlag", ":8888", "Http Proxy Port")
	collectMetricsFlag = flag.Bool("collectMetricsFlag", true, "Whether to collect Prometheus metrics in API server.")
)

func main() {
	flag.Parse()

	clientManager := manager.NewClientManager()
	resourceManager := manager.NewResourceManager(&clientManager)

	go startRpcServer(resourceManager)
	startHttpProxy()
}

type RegisterHttpHandlerFromEndpoint func(ctx context.Context, mux *runtime.ServeMux, endpoint string, opts []grpc.DialOption) error

func startRpcServer(resourceManager *manager.ResourceManager) {
	klog.Info("Starting gRPC server")

	listener, err := net.Listen("tcp", *rpcPortFlag)
	if err != nil {
		klog.Fatalf("Failed to start GPRC server: %v", err)
	}

	s := grpc.NewServer(
		grpc.StreamInterceptor(grpc_prometheus.StreamServerInterceptor),
		grpc.UnaryInterceptor(grpc_middleware.ChainUnaryServer(grpc_prometheus.UnaryServerInterceptor, interceptor.ApiServerInterceptor)),
		grpc.MaxRecvMsgSize(math.MaxInt32))
	api.RegisterClusterServiceServer(s, server.NewClusterServer(resourceManager, &server.ClusterServerOptions{CollectMetrics: *collectMetricsFlag}))
	api.RegisterComputeTemplateServiceServer(s, server.NewComputeTemplateServer(resourceManager, &server.ComputeTemplateServerOptions{CollectMetrics: *collectMetricsFlag}))
	api.RegisterRayJobServiceServer(s, server.NewRayJobServer(resourceManager, &server.JobServerOptions{CollectMetrics: *collectMetricsFlag}))

	// Register reflection service on gRPC server.
	reflection.Register(s)
	// Make sure all of the Prometheus metrics are initialized.
	grpc_prometheus.Register(s)
	// This is to enable `grpc_server_handling_seconds`, otherwise we won't have latency metrics.
	// see https://github.com/grpc-ecosystem/go-grpc-prometheus/blob/master/README.md#histograms for details.
	grpc_prometheus.EnableHandlingTimeHistogram()
	if err := s.Serve(listener); err != nil {
		klog.Fatalf("Failed to serve gRPC listener: %v", err)
	}

	klog.Info("gRPC server started")
}

func startHttpProxy() {
	klog.Info("Starting Http Proxy")

	ctx := context.Background()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Create gRPC HTTP MUX and register services.
	runtimeMux := runtime.NewServeMux(
		runtime.WithMarshalerOption(runtime.MIMEWildcard, &runtime.JSONPb{
			MarshalOptions: protojson.MarshalOptions{
				UseProtoNames:  true,
				UseEnumNumbers: true,
			},
		}),
		runtime.WithErrorHandler(runtime.DefaultHTTPErrorHandler),
	)
	registerHttpHandlerFromEndpoint(api.RegisterClusterServiceHandlerFromEndpoint, "ClusterService", ctx, runtimeMux)
	registerHttpHandlerFromEndpoint(api.RegisterComputeTemplateServiceHandlerFromEndpoint, "ComputeTemplateService", ctx, runtimeMux)
	registerHttpHandlerFromEndpoint(api.RegisterRayJobServiceHandlerFromEndpoint, "JobService", ctx, runtimeMux)

	// Create a top level mux to include both Http gRPC servers and other endpoints like metrics
	topMux := http.NewServeMux()
	// Seems /apis (matches /apis/v1alpha1/clusters) works fine
	topMux.Handle("/", runtimeMux)
	topMux.Handle("/metrics", promhttp.Handler())
	topMux.HandleFunc("/swagger/", serveSwaggerFile)
	serveSwaggerUI(topMux)

	if err := http.ListenAndServe(*httpPortFlag, topMux); err != nil {
		klog.Fatal(err)
	}

	klog.Info("Http Proxy started")
}

func serveSwaggerFile(w http.ResponseWriter, r *http.Request) {
	klog.Info("start serveSwaggerFile")

	if !strings.HasSuffix(r.URL.Path, "swagger.json") {
		klog.Errorf("Not Found: %s", r.URL.Path)
		http.NotFound(w, r)
		return
	}

	p := strings.TrimPrefix(r.URL.Path, "/swagger/")
	// Currently, we copy swagger.json to system root /workspace/proto/swagger/.
	// For the development, you can change path to `../proto/swagger`.
	// TODO(Jeffwan@): fix this later, we should not have dependency on system folder structure.
	p = path.Join("/workspace/proto/swagger/", p)

	klog.Infof("Serving swagger-file: %s", p)
	http.ServeFile(w, r, p)
}

// go-bindata --nocompress --pkg swagger -o pkg/swagger/datafile.go third_party/swagger-ui/...
// We will need to copy third_party folder to `backend` folder when building images
func serveSwaggerUI(mux *http.ServeMux) {
	fileServer := http.FileServer(&assetfs.AssetFS{
		Asset:    swagger.Asset,
		AssetDir: swagger.AssetDir,
		Prefix:   "third_party/swagger-ui",
	})

	prefix := "/swagger-ui/"
	mux.Handle(prefix, http.StripPrefix(prefix, fileServer))
}

func registerHttpHandlerFromEndpoint(handler RegisterHttpHandlerFromEndpoint, serviceName string, ctx context.Context, mux *runtime.ServeMux) {
	endpoint := "localhost" + *rpcPortFlag
	opts := []grpc.DialOption{grpc.WithInsecure(), grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(math.MaxInt32))}

	if err := handler(ctx, mux, endpoint, opts); err != nil {
		klog.Fatalf("Failed to register %v handler: %v", serviceName, err)
	}
}
