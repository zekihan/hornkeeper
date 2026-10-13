package helm_test

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/util/yaml"
)

// Helm's renderer accepts YAML scalars that Kubernetes cannot decode into string
// fields. Decode every resource through the actual Kubernetes API types as well.
func TestRenderedChartDecodesKubernetesStrings(t *testing.T) {
	helm := os.Getenv("HELM")
	if helm == "" {
		var err error
		helm, err = exec.LookPath("helm")
		if err != nil {
			t.Skip("Helm is unavailable; make chart-test runs this regression with HELM set")
		}
	}
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, appsv1.AddToScheme, rbacv1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	decoder := serializer.NewCodecFactory(scheme, serializer.EnableStrict).UniversalDeserializer()
	for _, tc := range []struct {
		name, release, namespace string
		values                   []string
	}{
		{name: "numeric release", release: "2026", namespace: "hornkeeper"},
		{name: "boolean release", release: "on", namespace: "hornkeeper"},
		{name: "boolean name override", release: "hornkeeper", namespace: "hornkeeper", values: []string{"nameOverride=on"}},
		{name: "numeric fullname", release: "hornkeeper", namespace: "hornkeeper", values: []string{"fullnameOverride=123"}},
		{name: "boolean service account", release: "hornkeeper", namespace: "hornkeeper", values: []string{"serviceAccount.name=yes"}},
		{name: "numeric namespace", release: "hornkeeper", namespace: "2026", values: []string{"controller.longhornNamespace=123"}},
		{name: "boolean namespace", release: "hornkeeper", namespace: "on", values: []string{"controller.longhornNamespace=off"}},
		{name: "combined scalar strings", release: "2026", namespace: "on", values: []string{"nameOverride=off", "fullnameOverride=yes", "serviceAccount.name=123", "controller.longhornNamespace=2026"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := []string{"template", tc.release, "../../charts/hornkeeper", "--namespace", tc.namespace, "--set", "metrics.enabled=true"}
			for _, value := range tc.values {
				args = append(args, "--set-string", value)
			}
			output, err := exec.CommandContext(t.Context(), helm, args...).CombinedOutput()
			if err != nil {
				t.Fatalf("helm template: %v\n%s", err, output)
			}
			docs := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(output), 4096)
			count := 0
			for {
				var raw runtime.RawExtension
				if err := docs.Decode(&raw); err == io.EOF {
					break
				} else if err != nil {
					t.Fatal(err)
				}
				if len(raw.Raw) == 0 {
					continue
				}
				obj, gvk, err := decoder.Decode(raw.Raw, nil, nil)
				if err != nil {
					t.Fatalf("Kubernetes decoding failed: %v\n%s", err, raw.Raw)
				}
				metadata := obj.(metav1.Object)
				if got := metadata.GetLabels()["app.kubernetes.io/instance"]; got != tc.release {
					t.Errorf("%s release label = %q, want %q", gvk.Kind, got, tc.release)
				}
				count++
			}
			if count != 9 {
				t.Fatalf("decoded %d resources, want 9", count)
			}
		})
	}
}
