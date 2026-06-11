package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
)

var (
	configMapName      string
	configMapNamespace string
	labelSelector      string
	watchKey           string
	mainContainerName  string
	healthAddr         string

	clientset       kubernetes.Interface
	namespaceLister corelisters.NamespaceLister
	lock            sync.Mutex

	// restartMainContainer is the action used to reload the ConfigMap.
	// It is a variable so tests can override it.
	restartMainContainer = killMainContainer
)

func init() {
	// Define command-line flags
	flag.StringVar(&configMapName, "configmap-name", "watch-namespace-config", "Name of the ConfigMap to update")
	flag.StringVar(&configMapNamespace, "configmap-namespace", "vault", "Namespace of the ConfigMap")
	flag.StringVar(&labelSelector, "label-selector", "foo=bar", "Label selector for namespaces to watch")
	flag.StringVar(&watchKey, "watch-key", "WATCH_NAMESPACE", "Key in the ConfigMap to store namespace list")
	flag.StringVar(&mainContainerName, "main-container", "main-container", "Name of the main container to restart")
	flag.StringVar(&healthAddr, "health-addr", ":8081", "Address for the liveness (/healthz) and readiness (/readyz) probe server")
}

func main() {
	// Parse flags
	flag.Parse()

	config, err := getKubeConfig()
	if err != nil {
		log.Fatalf("Error getting Kubernetes config: %v", err)
	}

	clientset, err = kubernetes.NewForConfig(config)
	if err != nil {
		log.Fatalf("Error creating Kubernetes client: %v", err)
	}

	// Cancel the context on SIGTERM/SIGINT for graceful shutdown.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	// Serve the liveness probe for the duration of the process.
	startHealthServer()

	// Start the namespace watcher and wait for its cache to sync.
	if err := watchNamespaces(ctx); err != nil {
		log.Fatalf("Error starting namespace watcher: %v", err)
	}

	// Ensure the ConfigMap reflects the current state once the cache is synced.
	// This also covers the case where no matching namespace exists yet, for
	// which no informer Add event would fire.
	updateConfigMap(ctx)

	// Block until a termination signal cancels the context.
	<-ctx.Done()
	log.Println("Received termination signal. Shutting down gracefully...")
}

// getKubeConfig tries in-cluster config first, then falls back to local kubeconfig
func getKubeConfig() (*rest.Config, error) {
	// Try in-cluster config
	config, err := rest.InClusterConfig()
	if err == nil {
		log.Println("Using in-cluster configuration")
		return config, nil
	}

	// Fall back to local kubeconfig
	log.Println("In-cluster config failed, falling back to local kubeconfig")
	kubeconfig := filepath.Join(os.Getenv("HOME"), ".kube", "config")
	if envKubeconfig := os.Getenv("KUBECONFIG"); envKubeconfig != "" {
		kubeconfig = envKubeconfig
	}

	config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("failed to load kubeconfig: %w", err)
	}

	log.Println("Using local kubeconfig")
	return config, nil
}

// healthHandler returns 200 once the process is running. It is used for the
// liveness probe.
func healthHandler(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// startHealthServer runs an HTTP server exposing the /healthz liveness probe.
func startHealthServer() {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", healthHandler)

	go func() {
		log.Printf("Serving health probe on %s", healthAddr)
		if err := http.ListenAndServe(healthAddr, mux); err != nil && err != http.ErrServerClosed {
			log.Printf("Health server error: %v", err)
		}
	}()
}

// watchNamespaces starts an informer that monitors namespace changes and keeps
// the ConfigMap in sync. It returns once the informer cache has synced; the
// informer keeps running in the background until ctx is cancelled.
func watchNamespaces(ctx context.Context) error {
	informerFactory := informers.NewSharedInformerFactoryWithOptions(clientset, time.Minute, informers.WithTweakListOptions(func(opts *metav1.ListOptions) {
		opts.LabelSelector = labelSelector
	}))
	namespaceInformer := informerFactory.Core().V1().Namespaces()
	namespaceLister = namespaceInformer.Lister()

	namespaceInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj interface{}) { updateConfigMap(ctx) },
		UpdateFunc: func(oldObj, newObj interface{}) { updateConfigMap(ctx) },
		DeleteFunc: func(obj interface{}) { updateConfigMap(ctx) },
	})

	informerFactory.Start(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(), namespaceInformer.Informer().HasSynced) {
		return fmt.Errorf("failed to sync namespace informer cache")
	}
	return nil
}

// updateConfigMap updates the ConfigMap with the latest namespace list
func updateConfigMap(ctx context.Context) {
	lock.Lock()
	defer lock.Unlock()

	namespaces, err := getFilteredNamespaces()
	if err != nil {
		log.Printf("Error getting namespaces: %v", err)
		return
	}

	newValue := strings.Join(namespaces, ",")

	// Fetch the existing ConfigMap
	cm, err := clientset.CoreV1().ConfigMaps(configMapNamespace).Get(ctx, configMapName, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			// If ConfigMap doesn't exist, create it
			createConfigMap(ctx, newValue)
		} else {
			log.Printf("Error getting ConfigMap: %v", err)
		}
		return
	}

	// Check if the value has changed
	if cm.Data[watchKey] == newValue {
		return
	}

	// Update ConfigMap (Data may be nil if the ConfigMap was created without any data)
	if cm.Data == nil {
		cm.Data = make(map[string]string)
	}
	cm.Data[watchKey] = newValue
	if _, err := clientset.CoreV1().ConfigMaps(configMapNamespace).Update(ctx, cm, metav1.UpdateOptions{}); err != nil {
		log.Printf("Error updating ConfigMap: %v", err)
		return
	}

	// Trigger restart of the main container
	restartMainContainer()
}

// getFilteredNamespaces retrieves namespaces with the specified label from the
// informer cache. The result is sorted so the joined value is stable and does
// not trigger spurious ConfigMap updates (and container restarts) caused by
// non-deterministic cache iteration order.
func getFilteredNamespaces() ([]string, error) {
	selector, err := labels.Parse(labelSelector)
	if err != nil {
		return nil, fmt.Errorf("invalid label selector %q: %w", labelSelector, err)
	}

	namespaces, err := namespaceLister.List(selector)
	if err != nil {
		return nil, err
	}

	nsList := make([]string, 0, len(namespaces))
	for _, ns := range namespaces {
		nsList = append(nsList, ns.Name)
	}
	sort.Strings(nsList)
	return nsList, nil
}

// createConfigMap creates a new ConfigMap
func createConfigMap(ctx context.Context, value string) {
	cm := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      configMapName,
			Namespace: configMapNamespace,
		},
		Data: map[string]string{
			watchKey: value,
		},
	}

	if _, err := clientset.CoreV1().ConfigMaps(configMapNamespace).Create(ctx, cm, metav1.CreateOptions{}); err != nil {
		log.Printf("Error creating ConfigMap: %v", err)
	}
}

// killMainContainer kills the main container to reload the ConfigMap
func killMainContainer() {
	log.Println("Restarting main container...")

	// Find the main container's PID (assumes PID namespace is shared)
	pid, err := getMainContainerPID()
	if err != nil {
		log.Printf("Error getting main container PID: %v", err)
		return
	}

	// Send SIGTERM to the main container
	if err := exec.Command("kill", "-SIGTERM", pid).Run(); err != nil {
		log.Printf("Error sending SIGTERM to main container: %v", err)
	}
}

// getMainContainerPID finds the process ID of the main container
func getMainContainerPID() (string, error) {
	out, err := exec.Command("pgrep", "-f", mainContainerName).Output()
	if err != nil {
		return "", err
	}

	// Return the first PID found
	pidList := strings.Fields(string(out))
	if len(pidList) > 0 {
		return pidList[0], nil
	}
	return "", fmt.Errorf("main container PID not found")
}
