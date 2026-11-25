//go:build integration

package controller

import (
	"context"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	corev1 "k8s.io/api/core/v1"
	kerror "k8s.io/apimachinery/pkg/api/errors"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// Exercise the executable's startup path, not just a separately constructed
// manager: startup must work without adding cluster-wide Namespace access.
func TestStartupWithBundledRBAC(t *testing.T) {
	binary := buildStartupBinary(t)
	admin, target, configPath := startStartupEnv(t)

	t.Run("valid default target reaches readiness", func(t *testing.T) {
		runReadinessStartup(t, binary, configPath)
	})
	if err := admin.Delete(t.Context(), target); err != nil {
		t.Fatal(err)
	}
	deleting := Resource(BackupTargetGVK)
	deleting.SetName("deleting")
	deleting.SetNamespace("longhorn")
	deleting.SetFinalizers([]string{"hornkeeper.noqer.com/test"})
	if err := admin.Create(t.Context(), deleting); err != nil {
		t.Fatal(err)
	}
	if err := admin.Delete(t.Context(), deleting); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, target, reason string
	}{
		{name: "missing default target", target: "default", reason: "not found"},
		{name: "deleting target", target: "deleting", reason: "is being deleted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runInvalidStartupValidation(t, binary, configPath, tc.target, tc.reason)
		})
	}
}

// buildStartupBinary builds the hornkeeper executable for startup tests.
func buildStartupBinary(t *testing.T) string {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "hornkeeper")
	build := exec.CommandContext(
		t.Context(),
		"go",
		"build",
		"-race",
		"-o",
		binary,
		"../../cmd/hornkeeper",
	)
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build hornkeeper: %v\n%s", err, output)
	}

	return binary
}

// startStartupEnv starts the API server, applies the example RBAC, seeds the
// default backup target and writes the isolated kubeconfig. It returns the
// admin client and the target.
func startStartupEnv(t *testing.T) (client.Client, *unstructured.Unstructured, string) {
	t.Helper()
	environment := &envtest.Environment{
		CRDDirectoryPaths:     []string{"testdata"},
		ErrorIfCRDPathMissing: true,
		UseExistingCluster:    new(false),
	}
	cfg, err := environment.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := environment.Stop(); err != nil {
			t.Error(err)
		}
	})
	scheme := k8sruntime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	admin, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	if err := admin.Create(
		t.Context(),
		&corev1.Namespace{Name: "longhorn"},
	); err != nil {
		t.Fatal(err)
	}
	applyExample(t, admin, "../../deploy/namespace.yaml")
	applyExample(t, admin, "../../deploy/rbac.yaml")
	target := Resource(BackupTargetGVK)
	target.SetName("default")
	target.SetNamespace("longhorn")
	if err := admin.Create(t.Context(), target); err != nil {
		t.Fatal(err)
	}
	saConfig := rest.CopyConfig(cfg)
	saConfig.Impersonate = rest.ImpersonationConfig{
		UserName: "system:serviceaccount:hornkeeper:hornkeeper",
		Groups: []string{
			"system:serviceaccounts",
			"system:serviceaccounts:hornkeeper",
			"system:authenticated",
		},
	}
	sa, err := client.New(saConfig, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(
		t,
		func() bool { return CheckBackupTarget(t.Context(), sa, "longhorn", "default") == nil },
		"bundled RBAC propagation",
	)
	if err := sa.Get(
		t.Context(),
		client.ObjectKey{Name: "longhorn"},
		&corev1.Namespace{},
	); !kerror.IsForbidden(
		err,
	) {
		t.Fatalf("bundled RBAC must deny Namespace GET, got %v", err)
	}
	// Supply only the isolated API server, with the bundled ServiceAccount's
	// permissions. Never read or modify the developer's current kubeconfig.
	kubeconfig := clientcmdapi.NewConfig()
	kubeconfig.Clusters["test"] = &clientcmdapi.Cluster{
		Server:                   cfg.Host,
		CertificateAuthorityData: cfg.CAData,
	}
	kubeconfig.AuthInfos["test"] = &clientcmdapi.AuthInfo{
		ClientCertificateData: cfg.CertData, ClientKeyData: cfg.KeyData,
		Impersonate: saConfig.Impersonate.UserName, ImpersonateGroups: saConfig.Impersonate.Groups,
	}
	kubeconfig.Contexts["test"] = &clientcmdapi.Context{Cluster: "test", AuthInfo: "test"}
	kubeconfig.CurrentContext = "test"
	configPath := filepath.Join(t.TempDir(), "kubeconfig")
	if err := clientcmd.WriteToFile(*kubeconfig, configPath); err != nil {
		t.Fatal(err)
	}

	return admin, target, configPath
}

// runReadinessStartup starts the binary and waits for HTTP readiness.
func runReadinessStartup(t *testing.T, binary, configPath string) {
	t.Helper()
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	// Keep the process alive through test cleanup so we can verify graceful
	// shutdown and any race-detector exit status instead of killing it.
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(
		ctx,
		binary,
		"--kubeconfig="+configPath,
		"--metrics-bind-address=0",
		"--health-probe-bind-address="+address,
	)
	logPath := filepath.Join(t.TempDir(), "hornkeeper.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = logFile.Close() })
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	var exitErr error
	go func() {
		exitErr = cmd.Wait()
		close(done)
	}()
	logs := func() string {
		data, err := os.ReadFile(logPath)
		if err != nil {
			return err.Error()
		}
		return string(data)
	}
	t.Cleanup(func() {
		stopStartupProcess(t, cmd, done, exitErr, cancel, logs)
	})
	httpClient := &http.Client{Timeout: time.Second}
	waitForReadiness(ctx, t, httpClient, address, done, exitErr, logs)
}

// stopStartupProcess interrupts the binary and waits for its shutdown.
func stopStartupProcess(
	t *testing.T,
	cmd *exec.Cmd,
	done <-chan struct{},
	exitErr error,
	cancel context.CancelFunc,
	logs func() string,
) {
	t.Helper()
	_ = cmd.Process.Signal(os.Interrupt)
	select {
	case <-done:
		if exitErr != nil && !t.Failed() {
			t.Errorf("hornkeeper shutdown failed: %v\n%s", exitErr, logs())
		}
	case <-time.After(10 * time.Second):
		cancel()
		<-done
		t.Errorf("hornkeeper shutdown timed out\n%s", logs())
	}
}

// waitForReadiness polls the readiness endpoint until it serves or the
// process dies or the context expires first.
func waitForReadiness(
	ctx context.Context,
	t *testing.T,
	httpClient *http.Client,
	address string,
	done <-chan struct{},
	exitErr error,
	logs func() string,
) {
	t.Helper()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			t.Fatalf("hornkeeper exited before readiness: %v\n%s", exitErr, logs())
		case <-ctx.Done():
			t.Fatalf("hornkeeper never became ready: %v\n%s", ctx.Err(), logs())
		case <-ticker.C:
			request, err := http.NewRequestWithContext(
				t.Context(),
				http.MethodGet,
				"http://"+address+"/readyz",
				http.NoBody,
			)
			if err != nil {
				t.Fatal(err)
			}
			response, err := httpClient.Do(request)
			if err == nil {
				_ = response.Body.Close()
				if response.StatusCode == http.StatusOK {
					return
				}
			}
		}
	}
}

// runInvalidStartupValidation runs the binary with an invalid default target.
func runInvalidStartupValidation(t *testing.T, binary, configPath, target, reason string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	cmd := exec.CommandContext(
		ctx,
		binary,
		"--kubeconfig="+configPath,
		"--default-backup-target="+target,
		"--metrics-bind-address=0",
		"--health-probe-bind-address=0",
	)
	output, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("startup validation did not exit: %v\n%s", ctx.Err(), output)
	}
	if err == nil ||
		!strings.Contains(string(output), "validate default backup target at startup") ||
		!strings.Contains(string(output), "longhorn/"+target) ||
		!strings.Contains(string(output), reason) {
		t.Fatalf("expected actionable startup validation failure, got %v\n%s", err, output)
	}
}
