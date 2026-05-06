package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/vclusterlabs-experiments/vbilling/internal/config"
	"github.com/vclusterlabs-experiments/vbilling/internal/controller"
	"github.com/vclusterlabs-experiments/vbilling/internal/destinations"
	"github.com/vclusterlabs-experiments/vbilling/internal/discovery"
	"github.com/vclusterlabs-experiments/vbilling/internal/metrics"

	// Register built-in adapters via blank import so their init() funcs run.
	_ "github.com/vclusterlabs-experiments/vbilling/internal/destinations/lago"
	_ "github.com/vclusterlabs-experiments/vbilling/internal/destinations/noop"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	metricsclient "k8s.io/metrics/pkg/client/clientset/versioned"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	log.Println("vBilling - vCluster Billing Controller")
	log.Println("=======================================")

	cfg := config.Load()
	log.Printf("Adapter: %s", cfg.Adapter)
	log.Printf("Plan: %s | Currency: %s", cfg.DefaultPlanCode, cfg.BillingCurrency)
	log.Printf("Collection: %s | Reconcile: %s", cfg.CollectionInterval, cfg.ReconcileInterval)

	dest, err := destinations.New(cfg.Adapter, cfg)
	if err != nil {
		log.Fatalf("Failed to initialize billing adapter: %v", err)
	}

	kubeConfig, err := getKubeConfig()
	if err != nil {
		log.Fatalf("Failed to get Kubernetes config: %v", err)
	}

	kubeClient, err := kubernetes.NewForConfig(kubeConfig)
	if err != nil {
		log.Fatalf("Failed to create Kubernetes client: %v", err)
	}

	metricsClient, err := metricsclient.NewForConfig(kubeConfig)
	if err != nil {
		log.Fatalf("Failed to create metrics client: %v", err)
	}

	dynamicClient, err := dynamic.NewForConfig(kubeConfig)
	if err != nil {
		log.Fatalf("Failed to create dynamic client: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	log.Printf("Bootstrapping %s adapter...", dest.Name())
	if err := dest.Bootstrap(ctx); err != nil {
		log.Printf("WARNING: %s bootstrap failed: %v", dest.Name(), err)
		log.Println("The controller will continue but billing may not work correctly.")
	}

	disc := discovery.NewDiscoverer(kubeClient, dynamicClient, cfg.WatchNamespaces)
	coll := metrics.NewCollector(kubeClient, metricsClient, cfg.PrometheusURL, cfg.SpotDiscountPercent)
	ctrl := controller.New(cfg, dest, disc, coll)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		sig := <-sigCh
		log.Printf("Received signal %s, shutting down...", sig)
		cancel()
	}()

	log.Println("Starting billing controller...")
	if err := ctrl.Run(ctx); err != nil && err != context.Canceled {
		log.Fatalf("Controller error: %v", err)
	}

	log.Println("vBilling stopped")
}

func getKubeConfig() (*rest.Config, error) {
	cfg, err := rest.InClusterConfig()
	if err == nil {
		log.Println("Using in-cluster Kubernetes config")
		return cfg, nil
	}

	kubeconfig := os.Getenv("KUBECONFIG")
	if kubeconfig == "" {
		home, _ := os.UserHomeDir()
		kubeconfig = home + "/.kube/config"
	}

	cfg, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return nil, err
	}

	log.Printf("Using kubeconfig: %s", kubeconfig)
	return cfg, nil
}
