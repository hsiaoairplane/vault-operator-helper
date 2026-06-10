package main

import (
	"context"
	"testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
)

// setupTest configures the package globals for a test run and returns the fake
// clientset so assertions can be made against it. The main-container restart is
// stubbed out so tests never shell out to pgrep/kill.
func setupTest(t *testing.T, objects ...runtime.Object) *fake.Clientset {
	t.Helper()

	cs := fake.NewSimpleClientset(objects...)

	configMapName = "watch-namespace-config"
	configMapNamespace = "vault"
	watchKey = "WATCH_NAMESPACE"
	// Empty selector: the fake clientset does not apply label-selector filtering,
	// so keep tests independent of that behaviour.
	labelSelector = ""

	clientset = cs

	restartMainContainer = func() {}
	t.Cleanup(func() { restartMainContainer = killMainContainer })

	return cs
}

func TestGetFilteredNamespaces(t *testing.T) {
	setupTest(t,
		&v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-a"}},
		&v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-b"}},
	)

	got, err := getFilteredNamespaces()
	if err != nil {
		t.Fatalf("getFilteredNamespaces returned error: %v", err)
	}

	want := map[string]bool{"team-a": true, "team-b": true}
	if len(got) != len(want) {
		t.Fatalf("got %d namespaces (%v), want %d", len(got), got, len(want))
	}
	for _, ns := range got {
		if !want[ns] {
			t.Errorf("unexpected namespace %q in result", ns)
		}
	}
}

func TestUpdateConfigMap_CreatesWhenMissing(t *testing.T) {
	cs := setupTest(t,
		&v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-a"}},
	)

	updateConfigMap()

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
	updateConfigMap()

	cm, err := cs.CoreV1().ConfigMaps(configMapNamespace).Get(context.TODO(), configMapName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get ConfigMap: %v", err)
	}
	if cm.Data[watchKey] != "team-a" {
		t.Errorf("got %q, want %q", cm.Data[watchKey], "team-a")
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

	updateConfigMap()

	cm, err := cs.CoreV1().ConfigMaps(configMapNamespace).Get(context.TODO(), configMapName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get ConfigMap: %v", err)
	}
	if cm.Data[watchKey] != "team-a" {
		t.Errorf("got %q, want %q", cm.Data[watchKey], "team-a")
	}
}
