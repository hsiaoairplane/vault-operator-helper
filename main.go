package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
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

	clientset *kubernetes.Clientset
	lock      sync.Mutex
)

func init() {
	// Define command-line flags
	flag.StringVar(&configMapName, "configmap-name", "watch-namespace-config", "Name of the ConfigMap to update")
	flag.StringVar(&configMapNamespace, "configmap-namespace", "vault", "Namespace of the ConfigMap")
	flag.StringVar(&labelSelector, "label-selector", "foo=bar", "Label selector for namespaces to watch")
	flag.StringVar(&watchKey, "watch-key", "WATCH_NAMESPACE", "Key in the ConfigMap to store namespace list")
	flag.StringVar(&mainContainerName, "main-container", "main-container", "Name of the main container to restart")

	// Parse flags
	flag.Parse()
}

func main() {
	config, err := getKubeConfig()
	if err != nil {
		fmt.Printf("Error getting Kubernetes config: %v\n", err)
		os.Exit(1)
	}

	clientset, err = kubernetes.NewForConfig(config)
	if err != nil {
		fmt.Printf("Error creating Kubernetes client: %v\n", err)
		os.Exit(1)
	}

	// Ensure the ConfigMap is created/updated at startup
	updateConfigMap()

	// Set up signal handling for graceful shutdown
	stopCh := make(chan struct{})
	signalCh := make(chan os.Signal, 1)

	// Listen for termination signals
	signal.Notify(signalCh, syscall.SIGTERM, syscall.SIGINT)

	// Listen for termination signals
	signal.Notify(signalCh, syscall.SIGTERM, syscall.SIGINT)

	go func() {
		<-signalCh
		fmt.Println("Received termination signal. Shutting down gracefully...")
		close(stopCh) // Stop the namespace watcher
		os.Exit(0)    // Exit the program
	}()

	// Start namespace watcher
	go watchNamespaces(stopCh)

	// Keep the program running
	<-stopCh
}

// getKubeConfig tries in-cluster config first, then falls back to local kubeconfig
func getKubeConfig() (*rest.Config, error) {
	// Try in-cluster config
	config, err := rest.InClusterConfig()
	if err == nil {
		fmt.Println("Using in-cluster configuration")
		return config, nil
	}

	// Fall back to local kubeconfig
	fmt.Println("In-cluster config failed, falling back to local kubeconfig")
	kubeconfig := filepath.Join(os.Getenv("HOME"), ".kube", "config")
	if envKubeconfig := os.Getenv("KUBECONFIG"); envKubeconfig != "" {
		kubeconfig = envKubeconfig
	}

	config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("failed to load kubeconfig: %w", err)
	}

	fmt.Println("Using local kubeconfig")
	return config, nil
}

// watchNamespaces monitors namespace creation and deletion
func watchNamespaces(stopCh chan struct{}) {
	informerFactory := informers.NewSharedInformerFactoryWithOptions(clientset, time.Minute, informers.WithTweakListOptions(func(opts *metav1.ListOptions) {
		opts.LabelSelector = labelSelector
	}))
	namespaceInformer := informerFactory.Core().V1().Namespaces().Informer()

	namespaceInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj interface{}) { updateConfigMap() },
		UpdateFunc: func(oldObj, newObj interface{}) { updateConfigMap() },
		DeleteFunc: func(obj interface{}) { updateConfigMap() },
	})

	informerFactory.Start(stopCh)
	informerFactory.WaitForCacheSync(stopCh)
}

// updateConfigMap updates the ConfigMap with the latest namespace list
func updateConfigMap() {
	lock.Lock()
	defer lock.Unlock()

	namespaces, err := getFilteredNamespaces()
	if err != nil {
		fmt.Printf("Error getting namespaces: %v\n", err)
		return
	}

	newValue := strings.Join(namespaces, ",")

	// Fetch the existing ConfigMap
	cm, err := clientset.CoreV1().ConfigMaps(configMapNamespace).Get(context.TODO(), configMapName, metav1.GetOptions{})
	if err != nil {
		// If ConfigMap doesn't exist, create it
		createConfigMap(newValue)
		return
	}

	// Check if the value has changed
	if cm.Data[watchKey] == newValue {
		return
	}

	// Update ConfigMap
	cm.Data[watchKey] = newValue
	_, err = clientset.CoreV1().ConfigMaps(configMapNamespace).Update(context.TODO(), cm, metav1.UpdateOptions{})
	if err != nil {
		fmt.Printf("Error updating ConfigMap: %v\n", err)
		return
	}

	// Trigger restart of the main container
	killMainContainer()
}

// getFilteredNamespaces retrieves namespaces with the specified label
func getFilteredNamespaces() ([]string, error) {
	namespaces, err := clientset.CoreV1().Namespaces().List(context.TODO(), metav1.ListOptions{
		LabelSelector: labelSelector,
	})
	if err != nil {
		return nil, err
	}

	var nsList []string
	for _, ns := range namespaces.Items {
		nsList = append(nsList, ns.Name)
	}
	return nsList, nil
}

// createConfigMap creates a new ConfigMap
func createConfigMap(value string) {
	cm := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      configMapName,
			Namespace: configMapNamespace,
		},
		Data: map[string]string{
			watchKey: value,
		},
	}

	_, err := clientset.CoreV1().ConfigMaps(configMapNamespace).Create(context.TODO(), cm, metav1.CreateOptions{})
	if err != nil {
		fmt.Printf("Error creating ConfigMap: %v\n", err)
	}
}

// killMainContainer kills the main container to reload the ConfigMap
func killMainContainer() {
	fmt.Println("Restarting main container...")

	// Find the main container's PID (assumes PID namespace is shared)
	pid, err := getMainContainerPID()
	if err != nil {
		fmt.Printf("Error getting main container PID: %v\n", err)
		return
	}

	// Send SIGTERM to the main container
	err = exec.Command("kill", "-SIGTERM", pid).Run()
	if err != nil {
		fmt.Printf("Error sending SIGTERM to main container: %v\n", err)
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
