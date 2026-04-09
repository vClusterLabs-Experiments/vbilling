package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/loft-sh/vbilling/internal/config"
	"github.com/loft-sh/vbilling/internal/controller"
	"github.com/loft-sh/vbilling/internal/discovery"
	"github.com/loft-sh/vbilling/internal/lago"
	"github.com/loft-sh/vbilling/internal/metrics"

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

	// Load configuration
	cfg := config.Load()
	if cfg.LagoAPIKey == "" {
		log.Fatal("LAGO_API_KEY is required")
	}
	log.Printf("Lago API: %s", cfg.LagoAPIURL)
	log.Printf("Plan: %s | Currency: %s", cfg.DefaultPlanCode, cfg.BillingCurrency)
	log.Printf("Collection: %s | Reconcile: %s", cfg.CollectionInterval, cfg.ReconcileInterval)

	// Create Kubernetes clients
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

	// Create Lago client
	lagoClient := lago.NewClient(cfg.LagoAPIURL, cfg.LagoAPIKey)

	// Bootstrap Lago with billing configuration
	log.Println("Bootstrapping Lago billing configuration...")
	if err := lago.Bootstrap(lagoClient, cfg); err != nil {
		log.Printf("WARNING: Lago bootstrap failed: %v", err)
		log.Println("The controller will continue but billing may not work correctly.")
		log.Println("Ensure Lago is running and accessible at", cfg.LagoAPIURL)
	}

	// Create components
	disc := discovery.NewDiscoverer(kubeClient, dynamicClient, cfg.WatchNamespaces)
	coll := metrics.NewCollector(kubeClient, metricsClient, cfg.PrometheusURL, cfg.SpotDiscountPercent)
	ctrl := controller.New(cfg, lagoClient, disc, coll)

	// Run with graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

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
	// Try in-cluster config first (running in a pod)
	cfg, err := rest.InClusterConfig()
	if err == nil {
		log.Println("Using in-cluster Kubernetes config")
		return cfg, nil
	}

	// Fall back to kubeconfig file (local development)
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
