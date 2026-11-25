package helm_test

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/zekihan/hornkeeper/internal/config"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/yaml"
)

func helmBinary(t *testing.T) string {
	t.Helper()
	helm := os.Getenv("HELM")
	if helm == "" {
		var err error
		helm, err = exec.LookPath("helm")
		if err != nil {
			t.Skip("Helm is unavailable; make chart-test runs this regression with HELM set")
		}
	}
	return helm
}

// Helm's renderer accepts YAML scalars that Kubernetes cannot decode into string
// fields and resource names that Kubernetes can decode but rejects on admission.
func TestRenderedChartKubernetesNames(t *testing.T) {
	helm := helmBinary(t)
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, appsv1.AddToScheme, rbacv1.AddToScheme, policyv1.AddToScheme} {
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
		{name: "dotted release", release: "ops.test", namespace: "hornkeeper"},
		{name: "truncation through consecutive hyphens", release: "volumes", namespace: "hornkeeper", values: []string{"nameOverride=" + strings.Repeat("a", 53) + "--b"}},
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
				if err := docs.Decode(&raw); errors.Is(err, io.EOF) {
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
				name := metadata.GetName()
				var nameErrors []string
				switch obj.(type) {
				case *corev1.Service:
					nameErrors = validation.IsDNS1123Label(name)
				case *appsv1.Deployment, *corev1.ServiceAccount, *policyv1.PodDisruptionBudget:
					nameErrors = validation.IsDNS1123Subdomain(name)
				}
				if len(nameErrors) != 0 {
					t.Errorf("%s name %q is invalid: %v", gvk.Kind, name, nameErrors)
				}
				count++
			}
			if count != 10 {
				t.Fatalf("decoded %d resources, want 10", count)
			}
		})
	}
}

func TestBackupTargetMatchesRuntime(t *testing.T) {
	helm := helmBinary(t)
	for _, tc := range []struct {
		name, target string
		valid        bool
	}{
		{"uppercase", "Backup", false},
		{"underscore", "backup_target", false},
		{"empty DNS component", "backup..target", false},
		{"leading component hyphen", "backup.-target", false},
		{"trailing component hyphen", "backup-.target", false},
		{"empty", "", false},
		{"one character", "a", true},
		{"63 characters", strings.Repeat("a", 63), true},
		{"63 characters with dot", strings.Repeat("a", 31) + "." + strings.Repeat("b", 31), true},
		{"64 characters", strings.Repeat("a", 64), true},
		{"253 characters", strings.Repeat("a", 253), true},
		{"254 characters", strings.Repeat("a", 254), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Default()
			cfg.BackupTarget = tc.target
			if err := cfg.Validate(); (err == nil) != tc.valid {
				t.Fatalf("runtime validation = %v, valid = %v", err, tc.valid)
			}
			output, err := exec.CommandContext(t.Context(), helm, "template", "hornkeeper", "../../charts/hornkeeper", "--set-string", "controller.defaultBackupTarget="+tc.target).CombinedOutput()
			if (err == nil) != tc.valid {
				t.Fatalf("chart validation = %v, valid = %v\n%s", err, tc.valid, output)
			}
			if !tc.valid && (!bytes.Contains(output, []byte("schema")) || !bytes.Contains(output, []byte("defaultBackupTarget"))) {
				t.Fatalf("expected target schema validation error, got %s", output)
			}
		})
	}
}
