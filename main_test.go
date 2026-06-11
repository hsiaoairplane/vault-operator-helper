package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
)

// setupTest configures the package globals for a test run and returns the fake
// clientset so assertions can be made against it. A namespace lister backed by
// the fake clientset is wired up the same way production does, and the
// main-container restart is stubbed out so tests never shell out to pgrep/kill.
func setupTest(t *testing.T, objects ...runtime.Object) *fake.Clientset {
	t.Helper()

	cs := fake.NewSimpleClientset(objects...)

	configMapName = "watch-namespace-config"
	configMapNamespace = "vault"
	watchKey = "WATCH_NAMESPACE"
	// Empty selector matches every namespace; the lister applies the selector
	// in-memory, so tests stay independent of any pre-filtering.
	labelSelector = ""

	clientset = cs

	// Build and sync a namespace lister from the fake clientset.
	factory := informers.NewSharedInformerFactory(cs, 0)
	namespaceLister = factory.Core().V1().Namespaces().Lister()
	stopCh := make(chan struct{})
	factory.Start(stopCh)
	factory.WaitForCacheSync(stopCh)

	restartMainContainer = func() {}
	t.Cleanup(func() {
		close(stopCh)
		restartMainContainer = killMainContainer
		namespaceLister = nil
	})

	return cs
}

func TestGetFilteredNamespaces(t *testing.T) {
	setupTest(t,
		&v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-b"}},
		&v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-a"}},
	)

	got, err := getFilteredNamespaces()
	if err != nil {
		t.Fatalf("getFilteredNamespaces returned error: %v", err)
	}

	// Result must be sorted for a stable, restart-friendly ConfigMap value.
	want := []string{"team-a", "team-b"}
	if len(got) != len(want) {
		t.Fatalf("got %d namespaces (%v), want %v", len(got), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got %v, want %v (not sorted)", got, want)
			break
		}
	}
}

func TestUpdateConfigMap_CreatesWhenMissing(t *testing.T) {
	cs := setupTest(t,
		&v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-a"}},
	)

	updateConfigMap(context.Background())

	cm, err := cs.CoreV1().ConfigMaps(configMapNamespace).Get(context.TODO(), configMapName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("expected ConfigMap to be created, got error: %v", err)
	}
	if cm.Data[watchKey] != "team-a" {
		t.Errorf("got %q, want %q", cm.Data[watchKey], "team-a")
	}
}

// TestUpdateConfigMap_NilData is a regression test: updating an existing
// ConfigMap whose Data map is nil must not panic.
func TestUpdateConfigMap_NilData(t *testing.T) {
	cs := setupTest(t,
		&v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-a"}},
		&v1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "watch-namespace-config", Namespace: "vault"},
			// Data intentionally nil.
		},
	)

	// Would panic before the nil-map guard was added.
	updateConfigMap(context.Background())

	cm, err := cs.CoreV1().ConfigMaps(configMapNamespace).Get(context.TODO(), configMapName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get ConfigMap: %v", err)
	}
	if cm.Data[watchKey] != "team-a" {
		t.Errorf("got %q, want %q", cm.Data[watchKey], "team-a")
	}
}

func TestHealthHandler(t *testing.T) {
	rec := httptest.NewRecorder()
	healthHandler(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("healthHandler status = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestUpdateConfigMap_NoChangeKeepsValue(t *testing.T) {
	cs := setupTest(t,
		&v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-a"}},
		&v1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "watch-namespace-config", Namespace: "vault"},
			Data:       map[string]string{"WATCH_NAMESPACE": "team-a"},
		},
	)

	updateConfigMap(context.Background())

	cm, err := cs.CoreV1().ConfigMaps(configMapNamespace).Get(context.TODO(), configMapName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get ConfigMap: %v", err)
	}
	if cm.Data[watchKey] != "team-a" {
		t.Errorf("got %q, want %q", cm.Data[watchKey], "team-a")
	}
}
